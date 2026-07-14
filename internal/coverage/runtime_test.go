package coverage

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
)

// v8JSXLCOV is a REAL lcov tracefile, produced by running vitest with
// `coverage: { provider: 'v8' }` (the provider coze pins in its shared
// vitest preset) over the component in v8JSXSource below. It is not
// hand-written: it is what @vitest/coverage-v8 emits, captured verbatim.
//
// The three lines that matter, and why this one file is the whole feature:
//
//	line 7  — a JSX className attribute inside the `editing` branch, which the
//	          test never renders. NO DA record. It did not run.
//	line 14 — a JSX className attribute inside the rendered branch. NO DA record
//	          either — v8 folds JSX attribute lines into the enclosing `return`.
//	          It DID run.
//	line 24 — the body of a function nobody calls. DA:24,0 — instrumented and
//	          positively unexecuted.
//
// So the engine emits nothing at all for lines 7 and 14, and the naive question
// "does a covered line intersect this hunk?" answers "no" for both. The only
// signal that separates them is the enclosing statement record: line 7 hangs off
// `DA:5,0` (a `return (` that never ran) and line 14 off `DA:12,1` (a `return (`
// that ran). Note FNDA:1,Row — the enclosing FUNCTION was hit for BOTH, which is
// why a function-level signal would have called the dead hunk executed.
const v8JSXLCOV = `TN:
SF:src/Row.tsx
FN:3,Row
FN:23,neverCalled
FNF:2
FNH:1
FNDA:1,Row
FNDA:0,neverCalled
DA:4,1
DA:5,0
DA:12,1
DA:24,0
DA:25,0
LF:5
LH:2
BRDA:4,0,0,0
BRDA:4,0,1,1
BRF:2
BRH:1
end_of_record
`

// The file the lcov above was captured from, with line numbers, so the fixture
// can be read without re-running vitest:
//
//	 1  import React from 'react';
//	 2
//	 3  export function Row({ name, editing }) {
//	 4    if (editing) {
//	 5      return (
//	 6        <input
//	 7          className="border-accent bg-surface"   <- changed, never rendered
//	 8          defaultValue={name}
//	 9        />
//	10      );
//	11    }
//	12    return (
//	13      <div
//	14        className="text-accent font-medium"      <- changed, rendered
//	15        data-testid="row"
//	16        title={name}
//	17      >
//	18        {name}
//	19      </div>
//	20    );
//	21  }
//	22
//	23  export function neverCalled(a) {
//	24    const doubled = a * 2;                       <- changed, never called
//	25    return doubled + 1;
//	26  }

func v8Dataset(t *testing.T) CoverageData {
	t.Helper()
	d, err := ParseLCOV(v8JSXLCOV)
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}
	return d
}

// The three states, decided against real v8 output.
func TestClassifyHunk_ThreeStatesOnRealV8LCOV(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}

	cases := []struct {
		name string
		hunk Hunk
		want string
	}{
		{
			// The line the enclosing `return (` on 12 executed. v8 emits no record
			// for it; the gate must NOT read that silence as non-execution.
			name: "JSX attribute in a rendered branch is uninstrumented, not unexecuted",
			hunk: Hunk{File: "src/Row.tsx", Start: 14, End: 14},
			want: RuntimeUninstrumented,
		},
		{
			// Same construct, same file, same hit function — but its `return (` on
			// line 5 has zero hits. This hunk really is dead, and the classifier
			// must still catch it.
			name: "JSX attribute in a branch that never rendered is not-executed",
			hunk: Hunk{File: "src/Row.tsx", Start: 7, End: 7},
			want: RuntimeNotExecuted,
		},
		{
			name: "body of a function nobody calls is not-executed",
			hunk: Hunk{File: "src/Row.tsx", Start: 24, End: 25},
			want: RuntimeNotExecuted,
		},
		{
			name: "an executed statement is runtime-verified",
			hunk: Hunk{File: "src/Row.tsx", Start: 12, End: 12},
			want: RuntimeExecuted,
		},
		{
			name: "a file no instrumentation reached is no-data, not not-executed",
			hunk: Hunk{File: "src/Other.tsx", Start: 3, End: 9},
			want: RuntimeNoData,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyHunk(tc.hunk, datasets, nil)
			if got.Class != tc.want {
				t.Fatalf("ClassifyHunk(%v) = %q (%s), want %q", tc.hunk, got.Class, got.Detail, tc.want)
			}
			if got.Detail == "" {
				t.Fatal("every verdict must carry a reviewer-readable detail")
			}
		})
	}
}

