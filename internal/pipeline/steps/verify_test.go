package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// verdictAgent returns a fixed skeptic verdict for every call.
func verdictAgent(verdict, rationale string) *mockAgent {
	return &mockAgent{
		name: "mock",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			out, _ := json.Marshal(skeptic{Verdict: verdict, Rationale: rationale})
			return &agent.Result{Output: out}, nil
		},
	}
}

// seedVerifyContext builds a verify StepContext over a real git repo (so the
// coverage audit can diff base..head) with paths, an evidence store, one
// captured command-output entry, and captured instrumentation coverage over the
// changed hunk — i.e. a run whose behavior claims ARE runtime-backed. Returns
// the context and the command-output evidence id.
func seedVerifyContext(t *testing.T, ag agent.Agent) (*pipeline.StepContext, string) {
	t.Helper()
	sctx, evID, store := seedVerifyContextWithoutRuntimeCoverage(t, ag)
	// The repo template's diff is feature.txt:1. Capture instrumentation that
	// executed exactly that line, so backfill promotes the hunk to
	// runtime-verified the way a real `go test -coverprofile` run would.
	profile := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(profile, []byte("mode: set\nfeature.txt:1.1,1.13 1 1\n"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	if _, err := store.Coverage(context.Background(), evidence.CoverageOpts{
		Label: "unit tests with coverage", Argv: []string{"printf", "ok"},
		Format: coverage.FormatGo, CoverProfile: profile,
		Dir: sctx.WorkDir, RepoRoot: sctx.WorkDir,
	}); err != nil {
		t.Fatalf("coverage evidence: %v", err)
	}
	return sctx, evID
}

// seedVerifyContextWithoutRuntimeCoverage is the same context with NO
// instrumentation coverage captured: every changed hunk stays attested /
// unverified. This is the coze 6951 shape — an agent asserting user-visible
// behavior with nothing that ever executed the changed code.
func seedVerifyContextWithoutRuntimeCoverage(t *testing.T, ag agent.Agent) (*pipeline.StepContext, string, *evidence.Store) {
	t.Helper()
	repoDir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, ag, repoDir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Verify = config.Verify{Skeptics: 3}

	sctx.Paths = paths.WithRoot(t.TempDir())
	key, err := evidence.LoadOrCreateKey(sctx.Paths.EvidenceKeyFile())
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	store, err := evidence.Open(evidence.DirForBranch(repoDir, sctx.Run.Branch), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, err := store.Exec(context.Background(), evidence.ExecOpts{
		Label: "login e2e", Argv: []string{"printf", "PASS"}, Dir: repoDir, RepoRoot: repoDir,
	})
	if err != nil {
		t.Fatalf("exec evidence: %v", err)
	}
	return sctx, entry.ID, store
}

func TestVerifyStep_RefutedParks(t *testing.T) {
	ag := verdictAgent(claims.VerdictRefuted, "the output does not show the login page at all")
	sctx, evID := seedVerifyContext(t, ag)
	claim, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "login works on mobile",
		Kind: claims.KindBehavior, Evidence: []string{evID},
	})
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("REFUTED claim must park (NeedsApproval)")
	}
	if !outcome.AutoFixable {
		t.Fatal("REFUTED claim should be auto-fixable so respond --action fix works")
	}

	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictRefuted {
		t.Fatalf("claim verdict = %q, want REFUTED", got[0].Verdict)
	}

	verdicts, _ := sctx.DB.GetVerifyVerdictsByRun(sctx.Run.ID)
	if len(verdicts) != 1 || verdicts[0].ClaimID != claim.ID {
		t.Fatalf("expected one verdict record for the claim, got %+v", verdicts)
	}
	if len(verdicts[0].Votes) != 3 {
		t.Fatalf("expected 3 skeptic votes, got %d", len(verdicts[0].Votes))
	}

	// The finding carries the rationale and the evidence id. (The coverage audit
	// also contributes a non-parking warning here: this fixture is not a git
	// repo, so the audit cannot run — see TestVerifyStep_CoverageAuditFailureIsVisible.)
	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	var errFindings []Finding
	for _, it := range f.Items {
		if it.Severity == "error" {
			errFindings = append(errFindings, it)
		}
	}
	if len(errFindings) != 1 {
		t.Fatalf("expected one error finding, got %+v", f.Items)
	}
	if !strings.Contains(errFindings[0].Description, "REFUTED") {
		t.Fatalf("error finding must carry the refutation, got %q", errFindings[0].Description)
	}
}

