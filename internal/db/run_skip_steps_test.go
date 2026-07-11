package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInsertRunWithSkipSteps_RoundTrips(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-skip", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	skips := []types.StepName{types.StepReview, types.StepQA}
	run, err := d.InsertRunWithSkipSteps(repo.ID, "feature", "head", "base", skips)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.SkipSteps) != 2 || got.SkipSteps[0] != types.StepReview || got.SkipSteps[1] != types.StepQA {
		t.Fatalf("skip steps = %v, want %v", got.SkipSteps, skips)
	}

	// The list surfaces have to carry it too: resume looks the run up by branch.
	runs, err := d.GetRunsByRepoBranch(repo.ID, "feature")
	if err != nil || len(runs) != 1 {
		t.Fatalf("get runs by branch: %v (%d runs)", err, len(runs))
	}
	if len(runs[0].SkipSteps) != 2 {
		t.Fatalf("branch-listed run skip steps = %v, want 2 entries", runs[0].SkipSteps)
	}
}

func TestInsertRun_HasNoSkipSteps(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-noskip", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.SkipSteps != nil {
		t.Fatalf("skip steps = %v, want nil", got.SkipSteps)
	}
}

// TestOpenMigratesRunsSkipStepsColumn covers the additive migration: a database
// created before runs.skip_steps existed must gain the column, and its old rows
// must read back as "nothing skipped" rather than failing the scan.
func TestOpenMigratesRunsSkipStepsColumn(t *testing.T) {
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
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at)
		VALUES ('legacy-run', 'repo-1', 'feature', 'head', 'base', 'failed', 1, 1);
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

	if !hasColumn(t, d, "runs", "skip_steps") {
		t.Fatal("runs.skip_steps column missing after migration")
	}
	run, err := d.GetRun("legacy-run")
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	if run == nil {
		t.Fatal("legacy run not readable after migration")
	}
	if run.SkipSteps != nil {
		t.Fatalf("legacy run skip steps = %v, want nil", run.SkipSteps)
	}
}