// The hard requirement: a genuinely un-executed hunk is still caught, and it is
// caught as not-executed — never quietly rounded up to "the engine could not see
// it". Both dead shapes from the real fixture are asserted, including the JSX one
// that shares its (hit) enclosing function with a live hunk.
func TestAudit_TrulyUnexecutedHunkIsStillFlagged(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}
	dead := Hunk{File: "src/Row.tsx", Start: 24, End: 25}
	deadJSX := Hunk{File: "src/Row.tsx", Start: 7, End: 7}
	live := Hunk{File: "src/Row.tsx", Start: 14, End: 14}
	changed := []Hunk{deadJSX, live, dead}

	// The agent labelled everything runtime-verified, as an agent under pressure
	// does. Backfill is what makes the labels irrelevant.
	ledger := []LedgerEntry{
		{File: dead.File, StartLine: dead.Start, EndLine: dead.End, State: StateRuntimeVerified},
		{File: deadJSX.File, StartLine: deadJSX.Start, EndLine: deadJSX.End, State: StateRuntimeVerified},
		{File: live.File, StartLine: live.Start, EndLine: live.End, State: StateRuntimeVerified},
	}
	backfilled, downgrades := Backfill(ledger, datasets, []string{"ev-cov"}, nil)
	if len(downgrades) != 3 {
		t.Fatalf("downgrades = %+v, want all three false runtime-verified labels stripped", downgrades)
	}
	for _, e := range backfilled {
		if e.State == StateRuntimeVerified {
			t.Fatalf("no hunk here was executed by an instrumented line record; %v kept runtime-verified", e.Hunk())
		}
	}

	report := Audit(changed, backfilled, datasets, nil)
	if report.RuntimeVerified != 0 {
		t.Fatalf("RuntimeVerified = %d, want 0", report.RuntimeVerified)
	}
	if len(report.NotExecuted) != 2 {
		t.Fatalf("NotExecuted = %+v, want both dead hunks (%v and %v)", report.NotExecuted, deadJSX, dead)
	}
	if len(report.Blind) != 1 || report.Blind[0] != live {
		t.Fatalf("Blind = %+v, want only the uninstrumented-but-executed hunk %v", report.Blind, live)
	}
	// And the blind hunk is not laundered into a pass: it stays in the uncovered
	// set that §6 routing reads.
	if len(report.Uncovered) != 3 {
		t.Fatalf("Uncovered = %+v, want all three (a blind hunk is not runtime-verified)", report.Uncovered)
	}
	if !strings.Contains(report.String(), "2 not executed") || !strings.Contains(report.String(), "1 uninstrumented") {
		t.Fatalf("summary must count the two states separately, got %q", report.String())
	}
}

// The downgrade reason is the sentence that convinced the captain the code never
// ran. It must now say what actually happened.
func TestBackfill_UninstrumentedHunkIsNotToldItNeverRan(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}
	ledger := []LedgerEntry{{File: "src/Row.tsx", StartLine: 14, EndLine: 14, State: StateRuntimeVerified}}

	backfilled, downgrades := Backfill(ledger, datasets, []string{"ev-cov"}, nil)
	if len(downgrades) != 1 {
		t.Fatalf("downgrades = %+v, want 1", downgrades)
	}
	if downgrades[0].Runtime != RuntimeUninstrumented {
		t.Fatalf("Downgrade.Runtime = %q, want %q", downgrades[0].Runtime, RuntimeUninstrumented)
	}
	if strings.Contains(downgrades[0].Reason, "no captured instrumentation coverage intersects this hunk") {
		t.Fatalf("the old reason asserts non-execution and is false here: %q", downgrades[0].Reason)
	}
	if !strings.Contains(downgrades[0].Reason, "emitted no line record") {
		t.Fatalf("reason must name the engine's blindness, got %q", downgrades[0].Reason)
	}
	if !backfilled[0].Blind() {
		t.Fatalf("Runtime = %q, want the hunk marked blind so the dossier can show it", backfilled[0].Runtime)
	}
}

// A dataset captured before the parser recorded unexecuted lines cannot tell
// "no record" from "zero hits", so it must infer nothing from an absence.
func TestClassifyHunk_LegacyDatasetInfersNothingFromMissingRecords(t *testing.T) {
	legacy := CoverageData{Format: FormatLCOV, Files: []FileCoverage{{
		File:    "src/Row.tsx",
		Covered: []LineRange{{Start: 12, End: 12}},
		// Enumerated false: an older collector recorded only executed lines.
	}}}
	got := ClassifyHunk(Hunk{File: "src/Row.tsx", Start: 14, End: 14}, []CoverageData{legacy}, nil)
	if got.Class != RuntimeNoData {
		t.Fatalf("Class = %q, want %q — an unenumerated dataset proves nothing about a line it does not list", got.Class, RuntimeNoData)
	}
}

