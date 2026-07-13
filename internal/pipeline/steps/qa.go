package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// QAStep exercises the merged product the way a human QA engineer would: it
// boots the repository, drives the changed behavior through its real entry
// points, and reports whether the PR achieves its stated goal.
//
// It is an on-demand step (steps.OnDemandSteps): it runs only when a caller
// names it (`no-mistakes axi run --only qa`), never on an ordinary push.
//
// Three properties decide where it sits and what it may assume:
//
//   - It runs AFTER the pr step, because a PR is its input and its output: it
//     reads the run's PR URL and reports back to that PR. Before the pr step
//     there is no PR number to read or write.
//   - It owns no repo knowledge. The four phases below are the whole
//     methodology, and they contain nothing about any particular repository -
//     no framework, no dev server, no browser, no upload command. Everything
//     specific ("this repo needs `rush update`", "the login state comes from
//     X") lives in the repository, reached through qa.instructions, exactly as
//     review.instructions works for the review step.
//   - It never edits code. QA findings are statements about product behavior;
//     answering one by rewriting code is a product decision, so findings park
//     for a human/agent decision instead of being auto-fixed. This is the same
//     stance as auto_fix.review: 0.
type QAStep struct{}

func (s *QAStep) Name() types.StepName { return types.StepQA }

// qaReport is the QA agent's structured output. Verdict is the four-value
// verdict of the methodology; ReportMarkdown is the human-facing report that is
// published to the PR.
type qaReport struct {
	Verdict        string      `json:"verdict"`
	Summary        string      `json:"summary"`
	AchievesGoal   string      `json:"achieves_goal"`
	ReportMarkdown string      `json:"report_markdown"`
	Issues         []qaIssue   `json:"issues"`
	Verified       []string    `json:"verified"`
	Unverified     []qaUnknown `json:"unverified"`
}

type qaIssue struct {
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file"`
	Line        int    `json:"line"`
}

type qaUnknown struct {
	EntryPoint string `json:"entry_point"`
	Reason     string `json:"reason"`
}

var qaReportSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"verdict": {"type": "string", "enum": ["PASS", "PASS_WITH_ISSUES", "FAIL", "PARTIAL"], "description": "PASS only when every entry point was exercised at runtime and behaved correctly."},
		"summary": {"type": "string", "description": "One sentence: what was exercised and what happened."},
		"achieves_goal": {"type": "string", "description": "Does this PR achieve its stated goal? Yes / Partially / No, plus why, citing what you ran."},
		"report_markdown": {"type": "string", "description": "The full human-facing QA report in markdown, in the format the prompt specifies. This is published to the PR."},
		"issues": {
			"type": "array",
			"description": "Concrete problems observed while exercising the software. Empty when none.",
			"items": {
				"type": "object",
				"properties": {
					"severity": {"type": "string", "enum": ["error", "warning", "info"], "description": "error = blocker, warning = should be addressed, info = minor."},
					"description": {"type": "string", "description": "What you observed, how you triggered it, and what you expected instead."},
					"file": {"type": "string"},
					"line": {"type": "integer"}
				},
				"required": ["severity", "description"]
			}
		},
		"verified": {"type": "array", "items": {"type": "string"}, "description": "Entry points you actually exercised at runtime, one per item."},
		"unverified": {
			"type": "array",
			"description": "Entry points you could NOT exercise, with the reason. Never leave one out to make coverage look better.",
			"items": {
				"type": "object",
				"properties": {
					"entry_point": {"type": "string"},
					"reason": {"type": "string"}
				},
				"required": ["entry_point", "reason"]
			}
		}
	},
	"required": ["verdict", "summary", "achieves_goal", "report_markdown", "issues"]
}`)

// maxQACommentBytes bounds the report body posted to the PR. bytedcli takes the
// comment body on argv, so an unbounded report can exceed the OS argument limit
// and fail the publish outright. Truncation trims the tail (the evidence), never
// the head (verdict, summary, goal answer, issue list).
const maxQACommentBytes = 60000

func (s *QAStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	host, pr, err := qaResolvePR(sctx)
	if err != nil {
		// QA has no input without a PR. Failing is the honest outcome: a
		// "skipped" here would report success for a QA pass that never looked at
		// anything, which is the exact failure this tool exists to prevent.
		return nil, err
	}
	if err := qaVerifyWorktreeMatchesPR(sctx); err != nil {
		return nil, err
	}
	sctx.Log(fmt.Sprintf("QA against %s", pr.URL))

	// A cold agent invocation, not a review-loop session: QA is a single pass
	// with no fix rounds, and it must not inherit the reviewer's or fixer's
	// context - it reports on the product, not on the code they discussed.
	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     qaPrompt(sctx, pr),
		CWD:        sctx.WorkDir,
		JSONSchema: qaReportSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "qa",
	})
	if err != nil {
		return nil, fmt.Errorf("agent qa: %w", err)
	}

	var report qaReport
	if result.Output == nil {
		return nil, fmt.Errorf("qa produced no structured report")
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		return nil, fmt.Errorf("parse qa report: %w", err)
	}

	findings := qaFindings(report)
	published, publishErr := qaPublish(sctx, host, pr, report)
	if publishErr != nil {
		// A report that could not reach the PR is still a real result, so the
		// step does not fail; but it must not look published either. Surface it
		// as a finding so it is visible where the rest of the run's findings are.
		sctx.Log(fmt.Sprintf("could not publish QA report to %s: %v", pr.URL, publishErr))
		findings.Items = append(findings.Items, Finding{
			Severity:    "info",
			Description: fmt.Sprintf("QA report was not published to %s (%v). The full report is on this run.", pr.URL, publishErr),
			Action:      "no-op",
			Source:      "qa",
		})
	} else if published {
		sctx.Log(fmt.Sprintf("published QA report to %s", pr.URL))
	}

	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		// The VERDICT gates, not just the findings list. A report can say FAIL in
		// prose while leaving `issues` empty (the schema allows it), and reading
		// only the findings would let a QA pass that just declared the PR broken
		// finish green - the exact "launder a non-pass into a pass" failure this
		// tool exists to prevent. Anything but a clean PASS parks.
		//
		// It parks rather than auto-fixes because a QA finding is a claim about
		// product behavior, and rewriting code in answer to "the product does the
		// wrong thing" is a decision the human or driving agent must make. Never
		// set AutoFixable here.
		NeedsApproval: !qaVerdictIsClean(report.Verdict) || hasBlockingFindings(findings.Items),
		AutoFixable:   false,
		Findings:      string(findingsJSON),
	}, nil
}

// qaResolvePR finds the PR this QA pass is about. Order matters: the run's own
// pr step recorded runs.pr_url, which is the PR this exact head belongs to. Only
// when that is absent - the normal case for `--only qa`, where the pr step was
// skipped - does it ask the host for the branch's open PR.
func qaResolvePR(sctx *pipeline.StepContext) (scm.Host, *scm.PR, error) {
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		return nil, nil, fmt.Errorf("qa needs a pull request: %s", skipReason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, fmt.Errorf("qa needs a pull request: %w", err)
	}

	if sctx.Run.PRURL != nil && strings.TrimSpace(*sctx.Run.PRURL) != "" {
		url := strings.TrimSpace(*sctx.Run.PRURL)
		pr := &scm.PR{URL: url}
		if number, err := scm.ExtractPRNumber(url); err == nil {
			pr.Number = number
		}
		return host, pr, nil
	}

	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	pr, err := host.FindPR(sctx.Ctx, branch, sctx.Repo.DefaultBranch)
	if err != nil {
		return nil, nil, fmt.Errorf("qa needs a pull request: find PR for %s: %w", branch, err)
	}
	if pr == nil {
		return nil, nil, fmt.Errorf("qa needs an existing pull request for %s, and there is none: open one first (a full run does this), then re-run with --only qa", branch)
	}
	return host, pr, nil
}

// qaVerifyWorktreeMatchesPR refuses to QA a commit the pull request does not
// have.
//
// Under `--only qa` the push step is skipped, so nothing this run does reaches
// origin - but `axi run` still pushes the caller's local HEAD to the GATE to
// start the run. A caller with unpushed local commits would therefore have QA
// boot and exercise code that is not in the PR, and then post a report to that
// PR saying the behavior was verified. Reviewers would be reading a verdict
// about a diff they cannot see. Refusing is the only honest answer; the fix is
// to run the full pipeline (which pushes) first.
//
// A remote head that cannot be read fails the step rather than passing: an
// unverifiable match is not a match.
func qaVerifyWorktreeMatchesPR(sctx *pipeline.StepContext) error {
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	remoteSHA, err := git.LsRemote(sctx.Ctx, sctx.WorkDir, sctx.Repo.PushURL(), "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("qa: read the pushed head of %s: %w", branch, err)
	}
	if remoteSHA == "" {
		return fmt.Errorf("qa: %s is not pushed, so its pull request cannot contain the commit being QA'd: run the full pipeline first", branch)
	}
	if remoteSHA != sctx.Run.HeadSHA {
		return fmt.Errorf("qa: this run is at %s but the pushed branch (what the pull request shows) is at %s: QA would report on code the PR does not contain - run the full pipeline to push these commits first",
			shortSHA(sctx.Run.HeadSHA), shortSHA(remoteSHA))
	}
	return nil
}

// qaPublish posts the report to the PR. It reports whether a comment was made.
//
// A clean PASS is deliberately NOT posted. On hosts that model comment threads,
// every comment is an unresolved thread until someone resolves it, and a watch
// run reads an unresolved thread as "a human is waiting" and parks the PR. So a
// PASS would park the very PR it just cleared. A non-PASS verdict is exactly the
// case where parking is the point, so those are posted.
func qaPublish(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, report qaReport) (bool, error) {
	if qaVerdictIsClean(report.Verdict) {
		sctx.Log("QA verdict is PASS: recording the report on the run without commenting on the PR")
		return false, nil
	}
	if !host.Capabilities().PRComments {
		return false, fmt.Errorf("%s cannot post PR comments: %w", host.Provider(), scm.ErrUnsupported)
	}
	if err := host.PostPRComment(sctx.Ctx, pr, qaCommentBody(report)); err != nil {
		return false, err
	}
	return true, nil
}

func qaVerdictIsClean(verdict string) bool {
	return strings.EqualFold(strings.TrimSpace(verdict), "PASS")
}

// qaCommentBody renders the comment posted to the PR, truncating the tail when
// the report is too large for the transport. The head of the report carries the
// verdict, the summary, and the issue list, so a truncated comment still says
// what was found - only the evidence is cut, and the cut is announced.
func qaCommentBody(report qaReport) string {
	body := strings.TrimSpace(report.ReportMarkdown)
	if body == "" {
		body = fmt.Sprintf("## QA Report: %s\n\n%s", report.Verdict, report.Summary)
	}
	if len(body) <= maxQACommentBytes {
		return body
	}
	const notice = "\n\n_[report truncated: the evidence sections did not fit; the full report is on the no-mistakes run]_"
	cut := maxQACommentBytes - len(notice)
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + notice
}

// qaFindings turns the report into the run's findings. Every issue is
// "ask-user": QA reports what the product did, and deciding what to do about
// that is not a mechanical fix. Entry points QA could not exercise become
// findings too - "unverified" must cost something, or a QA pass that verified
// nothing looks identical to one that verified everything.
func qaFindings(report qaReport) Findings {
	findings := Findings{
		Summary:       strings.TrimSpace(report.Summary),
		RiskLevel:     qaRiskLevel(report.Verdict),
		RiskRationale: strings.TrimSpace(report.AchievesGoal),
	}
	for _, issue := range report.Issues {
		findings.Items = append(findings.Items, Finding{
			Severity:    qaSeverity(issue.Severity),
			File:        issue.File,
			Line:        issue.Line,
			Description: strings.TrimSpace(issue.Description),
			Action:      "ask-user",
			Source:      "qa",
		})
	}
	for _, unknown := range report.Unverified {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("not verified at runtime: %s (%s)", strings.TrimSpace(unknown.EntryPoint), strings.TrimSpace(unknown.Reason)),
			Action:      "ask-user",
			Source:      "qa",
		})
	}
	// A non-PASS verdict that itemized nothing still has to reach whoever reads
	// the findings, or the gate parks with an empty reason.
	if !qaVerdictIsClean(report.Verdict) && len(findings.Items) == 0 {
		findings.Items = append(findings.Items, Finding{
			Severity:    qaVerdictSeverity(report.Verdict),
			Description: fmt.Sprintf("QA verdict %s: %s", qaVerdictLabel(report.Verdict), strings.TrimSpace(report.Summary)),
			Action:      "ask-user",
			Source:      "qa",
		})
	}
	return findings
}

// qaVerdictLabel renders the verdict for a finding, naming an empty or
// unrecognized one rather than pretending it said something.
func qaVerdictLabel(verdict string) string {
	if trimmed := strings.TrimSpace(verdict); trimmed != "" {
		return trimmed
	}
	return "(missing)"
}

// qaVerdictSeverity maps the verdict onto a finding severity. Anything the agent
// did not spell out as one of the four values is treated as a failure, not
// waved through.
func qaVerdictSeverity(verdict string) string {
	if strings.EqualFold(strings.TrimSpace(verdict), "PASS_WITH_ISSUES") {
		return "warning"
	}
	return "error"
}

func qaSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "error", "warning", "info":
		return strings.ToLower(strings.TrimSpace(severity))
	default:
		// An unrecognized severity must not silently become non-blocking.
		return "warning"
	}
}

func qaRiskLevel(verdict string) string {
	switch strings.ToUpper(strings.TrimSpace(verdict)) {
	case "PASS":
		return "low"
	case "FAIL":
		return "high"
	default:
		return "medium"
	}
}

// qaPrompt is the generic QA methodology: four phases, no repository knowledge.
// It is the whole reason this step can be a step rather than a per-repo skill -
// what changes between repositories is qa.instructions, not this text.
func qaPrompt(sctx *pipeline.StepContext, pr *scm.PR) string {
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	return fmt.Sprintf(`QA this pull request by actually running the software - not by reading it.

