package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
)

func newRunForClaims(t *testing.T, d *DB) *Run {
	t.Helper()
	repo, err := d.InsertRepo("/tmp/repo", "https://example.com/x.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "fm/x", "headsha", "basesha")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return run
}

func TestInsertAndGetClaims(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)

	c, err := d.InsertClaim(claims.Claim{
		RunID:    run.ID,
		Step:     "test",
		Text:     "login no longer overflows on mobile",
		Kind:     claims.KindRegressionFixed,
		Evidence: []string{"ev-7f3a", "ev-8b21"},
		Hunks:    []string{"src/login.tsx:40-88"},
	})
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected assigned claim ID")
	}

	got, err := d.GetClaimsByRun(run.ID)
	if err != nil {
		t.Fatalf("get claims: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
	if len(got[0].Evidence) != 2 || got[0].Evidence[0] != "ev-7f3a" {
		t.Fatalf("evidence round-trip failed: %v", got[0].Evidence)
	}
	if got[0].SelfAttested() {
		t.Fatal("evidence-bound claim must not be self-attested")
	}
}

func TestEvidenceLessClaimIsSelfAttested(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)

	if _, err := d.InsertClaim(claims.Claim{
		RunID: run.ID,
		Step:  "test",
		Text:  "I believe this works",
		Kind:  claims.KindBehavior,
		// no evidence
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	got, err := d.GetClaimsByRun(run.ID)
	if err != nil {
		t.Fatalf("get claims: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d claims, want 1", len(got))
	}
	if !got[0].SelfAttested() {
		t.Fatal("evidence-less claim must be self-attested (machine-enforced demotion)")
	}
}

func TestSetClaimVerdict(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)
	c, _ := d.InsertClaim(claims.Claim{RunID: run.ID, Step: "test", Text: "x", Kind: claims.KindBehavior, Evidence: []string{"ev-1"}})

	if err := d.SetClaimVerdict(c.ID, claims.VerdictRefuted, "verify/skeptic-majority"); err != nil {
		t.Fatalf("set verdict: %v", err)
	}
	got, _ := d.GetClaimsByRun(run.ID)
	if got[0].Verdict != claims.VerdictRefuted {
		t.Fatalf("verdict = %q, want REFUTED", got[0].Verdict)
	}
	if got[0].VerdictBy != "verify/skeptic-majority" {
		t.Fatalf("verdict_by = %q", got[0].VerdictBy)
	}
}

func TestVerifyVerdictRecords(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)
	c, _ := d.InsertClaim(claims.Claim{RunID: run.ID, Step: "test", Text: "x", Kind: claims.KindBehavior, Evidence: []string{"ev-1"}})

	if _, err := d.InsertVerifyVerdict(VerifyVerdict{
		RunID:     run.ID,
		ClaimID:   c.ID,
		Verdict:   claims.VerdictRefuted,
		Rationale: "the screenshot shows an unrelated page",
		Evidence:  []string{"ev-1"},
		Votes:     []string{"REFUTED", "REFUTED", "PLAUSIBLE"},
	}); err != nil {
		t.Fatalf("insert verify verdict: %v", err)
	}

	got, err := d.GetVerifyVerdictsByRun(run.ID)
	if err != nil {
		t.Fatalf("get verdicts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d verdicts, want 1", len(got))
	}
	if got[0].Verdict != claims.VerdictRefuted || len(got[0].Votes) != 3 {
		t.Fatalf("verdict record round-trip failed: %+v", got[0])
	}
}
