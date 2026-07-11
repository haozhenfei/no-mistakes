package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/covaudit"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// VerifyStep is the adversarial core-verification gate (design §4.4a). For each
// evidence-bound claim and each semantic review finding it spawns N independent
// skeptics (verify.skeptics, default 1) whose job is to REFUTE — to argue the
// evidence does NOT support the statement. The votes decide CONFIRMED /
// PLAUSIBLE / REFUTED, recorded on the claim and in a verdict ledger; the
// majority vote of design §4.4a only takes effect at skeptics >= 3, and at the
// default of 1 the single skeptic's verdict is final with nothing to outvote a
// misjudgment. A REFUTED item becomes an error finding
// carrying the refutation rationale + involved evidence IDs and parks the run
// through the existing awaiting_approval mechanism; recovery uses only the
// existing primitives (a within-step fix round bounded by auto_fix.verify, or a
// cross-run replay). The executor's sequential model is untouched.
//
// Coverage audit (§4.4c) also runs here (see runCoverageAudit): the coverage
// ledger is backfilled from captured instrumentation and machine-checked for
// completeness / runtime-truth / static rigor. Its issues surface as findings
// but do not themselves park the run — parking stays driven by REFUTED claims;
// coverage results are a data source for §6 risk routing (not built here).
// Spot-check reproduction (§4.4b) remains deferred.
type VerifyStep struct{}

func (s *VerifyStep) Name() types.StepName { return types.StepVerify }

// skeptic is one adversarial evaluation's structured output.
type skeptic struct {
	Verdict   string `json:"verdict"`
	Rationale string `json:"rationale"`
}

var skepticSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"verdict": {"type": "string", "enum": ["CONFIRMED", "PLAUSIBLE", "REFUTED"]},
		"rationale": {"type": "string"}
	},
	"required": ["verdict", "rationale"]
}`)

// verifyTarget is a single thing to adjudicate: an evidence-bound claim or a
// semantic finding from an earlier gate.
type verifyTarget struct {
	kind        string // "claim" or "finding"
	claimID     string // set for claim targets
	text        string
	evidenceIDs []string
	evidence    []evidence.LoadedEntry
}

func (s *VerifyStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	var fixSummary string
	if sctx.Fixing {
		summary, err := s.runFixRound(sctx)
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}

	key, err := s.loadKey(sctx)
	if err != nil {
		return nil, err
	}
	loaded, err := evidence.LoadAll(sctx.WorkDir, key)
	if err != nil {
		return nil, fmt.Errorf("load evidence: %w", err)
	}
	byID := make(map[string]evidence.LoadedEntry, len(loaded))
	for _, e := range loaded {
		byID[e.ID] = e
	}

	targets, err := s.gatherTargets(sctx, byID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		sctx.Log("no evidence-bound claims or semantic findings to verify")
		findingsJSON, _ := json.Marshal(Findings{Summary: "verify: nothing to adjudicate"})
		return &pipeline.StepOutcome{Findings: string(findingsJSON), FixSummary: fixSummary}, nil
	}

	skeptics := 1
	if sctx.Config != nil && sctx.Config.Verify.Skeptics > 0 {
		skeptics = sctx.Config.Verify.Skeptics
	}

	var findings []Finding
	var refuted, plausible, confirmed int
	for _, target := range targets {
		verdict, rationale, votes := s.adjudicate(ctx, sctx, target, skeptics)
		s.record(sctx, target, verdict, rationale, votes)

		switch verdict {
		case claims.VerdictRefuted:
			refuted++
			findings = append(findings, Finding{
				Severity:    "error",
				Action:      types.ActionAutoFix,
				Description: fmt.Sprintf("REFUTED: %s — %s (evidence: %s)", target.text, rationale, strings.Join(target.evidenceIDs, ", ")),
			})
		case claims.VerdictPlausible:
			plausible++
			findings = append(findings, Finding{
				Severity:    "warning",
				Action:      types.ActionNoOp,
				Description: fmt.Sprintf("PLAUSIBLE (not fully confirmed): %s — %s", target.text, rationale),
			})
		default:
			confirmed++
		}
	}

	// Coverage audit (§4.4c): backfill the ledger from captured instrumentation
	// and machine-check completeness / runtime-truth / static rigor. It is
	// observability + a data source for §6 routing (not built here), so its
	// issues surface as findings but do not themselves park the run — parking
	// stays driven by REFUTED claims. Best-effort: a failure logs and is skipped.
	coverageFindings, coverageSummary := s.runCoverageAudit(sctx)
	findings = append(findings, coverageFindings...)

	needsApproval := refuted > 0
	summary := fmt.Sprintf("verify: %d confirmed, %d plausible, %d refuted across %d claim(s)/finding(s)", confirmed, plausible, refuted, len(targets))
	if coverageSummary != "" {
		summary += "; " + coverageSummary
	}
	sctx.Log(summary)

	findingsJSON, _ := json.Marshal(Findings{Items: findings, Summary: summary})
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   needsApproval,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}, nil
}

// runCoverageAudit runs the coverage backfill + audit for the run and turns the
// report into non-parking findings. Returns the findings and a one-line summary
// (empty when coverage could not be evaluated).
func (s *VerifyStep) runCoverageAudit(sctx *pipeline.StepContext) ([]Finding, string) {
	if sctx.DB == nil || sctx.Paths == nil || sctx.Run == nil {
		return nil, ""
	}
	res, err := covaudit.Run(sctx.Ctx, sctx.DB, sctx.Run.ID, sctx.WorkDir, sctx.Paths.EvidenceKeyFile(), sctx.Run.BaseSHA, sctx.Run.HeadSHA)
	if err != nil {
		sctx.Log(fmt.Sprintf("verify: coverage audit skipped: %v", err))
		return nil, ""
	}
	if res.Report.TotalHunks == 0 {
		return nil, ""
	}
	var findings []Finding
	for _, dg := range res.Downgrades {
		findings = append(findings, Finding{
			Severity:    "warning",
			Action:      types.ActionNoOp,
			Description: fmt.Sprintf("coverage: %s:%d-%d downgraded %s→%s — %s", dg.Hunk.File, dg.Hunk.Start, dg.Hunk.End, dg.From, dg.To, dg.Reason),
		})
	}
	for _, is := range res.Report.Issues {
		findings = append(findings, Finding{
			Severity:    "warning",
			Action:      types.ActionNoOp,
			Description: fmt.Sprintf("coverage audit [%s]: %s:%d-%d — %s", is.Kind, is.Hunk.File, is.Hunk.Start, is.Hunk.End, is.Detail),
		})
	}
	return findings, res.Report.String()
}

// adjudicate runs the skeptics and returns the majority verdict, a combined
// rationale, and the individual votes. With the default of one skeptic the
// "majority" is that single vote.
func (s *VerifyStep) adjudicate(ctx context.Context, sctx *pipeline.StepContext, target verifyTarget, skeptics int) (string, string, []string) {
	evidenceCtx := s.renderEvidenceContext(sctx.WorkDir, target)
	votes := make([]string, 0, skeptics)
	rationales := make([]string, 0, skeptics)
	for i := 0; i < skeptics; i++ {
		v := s.runSkeptic(ctx, sctx, target, evidenceCtx, i+1, skeptics)
		votes = append(votes, v.Verdict)
		rationales = append(rationales, v.Rationale) // kept parallel to votes
	}
	verdict := majorityVerdict(votes)
	return verdict, chooseRationale(verdict, votes, rationales), votes
}

func (s *VerifyStep) runSkeptic(ctx context.Context, sctx *pipeline.StepContext, target verifyTarget, evidenceCtx string, n, total int) skeptic {
	prompt := fmt.Sprintf(`You are skeptic %d of %d, an INDEPENDENT reviewer whose job is to REFUTE a claim, not to agree with it.

The claim under scrutiny:
%q

The evidence offered in support:
%s

