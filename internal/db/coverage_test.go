package db

import (
	"database/sql"
	"path/filepath"
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

// TestOpenMigratesCoverageEntriesRuntimeColumns: an existing database has
// coverage_entries without the runtime columns. Open must add them, and a legacy
// row must read back with an empty runtime class — "this row was never audited
// for its runtime class", which every caller treats as no backing at all, rather
// than as a silent pass.
func TestOpenMigratesCoverageEntriesRuntimeColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE coverage_entries (
			id            TEXT PRIMARY KEY,
			run_id        TEXT NOT NULL,
			file          TEXT NOT NULL,
			start_line    INTEGER NOT NULL,
			end_line      INTEGER NOT NULL,
			state         TEXT NOT NULL,
			reason        TEXT,
			evidence_json TEXT,
			source        TEXT,
			created_at    INTEGER NOT NULL
		);
		INSERT INTO coverage_entries (id, run_id, file, start_line, end_line, state, created_at)
		VALUES ('cov-legacy', 'run-legacy', 'src/Row.tsx', 14, 14, 'attested', 1);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy coverage_entries table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer d.Close()

	entries, err := d.GetCoverageEntriesByRun("run-legacy")
	if err != nil {
		t.Fatalf("get coverage entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want the legacy row preserved", entries)
	}
	if entries[0].Runtime != "" || entries[0].Blind() {
		t.Fatalf("legacy row Runtime = %q, want empty (never audited), and never blind", entries[0].Runtime)
	}
}
