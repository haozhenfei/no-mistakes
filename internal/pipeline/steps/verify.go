package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/covaudit"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
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
// and do not park the run by themselves — with ONE exception that is a gate, not
// an audit note: a behavior-class claim must be backed by a runtime-verified
// hunk (see coverage.UnbackedBehaviorClaims). "attested" is the ledger honestly
// reporting that nothing executed the changed code, so it cannot support a claim
// about what a user observes; such a claim is capped at PLAUSIBLE and parks.
// Coverage results are also a data source for §6 risk routing (not built here).
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

	targets, claimList, err := s.gatherTargets(sctx, byID)
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

	// The coverage audit runs BEFORE adjudication because its (backfilled)
	// ledger decides whether a behavior-class claim has any runtime evidence at
	// all. A claim with none can never be recorded as CONFIRMED below.
	coverageFindings, coverageSummary, ledger := s.runCoverageAudit(sctx)
	gaps := coverage.UnbackedBehaviorClaims(claimList, ledger)
	gapByClaim := make(map[string]coverage.BehaviorGap, len(gaps))
	for _, g := range gaps {
		gapByClaim[g.ClaimID] = g
	}

	var findings []Finding
	var refuted, plausible, confirmed int
	for _, target := range targets {
		verdict, rationale, votes, err := s.adjudicate(ctx, sctx, target, skeptics)
		if err != nil {
			// "Could not verify" is NOT "verified". A skeptic that never ran has
			// cast no vote, so there is nothing to adjudicate and no verdict to
			// record: fail the step loudly instead of laundering an
			// infrastructure failure into an optimistic verdict that lets the
			// pipeline through. See runSkeptic.
			return nil, fmt.Errorf("verification did not run: %w", err)
		}
		// A behavior claim with no runtime-verified hunk behind it cannot be a
		// CONFIRMED runtime pass, whatever the skeptic thought: the skeptic
		// judges the prose evidence it was shown, and the ledger is the machine
		// fact about whether the changed code ever ran. Cap the verdict rather
		// than overwrite it, so a REFUTED or PLAUSIBLE verdict keeps its
		// (stronger or equal) meaning.
		if gap, ok := gapByClaim[target.claimID]; ok && verdict == claims.VerdictConfirmed {
			verdict = claims.VerdictPlausible
			rationale = fmt.Sprintf("capped to PLAUSIBLE — no runtime evidence: %s (skeptic said: %s)", gap.Detail, rationale)
		}
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

	// A behavior claim with no runtime evidence is a gate failure, not a note.
	// "attested" is the ledger honestly saying "an agent asserted this, nothing
	// executed it"; leaving that as an acceptable terminal state is how a
	// synthetic render page of a code branch the user can never reach got
	// registered as a fixed, user-visible regression (coze MR 6951) while the run
	// reported completed. It parks for a decision and is NOT auto-fixable: the
	// honest remedies are to capture real instrumentation coverage or to restate
	// the claim, and letting the fixer agent choose means letting it delete the
	// claim that is failing the gate.
	for _, g := range gaps {
		findings = append(findings, Finding{
			Severity: "error",
			Action:   types.ActionAskUser,
			Description: fmt.Sprintf("NO RUNTIME EVIDENCE for %s claim %q — %s. Capture instrumentation that executes the changed code (`no-mistakes evidence coverage --profile <file> --format go|lcov`) and re-run, or restate the claim so it does not assert a runtime pass.",
				g.Kind, g.Text, g.Detail),
		})
	}

	findings = append(findings, coverageFindings...)

	needsApproval := refuted > 0 || len(gaps) > 0
	summary := fmt.Sprintf("verify: %d confirmed, %d plausible, %d refuted across %d claim(s)/finding(s)", confirmed, plausible, refuted, len(targets))
	if len(gaps) > 0 {
		summary += fmt.Sprintf("; %d behavior claim(s) with no runtime evidence", len(gaps))
	}
	if coverageSummary != "" {
		summary += "; " + coverageSummary
	}
	sctx.Log(summary)

	findingsJSON, _ := json.Marshal(Findings{Items: findings, Summary: summary})
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   refuted > 0 && len(gaps) == 0,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}, nil
}

