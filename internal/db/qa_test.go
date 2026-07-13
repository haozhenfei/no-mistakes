package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Every installed database predates the QA verdict column and the notice ledger.
// Both must appear on open, and every existing row must keep loading.
func TestOpenMigratesQAVerdictAndNotices(t *testing.T) {
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
		VALUES ('legacy-run', 'repo-1', 'feature', 'head', 'base', 'completed', 1, 1);
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

	run, err := d.GetRun("legacy-run")
	if err != nil {
		t.Fatalf("read legacy run: %v", err)
	}
	if run == nil {
		t.Fatal("the legacy run did not survive the migration")
	}
	if run.QAVerdict != nil {
		t.Fatalf("a legacy run reports a QA verdict (%q); it never ran QA", *run.QAVerdict)
	}
	// And it is not mistaken for a QA verdict the watch step could report on.
	latest, err := d.LatestQAVerdict("repo-1", "feature")
	if err != nil {
		t.Fatalf("latest qa verdict: %v", err)
	}
	if latest != nil {
		t.Fatalf("a run that never ran QA was returned as the branch's QA verdict: %s", latest.ID)
	}

	if err := d.UpdateRunQAVerdict("legacy-run", "PARTIAL"); err != nil {
		t.Fatalf("record a verdict on the migrated row: %v", err)
	}
	latest, err = d.LatestQAVerdict("repo-1", "feature")
	if err != nil {
		t.Fatalf("latest qa verdict: %v", err)
	}
	if latest == nil || latest.QAVerdict == nil || *latest.QAVerdict != "PARTIAL" {
		t.Fatalf("LatestQAVerdict() = %v, want the recorded PARTIAL", latest)
	}

	// The notice ledger exists and is idempotent per (PR, QA run, head).
	posted, err := d.QANoticePosted("https://example.com/pull/1", "legacy-run", "head2")
	if err != nil {
		t.Fatalf("qa notice posted: %v", err)
	}
	if posted {
		t.Fatal("a notice was reported as posted before anything posted it")
	}
	for i := 0; i < 2; i++ {
		if err := d.RecordQANotice("https://example.com/pull/1", "legacy-run", "head2"); err != nil {
			t.Fatalf("record qa notice: %v", err)
		}
	}
	posted, err = d.QANoticePosted("https://example.com/pull/1", "legacy-run", "head2")
	if err != nil {
		t.Fatalf("qa notice posted: %v", err)
	}
	if !posted {
		t.Fatal("a recorded notice does not read back as posted; a restart would repost it")
	}
	// A later head is a different notice: the reviewer needs to know the drift grew.
	posted, err = d.QANoticePosted("https://example.com/pull/1", "legacy-run", "head3")
	if err != nil {
		t.Fatalf("qa notice posted: %v", err)
	}
	if posted {
		t.Fatal("the notice for one head suppressed the notice for a later one")
	}
}

// The QA verdict is scoped to a branch: another branch's QA pass must never be
// read as this branch's.
func TestLatestQAVerdictIsScopedToTheBranch(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertRunWithOptions(repo.ID, "other", "sha-o", "base", RunOptions{Kind: types.RunKindWatch})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunQAVerdict(other.ID, "PASS"); err != nil {
		t.Fatal(err)
	}
	latest, err := d.LatestQAVerdict(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatalf("another branch's QA verdict leaked into this branch: %s", latest.ID)
	}
}