// TestVerifyStep_CoverageAuditFailureIsVisible: a coverage audit that could not
// run is not a coverage audit that passed. It stays non-parking, but it must
// show up as a finding — logging "skipped" reads as "nothing to report", the
// same silent-degradation shape as the skeptic bug above.
func TestVerifyStep_CoverageAuditFailureIsVisible(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "clear")
	sctx, evID := seedVerifyContext(t, ag)
	sctx.WorkDir = t.TempDir() // not a git repo: the audit cannot diff
	// A rule-compliance claim asserts no runtime behavior, so the audit failure
	// is the only thing under test here: it must be visible, and on its own it
	// must not park. (A behavior claim in the same situation DOES park — see
	// TestVerifyStep_CoverageAuditFailureBlocksBehaviorRuntimePass.)
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "no new lint suppressions", Kind: claims.KindRuleCompliance, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("a coverage-audit failure must not park the run")
	}
	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	var found bool
	for _, it := range f.Items {
		if strings.Contains(it.Description, "coverage audit did not run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("coverage audit failure must surface as a finding, got %+v", f.Items)
	}
	if !strings.Contains(f.Summary, "coverage audit did not run") {
		t.Fatalf("summary must not claim coverage was evaluated, got %q", f.Summary)
	}
}

func TestVerifyStep_ConfirmedPasses(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the transcript clearly shows the fix")
	sctx, evID := seedVerifyContext(t, ag)
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "login works", Kind: claims.KindBehavior, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("CONFIRMED claim must not park")
	}
	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictConfirmed {
		t.Fatalf("claim verdict = %q, want CONFIRMED", got[0].Verdict)
	}
}

func TestVerifyStep_SelfAttestedClaimNotVerified(t *testing.T) {
	ag := verdictAgent(claims.VerdictRefuted, "no evidence")
	sctx, _ := seedVerifyContext(t, ag)
	// Evidence-less claim: must be skipped by verify (nothing to refute).
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "trust me", Kind: claims.KindBehavior,
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("self-attested claim must not be adjudicated or park the run")
	}
	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictUnverified {
		t.Fatalf("self-attested claim must keep an empty verdict, got %q", got[0].Verdict)
	}
}

func TestVerifyStep_NoClaimsNoOp(t *testing.T) {
	ag := verdictAgent(claims.VerdictRefuted, "should not be called")
	sctx, _ := seedVerifyContext(t, ag)
	// No claims inserted.
	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("no targets means no park")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("skeptics must not run when there is nothing to verify, got %d calls", len(ag.calls))
	}
}

// TestVerifyStep_SkepticStartFailureFailsStep pins the core semantics: an agent
// that never ran produced NO verdict. The step must fail loudly rather than
// report `completed` with a fabricated PLAUSIBLE that lets the pipeline through.
// Observed in run 01KX979BRHJ2E7BJX586CMJJCE: both skeptics died with
// "fork/exec .../cc: invalid argument", verify finished in 26ms and reported
// "0 confirmed, 2 plausible, 0 refuted" — a green gate that verified nothing.
func TestVerifyStep_SkepticStartFailureFailsStep(t *testing.T) {
	startErr := errors.New("claude start: fork/exec /usr/local/bin/cc: invalid argument")
	ag := &mockAgent{
		name: "mock",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return nil, startErr
		},
	}
	sctx, evID := seedVerifyContext(t, ag)
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "login works", Kind: claims.KindBehavior, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("a skeptic that could not start must fail the step, got outcome %+v", outcome)
	}
	if outcome != nil {
		t.Fatalf("a failed step must not return an outcome, got %+v", outcome)
	}
	if !strings.Contains(err.Error(), "verification did not run") {
		t.Fatalf("error must say the verification did not run, got %v", err)
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("error must wrap the agent failure, got %v", err)
	}

	// No optimistic verdict may be invented anywhere.
	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict == claims.VerdictPlausible || got[0].Verdict == claims.VerdictConfirmed {
		t.Fatalf("claim must not get a verdict when no skeptic ran, got %q", got[0].Verdict)
	}
	verdicts, _ := sctx.DB.GetVerifyVerdictsByRun(sctx.Run.ID)
	if len(verdicts) != 0 {
		t.Fatalf("no verdict may be recorded when no skeptic ran, got %+v", verdicts)
	}
}

// TestVerifyStep_EmptySkepticOutputFailsStep: the agent process ran but returned
// no structured verdict — still "could not verify", not "plausible".
func TestVerifyStep_EmptySkepticOutputFailsStep(t *testing.T) {
	ag := &mockAgent{
		name: "mock",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{}, nil // no Output
		},
	}
	sctx, evID := seedVerifyContext(t, ag)
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "login works", Kind: claims.KindBehavior, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	if _, err := (&VerifyStep{}).Execute(sctx); err == nil {
		t.Fatal("a skeptic that returned no structured verdict must fail the step")
	}
}

