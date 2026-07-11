package steps

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

// seedVerifyContext builds a verify StepContext with paths, an evidence store,
// and one captured evidence entry, returning the context and the evidence id.
func seedVerifyContext(t *testing.T, ag agent.Agent) (*pipeline.StepContext, string) {
	t.Helper()
	workDir := t.TempDir()
	sctx := newTestContextWithDBRecords(t, ag, workDir, "base", "head", config.Commands{})
	sctx.Config.Verify = config.Verify{Skeptics: 3}

	nmHome := t.TempDir()
	sctx.Paths = paths.WithRoot(nmHome)
	key, err := evidence.LoadOrCreateKey(sctx.Paths.EvidenceKeyFile())
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	store, err := evidence.Open(evidence.DirForBranch(workDir, sctx.Run.Branch), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	entry, err := store.Exec(context.Background(), evidence.ExecOpts{
		Label: "login e2e", Argv: []string{"printf", "PASS"}, Dir: workDir, RepoRoot: workDir,
	})
	if err != nil {
		t.Fatalf("exec evidence: %v", err)
	}
	return sctx, entry.ID
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

	// The finding carries the rationale and the evidence id.
	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(f.Items) != 1 || f.Items[0].Severity != "error" {
		t.Fatalf("expected one error finding, got %+v", f.Items)
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