// runCoverageAudit runs the coverage backfill + audit for the run and turns the
// report into non-parking findings. Returns the findings, a one-line summary
// (empty when coverage could not be evaluated), and the backfilled ledger the
// behavior-claim runtime-backing check reads.
//
// The ledger is nil whenever the audit did not produce one (no DB, or the audit
// failed), which leaves every behavior claim unbacked. That is deliberate: an
// audit that could not run has not found the change runtime-verified.
func (s *VerifyStep) runCoverageAudit(sctx *pipeline.StepContext) ([]Finding, string, []coverage.LedgerEntry) {
	if sctx.DB == nil || sctx.Paths == nil || sctx.Run == nil {
		return nil, "", nil
	}
	// Run.BaseSHA is the post-receive hook's "old" SHA, which is all-zero for
	// the first push of a branch; handing that to `git diff` yields "Invalid
	// revision range" and the audit never runs. Every other step resolves it
	// the same way — see resolveBranchBaseSHA.
	var defaultBranch string
	if sctx.Repo != nil {
		defaultBranch = sctx.Repo.DefaultBranch
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, defaultBranch)
	res, err := covaudit.Run(sctx.Ctx, sctx.DB, sctx.Run.ID, sctx.WorkDir, sctx.Paths.EvidenceKeyFile(), baseSHA, sctx.Run.HeadSHA)
	if err != nil {
		// A coverage audit that could not run is not a coverage audit that
		// passed. It stays non-parking (per the step contract above), but it
		// must be visible as a finding — "skipped" in a log line reads as
		// "nothing to report", which is exactly the silent-degradation shape
		// this step is supposed to defend against.
		sctx.Log(fmt.Sprintf("verify: coverage audit could not run: %v", err))
		return []Finding{{
			Severity:    "warning",
			Action:      types.ActionNoOp,
			Description: fmt.Sprintf("coverage audit did not run — code coverage was NOT evaluated for this change: %v", err),
		}}, "coverage audit did not run", nil
	}
	if res.Report.TotalHunks == 0 {
		return nil, "", res.Ledger
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
	return findings, res.Report.String(), res.Ledger
}

// adjudicate runs the skeptics and returns the majority verdict, a combined
// rationale, and the individual votes. With the default of one skeptic the
// "majority" is that single vote. An error means at least one skeptic could not
// be evaluated at all, so no verdict was reached — the caller must fail, never
// substitute a default verdict.
func (s *VerifyStep) adjudicate(ctx context.Context, sctx *pipeline.StepContext, target verifyTarget, skeptics int) (string, string, []string, error) {
	evidenceCtx := s.renderEvidenceContext(sctx.WorkDir, target)
	votes := make([]string, 0, skeptics)
	rationales := make([]string, 0, skeptics)
	for i := 0; i < skeptics; i++ {
		v, err := s.runSkeptic(ctx, sctx, target, evidenceCtx, i+1, skeptics)
		if err != nil {
			return "", "", nil, fmt.Errorf("skeptic %d of %d could not evaluate %q: %w", i+1, skeptics, target.text, err)
		}
		votes = append(votes, v.Verdict)
		rationales = append(rationales, v.Rationale) // kept parallel to votes
	}
	verdict := majorityVerdict(votes)
	return verdict, chooseRationale(verdict, votes, rationales), votes, nil
}

// runSkeptic evaluates one skeptic. It returns an error — never a fabricated
// verdict — whenever the skeptic did not actually produce a judgment: the agent
// failed to start or crashed (infrastructure), or it returned no parseable
// structured output. "The verification failed" and "the verification could not
// be performed" are different facts, and only the first may be reported as a
// verdict. Reporting the second as PLAUSIBLE (as this used to) made a verify
// step in which zero skeptics ran report `completed` with a "plausible" verdict
// and pass the gate.
func (s *VerifyStep) runSkeptic(ctx context.Context, sctx *pipeline.StepContext, target verifyTarget, evidenceCtx string, n, total int) (skeptic, error) {
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
		// A NUL byte anywhere in argv makes exec fail with EINVAL
		// ("fork/exec ...: invalid argument"), which would kill every skeptic
		// for this target. renderEvidenceContext already refuses to inline
		// binary artifacts; this is the last-resort scrub for anything else
		// (claim text, finding text) that reaches the prompt.
		Prompt:     stripNUL(prompt),
		CWD:        sctx.WorkDir,
		JSONSchema: skepticSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		sctx.Log(fmt.Sprintf("verify: skeptic %d of %d failed to run: %v", n, total, err))
		return skeptic{}, err
	}
	var out skeptic
	if len(result.Output) == 0 {
		return skeptic{}, fmt.Errorf("skeptic returned no structured verdict")
	}
	if err := json.Unmarshal(result.Output, &out); err != nil {
		return skeptic{}, fmt.Errorf("skeptic returned unparseable output: %w", err)
	}
	if strings.TrimSpace(out.Verdict) == "" {
		return skeptic{}, fmt.Errorf("skeptic returned an empty verdict")
	}
	out.Verdict = normalizeVerdict(out.Verdict)
	return out, nil
}

