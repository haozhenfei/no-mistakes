package steps

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxFixLogBytes = 32 * 1024

// FixStep applies the seed findings a watch run handed to this gate run, and
// does nothing at all in every other run.
//
// This is where the post-PR loop closes. A watch run never edits code: when it
// finds a problem it derives a gate run whose parent_run_id points back at it,
// and this step - which runs after rebase, so the fix lands on a current base,
// and before review, so the fix is reviewed like any other change - reads the
// findings off the parent's watch step row and hands them to the agent. The old
// blocking ci step fixed CI in place and force-pushed, skipping review, test,
// and lint entirely; a fix that goes back through the whole gate cannot smuggle
// a new bug in under the cover of fixing an old one.
//
// The seed is read from the DB rather than passed in memory on purpose: the run
// that derives the fix and the run that applies it are different processes'
// concerns, and parent_run_id is the durable link between them.
type FixStep struct{}

func (s *FixStep) Name() types.StepName { return types.StepFix }

func (s *FixStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sctx.Run.ParentRunID == nil || *sctx.Run.ParentRunID == "" {
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	seed, err := seedFindings(sctx, *sctx.Run.ParentRunID)
	if err != nil {
		return nil, err
	}
	if seed == nil || len(seed.Items) == 0 {
		sctx.Log("no seed findings on the parent run, nothing to fix")
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	sctx.Log(fmt.Sprintf("applying %d finding(s) carried over from the watched PR...", len(seed.Items)))

	prompt := fixSeedPrompt(sctx, *seed)
	if logs := s.failedCheckLogs(sctx, *seed); logs != "" {
		prompt += "\n\nCI logs:\n" + logs
	}
	prompt += userIntentPromptSection(sctx)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("agent fix: %w", err)
	}
	summary, summaryErr := extractCommitSummary(result)
	if summaryErr != nil {
		slog.Debug("fix step: no structured commit summary", "run_id", sctx.Run.ID, "error", summaryErr)
	}

	previousHead := sctx.Run.HeadSHA
	if err := commitAgentFixes(sctx, types.StepFix, summary, "apply findings from the watched PR"); err != nil {
		return nil, err
	}
	if sctx.Run.HeadSHA == previousHead {
		// The agent produced nothing. Pushing on would re-open the same PR
		// with the same failure and derive the same watch run: a loop that
		// burns an agent invocation per lap and changes nothing. Park instead
		// and let a person look.
		sctx.Log("fix produced no changes - the findings need a human")
		findings, _ := json.Marshal(Findings{
			Summary: "The fix agent produced no changes for the findings carried over from the watched PR. They need a person.",
			Items:   seed.Items,
		})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findings),
		}, nil
	}
	return &pipeline.StepOutcome{FixSummary: summary}, nil
}

// seedFindings loads the findings the parent run recorded on its watch step.
func seedFindings(sctx *pipeline.StepContext, parentRunID string) (*Findings, error) {
	results, err := sctx.DB.GetStepsByRun(parentRunID)
	if err != nil {
		return nil, fmt.Errorf("load parent run steps: %w", err)
	}
	for _, sr := range results {
		if sr.StepName != types.StepWatch || sr.FindingsJSON == nil || strings.TrimSpace(*sr.FindingsJSON) == "" {
			continue
		}
		var findings Findings
		if err := json.Unmarshal([]byte(*sr.FindingsJSON), &findings); err != nil {
			return nil, fmt.Errorf("parse parent watch findings: %w", err)
		}
		return &findings, nil
	}
	return nil, nil
}

func fixSeedPrompt(sctx *pipeline.StepContext, seed Findings) string {
	var b strings.Builder
	b.WriteString("A pull request opened from this branch has problems that were found after it was opened. Fix them on this branch.\n\n")
	b.WriteString("Context:\n")
	fmt.Fprintf(&b, "- branch: %s\n", sctx.Run.Branch)
	fmt.Fprintf(&b, "- target commit: %s\n", sctx.Run.HeadSHA)
	if sctx.Run.PRURL != nil && *sctx.Run.PRURL != "" {
		fmt.Fprintf(&b, "- pull request: %s\n", *sctx.Run.PRURL)
	}
	if seed.Summary != "" {
		fmt.Fprintf(&b, "- summary: %s\n", seed.Summary)
	}
	b.WriteString("\nFindings to fix:\n")
	for i, item := range seed.Items {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, item.Severity, item.Description)
		if item.File != "" {
			fmt.Fprintf(&b, " (%s", item.File)
			if item.Line > 0 {
				fmt.Fprintf(&b, ":%d", item.Line)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString(`
Rules:
- Make the smallest correct root-cause fix for each finding. Do not refactor beyond that.
- If a check fails only on one OS (CRLF, path separators), make the code cross-platform rather than skipping the check.
- If a test is flaky, make it deterministic.
- Verify your fix by running the most relevant commands locally before finishing.
- Do not amend or rewrite existing commits; add new ones. The rest of this pipeline reviews, tests, and lints what you write before it reaches the PR.
- If a finding genuinely requires no code change, leave the code alone and say so; do not invent a change to look productive.`)
	return b.String()
}

// failedCheckLogs pulls the logs of the failing checks named in the seed, so
// the fix agent sees why they failed rather than only that they did. It is
// best-effort: the fix proceeds without logs when the provider cannot serve
// them. The watch run deliberately does not fetch these - a log dump belongs in
// a prompt, not in the findings row of a poller.
func (s *FixStep) failedCheckLogs(sctx *pipeline.StepContext, seed Findings) string {
	names := failingCheckNamesFromFindings(seed)
	if len(names) == 0 {
		return ""
	}
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		return ""
	}
	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown {
		provider = scm.DetectProvider(prURL)
	}
	host, _ := buildHost(sctx, provider)
	if host == nil || !host.Capabilities().FailedCheckLogs {
		return ""
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return ""
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return ""
	}
	raw, err := host.FetchFailedCheckLogs(sctx.Ctx, &scm.PR{Number: prNumber, URL: prURL}, sctx.Run.Branch, sctx.Run.HeadSHA, names)
	if err != nil && err != scm.ErrUnsupported {
		slog.Warn("fix step: could not fetch failed check logs", "run_id", sctx.Run.ID, "error", err)
	}
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return trimLogOutput(strings.TrimSpace(raw), maxFixLogBytes)
}

// ciCheckFindingPrefix is the shape watch findings use for a failing check.
const ciCheckFindingPrefix = "CI check failing: "

func failingCheckNamesFromFindings(seed Findings) []string {
	var names []string
	for _, item := range seed.Items {
		if name, ok := strings.CutPrefix(item.Description, ciCheckFindingPrefix); ok {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				names = append(names, trimmed)
			}
		}
	}
	return names
}