// Verdict-level consequence: an engine blind spot must not cap a behavior claim,
// while code that provably never ran still must.
func TestBehaviorBacking_BlindSpotIsNotAGap(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}
	ledger, _ := Backfill([]LedgerEntry{
		{File: "src/Row.tsx", StartLine: 14, EndLine: 14, State: StateAttested},
		{File: "src/Row.tsx", StartLine: 24, EndLine: 25, State: StateAttested},
	}, datasets, []string{"ev-cov"}, nil)

	blindClaim := claims.Claim{
		ID: "c1", Kind: claims.KindRegressionFixed, Text: "the row title is no longer green",
		Evidence: []string{"ev-1"}, Hunks: []string{"src/Row.tsx:14"},
	}
	deadClaim := claims.Claim{
		ID: "c2", Kind: claims.KindBehavior, Text: "the helper doubles the input",
		Evidence: []string{"ev-1"}, Hunks: []string{"src/Row.tsx:24-25"},
	}

	gaps, blind := BehaviorBacking([]claims.Claim{blindClaim, deadClaim}, ledger)
	if len(blind) != 1 || blind[0].ClaimID != "c1" {
		t.Fatalf("blind = %+v, want the JSX claim reported as unmeasured, not unbacked", blind)
	}
	if len(gaps) != 1 || gaps[0].ClaimID != "c2" {
		t.Fatalf("gaps = %+v, want the claim over never-executed code still unbacked", gaps)
	}
	if !strings.Contains(blind[0].Detail, "unmeasured, not unexecuted") {
		t.Fatalf("blind detail must say what it means: %q", blind[0].Detail)
	}
}

// A claim resting on one blind hunk AND one provably dead hunk is a gap, not a
// blind spot: the engine's blindness elsewhere cannot buy back a line it watched
// and saw never run.
func TestBehaviorBacking_DeadHunkBeatsBlindHunkWithinOneClaim(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}
	ledger, _ := Backfill([]LedgerEntry{
		{File: "src/Row.tsx", StartLine: 14, EndLine: 14, State: StateAttested},
		{File: "src/Row.tsx", StartLine: 7, EndLine: 7, State: StateAttested},
	}, datasets, []string{"ev-cov"}, nil)

	c := claims.Claim{
		ID: "c1", Kind: claims.KindBehavior, Text: "both rows render the new accent",
		Evidence: []string{"ev-1"}, Hunks: []string{"src/Row.tsx:14", "src/Row.tsx:7"},
	}
	gaps, blind := BehaviorBacking([]claims.Claim{c}, ledger)
	if len(gaps) != 1 || len(blind) != 0 {
		t.Fatalf("gaps=%+v blind=%+v, want the claim capped: one of its hunks provably never ran", gaps, blind)
	}
	if !strings.Contains(gaps[0].Detail, "zero hits") {
		t.Fatalf("gap must name the dead hunk, got %q", gaps[0].Detail)
	}
}

// The run-wide fallback (a claim naming no hunks) obeys the same precedence: any
// provably dead changed hunk in the run sinks it, blind hunks alone do not.
func TestBehaviorBacking_RunWideFallbackSeparatesBlindFromDead(t *testing.T) {
	datasets := []CoverageData{v8Dataset(t)}
	c := claims.Claim{ID: "c1", Kind: claims.KindBehavior, Text: "the row renders the new accent", Evidence: []string{"ev-1"}}

	blindOnly, _ := Backfill([]LedgerEntry{
		{File: "src/Row.tsx", StartLine: 14, EndLine: 14, State: StateAttested},
	}, datasets, []string{"ev-cov"}, nil)
	gaps, blind := BehaviorBacking([]claims.Claim{c}, blindOnly)
	if len(gaps) != 0 || len(blind) != 1 {
		t.Fatalf("gaps=%+v blind=%+v, want blind: the run's only changed hunk is one the engine cannot instrument", gaps, blind)
	}

	withDead, _ := Backfill([]LedgerEntry{
		{File: "src/Row.tsx", StartLine: 14, EndLine: 14, State: StateAttested},
		{File: "src/Row.tsx", StartLine: 24, EndLine: 25, State: StateAttested},
	}, datasets, []string{"ev-cov"}, nil)
	gaps, blind = BehaviorBacking([]claims.Claim{c}, withDead)
	if len(gaps) != 1 || len(blind) != 0 {
		t.Fatalf("gaps=%+v blind=%+v, want a gap: the run changed code that provably never ran", gaps, blind)
	}
}