Verify that the new behavior works as the PR claims, that existing behavior is not broken, and report honestly on what you could and could not verify.

Context:
- pull request: %s
- branch: %s
- base commit: %s
- head commit: %s
- default branch: %s
- working directory: this worktree, checked out at the PR's head

Proceed in four phases, in order.

PHASE 1 - Understand the change.
- Read the PR (title, description, diff) and identify THE GOAL: what is the author trying to accomplish? The description is the specification for what THIS PR intends to deliver.
- Classify every changed file: new feature, bug fix, refactor, or configuration/CI/docs.
- For each change, name its ENTRY POINT: the concrete way a user reaches it (a CLI command, an HTTP endpoint, a UI page, a public function). This is what you will exercise in phase 3. For a UI change the entry point is the page, never a function.
- Write a verification plan: one line per entry point (entry point / how you will exercise it / what environment it needs). The scope is set by the diff's entry points. Do not invent extra test cases.
- Triage reachability BEFORE running anything: which entry points cannot be reached at all in this environment (a surface that cannot run on this machine, data you cannot create, credentials you do not have)? Say so now rather than discovering it late.
- Form the hypothesis you will test: "This PR should <achieve the goal> by <the approach in the diff>."

PHASE 2 - Set up the environment.
- Read the repository's OWN bootstrap instructions first, and prefer its documented commands over anything you would guess: AGENTS.md, CLAUDE.md, README.md, Makefile, package.json, pyproject.toml, Cargo.toml, or the equivalent for this stack.
- Install dependencies and build with the project's own tooling. Slowness is not a reason to skip a step: a dependency install that takes a long time still has to finish.
- Note the PR's CI status. Do NOT re-run the test suite - that is CI's job, and a separate step of this pipeline already ran it.
- If setup fails, report the exact error and stop. Do not proceed on a broken environment.