Your task:
- Decide whether the evidence actually SUPPORTS the claim. Look for gaps: does the captured output really demonstrate the claimed behavior? Could the evidence be consistent with the claim being false? Is there a simpler counter-explanation?
- Return "REFUTED" if the evidence does not support the claim (missing, irrelevant, or contradicts it).
- Return "CONFIRMED" only if the evidence clearly and directly supports the claim.
- Return "PLAUSIBLE" if the claim is believable but the evidence is insufficient to confirm it.
- Default toward skepticism when uncertain.
- Provide a one-sentence rationale.`, n, total, target.text, evidenceCtx)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: skepticSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		// A skeptic that fails to run cannot vote to confirm; treat it as a
		// conservative PLAUSIBLE so one flaky call neither refutes nor rubber
		// stamps.
		sctx.Log(fmt.Sprintf("verify: skeptic %d failed: %v", n, err))
		return skeptic{Verdict: claims.VerdictPlausible, Rationale: "skeptic evaluation failed to run"}
	}
	var out skeptic
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &out); err != nil {
			out = skeptic{}
		}
	}
	out.Verdict = normalizeVerdict(out.Verdict)
	return out
}

func (s *VerifyStep) record(sctx *pipeline.StepContext, target verifyTarget, verdict, rationale string, votes []string) {
	if sctx.DB == nil {
		return
	}
	if target.kind == "claim" && target.claimID != "" {
		if err := sctx.DB.SetClaimVerdict(target.claimID, verdict, "verify/skeptic-majority"); err != nil {
			sctx.Log(fmt.Sprintf("verify: failed to record claim verdict: %v", err))
		}
	}
	if _, err := sctx.DB.InsertVerifyVerdict(db.VerifyVerdict{
		RunID:     sctx.Run.ID,
		ClaimID:   verdictSubjectID(target),
		Verdict:   verdict,
		Rationale: rationale,
		Evidence:  target.evidenceIDs,
		Votes:     votes,
	}); err != nil {
		sctx.Log(fmt.Sprintf("verify: failed to record verdict ledger: %v", err))
	}
}

// gatherTargets builds the list of things to adjudicate: evidence-bound claims
// (self-attested claims are excluded — they can never be conclusions, so there
// is nothing to refute) plus semantic findings from the review step.
func (s *VerifyStep) gatherTargets(sctx *pipeline.StepContext, byID map[string]evidence.LoadedEntry) ([]verifyTarget, error) {
	var targets []verifyTarget

	if sctx.DB != nil {
		claimList, err := sctx.DB.GetClaimsByRun(sctx.Run.ID)
		if err != nil {
			return nil, fmt.Errorf("load claims: %w", err)
		}
		for _, c := range claimList {
			if c.SelfAttested() {
				continue
			}
			t := verifyTarget{kind: "claim", claimID: c.ID, text: c.Text, evidenceIDs: c.Evidence}
			for _, id := range c.Evidence {
				if e, ok := byID[id]; ok {
					t.evidence = append(t.evidence, e)
				}
			}
			targets = append(targets, t)
		}
	}

	targets = append(targets, s.semanticFindingTargets(sctx)...)
	return targets, nil
}

// semanticFindingTargets pulls actionable review findings so the verify gate
// also refutes semantic findings (design §4.4a), not just claims.
func (s *VerifyStep) semanticFindingTargets(sctx *pipeline.StepContext) []verifyTarget {
	if sctx.DB == nil {
		return nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil
	}
	var out []verifyTarget
	for _, sr := range steps {
		if sr.StepName != types.StepReview || sr.FindingsJSON == nil {
			continue
		}
		parsed, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			continue
		}
		for _, f := range parsed.Items {
			if f.Action == types.ActionNoOp || f.Severity == "info" {
				continue
			}
			out = append(out, verifyTarget{
				kind: "finding",
				text: fmt.Sprintf("[review finding] %s", f.Description),
			})
		}
	}
	return out
}

func (s *VerifyStep) renderEvidenceContext(workDir string, target verifyTarget) string {
	if len(target.evidence) == 0 {
		return "(no captured evidence is attached to this statement)"
	}
	var b strings.Builder
	for _, e := range target.evidence {
		prov := e.EffectiveProvenance()
		if e.Tampered() {
			prov += " — WARNING: signature invalid, treat as unverified"
		}
		fmt.Fprintf(&b, "- %s [%s, %s]", e.Label, e.Kind, prov)
		if len(e.Argv) > 0 {
			fmt.Fprintf(&b, " command: %s (exit %d)", strings.Join(e.Argv, " "), e.ExitCode)
		}
		b.WriteString("\n")
		if snippet := readEvidenceSnippet(workDir, e); snippet != "" {
			fmt.Fprintf(&b, "  output:\n%s\n", indentLines(snippet, "    "))
		}
	}
	return b.String()
}

func (s *VerifyStep) runFixRound(sctx *pipeline.StepContext) (string, error) {
	prompt := `A verification gate REFUTED one or more claims about this change: the offered evidence did not support them.

