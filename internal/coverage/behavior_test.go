package coverage

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
)

func behaviorClaim(id, kind string, hunks ...string) claims.Claim {
	return claims.Claim{ID: id, Kind: kind, Text: "the row is no longer green", Evidence: []string{"ev-1"}, Hunks: hunks}
}

// The coze 6951 shape: the ledger honestly says every product hunk is attested
// (an agent's word, nothing executed it), and a regression-fixed claim rests on
// exactly those hunks.
func TestBehaviorBacking_AttestedHunksBackNothing(t *testing.T) {
	ledger := []LedgerEntry{
		{File: "FileExplorerRow.tsx", StartLine: 280, EndLine: 280, State: StateAttested},
		{File: "FileExplorerRow.tsx", StartLine: 411, EndLine: 411, State: StateAttested},
	}
	gaps, _ := BehaviorBacking([]claims.Claim{
		behaviorClaim("c1", claims.KindRegressionFixed, "FileExplorerRow.tsx:280-280", "FileExplorerRow.tsx:411"),
	}, ledger)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want 1", gaps)
	}
	if !strings.Contains(gaps[0].Detail, "attested") || len(gaps[0].Hunks) != 2 {
		t.Fatalf("gap must name the attested hunks it rests on, got %+v", gaps[0])
	}
}

func TestBehaviorBacking_RuntimeVerifiedHunkBacksTheClaim(t *testing.T) {
	ledger := []LedgerEntry{
		{File: "row.tsx", StartLine: 10, EndLine: 12, State: StateRuntimeVerified},
		{File: "row.tsx", StartLine: 40, EndLine: 41, State: StateAttested},
	}
	gaps, _ := BehaviorBacking([]claims.Claim{
		behaviorClaim("c1", claims.KindBehavior, "row.tsx:10-12", "row.tsx:40-41"),
	}, ledger)
	if len(gaps) != 0 {
		t.Fatalf("one runtime-verified hunk is enough backing, got %+v", gaps)
	}
}

// A claim that names no hunks (the common case — --hunks is optional) falls back
// to the run: if nothing in the whole diff was runtime-verified, no
// instrumentation ever saw the change, so the claim has no runtime backing.
func TestBehaviorBacking_RunWideFallback(t *testing.T) {
	noRuntime := []LedgerEntry{{File: "row.tsx", StartLine: 1, EndLine: 2, State: StateAttested}}
	if gaps, _ := BehaviorBacking([]claims.Claim{behaviorClaim("c1", claims.KindBehavior)}, noRuntime); len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want 1 (nothing in the run is runtime-verified)", gaps)
	}

	someRuntime := append(noRuntime, LedgerEntry{File: "row.tsx", StartLine: 9, EndLine: 9, State: StateRuntimeVerified})
	if gaps, _ := BehaviorBacking([]claims.Claim{behaviorClaim("c1", claims.KindBehavior)}, someRuntime); len(gaps) != 0 {
		t.Fatalf("gaps = %+v, want 0 (the run does have instrumentation truth)", gaps)
	}
}

// An empty ledger — including the fail-closed "the coverage audit could not run"
// case — is not a clean bill of health.
func TestBehaviorBacking_EmptyLedgerBacksNothing(t *testing.T) {
	if gaps, _ := BehaviorBacking([]claims.Claim{behaviorClaim("c1", claims.KindBehavior)}, nil); len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want 1: could-not-check is not checked-and-clean", gaps)
	}
}

// rule-compliance and non-goal claims assert nothing about runtime behavior, and
// a self-attested claim can never be a conclusion, so none of them is subject to
// the runtime-backing requirement.
func TestBehaviorBacking_NonBehaviorKindsAndSelfAttestedAreExempt(t *testing.T) {
	cs := []claims.Claim{
		behaviorClaim("c1", claims.KindRuleCompliance),
		behaviorClaim("c2", claims.KindNonGoal),
		{ID: "c3", Kind: claims.KindBehavior, Text: "trust me"}, // no evidence
	}
	if gaps, _ := BehaviorBacking(cs, nil); len(gaps) != 0 {
		t.Fatalf("gaps = %+v, want none", gaps)
	}
}

func TestParseHunkRef(t *testing.T) {
	cases := []struct {
		in    string
		file  string
		start int
		end   int
		rng   bool
	}{
		{"a/b.tsx:10-12", "a/b.tsx", 10, 12, true},
		{"a/b.tsx:10", "a/b.tsx", 10, 10, true},
		{"a/b.tsx", "a/b.tsx", 0, 0, false},
		{"a/b.tsx:notanumber", "a/b.tsx:notanumber", 0, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseHunkRef(tc.in)
		if !ok || got.file != tc.file || got.hasRange != tc.rng {
			t.Fatalf("parseHunkRef(%q) = %+v ok=%v, want file=%q range=%v", tc.in, got, ok, tc.file, tc.rng)
		}
		if tc.rng && (got.rng.Start != tc.start || got.rng.End != tc.end) {
			t.Fatalf("parseHunkRef(%q) range = %+v, want %d-%d", tc.in, got.rng, tc.start, tc.end)
		}
	}
}