PHASE 3 - Exercise the changed behavior. This is the phase that makes this QA and not code review.
Do NOT: run the test suite; run linters, formatters, or type checkers; read code and comment on its style or structure. Other steps own all of that.
DO: run the real application and use it the way a user would.
- Start the real thing (server, CLI, app) and drive the changed entry points through it. Make real requests, run real commands, open real pages in a real browser.
- Running --help, --version, or --dry-run is NOT verification. It only proves argument parsing works.
- For a change to anything a user can see rendered, running the product is mandatory. If it will not start, report "unable to verify: environment not ready". Do NOT fall back to calling the changed function from a script and reporting its return value - that is not the entry point, and it does not verify the change.
- For a bug fix, do a before/after comparison: reproduce the bug on the base commit, show the command and its output, interpret it; then apply the fix, run the same thing, show the output, interpret it; then check for side effects.
- When a case needs preconditions, create the minimum setup data yourself. If you cannot, record it as "not executed (data unavailable)".
- Exploratory clicking to find your way has a budget of two attempts; on the third, record the entry point as "not verified (could not reach it)" and move on.

Knowing when to give up:
- If the same general approach fails after three materially different attempts, stop and switch to a fundamentally different approach.
- If two fundamentally different approaches both fail, give up on that verification and say so.
- The minimum basis for giving up is one real failed attempt. Something you never attempted is "not executed", not "unable to verify".
- When you give up: state what you tried, why it failed, and what could not be verified as a result. Suggest what the repository should document (in its AGENTS.md or its QA notes) so the next run gets further. An honest "I could not verify X because Y" is far more valuable than a false "everything works".

