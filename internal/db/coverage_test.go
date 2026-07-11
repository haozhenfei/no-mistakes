package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
)

func TestInsertAndGetCoverageEntries(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)

	e1, err := d.InsertCoverageEntry(coverage.LedgerEntry{
		RunID:     run.ID,
		File:      "internal/foo.go",
		StartLine: 10,
		EndLine:   12,
		State:     coverage.StateRuntimeVerified,
		Evidence:  []string{"ev-cov1"},
		Source:    "backfill",
	})
	if err != nil {
		t.Fatalf("insert coverage entry: %v", err)
	}
	if e1.ID == "" {
		t.Fatal("expected assigned coverage entry ID")
	}
	if _, err := d.InsertCoverageEntry(coverage.LedgerEntry{
		RunID:     run.ID,
		File:      "internal/foo.go",
		StartLine: 20,
		EndLine:   22,
		State:     coverage.StateUnverified,
		Reason:    "no gate recorded verification",
		Source:    "coverage-audit",
	}); err != nil {
		t.Fatalf("insert second entry: %v", err)
	}

	got, err := d.GetCoverageEntriesByRun(run.ID)
	if err != nil {
		t.Fatalf("get coverage entries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].State != coverage.StateRuntimeVerified || len(got[0].Evidence) != 1 || got[0].Evidence[0] != "ev-cov1" {
		t.Fatalf("first entry round-trip failed: %+v", got[0])
	}
	if got[1].Reason != "no gate recorded verification" {
		t.Fatalf("reason round-trip failed: %q", got[1].Reason)
	}
}

func TestUpdateCoverageEntry(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)

	e, err := d.InsertCoverageEntry(coverage.LedgerEntry{
		RunID:     run.ID,
		File:      "foo.go",
		StartLine: 5,
		EndLine:   9,
		State:     coverage.StateRuntimeVerified,
		Source:    "agent",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Simulate a backfill downgrade: no instrumentation covered the hunk.
	e.State = coverage.StateAttested
	e.Reason = "claimed runtime-verified but no coverage intersects"
	e.Source = "backfill"
	if err := d.UpdateCoverageEntry(e); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := d.GetCoverageEntriesByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 || got[0].State != coverage.StateAttested {
		t.Fatalf("update not persisted: %+v", got)
	}
	if got[0].Reason == "" || got[0].Source != "backfill" {
		t.Fatalf("downgrade metadata not persisted: %+v", got[0])
	}
}

func TestDeleteCoverageEntriesByRun(t *testing.T) {
	d := openTestDB(t)
	run := newRunForClaims(t, d)
	if _, err := d.InsertCoverageEntry(coverage.LedgerEntry{
		RunID:     run.ID,
		File:      "foo.go",
		StartLine: 5,
		EndLine:   9,
		State:     coverage.StateRuntimeVerified,
		Source:    "agent",
	}); err != nil {
		t.Fatalf("insert coverage: %v", err)
	}

	if err := d.DeleteCoverageEntriesByRun(run.ID); err != nil {
		t.Fatalf("delete coverage entries: %v", err)
	}

	got, err := d.GetCoverageEntriesByRun(run.ID)
	if err != nil {
		t.Fatalf("get coverage entries: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("coverage entries after delete = %d, want 0", len(got))
	}
}
