package steps

import (
	"context"
	"encoding/json"
	"os"
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

// rowBase is a React component with two returns: one rendered by the test, one
// not. rowHead changes a JSX className attribute in each — a hunk on line 14
// (rendered) and a hunk on line 7 (never rendered).
const rowBase = `import React from 'react';

export function Row({ name, editing }) {
  if (editing) {
    return (
      <input
        className="border-old"
        defaultValue={name}
      />
    );
  }
  return (
    <div
      className="text-old"
      data-testid="row"
      title={name}
    >
      {name}
    </div>
  );
}

export function neverCalled(a) {
  const doubled = a * 2;
  return doubled + 1;
}
`

// rowV8LCOV is the REAL lcov @vitest/coverage-v8 emits for this component when a
// test renders only the non-editing branch. Captured by running vitest, not
// hand-written. v8 emits NO DA record for either className line (7 and 14): it
// folds JSX attributes into the enclosing `return`. Line 5's return has 0 hits,
// line 12's has 1.
const rowV8LCOV = `TN:
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
end_of_record
`

// seedJSXVerifyContext builds a run over a real git repo whose diff changes the
// className on the given line of src/Row.tsx, with the real v8 lcov above
// captured as signed coverage evidence. Returns the context and the id of a
// captured command-output evidence entry the claim can hang off.
func seedJSXVerifyContext(t *testing.T, ag agent.Agent, changedLine int, newClass string) (*pipeline.StepContext, string) {
	t.Helper()
	repoDir := t.TempDir()
	git := func(args ...string) string { return gitCmd(t, repoDir, args...) }
	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test.com")
	git("checkout", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repoDir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rowPath := filepath.Join(repoDir, "src", "Row.tsx")
	if err := os.WriteFile(rowPath, []byte(rowBase), 0o644); err != nil {
		t.Fatalf("write Row.tsx: %v", err)
	}
	git("add", "-A")
	git("commit", "-m", "base")
	baseSHA := git("rev-parse", "HEAD")

	git("checkout", "-b", "feature")
	lines := strings.Split(rowBase, "\n")
	lines[changedLine-1] = strings.Replace(lines[changedLine-1], "-old", "-"+newClass, 1)
	if err := os.WriteFile(rowPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("rewrite Row.tsx: %v", err)
	}
	git("add", "-A")
	git("commit", "-m", "restyle the row")
	headSHA := git("rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, ag, repoDir, baseSHA, headSHA, config.Commands{})
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
		Label: "computed styles read out of a browser", Argv: []string{"printf", "rgb(59,130,246)"},
		Dir: repoDir, RepoRoot: repoDir,
	})
	if err != nil {
		t.Fatalf("exec evidence: %v", err)
	}

	profile := filepath.Join(t.TempDir(), "lcov.info")
	if err := os.WriteFile(profile, []byte(rowV8LCOV), 0o644); err != nil {
		t.Fatalf("write lcov: %v", err)
	}
	if _, err := store.Coverage(context.Background(), evidence.CoverageOpts{
		Label: "vitest run with v8 coverage", Argv: []string{"printf", "ok"},
		Format: coverage.FormatLCOV, CoverProfile: profile,
		Dir: repoDir, RepoRoot: repoDir,
	}); err != nil {
		t.Fatalf("coverage evidence: %v", err)
	}
	return sctx, entry.ID
}

func verifyFindings(t *testing.T, outcome *pipeline.StepOutcome) []Finding {
	t.Helper()
	var f Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &f); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	return f.Items
}

func findingWith(items []Finding, substr string) (Finding, bool) {
	for _, it := range items {
		if strings.Contains(it.Description, substr) {
			return it, true
		}
	}
	return Finding{}, false
}

