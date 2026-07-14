package gate

import (
	"context"
	"strings"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// initTestGate initializes a gate for a fresh working repo and returns the
// working dir and the gate's bare dir.
func initTestGate(t *testing.T) (workDir, bareDir string) {
	t.Helper()
	workDir = setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	repo, _, err := Init(context.Background(), d, p, workDir)
	if err != nil {
		t.Fatalf("init gate: %v", err)
	}
	return workDir, p.RepoDir(repo.ID)
}

// TestPushHead_RebasedBranchStillEntersTheGate is the regression for a branch
// whose history was rewritten: rebasing onto a fresh release branch leaves the
// branch with the same name but a head that is not a descendant of the one the
// gate mirror already holds. A fast-forward-only push is rejected there, so the
// run never started and the branch could only be gated after somebody deleted
// the ref inside the mirror by hand.
func TestPushHead_RebasedBranchStillEntersTheGate(t *testing.T) {
	workDir, bareDir := initTestGate(t)
	ctx := context.Background()
	base := mustGit(t, workDir, "rev-parse", "--abbrev-ref", "HEAD")

	mustGit(t, workDir, "checkout", "-b", "feat")
	mustGit(t, workDir, "commit", "--allow-empty", "-m", "feature work")
	if err := PushHead(ctx, workDir, "feat", nil); err != nil {
		t.Fatalf("first gate push: %v", err)
	}
	preRebase := mustGit(t, workDir, "rev-parse", "HEAD")
	if got := mustGit(t, bareDir, "rev-parse", "refs/heads/feat"); got != preRebase {
		t.Fatalf("gate ref = %s, want the pushed head %s", got, preRebase)
	}

	// The default branch moves (a new release branch is cut) and the feature
	// branch is rebased onto it: same ref name, rewritten history.
	mustGit(t, workDir, "checkout", base)
	mustGit(t, workDir, "commit", "--allow-empty", "-m", "release commit")
	mustGit(t, workDir, "checkout", "feat")
	mustGit(t, workDir, "rebase", base)
	rebased := mustGit(t, workDir, "rev-parse", "HEAD")
	if rebased == preRebase {
		t.Fatal("rebase did not rewrite the branch head; the test proves nothing")
	}

	// This is the failure the user hits: git refuses the fast-forward-only push
	// the gate used to make, and the pipeline never starts.
	err := gitpkg.PushWithOptions(ctx, workDir, RemoteName, "refs/heads/feat", "", false, nil)
	if err == nil {
		t.Fatal("expected a non-force push of a rebased branch to be rejected by git")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("unexpected non-force push error: %v", err)
	}

	if err := PushHead(ctx, workDir, "feat", nil); err != nil {
		t.Fatalf("gate push of the rebased branch: %v", err)
	}
	if got := mustGit(t, bareDir, "rev-parse", "refs/heads/feat"); got != rebased {
		t.Fatalf("gate ref = %s after the rebase, want the rebased head %s", got, rebased)
	}
}

// TestPushHead_PreservedRunHeadSurvivesTheForcedUpdate pins the safety claim that
// makes the forced update acceptable: the daemon backs every run's worktree head
// (including commits the run's agents made) up at refs/no-mistakes/runs/<id>/head
// before dropping the worktree, and overwriting refs/heads/<branch> leaves that
// ref - and the commits it holds - intact.
func TestPushHead_PreservedRunHeadSurvivesTheForcedUpdate(t *testing.T) {
	workDir, bareDir := initTestGate(t)
	ctx := context.Background()
	base := mustGit(t, workDir, "rev-parse", "--abbrev-ref", "HEAD")

	mustGit(t, workDir, "checkout", "-b", "feat")
	mustGit(t, workDir, "commit", "--allow-empty", "-m", "feature work")
	if err := PushHead(ctx, workDir, "feat", nil); err != nil {
		t.Fatalf("first gate push: %v", err)
	}
	agentHead := mustGit(t, workDir, "rev-parse", "HEAD")
	backupRef := "refs/no-mistakes/runs/RUN123/head"
	mustGit(t, bareDir, "update-ref", backupRef, agentHead)

	mustGit(t, workDir, "checkout", base)
	mustGit(t, workDir, "commit", "--allow-empty", "-m", "release commit")
	mustGit(t, workDir, "checkout", "feat")
	mustGit(t, workDir, "rebase", base)
	if err := PushHead(ctx, workDir, "feat", nil); err != nil {
		t.Fatalf("gate push of the rebased branch: %v", err)
	}

	if got := mustGit(t, bareDir, "rev-parse", backupRef); got != agentHead {
		t.Fatalf("run backup ref = %s, want %s: the forced branch update must not touch it", got, agentHead)
	}
	mustGit(t, bareDir, "cat-file", "-e", agentHead+"^{commit}")
}
