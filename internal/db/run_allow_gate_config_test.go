package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// A legacy row predates the change boundary, so it never asked for permission to
// write the gate's own config. The migration's DEFAULT 0 has to read back as the
// default-deny - anything else would hand an old run, recovered after an
// upgrade, a permission nobody granted it.
func TestOpenMigratesRunsAllowGateConfigColumn(t *testing.T) {
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
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at)
		VALUES ('legacy-run', 'repo-1', 'feature', 'head', 'base', 'running', 1, 1);
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
	if run.AllowGateConfig {
		t.Fatal("a legacy run must read back with the default-deny, not the gate-config opt-in")
	}
}

func TestInsertRun_RecordsTheGateConfigOptIn(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	plain, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	stored, err := d.GetRun(plain.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.AllowGateConfig {
		t.Fatal("an ordinary run must not carry the gate-config opt-in")
	}

	opted, err := d.InsertRunWithOptions(repo.ID, "feature", "head", "base", RunOptions{AllowGateConfig: true})
	if err != nil {
		t.Fatalf("insert opted-in run: %v", err)
	}
	stored, err = d.GetRun(opted.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !stored.AllowGateConfig {
		t.Fatal("the opt-in must survive the round trip: a resume or a derived fix round reads it back from here")
	}
}
