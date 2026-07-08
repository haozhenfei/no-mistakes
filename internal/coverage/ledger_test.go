package coverage

import (
	"testing"
)

func goCov(file string, ranges ...LineRange) CoverageData {
	return CoverageData{Format: FormatGo, Files: []FileCoverage{{File: file, Covered: ranges}}}
}

func TestBackfill_DowngradesFalseRuntimeClaim(t *testing.T) {
	// Agent labelled two hunks runtime-verified; instrumentation only executed
	// the first. The second must be downgraded — this is the core anti-abuse
	// mechanism (design §4 principle 4).
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateRuntimeVerified},
		{File: "foo.go", StartLine: 50, EndLine: 55, State: StateRuntimeVerified},
	}
	datasets := []CoverageData{goCov("github.com/org/repo/foo.go", LineRange{10, 12})}

	out, downgrades := Backfill(ledger, datasets, []string{"ev-cov1"})

	if out[0].State != StateRuntimeVerified {
		t.Errorf("covered hunk state = %q, want runtime-verified", out[0].State)
	}
	if len(out[0].Evidence) != 1 || out[0].Evidence[0] != "ev-cov1" {
		t.Errorf("covered hunk evidence = %v, want [ev-cov1]", out[0].Evidence)
	}
	if out[1].State != StateAttested {
		t.Errorf("uncovered hunk state = %q, want attested (downgraded)", out[1].State)
	}
	if out[1].Reason == "" {
		t.Error("downgraded hunk must carry a reason")
	}
	if len(downgrades) != 1 {
		t.Fatalf("downgrades = %d, want 1", len(downgrades))
	}
	if downgrades[0].From != StateRuntimeVerified || downgrades[0].To != StateAttested {
		t.Errorf("downgrade = %+v, want runtime-verified→attested", downgrades[0])
	}
}

func TestBackfill_PromotesCoveredHunk(t *testing.T) {
	// A hunk the agent left "unverified" but instrumentation executed becomes
	// runtime-verified: the machine found real execution.
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateUnverified, Reason: "no test written"},
	}
	datasets := []CoverageData{goCov("foo.go", LineRange{11, 11})}

	out, downgrades := Backfill(ledger, datasets, []string{"ev-cov"})
	if out[0].State != StateRuntimeVerified {
		t.Errorf("state = %q, want runtime-verified", out[0].State)
	}
	if out[0].Reason != "" {
		t.Errorf("promoted hunk should clear its reason, got %q", out[0].Reason)
	}
	if len(downgrades) != 0 {
		t.Errorf("promotion should record no downgrades, got %v", downgrades)
	}
}

func TestBackfill_DowngradeToStaticWhenEvidencePresent(t *testing.T) {
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateRuntimeVerified, Evidence: []string{"ev-typecheck"}},
	}
	// No instrumentation covers it.
	out, downgrades := Backfill(ledger, nil, nil)
	if out[0].State != StateStaticVerified {
		t.Errorf("state = %q, want static-verified (has evidence)", out[0].State)
	}
	if len(downgrades) != 1 || downgrades[0].To != StateStaticVerified {
		t.Errorf("downgrade = %+v, want →static-verified", downgrades)
	}
}

func TestBackfill_LeavesNonRuntimeUncoveredAlone(t *testing.T) {
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateStaticVerified, Evidence: []string{"ev-1"}},
		{File: "foo.go", StartLine: 20, EndLine: 22, State: StateUnverified, Reason: "config only"},
	}
	out, downgrades := Backfill(ledger, nil, nil)
	if out[0].State != StateStaticVerified || out[1].State != StateUnverified {
		t.Errorf("non-runtime uncovered entries changed: %+v", out)
	}
	if len(downgrades) != 0 {
		t.Errorf("no downgrades expected, got %v", downgrades)
	}
}

func TestAudit_Completeness(t *testing.T) {
	changed := []Hunk{
		{File: "foo.go", Start: 10, End: 12},
		{File: "foo.go", Start: 30, End: 31}, // no ledger entry → missing
	}
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateRuntimeVerified},
	}
	datasets := []CoverageData{goCov("foo.go", LineRange{10, 12})}

	rep := Audit(changed, ledger, datasets, nil)
	if rep.Pass {
		t.Error("audit should fail when a changed hunk has no ledger entry")
	}
	if got := countIssues(rep, "missing-ledger"); got != 1 {
		t.Errorf("missing-ledger issues = %d, want 1", got)
	}
	if rep.TotalHunks != 2 || rep.RuntimeVerified != 1 {
		t.Errorf("totals wrong: %+v", rep)
	}
	if rep.CoverageRate != 0.5 {
		t.Errorf("CoverageRate = %v, want 0.5", rep.CoverageRate)
	}
}

func TestAudit_FalseRuntimeCaught(t *testing.T) {
	// A runtime-verified label that survived without backfill correction (e.g. a
	// ledger written directly) is caught by the audit's truth check.
	changed := []Hunk{{File: "foo.go", Start: 10, End: 12}}
	ledger := []LedgerEntry{{File: "foo.go", StartLine: 10, EndLine: 12, State: StateRuntimeVerified}}
	rep := Audit(changed, ledger, nil /* no coverage */, nil)
	if rep.Pass {
		t.Error("audit should fail on runtime-verified hunk with no coverage")
	}
	if countIssues(rep, "false-runtime") != 1 {
		t.Errorf("want 1 false-runtime issue, got %+v", rep.Issues)
	}
}

