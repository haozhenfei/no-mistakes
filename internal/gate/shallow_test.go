package gate

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// setupShallowCloneRepo builds an upstream bare repo with real history, then a
// shallow (--depth=1) clone of it carrying one branch commit, and returns the
// clone's path plus the upstream's file:// URL. Local-path clones ignore
// --depth, so the clone must go through file://.
func setupShallowCloneRepo(t *testing.T) (workDir, upstreamURL string) {
	t.Helper()

	root := resolveSymlinks(t, t.TempDir())
	upstream := filepath.Join(root, "upstream.git")
	mustGit(t, "", "init", "--bare", "-b", "main", upstream)

	seed := filepath.Join(root, "seed")
	mustGit(t, "", "init", "-b", "main", seed)
	mustGit(t, seed, "config", "user.email", "test@test.com")
	mustGit(t, seed, "config", "user.name", "Test")
	mustGit(t, seed, "remote", "add", "origin", upstream)
	for _, msg := range []string{"c1", "c2", "c3", "c4"} {
		mustGit(t, seed, "commit", "--allow-empty", "-m", msg)
	}
	mustGit(t, seed, "push", "origin", "main")

	upstreamURL = "file://" + filepath.ToSlash(upstream)
	work := filepath.Join(root, "work")
	mustGit(t, "", "clone", "--depth=1", upstreamURL, work)
	mustGit(t, work, "config", "user.email", "test@test.com")
	mustGit(t, work, "config", "user.name", "Test")
	mustGit(t, work, "checkout", "-b", "feat")
	mustGit(t, work, "commit", "--allow-empty", "-m", "feature work")

	if out := mustGit(t, work, "rev-parse", "--is-shallow-repository"); out != "true" {
		t.Fatalf("clone is not shallow: %q", out)
	}
	return work, upstreamURL
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPlainBareRepoRejectsShallowPush pins the root cause the gate has to work
// around: an empty bare repo refuses a push whose history is truncated, because
// it cannot reconstruct the commit graph. This is what made every shallow-cloned
// repository (the norm for large monorepos) unusable with no-mistakes.
func TestPlainBareRepoRejectsShallowPush(t *testing.T) {
	workDir, _ := setupShallowCloneRepo(t)
	bare := filepath.Join(resolveSymlinks(t, t.TempDir()), "plain.git")
	mustGit(t, "", "init", "--bare", bare)

	cmd := exec.Command("git", "push", bare, "feat:refs/heads/feat")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a plain bare repo to reject the shallow push, got success: %s", out)
	}
	if !strings.Contains(string(out), "shallow update not allowed") {
		t.Fatalf("expected a shallow-update rejection, got: %s", out)
	}
}

// TestInitGateAcceptsShallowClonePush is the regression for the fix: a gate
// created by Init takes a push from a shallow clone.
func TestInitGateAcceptsShallowClonePush(t *testing.T) {
	workDir, _ := setupShallowCloneRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	bareDir := p.RepoDir(repo.ID)
	if got := mustGit(t, bareDir, "config", "--get", "receive.shallowUpdate"); got != "true" {
		t.Fatalf("receive.shallowUpdate = %q, want true", got)
	}

	if err := gitpkg.PushWithOptions(ctx, workDir, RemoteName, "refs/heads/feat", "", false, nil); err != nil {
		t.Fatalf("push shallow branch to gate: %v", err)
	}

	// The gate now holds the branch and is itself shallow; a worktree cut from
	// it still resolves the branch (the daemon's execution path).
	if got := mustGit(t, bareDir, "rev-parse", "refs/heads/feat"); got == "" {
		t.Fatal("gate has no feat ref after push")
	}
	if got := mustGit(t, bareDir, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("expected the gate to be shallow after the push, got %q", got)
	}
}

// TestExplainPushError_OnRealGitRejection pins the whole (b) path against a real
// git failure: the rejection text must survive git.Run's error wrapping so the
// shallow case is recognizable, and the explained error must carry the fix.
// This is what a gate created before receive.shallowUpdate existed still hits.
func TestExplainPushError_OnRealGitRejection(t *testing.T) {
	workDir, _ := setupShallowCloneRepo(t)
	bare := filepath.Join(resolveSymlinks(t, t.TempDir()), "legacy-gate.git")
	mustGit(t, "", "init", "--bare", bare)
	mustGit(t, workDir, "remote", "add", RemoteName, bare)

	err := gitpkg.PushWithOptions(context.Background(), workDir, RemoteName, "refs/heads/feat", "", false, nil)
	if err == nil {
		t.Fatal("expected the push to a legacy gate to fail")
	}
	if !ShallowPushRejected(err) {
		t.Fatalf("shallow rejection not recognizable in the git error: %v", err)
	}
	explained := ExplainPushError(err).Error()
	for _, want := range []string{"shallow clone", "no-mistakes init", "git fetch --unshallow"} {
		if !strings.Contains(explained, want) {
			t.Fatalf("explained error %q does not mention %q", explained, want)
		}
	}
}

func TestShallowPushRejected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("Permission denied (publickey)"), false},
		{"git rejection", errors.New("push failed: ! [remote rejected] feat -> feat (shallow update not allowed)"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShallowPushRejected(tt.err); got != tt.want {
				t.Fatalf("ShallowPushRejected = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExplainPushError(t *testing.T) {
	raw := errors.New("! [remote rejected] feat -> feat (shallow update not allowed)")
	got := ExplainPushError(raw).Error()
	for _, want := range []string{"shallow clone", "no-mistakes init", "git fetch --unshallow"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explained error %q does not mention %q", got, want)
		}
	}
	if !errors.Is(ExplainPushError(raw), raw) {
		t.Fatal("explained error must wrap the original git error")
	}

	other := errors.New("Permission denied (publickey)")
	if ExplainPushError(other) != other {
		t.Fatal("non-shallow errors must pass through unchanged")
	}
	if ExplainPushError(nil) != nil {
		t.Fatal("nil must pass through")
	}
}