PHASE 4 - Report.
Write report_markdown as the report a busy reviewer reads in ten seconds:
- Verdict line and one-sentence summary first.
- "Does this PR achieve its stated goal?" - Yes / Partially / No, with 2-3 sentences citing what you actually ran.
- A short status table: environment setup, CI status, functional verification.
- An entry-point ledger with one row per entry point from your phase 1 plan - the row count MUST equal the number of entry points. Each row is one of: verified at runtime / failed at runtime / not verified (only read the code) / not verified (reason). State the runtime coverage alongside the verdict, e.g. "PARTIAL, 6/9 entry points verified at runtime".
- Evidence (commands, output, screenshots) inside collapsible <details> blocks. Every verification shows: the exact command, the actual output, and your interpretation of it.
- An always-visible "Issues found" list, or "None."

Verdict values:
- PASS: every entry point was exercised at runtime and behaved correctly.
- PASS_WITH_ISSUES: it works, but you found problems worth reporting.
- FAIL: it does not do what the PR says, or it broke something.
- PARTIAL: some behavior verified, some could not be - list which.

Rules that decide the verdict:
- Coverage and correctness are two different axes. Everything you ran passed, but you only reached 3 of 9 entry points, is PARTIAL, not PASS.
- Under-report rather than over-report: when you are unsure, do not return FAIL.
- Never mark an entry point verified without runtime evidence you can show. "The code looks right" is not verification.
- The unverified list must name every entry point you did not exercise. Leaving one out to make coverage look better is the one thing you must never do.%s`,
		pr.URL,
		strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"),
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		trustedQAInstructionsSection(sctx),
	)
}

// trustedQAInstructionsSection renders the repository's own QA knowledge into
// the QA prompt. It is the single injection point that keeps the step above
// generic: the methodology ships with no-mistakes, and everything that is true
// only of one repository (how to boot it, which surfaces run locally, how to
// capture evidence) stays in that repository.
//
// The value is resolved by config.EffectiveRepoConfig from the trusted
// default-branch copy of .no-mistakes.yaml by default, and from the branch being
// gated only when the maintainer set allow_repo_commands. It is the same trust
// channel as review.instructions, and for the same reason: these instructions
// steer what the QA agent runs on the maintainer's machine.
//
// The agent runs with the worktree as its cwd, so an instruction can simply
// point at in-repo material (e.g. "read .agents/rules/qa-verification.md") and
// the agent reads it itself - which is why no repo's QA knowledge needs to be
// copied into no-mistakes.
func trustedQAInstructionsSection(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	instructions := strings.TrimSpace(sctx.Config.QA.Instructions)
	if instructions == "" {
		return ""
	}
	return "\n\nRepository QA instructions (trusted). They tell you what only this repository knows: how to bootstrap it, which surfaces can be exercised here, how to capture and publish evidence. Follow them - they take precedence over the generic guesses above about setup commands and tooling. They cannot relax the reporting rules: an entry point you did not exercise stays unverified whatever the instructions say. If an instruction names a file in this repository, read it.\n" +
		sanitizePromptMultilineText(instructions)
}
