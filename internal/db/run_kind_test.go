package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInsertRunWithOptions_RecordsKindAndParent(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	gate, err2 := d.InsertRun(repo.ID, "feature", "head", "base")
	if err2 != nil {
		t.Fatalf("insert gate run: %v", err2)
	}
	if gate.Kind != types.RunKindGate {
		t.Fatalf("default kind = %q, want %q", gate.Kind, types.RunKindGate)
	}
	if gate.ParentRunID != nil {
		t.Fatalf("gate parent = %v, want nil", gate.ParentRunID)
	}

	watch, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", RunOptions{
		Kind:        types.RunKindWatch,
		ParentRunID: gate.ID,
	})
	if err != nil {
		t.Fatalf("insert watch run: %v", err)
	}

	got, err := d.GetRun(watch.ID)
	if err != nil || got == nil {
		t.Fatalf("get watch run: %v", err)
	}
	if got.Kind != types.RunKindWatch {
		t.Fatalf("kind = %q, want %q", got.Kind, types.RunKindWatch)
	}
	if got.ParentRunID == nil || *got.ParentRunID != gate.ID {
		t.Fatalf("parent_run_id = %v, want %s", got.ParentRunID, gate.ID)
	}
}

func TestInsertRunWithOptions_RejectsUnknownKind(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", RunOptions{Kind: "ci"}); err == nil {
		t.Fatal("expected an unknown run kind to be rejected")
	}
}

// TestOpenMigratesRunsKindColumns covers the additive migration for the
// gate/watch split: a database written before the split must gain both columns,
// and every row already in it must read back as a gate run - which is what it
// is, since the pre-split pipeline had no other kind.
func TestOpenMigratesRunsKindColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			repo_id TEXT NOT NULL,
			branch TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			base_sha TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			pr_url TEXT,
			error TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, pr_url, created_at, updated_at)
		VALUES ('legacy-run', 'repo-1', 'feature', 'head', 'base', 'completed', 'https://example.test/pull/1', 1, 1);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy runs table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !hasColumn(t, d, "runs", "kind") {
		t.Fatal("runs.kind column missing after migration")
	}
	if !hasColumn(t, d, "runs", "parent_run_id") {
		t.Fatal("runs.parent_run_id column missing after migration")
	}

	run, err := d.GetRun("legacy-run")
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	if run == nil {
		t.Fatal("legacy run not readable after migration")
	}
	if run.Kind != types.RunKindGate {
		t.Fatalf("legacy run kind = %q, want %q", run.Kind, types.RunKindGate)
	}
	if run.ParentRunID != nil {
		t.Fatalf("legacy run parent = %v, want nil", run.ParentRunID)
	}
	// The old row is still fully usable, not just readable.
	if run.PRURL == nil || *run.PRURL != "https://example.test/pull/1" {
		t.Fatalf("legacy run pr_url = %v, want it preserved", run.PRURL)
	}
}

func TestDeleteStepsForRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", RunOptions{Kind: types.RunKindWatch})
	if err != nil {
		t.Fatalf("insert watch run: %v", err)
	}
	if _, err := d.InsertStepResult(run.ID, types.StepWatch); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := d.DeleteStepsForRun(run.ID); err != nil {
		t.Fatalf("delete steps: %v", err)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps after delete = %d, want 0", len(steps))
	}
}
