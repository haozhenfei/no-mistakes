package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// fakeQAGH is a gh that knows the branch's PR, and records every invocation
// (including comment bodies sent over stdin) to a log file.
func fakeQAGH(t *testing.T, prURL string) ([]string, string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	logFile := t.TempDir() + "/gh.log"
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":   "gh",
		"FAKE_CLI_PR_URL": prURL,
		"FAKE_CLI_LOG":    logFile,
	})
	return env, logFile
}

func ghLog(t *testing.T, logFile string) string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil {
		return ""
	}
	return string(data)
}

// qaContext builds a QA step context whose branch is actually pushed to a bare
// remote, so the step's "is this commit in the PR?" check sees a real match.
// Every QA test needs it: QA refuses to report on a commit the PR cannot have.
func qaContext(t *testing.T, ag agent.Agent, dir, baseSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	// The repo's upstream stays a GitHub URL (provider detection depends on it),
	// but a local url.<dir>.insteadOf rewrite makes every git call against it hit
	// a bare repo on disk. That keeps the step's "is this commit in the PR?"
	// check exercising real git rather than being mocked away.
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "config", "url."+upstream+".insteadOf", sctx.Repo.UpstreamURL)
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	gitCmd(t, dir, "push", upstream, headSHA+":refs/heads/"+branch)
	return sctx
}

func qaAgentReturning(t *testing.T, report string, capturedPrompt *string) *mockAgent {
	t.Helper()
	return &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if capturedPrompt != nil {
				*capturedPrompt = opts.Prompt
			}
			return &agent.Result{Output: json.RawMessage(report)}, nil
		},
	}
}

const qaPassReport = `{"verdict":"PASS","summary":"exercised the new page","achieves_goal":"Yes","report_markdown":"## QA Report: PASS","issues":[]}`

