//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPRHandsOffToWatchRun is the end-to-end proof of the gate/watch split: a
// real push, through a real gate, through the real pipeline, opens a real PR -
// and at that point the gate run ENDS, its worktree is released, and a watch run
// with no worktree takes the PR over.
//
// Before the split, that same push produced one run that opened the PR and then
// sat inside a blocking CI step, holding its worktree, for up to seven days.
func TestPRHandsOffToWatchRun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	// The pr step needs a real provider, and the harness's upstream is a
	// provider-less file:// path. Borrow TestForkRouting's setup: git URL
	// rewrites keep every byte of git traffic local while origin reads as a
	// GitHub URL. It has to be the fork form, because plain init records the
	// *rewritten* origin URL (git remote get-url applies insteadOf) and would
	// land back on file://, while fork routing preserves the literal parent URL.
	// The fork itself is incidental here - what this test is about is what
	// happens the moment the PR exists.
	parentURL := "https://github.com/test-owner/no-mistakes.git"
	forkURL := "https://github.com/fork-owner/no-mistakes.git"

	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "watch-fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set github origin: %v\n%s", err, out)
	}

	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_PARENT", "test-owner/no-mistakes")
	// The PR is merged by the time the watcher looks, so the watch run
	// converges on its first poll instead of idling for the test's duration.
	t.Setenv("FAKEAGENT_GH_PR_STATE", "MERGED")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/watch-handoff-e2e"
	h.CommitChange(branch, "handoff.txt", "handoff\n", "add handoff")
	h.PushToGate(branch)

	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	gate := waitForKindTerminal(t, database, branch, types.RunKindGate, 120*time.Second)
	if gate.Status != types.RunCompleted {
		t.Fatalf("gate run status = %s, error = %v", gate.Status, deref(gate.Error))
	}
	// The gate pipeline stops at the PR. No blocking CI step ran, or could.
	gateSteps, err := database.GetStepsByRun(gate.ID)
	if err != nil {
		t.Fatalf("get gate steps: %v", err)
	}
	if gate.PRURL == nil || *gate.PRURL == "" {
		for _, sr := range gateSteps {
			t.Logf("step %-9s %-10s err=%q", sr.StepName, sr.Status, deref(sr.Error))
		}
		t.Fatal("the gate run opened no PR, so there is nothing to hand off")
	}
	for _, sr := range gateSteps {
		if sr.StepName == types.StepCI {
			t.Fatal("the gate pipeline still contains the blocking ci step")
		}
		if sr.StepName == types.StepWatch {
			t.Fatal("a gate run must not run the watch step")
		}
	}
	if gateSteps[len(gateSteps)-1].StepName != types.StepPR {
		t.Fatalf("gate pipeline ends at %q, want it to end at the pr step", gateSteps[len(gateSteps)-1].StepName)
	}

	// A watch run took the PR over. The handoff is the last thing a gate run
	// does - deliberately after its worktree is released - so the watch run's
	// existence is itself the proof that the worktree is already gone. Reading
	// the filesystem off the gate's `completed` row instead would race the run
	// goroutine's cleanup, which runs after the status flips.
	watch := waitForKindTerminal(t, database, branch, types.RunKindWatch, 60*time.Second)
	if watch.Status != types.RunCompleted {
		t.Fatalf("watch run status = %s, error = %v", watch.Status, deref(watch.Error))
	}
	if watch.ParentRunID == nil || *watch.ParentRunID != gate.ID {
		t.Fatalf("watch run parent = %v, want the gate run %s", watch.ParentRunID, gate.ID)
	}
	if watch.PRURL == nil || *watch.PRURL != *gate.PRURL {
		t.Fatalf("watch run PR = %v, want the gate run's PR %s", watch.PRURL, *gate.PRURL)
	}

	watchSteps, err := database.GetStepsByRun(watch.ID)
	if err != nil {
		t.Fatalf("get watch steps: %v", err)
	}
	if len(watchSteps) != 1 || watchSteps[0].StepName != types.StepWatch {
		t.Fatalf("watch run steps = %+v, want exactly one watch step", watchSteps)
	}
	if watchSteps[0].Status != types.StepStatusCompleted {
		t.Fatalf("watch step status = %s, want completed (the PR was merged)", watchSteps[0].Status)
	}

	// The gate run's worktree is gone: a PR under review no longer pins one.
	wtDir := paths.WithRoot(h.NMHome).WorktreeDir(gate.RepoID, gate.ID)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("gate worktree %s survived the run (err=%v)", wtDir, err)
	}
	// And the watch run never held one - not "released one", never made one.
	watchWT := paths.WithRoot(h.NMHome).WorktreeDir(watch.RepoID, watch.ID)
	if _, err := os.Stat(watchWT); !os.IsNotExist(err) {
		t.Fatalf("watch run created a worktree at %s", watchWT)
	}
	entries, err := os.ReadDir(filepath.Join(paths.WithRoot(h.NMHome).WorktreesDir(), gate.RepoID))
	if err == nil && len(entries) != 0 {
		t.Fatalf("worktrees left behind after the PR was opened: %d", len(entries))
	}
}

// waitForKindTerminal waits for a run of the given kind on branch to reach a
// terminal status.
func waitForKindTerminal(t *testing.T, database *db.DB, branch string, kind types.RunKind, timeout time.Duration) *db.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all, err := allRunsForBranch(t, database, branch)
		if err != nil {
			t.Fatalf("get branch runs: %v", err)
		}
		for _, run := range all {
			if run.Kind != kind {
				continue
			}
			switch run.Status {
			case types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunInterrupted:
				return run
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no terminal %s run for branch %s within %s", kind, branch, timeout)
	return nil
}

// allRunsForBranch returns every run on a branch across the (single) repo the
// harness registers, without needing to know how init normalized its path.
func allRunsForBranch(t *testing.T, database *db.DB, branch string) ([]*db.Run, error) {
	t.Helper()
	ids, err := repoIDs(t)
	if err != nil {
		return nil, err
	}
	var out []*db.Run
	for _, id := range ids {
		runs, err := database.GetRunsByRepoBranch(id, branch)
		if err != nil {
			return nil, err
		}
		out = append(out, runs...)
	}
	return out, nil
}

func repoIDs(t *testing.T) ([]string, error) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", paths.WithRoot(os.Getenv("NM_HOME")).DB())
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	rows, err := sqlDB.Query(`SELECT id FROM repos`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