// The coze MR 6951 regression, end to end: a JSX className change v8 cannot
// instrument must NOT cap a behavior claim its skeptic confirmed. The engine's
// silence about those lines is not evidence that they never ran — and the whole
// cascade (hunk downgraded → "no runtime evidence" → CONFIRMED capped to
// PLAUSIBLE → run parked) started with reading it as if it were.
func TestVerifyStep_UninstrumentedJSXHunkDoesNotCapAConfirmedBehaviorClaim(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the diff sets text-accent and the theme's --accent is the measured color")
	sctx, evID := seedJSXVerifyContext(t, ag, 14, "accent") // the RENDERED branch
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "the row title is no longer green",
		Kind: claims.KindRegressionFixed, Evidence: []string{evID}, Hunks: []string{"src/Row.tsx:14"},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("an unmeasurable hunk is not an unexecuted one; the run must not park. findings: %s", outcome.Findings)
	}

	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictConfirmed {
		t.Fatalf("verdict = %q, want the skeptic's CONFIRMED left standing", got[0].Verdict)
	}

	items := verifyFindings(t, outcome)
	if _, ok := findingWith(items, "NO RUNTIME EVIDENCE"); ok {
		t.Fatalf("the engine could not look, so it did not find an absence of execution: %+v", items)
	}
	blind, ok := findingWith(items, "COVERAGE BLIND SPOT")
	if !ok {
		t.Fatalf("the blindness must still be on the record, not silently upgraded to a pass: %+v", items)
	}
	if blind.Action != types.ActionNoOp || blind.Severity != "warning" {
		t.Fatalf("a blind spot is reported, not gated: %+v", blind)
	}

	// And the ledger row says which of the three things the machine actually saw.
	entries, _ := sctx.DB.GetCoverageEntriesByRun(sctx.Run.ID)
	var row coverage.LedgerEntry
	for _, e := range entries {
		if e.StartLine <= 14 && e.EndLine >= 14 {
			row = e
		}
	}
	if row.Runtime != coverage.RuntimeUninstrumented {
		t.Fatalf("ledger Runtime = %q, want %q persisted for the dossier", row.Runtime, coverage.RuntimeUninstrumented)
	}
	if row.State == coverage.StateRuntimeVerified {
		t.Fatal("a blind hunk must never be promoted to runtime-verified — it was not measured")
	}
}

// The other half of the requirement, and the one that matters more: the SAME
// construct in a branch the tests never render is still caught. v8 emits no DA
// for line 7 either, and the enclosing function was hit — only the enclosing
// statement record (DA:5,0) tells them apart. A CONFIRMED skeptic verdict is
// still capped, and the run still parks.
func TestVerifyStep_UnexecutedJSXHunkStillCapsTheBehaviorClaim(t *testing.T) {
	ag := verdictAgent(claims.VerdictConfirmed, "the diff sets border-accent on the editing input")
	sctx, evID := seedJSXVerifyContext(t, ag, 7, "accent") // the branch NEVER rendered
	if _, err := sctx.DB.InsertClaim(claims.Claim{
		RunID: sctx.Run.ID, Step: "test", Text: "the editing input border is no longer green",
		Kind: claims.KindRegressionFixed, Evidence: []string{evID}, Hunks: []string{"src/Row.tsx:7"},
	}); err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	outcome, err := (&VerifyStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("verify execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("changed code that provably never ran must still park: %s", outcome.Findings)
	}
	if outcome.AutoFixable {
		t.Fatal("the remedy is a decision (exercise the branch, or restate the claim), not an auto-fix")
	}

	got, _ := sctx.DB.GetClaimsByRun(sctx.Run.ID)
	if got[0].Verdict != claims.VerdictPlausible {
		t.Fatalf("verdict = %q, want CONFIRMED capped to PLAUSIBLE: nothing executed the changed line", got[0].Verdict)
	}

	items := verifyFindings(t, outcome)
	gap, ok := findingWith(items, "NO RUNTIME EVIDENCE")
	if !ok {
		t.Fatalf("an unexecuted behavior claim must still raise the gap finding: %+v", items)
	}
	if gap.Action != types.ActionAskUser || gap.Severity != "error" {
		t.Fatalf("gap finding must park for a decision: %+v", gap)
	}
	if _, ok := findingWith(items, "COVERAGE BLIND SPOT"); ok {
		t.Fatalf("this hunk was watched and never ran; calling it a blind spot would be the original bug: %+v", items)
	}
	if _, ok := findingWith(items, "NOT EXECUTED"); !ok {
		t.Fatalf("the audit must name the hunk instrumentation saw never run: %+v", items)
	}

	entries, _ := sctx.DB.GetCoverageEntriesByRun(sctx.Run.ID)
	var row coverage.LedgerEntry
	for _, e := range entries {
		if e.StartLine <= 7 && e.EndLine >= 7 {
			row = e
		}
	}
	if row.Runtime != coverage.RuntimeNotExecuted {
		t.Fatalf("ledger Runtime = %q, want %q", row.Runtime, coverage.RuntimeNotExecuted)
	}
}