// The repository's own QA knowledge must reach the QA agent's prompt. This is
// the whole mechanism that keeps the step generic: no-mistakes ships the
// methodology, the repo ships what only it knows.
func TestQAStep_RepoInstructionsReachThePrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var prompt string
	sctx := qaContext(t, qaAgentReturning(t, qaPassReport, &prompt), dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	sctx.Config.QA = config.QA{Instructions: "Read .agents/rules/qa-verification.md before starting. The web app runs on port 8080."}
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	if _, err := (&QAStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !strings.Contains(prompt, "Read .agents/rules/qa-verification.md before starting.") {
		t.Fatalf("QA prompt does not carry the repo's qa.instructions:\n%s", prompt)
	}
	if !strings.Contains(prompt, "The web app runs on port 8080.") {
		t.Fatalf("QA prompt dropped part of the repo's qa.instructions:\n%s", prompt)
	}
	if !strings.Contains(prompt, "If an instruction names a file in this repository, read it.") {
		t.Fatalf("QA prompt does not tell the agent to read in-repo material:\n%s", prompt)
	}
}

// The methodology is hardcoded and repo-agnostic: it must carry the four phases
// and the rules that stop an agent from reporting unverified work as verified,
// with no repository-specific tooling baked in.
func TestQAStep_PromptCarriesTheGenericMethodology(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var prompt string
	sctx := qaContext(t, qaAgentReturning(t, qaPassReport, &prompt), dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	if _, err := (&QAStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	for _, want := range []string{
		"PHASE 1 - Understand the change.",
		"PHASE 2 - Set up the environment.",
		"PHASE 3 - Exercise the changed behavior.",
		"PHASE 4 - Report.",
		"Do NOT: run the test suite",
		"is NOT verification",
		"Never mark an entry point verified without runtime evidence",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("QA prompt is missing %q:\n%s", want, prompt)
		}
	}
	// The step owns no repository knowledge. If any of these ever appear in the
	// prompt, the generic step has started absorbing one repo's world.
	for _, forbidden := range []string{"bifrost", "ego-browser", "bytedcli", "rush ", "playwright"} {
		if strings.Contains(strings.ToLower(prompt), forbidden) {
			t.Fatalf("QA prompt leaked repo-specific tooling %q; that belongs in qa.instructions", forbidden)
		}
	}
}

// `--only qa` skips the pr step, so the run row has no PR URL and QA falls back
// to asking the host for the branch's open PR.
func TestQAStep_FindsPRWhenTheRunRowHasNone(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var prompt string
	sctx := qaContext(t, qaAgentReturning(t, qaPassReport, &prompt), dir, baseSHA, headSHA)
	env, log := fakeQAGH(t, "https://github.com/test/repo/pull/42")
	sctx.Env = env

	if sctx.Run.PRURL != nil {
		t.Fatal("precondition: run row should carry no PR URL")
	}
	if _, err := (&QAStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(prompt, "https://github.com/test/repo/pull/42") {
		t.Fatalf("QA prompt does not name the PR it found:\n%s", prompt)
	}
	if !strings.Contains(ghLog(t, log), "pr list") {
		t.Fatal("QA did not ask the host for the branch's PR")
	}
}

// The run's own pr step recorded runs.pr_url; that is the PR for this exact
// head, so QA must use it rather than re-querying the host.
func TestQAStep_PrefersThePRURLOnTheRunRow(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var prompt string
	sctx := qaContext(t, qaAgentReturning(t, qaPassReport, &prompt), dir, baseSHA, headSHA)
	// The host would answer with a different PR; the run row must win.
	env, log := fakeQAGH(t, "https://github.com/test/repo/pull/999")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	if _, err := (&QAStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(prompt, "https://github.com/test/repo/pull/7") {
		t.Fatalf("QA did not use the run row's PR URL:\n%s", prompt)
	}
	if strings.Contains(ghLog(t, log), "pr list") {
		t.Fatal("QA queried the host even though the run row already had the PR URL")
	}
}

// QA has no input without a PR. It must fail loudly rather than skip: a skipped
// step reports success for a QA pass that never looked at anything.
func TestQAStep_FailsWhenThereIsNoPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	called := false
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			called = true
			return &agent.Result{Output: json.RawMessage(qaPassReport)}, nil
		},
	}
	sctx := qaContext(t, ag, dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "") // no open PR for the branch
	sctx.Env = env

	outcome, err := (&QAStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("Execute() = %+v, want an error when no PR exists", outcome)
	}
	if !strings.Contains(err.Error(), "pull request") {
		t.Fatalf("error = %v, want it to name the missing pull request", err)
	}
	if called {
		t.Fatal("QA ran the agent without a PR to QA")
	}
}

// A QA finding says the product behaves wrongly. Answering that by rewriting
// code is a product decision, so findings park for a human - never auto-fix.
func TestQAStep_FindingsParkAndAreNeverAutoFixable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	report := `{"verdict":"FAIL","summary":"the new page 500s","achieves_goal":"No",
		"report_markdown":"## QA Report: FAIL\n\nthe new page 500s",
		"issues":[{"severity":"error","description":"opening /settings returns HTTP 500","file":"app/settings.tsx","line":12}]}`
	sctx := qaContext(t, qaAgentReturning(t, report, nil), dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	outcome, err := (&QAStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Error("a failing QA report must park the run for a decision")
	}
	if outcome.AutoFixable {
		t.Error("QA findings must never be auto-fixable")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != "ask-user" {
		t.Fatalf("findings = %+v, want one ask-user finding", findings.Items)
	}
}

// An entry point QA could not exercise has to cost something, or a pass that
// verified nothing looks the same as one that verified everything.
func TestQAStep_UnverifiedEntryPointsBecomeFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	report := `{"verdict":"PARTIAL","summary":"verified 1 of 2 entry points","achieves_goal":"Partially",
		"report_markdown":"## QA Report: PARTIAL","issues":[],
		"unverified":[{"entry_point":"the mobile app settings screen","reason":"cannot build the iOS app on this machine"}]}`
	sctx := qaContext(t, qaAgentReturning(t, report, nil), dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	outcome, err := (&QAStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want the unverified entry point reported", findings.Items)
	}
	if !strings.Contains(findings.Items[0].Description, "cannot build the iOS app") {
		t.Fatalf("finding does not carry the reason: %+v", findings.Items[0])
	}
	if !outcome.NeedsApproval {
		t.Error("an unverified entry point must park for a decision, not pass silently")
	}
}

// A non-PASS verdict is published to the PR, where reviewers already look.
func TestQAStep_PublishesANonPassReportToThePR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	report := `{"verdict":"FAIL","summary":"the new page 500s","achieves_goal":"No",
		"report_markdown":"## QA Report: FAIL\n\nOpening /settings returns HTTP 500.","issues":[]}`
	sctx := qaContext(t, qaAgentReturning(t, report, nil), dir, baseSHA, headSHA)
	env, log := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	if _, err := (&QAStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	logged := ghLog(t, log)
	if !strings.Contains(logged, "pr comment") {
		t.Fatalf("QA did not comment on the PR; gh log:\n%s", logged)
	}
	if !strings.Contains(logged, "Opening /settings returns HTTP 500.") {
		t.Fatalf("the published comment does not carry the report body; gh log:\n%s", logged)
	}
}

// A clean PASS is deliberately NOT posted: on hosts that model threads, every
// comment is an unresolved thread, and a watch run parks a PR that has one. A
// PASS comment would park the very PR it just cleared.
func TestQAStep_CleanPassDoesNotCommentOnThePR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := qaContext(t, qaAgentReturning(t, qaPassReport, nil), dir, baseSHA, headSHA)
	env, log := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	outcome, err := (&QAStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(ghLog(t, log), "pr comment") {
		t.Fatal("a PASS report must not open a comment thread on the PR")
	}
	if outcome.NeedsApproval {
		t.Error("a clean PASS must not park the run")
	}
	// The report is still recorded on the run, so it is never lost.
	if !strings.Contains(outcome.Findings, "exercised the new page") {
		t.Fatalf("the PASS report was not recorded on the run: %s", outcome.Findings)
	}
}

// An agent that returns nothing has QA'd nothing. The step must fail rather than
// report a pass it did not earn.
func TestQAStep_EmptyAgentOutputFailsTheStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{}, nil
		},
	}
	sctx := qaContext(t, ag, dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	if outcome, err := (&QAStep{}).Execute(sctx); err == nil {
		t.Fatalf("Execute() = %+v, want an error when the agent produced no report", outcome)
	}
}