// TestVerifyStep_BinaryEvidenceIsNotInlinedIntoPrompt is the root-cause
// regression: a claim citing a screenshot used to paste the PNG's raw bytes into
// the agent's argv, and the NUL byte at PNG offset 8 makes exec return EINVAL.
// The prompt must stay exec-safe and must still tell the skeptic the artifact
// exists.
func TestVerifyStep_BinaryEvidenceIsNotInlinedIntoPrompt(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the screenshot label matches")
	sctx, _ := seedVerifyContext(t, ag)

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...) // header + NUL-heavy body
	shot := filepath.Join(t.TempDir(), "after.png")
	if err := os.WriteFile(shot, png, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	key, err := evidence.LoadOrCreateKey(sctx.Paths.EvidenceKeyFile())
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	store, err := evidence.Open(evidence.DirForBranch(sctx.WorkDir, sctx.Run.Branch), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, err := store.Attach(evidence.AttachOpts{
		Label: "after: row uses bg-accent", File: shot, RepoRoot: sctx.WorkDir, RunID: sctx.Run.ID,
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "the row is no longer gray", Kind: claims.KindBehavior,
		Evidence: []string{entry.ID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	// The primary layer: the evidence renderer itself must never emit the PNG's
	// bytes. (stripNUL in runSkeptic is only the last-resort backstop, so
	// asserting on the delivered prompt alone would not pin this.)
	loadedEntry := evidence.LoadedEntry{Entry: entry, Verified: true}
	rendered := (&VerifyStep{}).renderEvidenceContext(sctx.WorkDir, verifyTarget{
		text: "the row is no longer gray", evidenceIDs: []string{entry.ID},
		evidence: []evidence.LoadedEntry{loadedEntry},
	})
	if strings.ContainsRune(rendered, 0) {
		t.Fatal("rendered evidence context contains PNG bytes: the skeptic prompt would fail exec with EINVAL")
	}

	if _, err := (&VerifyStep{}).Execute(sctx); err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("skeptic never ran")
	}
	prompt := ag.calls[0].Prompt
	if strings.ContainsRune(prompt, 0) {
		t.Fatal("prompt contains a NUL byte: exec would fail with EINVAL (fork/exec: invalid argument)")
	}
	if !strings.Contains(prompt, "binary file") {
		t.Fatalf("prompt must tell the skeptic a binary artifact exists, got:\n%s", prompt)
	}
	// Directly pin the failure mode: this prompt must be usable as an exec arg.
	if err := exec.Command("/bin/echo", prompt).Run(); err != nil {
		t.Fatalf("prompt is not exec-safe: %v", err)
	}
}

func TestReadEvidenceSnippet_SkipsBinaryPrefersText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), []byte("\x89PNG\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.txt"), []byte("PASS"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := evidence.LoadedEntry{Entry: evidence.Entry{
		Paths: []string{filepath.Join(dir, "shot.png"), filepath.Join(dir, "stdout.txt")},
	}}
	snippet, notes := readEvidenceSnippet(dir, e)
	if snippet != "PASS" {
		t.Fatalf("snippet = %q, want the text artifact", snippet)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "binary file") {
		t.Fatalf("binary artifact must be reported as a note, got %v", notes)
	}
}

// TestVerifyStep_CoverageAuditResolvesZeroBaseSHA: the post-receive hook sends
// an all-zero "old" SHA for the first push of a branch, so run.BaseSHA is
// 000...0. The coverage audit used to hand that straight to `git diff`, which
// answered "Invalid revision range 000...0..<head>" and the audit silently
// never ran. It must resolve the base like every other step does.
func TestVerifyStep_CoverageAuditResolvesZeroBaseSHA(t *testing.T) {
	const zeroSHA = "0000000000000000000000000000000000000000"
	repoDir, _, headSHA := setupGitRepo(t)

	ag := verdictAgent(claims.VerdictConfirmed, "clear")
	sctx := newTestContextWithDBRecords(t, ag, repoDir, zeroSHA, headSHA, config.Commands{})
	sctx.Config.Verify = config.Verify{Skeptics: 1}
	sctx.Paths = paths.WithRoot(t.TempDir())
	key, err := evidence.LoadOrCreateKey(sctx.Paths.EvidenceKeyFile())
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	store, err := evidence.Open(evidence.DirForBranch(repoDir, sctx.Run.Branch), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, err := store.Exec(context.Background(), evidence.ExecOpts{
		Label: "unit tests", Argv: []string{"printf", "PASS"}, Dir: repoDir, RepoRoot: repoDir,
	})
	if err != nil {
		t.Fatalf("exec evidence: %v", err)
	}
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "feature works", Kind: claims.KindBehavior, Evidence: []string{entry.ID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	for _, it := range f.Items {
		if strings.Contains(it.Description, "coverage audit did not run") {
			t.Fatalf("coverage audit must resolve the zero base SHA, got %q", it.Description)
		}
	}
	// The audit really ran over the diff: the changed hunks are in the ledger.
	entries, err := sctx.DB.GetCoverageEntriesByRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("coverage audit produced no ledger rows: it did not see the diff")
	}
}

func TestMajorityVerdict(t *testing.T) {
	cases := []struct {
		votes []string
		want  string
	}{
		{[]string{"REFUTED", "REFUTED", "CONFIRMED"}, claims.VerdictRefuted},
		{[]string{"CONFIRMED", "CONFIRMED", "REFUTED"}, claims.VerdictConfirmed},
		{[]string{"REFUTED", "CONFIRMED", "PLAUSIBLE"}, claims.VerdictPlausible},
		{[]string{"PLAUSIBLE", "PLAUSIBLE", "PLAUSIBLE"}, claims.VerdictPlausible},
		{nil, claims.VerdictPlausible},
	}
	for _, c := range cases {
		if got := majorityVerdict(c.votes); got != c.want {
			t.Errorf("majorityVerdict(%v) = %q, want %q", c.votes, got, c.want)
		}
	}
}

// TestVerifyStep_BehaviorClaimWithoutRuntimeEvidenceCannotPass pins the second
// half of the coze 6951 failure: all four product hunks sat at "attested" (the
// ledger honestly reporting "an agent said so, nothing executed it") while the
// run's one product-behavior claim was recorded as verified and the run
// completed. "attested" is a report of missing runtime evidence, so it can never
// back a behavior-class claim: the claim may not be CONFIRMED, and the run parks
// for a decision instead of passing quietly.
func TestVerifyStep_BehaviorClaimWithoutRuntimeEvidenceCannotPass(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the screenshots show the fixed row color")
	sctx, evID, _ := seedVerifyContextWithoutRuntimeCoverage(t, ag)
	if _, err := sctx.DB.InsertCoverageEntry(coverage.LedgerEntry{
		RunID: sctx.Run.ID, File: "feature.txt", StartLine: 1, EndLine: 1,
		State: coverage.StateAttested, Evidence: []string{evID}, Source: "test",
	}); err != nil {
		t.Fatalf("insert coverage entry: %v", err)
	}
	claim, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "the preview row is no longer green",
		Kind: claims.KindRegressionFixed, Evidence: []string{evID},
	})
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a behavior claim with no runtime evidence must park, not pass quietly")
	}
	if outcome.AutoFixable {
		t.Fatal("the remedy (capture coverage, or restate the claim) is a decision, not an auto-fix")
	}

	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].ID != claim.ID || got[0].Verdict != claims.VerdictPlausible {
		t.Fatalf("claim verdict = %q, want the skeptic's CONFIRMED capped to PLAUSIBLE", got[0].Verdict)
	}

	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	var found bool
	for _, it := range f.Items {
		if strings.Contains(it.Description, "NO RUNTIME EVIDENCE") {
			found = true
			if it.Severity != "error" || it.Action != types.ActionAskUser {
				t.Fatalf("runtime-evidence gap must be an ask-user error finding, got %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("no NO RUNTIME EVIDENCE finding in %+v", f.Items)
	}
}

// TestVerifyStep_CoverageAuditFailureBlocksBehaviorRuntimePass: an audit that
// could not run has not found the change runtime-verified. Fail closed — a
// behavior claim in such a run is not allowed a runtime pass either.
func TestVerifyStep_CoverageAuditFailureBlocksBehaviorRuntimePass(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "clear")
	sctx, evID, _ := seedVerifyContextWithoutRuntimeCoverage(t, ag)
	sctx.WorkDir = t.TempDir() // not a git repo: the audit cannot diff
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "login works on mobile",
		Kind: claims.KindBehavior, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("could-not-check must not become checked-and-clean for a behavior claim")
	}
	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictPlausible {
		t.Fatalf("claim verdict = %q, want PLAUSIBLE (no runtime backing could be established)", got[0].Verdict)
	}
}

// TestVerifyStep_RuntimeVerifiedHunkBacksBehaviorClaim is the other side of the
// contract: when instrumentation actually executed the changed code, a behavior
// claim passes untouched. Without this the gate above would just be a blanket
// "behavior claims always park".
func TestVerifyStep_RuntimeVerifiedHunkBacksBehaviorClaim(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the captured run shows it")
	sctx, evID := seedVerifyContext(t, ag) // captures coverage over feature.txt:1
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "the feature works",
		Kind: claims.KindBehavior, Evidence: []string{evID},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("a runtime-verified behavior claim must pass, got findings %s", outcome.Findings)
	}
	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictConfirmed {
		t.Fatalf("claim verdict = %q, want CONFIRMED", got[0].Verdict)
	}
	entries, _ := sctx.DB.GetCoverageEntriesByRun(sctx.Run.ID)
	if len(entries) == 0 || entries[0].State != coverage.StateRuntimeVerified {
		t.Fatalf("backfill should have promoted the executed hunk, got %+v", entries)
	}
}
