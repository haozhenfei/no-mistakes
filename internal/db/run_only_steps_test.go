package db

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The exclusive selection (`--only`) is persisted on its own column, separately
// from the skip set it resolves to, and it survives a round trip.
func TestInsertRunWithOnlySteps_RoundTrips(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-only", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	run, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", RunOptions{
		SkipSteps: []types.StepName{types.StepReview, types.StepPush, types.StepPR},
		OnlySteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !slices.Equal(got.OnlySteps, []types.StepName{types.StepQA}) {
		t.Fatalf("OnlySteps = %v, want [qa]", got.OnlySteps)
	}
	if !slices.Equal(got.SkipSteps, []types.StepName{types.StepReview, types.StepPush, types.StepPR}) {
		t.Fatalf("SkipSteps = %v, want the resolved skip set", got.SkipSteps)
	}
}

// A run with no selection stores NULL, which is exactly what every row written
// before --only shipped has. That is what lets the daemon tell "this run
// selected an on-demand step" apart from "this run predates the step".
func TestInsertRun_NoSelectionStoresNoOnlySteps(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-noonly", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	run, err := d.InsertRunWithSkipSteps(repo.ID, "feature", "head", "base", []types.StepName{types.StepLint})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(got.OnlySteps) != 0 {
		t.Fatalf("OnlySteps = %v, want none for a run that selected nothing", got.OnlySteps)
	}
}

// A database created before only_steps existed must gain the column and keep
// loading its rows, with no selection recorded on them.
func TestOpenMigratesRunsOnlyStepsColumn(t *testing.T) {
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
			skip_steps TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, skip_steps, created_at, updated_at)
		VALUES ('legacy-run', 'repo-1', 'feature', 'head', 'base', 'running', '["review"]', 1, 1);
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
	defer d.Close()

	run, err := d.GetRun("legacy-run")
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	if !slices.Equal(run.SkipSteps, []types.StepName{types.StepReview}) {
		t.Fatalf("legacy SkipSteps = %v, want [review]", run.SkipSteps)
	}
	// The whole point: a pre-upgrade run that skipped some steps records NO
	// selection, so recovering it can never revive an on-demand step.
	if len(run.OnlySteps) != 0 {
		t.Fatalf("legacy OnlySteps = %v, want none", run.OnlySteps)
	}
}