func setRunPRURL(t *testing.T, sctx *pipeline.StepContext, url string) {
	t.Helper()
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRURL = &url
}

// A report can say FAIL in prose while itemizing nothing (the schema allows an
// empty issues array). Reading only the findings list would let a QA pass that
// just declared the PR broken finish green - the gate must read the verdict.
func TestQAStep_NonPassVerdictParksEvenWithNoIssues(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"FAIL", "PARTIAL", "PASS_WITH_ISSUES", ""} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)

			report := fmt.Sprintf(`{"verdict":%q,"summary":"the new endpoint 500s","achieves_goal":"No",
				"report_markdown":"## QA Report","issues":[],"unverified":[]}`, verdict)
			sctx := qaContext(t, qaAgentReturning(t, report, nil), dir, baseSHA, headSHA)
			env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
			sctx.Env = env
			setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

			outcome, err := (&QAStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if !outcome.NeedsApproval {
				t.Fatalf("verdict %q with no itemized issues finished green; it must park", verdict)
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) == 0 {
				t.Fatal("the parked gate carries no finding, so nothing says why it parked")
			}
		})
	}
}

// `--only qa` skips the push step, so a caller with unpushed local commits would
// have QA exercise code the PR does not contain - and then report to that PR
// that it was verified. QA must refuse instead.
func TestQAStep_RefusesWhenTheCommitIsNotInThePR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	called := false
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			called = true
			return &agent.Result{Output: json.RawMessage(qaPassReport)}, nil
		},
	}
	sctx := qaContext(t, ag, dir, baseSHA, headSHA)
	env, _ := fakeQAGH(t, "https://github.com/test/repo/pull/7")
	sctx.Env = env
	setRunPRURL(t, sctx, "https://github.com/test/repo/pull/7")

	// A commit that exists locally but was never pushed: the PR cannot have it.
	os.WriteFile(filepath.Join(dir, "unpushed.txt"), []byte("local only\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "unpushed local commit")
	sctx.Run.HeadSHA = strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	outcome, err := (&QAStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("Execute() = %+v, want a refusal: the PR does not contain this commit", outcome)
	}
	if !strings.Contains(err.Error(), "PR does not contain") {
		t.Fatalf("error = %v, want it to explain that the PR lacks the commit", err)
	}
	if called {
		t.Fatal("QA ran the agent against a commit the PR does not have")
	}
}
