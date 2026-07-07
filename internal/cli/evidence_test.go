package cli

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// setupInRunWorktree creates a daemon-style managed worktree
// (<NM_HOME>/worktrees/<repoID>/<runID>), inits a git repo in it, chdirs into
// it, and returns the run. Commands resolve the run from this worktree path.
func setupInRunWorktree(t *testing.T) *db.Run {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("NM_HOME", tmp)

	p, d, err := openResources()
	if err != nil {
		t.Fatalf("open resources: %v", err)
	}
	repo, err := d.InsertRepo("/tmp/original-repo", "https://example.com/x.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "fm/login-fix", "headsha", "basesha")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	d.Close()

	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := exec.Command("git", "init", worktree).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	chdir(t, worktree)
	return run
}

func TestEvidenceExecCapturesAndListsAndClaims(t *testing.T) {
	setupInRunWorktree(t)

	// Capture a command as evidence.
	out, err := executeCmd("evidence", "exec", "--label", "login e2e", "--", "printf", "PASS")
	if err != nil {
		t.Fatalf("evidence exec: %v (%s)", err, out)
	}
	if !strings.Contains(out, "captured") || !strings.Contains(out, "exit=0") {
		t.Fatalf("unexpected exec output: %q", out)
	}
	evID := strings.Fields(out)[0]
	if !strings.HasPrefix(evID, "ev-") {
		t.Fatalf("expected ev- id, got %q", evID)
	}

	// List shows the captured entry.
	list, err := executeCmd("evidence", "list")
	if err != nil {
		t.Fatalf("evidence list: %v (%s)", err, list)
	}
	if !strings.Contains(list, evID) || !strings.Contains(list, "captured") {
		t.Fatalf("evidence list missing entry: %q", list)
	}

	// Register a claim bound to the evidence ID.
	claimOut, err := executeCmd("claim", "add", "--text", "login no longer overflows", "--kind", "regression-fixed", "--evidence", evID)
	if err != nil {
		t.Fatalf("claim add: %v (%s)", err, claimOut)
	}
	if !strings.Contains(claimOut, "evidence="+evID) {
		t.Fatalf("claim not bound to evidence: %q", claimOut)
	}
}

func TestEvidenceExecRequiresLabel(t *testing.T) {
	setupInRunWorktree(t)
	out, err := executeCmd("evidence", "exec", "--", "true")
	if err == nil {
		t.Fatalf("expected error without --label, got: %q", out)
	}
	if !strings.Contains(err.Error(), "label") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClaimAddWithoutEvidenceIsSelfAttested(t *testing.T) {
	setupInRunWorktree(t)
	out, err := executeCmd("claim", "add", "--text", "trust me it works")
	if err != nil {
		t.Fatalf("claim add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "self-attested") {
		t.Fatalf("evidence-less claim should be flagged self-attested: %q", out)
	}
}

func TestClaimListShowsRegisteredClaims(t *testing.T) {
	setupInRunWorktree(t)
	if _, err := executeCmd("claim", "add", "--text", "a", "--evidence", "ev-x"); err != nil {
		t.Fatalf("claim add: %v", err)
	}
	out, err := executeCmd("claim", "list")
	if err != nil {
		t.Fatalf("claim list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "evidence-bound") || !strings.Contains(out, "unverified") {
		t.Fatalf("claim list output unexpected: %q", out)
	}
}