// stripNUL removes NUL bytes, which cannot appear in an exec argv.
func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

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
// is nothing to refute) plus semantic findings from the review step. It also
// returns the run's claims, which the runtime-backing check needs for their kind
// and hunks (a verifyTarget deliberately carries neither).
func (s *VerifyStep) gatherTargets(sctx *pipeline.StepContext, byID map[string]evidence.LoadedEntry) ([]verifyTarget, []claims.Claim, error) {
	var targets []verifyTarget
	var claimList []claims.Claim

	if sctx.DB != nil {
		var err error
		claimList, err = sctx.DB.GetClaimsByRun(sctx.Run.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("load claims: %w", err)
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
	return targets, claimList, nil
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
		snippet, notes := readEvidenceSnippet(workDir, e)
		if snippet != "" {
			fmt.Fprintf(&b, "  output:\n%s\n", indentLines(snippet, "    "))
		}
		for _, note := range notes {
			fmt.Fprintf(&b, "  %s\n", note)
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

// readEvidenceSnippet returns a text snippet from the first readable *text*
// artifact of e, plus one note per artifact that exists but cannot be inlined.
//
// Binary artifacts (screenshots are the common case: `evidence attach` accepts
// any file) are NEVER inlined. Their bytes would land verbatim in the skeptic
// agent's argv, and a single NUL byte — a PNG has one at offset 8 — makes exec
// fail with EINVAL, surfacing as `fork/exec <agent>: invalid argument`. That
// killed every skeptic for any claim citing a screenshot, while the other
// pipeline steps (which never inline evidence) ran the same agent fine.
func readEvidenceSnippet(workDir string, e evidence.LoadedEntry) (string, []string) {
	const maxSnippet = 1500
	var notes []string
	for _, p := range e.Paths {
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(workDir, filepath.FromSlash(p))
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if isBinary(data) {
			notes = append(notes, fmt.Sprintf("artifact %s: binary file (%d bytes) — content not shown here; judge it from the label and the other evidence, and do not assume it supports the claim", filepath.Base(p), len(data)))
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		if len(text) > maxSnippet {
			text = text[:maxSnippet] + "\n…(truncated)"
		}
		return text, notes
	}
	return "", notes
}

// isBinary reports content that must not be pasted into a prompt: anything with
// a NUL byte (fatal for exec argv) or that is not valid UTF-8.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
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