func TestAudit_WeakStaticRejected(t *testing.T) {
	changed := []Hunk{
		{File: "a.go", Start: 1, End: 2},
		{File: "b.go", Start: 1, End: 2},
	}
	ledger := []LedgerEntry{
		{File: "a.go", StartLine: 1, EndLine: 2, State: StateStaticVerified, Evidence: []string{"ev-typecheck"}},
		{File: "b.go", StartLine: 1, EndLine: 2, State: StateStaticVerified, Evidence: []string{"ev-attested-note"}},
	}
	// Only ev-typecheck is captured executable static evidence.
	staticOK := func(id string) bool { return id == "ev-typecheck" }
	rep := Audit(changed, ledger, nil, staticOK)
	if rep.Pass {
		t.Error("audit should fail when a static-verified hunk lacks executable static evidence")
	}
	if countIssues(rep, "weak-static") != 1 {
		t.Errorf("want 1 weak-static issue, got %+v", rep.Issues)
	}
	if rep.StaticVerified != 1 {
		t.Errorf("StaticVerified = %d, want 1", rep.StaticVerified)
	}
}

func TestAudit_StaticVerifiedNoEvidenceRejected(t *testing.T) {
	changed := []Hunk{{File: "a.go", Start: 1, End: 2}}
	ledger := []LedgerEntry{{File: "a.go", StartLine: 1, EndLine: 2, State: StateStaticVerified}}
	rep := Audit(changed, ledger, nil, nil)
	if rep.Pass {
		t.Error("static-verified with no evidence at all must fail")
	}
	if countIssues(rep, "weak-static") != 1 {
		t.Errorf("want 1 weak-static issue, got %+v", rep.Issues)
	}
}

func TestAudit_CleanPass(t *testing.T) {
	changed := []Hunk{
		{File: "foo.go", Start: 10, End: 12},
		{File: "foo.go", Start: 20, End: 21},
	}
	ledger := []LedgerEntry{
		{File: "foo.go", StartLine: 10, EndLine: 12, State: StateRuntimeVerified},
		{File: "foo.go", StartLine: 20, EndLine: 21, State: StateStaticVerified, Evidence: []string{"ev-tc"}},
	}
	datasets := []CoverageData{goCov("foo.go", LineRange{10, 12})}
	rep := Audit(changed, ledger, datasets, func(string) bool { return true })
	if !rep.Pass {
		t.Errorf("expected clean pass, got issues %+v", rep.Issues)
	}
	if rep.RuntimeVerified != 1 || rep.StaticVerified != 1 {
		t.Errorf("totals wrong: %+v", rep)
	}
	// Uncovered list is data for §6 routing: the static hunk is not runtime-run.
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Start != 20 {
		t.Errorf("Uncovered = %+v, want the static hunk", rep.Uncovered)
	}
}

func TestAudit_EndToEndWithBackfill(t *testing.T) {
	// The full deliverable path: parse a go profile, parse a diff, backfill the
	// agent's over-eager ledger, then audit.
	profile := `mode: set
github.com/org/repo/calc.go:3.10,5.2 2 1
github.com/org/repo/calc.go:8.10,9.2 1 0
`
	cov, err := ParseGoProfile(profile)
	if err != nil {
		t.Fatalf("ParseGoProfile: %v", err)
	}
	diff := `diff --git a/calc.go b/calc.go
--- a/calc.go
+++ b/calc.go
@@ -2,0 +3,2 @@
+	return a + b
+	// covered
@@ -7,0 +8,1 @@
+	return a - b
`
	changed := ParseDiffHunks(diff)
	if len(changed) != 2 {
		t.Fatalf("expected 2 changed hunks, got %+v", changed)
	}
	// Agent optimistically claimed both runtime-verified.
	ledger := []LedgerEntry{
		{File: changed[0].File, StartLine: changed[0].Start, EndLine: changed[0].End, State: StateRuntimeVerified},
		{File: changed[1].File, StartLine: changed[1].Start, EndLine: changed[1].End, State: StateRuntimeVerified},
	}
	backfilled, downgrades := Backfill(ledger, []CoverageData{cov}, []string{"ev-cov"})
	if len(downgrades) != 1 {
		t.Fatalf("expected 1 downgrade (the uncovered subtraction hunk), got %+v", downgrades)
	}
	rep := Audit(changed, backfilled, []CoverageData{cov}, nil)
	// After backfill: hunk1 runtime-verified (covered), hunk2 downgraded to
	// attested. Audit passes (no false-runtime survives) but coverage is partial.
	if !rep.Pass {
		t.Errorf("audit should pass after backfill correction, issues: %+v", rep.Issues)
	}
	if rep.RuntimeVerified != 1 || rep.Attested != 1 {
		t.Errorf("post-backfill totals wrong: %+v", rep)
	}
	if rep.CoverageRate != 0.5 {
		t.Errorf("CoverageRate = %v, want 0.5", rep.CoverageRate)
	}
}

func countIssues(rep AuditReport, kind string) int {
	n := 0
	for _, i := range rep.Issues {
		if i.Kind == kind {
			n++
		}
	}
	return n
}