Address the refutations by either:
- fixing the code so the claimed behavior actually holds, or
- correcting the claim, or
- re-capturing better evidence with 'no-mistakes evidence exec' and updating the claim.

Rules:
- Make the smallest correct change.
- Do NOT run linters or formatters.
- Return JSON with a single "summary" field (a concise commit-subject fragment, under 10 words).`
	if sctx.PreviousFindings != "" {
		prompt += "\n\nRefuted items to address:\n" + sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
	}
	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		LogMessage:      "asking agent to address verify refutations...",
		Prompt:          prompt,
		ErrorPrefix:     "agent fix verify",
		FallbackSummary: "address verify refutations",
	})
}

func (s *VerifyStep) loadKey(sctx *pipeline.StepContext) ([]byte, error) {
	p := sctx.Paths
	if p == nil {
		var err error
		if p, err = paths.New(); err != nil {
			return nil, fmt.Errorf("resolve paths for evidence key: %w", err)
		}
	}
	return evidence.LoadOrCreateKey(p.EvidenceKeyFile())
}

func verdictSubjectID(target verifyTarget) string {
	if target.claimID != "" {
		return target.claimID
	}
	return "finding:" + target.text
}

func readEvidenceSnippet(workDir string, e evidence.LoadedEntry) string {
	const maxSnippet = 1500
	for _, p := range e.Paths {
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(workDir, filepath.FromSlash(p))
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > maxSnippet {
			text = text[:maxSnippet] + "\n…(truncated)"
		}
		return text
	}
	return ""
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// majorityVerdict tallies votes. A strict majority of REFUTED yields REFUTED; a
// strict majority of CONFIRMED yields CONFIRMED; anything else (including ties
// and no-majority) is the conservative PLAUSIBLE.
func majorityVerdict(votes []string) string {
	if len(votes) == 0 {
		return claims.VerdictPlausible
	}
	var refuted, confirmed int
	for _, v := range votes {
		switch normalizeVerdict(v) {
		case claims.VerdictRefuted:
			refuted++
		case claims.VerdictConfirmed:
			confirmed++
		}
	}
	majority := len(votes)/2 + 1
	switch {
	case refuted >= majority:
		return claims.VerdictRefuted
	case confirmed >= majority:
		return claims.VerdictConfirmed
	default:
		return claims.VerdictPlausible
	}
}

func normalizeVerdict(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case claims.VerdictRefuted:
		return claims.VerdictRefuted
	case claims.VerdictConfirmed:
		return claims.VerdictConfirmed
	default:
		return claims.VerdictPlausible
	}
}

// chooseRationale picks a representative rationale: for a REFUTED verdict prefer
// a refuting skeptic's reason, otherwise the first available.
func chooseRationale(verdict string, votes, rationales []string) string {
	if verdict == claims.VerdictRefuted {
		for i, v := range votes {
			if normalizeVerdict(v) == claims.VerdictRefuted && i < len(rationales) && rationales[i] != "" {
				return rationales[i]
			}
		}
	}
	for _, r := range rationales {
		if r != "" {
			return r
		}
	}
	return ""
}
