package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// installHangingGit puts a `git` earlier on PATH that never returns when its
// working directory is hangDir, and otherwise forwards to the real git.
//
// This is the observable behavior of a daemon whose git children cannot touch a
// path: the daemon that shipped this bug had been auto-started from inside an
// agent's sandbox and inherited it for life (macOS seatbelt is inherited by every
// descendant and cannot be dropped from inside), so its git children against the
// user's clone either never returned or were killed outright. The shim reproduces
// the first shape without needing a sandbox, which is what makes this a test
// rather than a machine-specific anecdote.
func installHangingGit(t *testing.T, hangDir string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$(pwd -P)\" = \"" + hangDir + "\" ]; then\n" +
		"  sleep 600\n" +
		"fi\n" +
		"exec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// setRunSetupTimeout appends a short setup deadline to the daemon's global
// config. startRun calls config.LoadGlobal per run, so this takes effect on the
// next push without restarting the daemon.
func setRunSetupTimeout(t *testing.T, p *paths.Paths, d time.Duration) {
	t.Helper()
	existing, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	updated := string(existing) + "run_setup_timeout: \"" + d.String() + "\"\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func latestRun(t *testing.T, d *db.DB, repoID, branch string) *db.Run {
	t.Helper()
	runs, err := d.GetRunsByRepoBranch(repoID, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no run row was created for the push")
	}
	return runs[0]
}

// TestPushRunFailsInsteadOfHangingWhenGitCannotReachTheRepo is the regression for
// the silent-pending bug. A daemon that cannot operate on the target repository
// used to insert the run row, block forever in the unbounded setup phase on a git
// child that never returned, and leave the run `pending` with zero steps and zero
// log lines while the client polled it every 250ms - for 22 minutes, in the run
// that produced this fix. The run must instead reach a terminal `failed` status
// within the setup deadline, and say why.
func TestPushRunFailsInsteadOfHangingWhenGitCannotReachTheRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim this test installs is a POSIX shell script")
	}

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, headSHA := setupTestGitRepo(t, p, d, "confined-repo")

	// The daemon can reach its own gate and worktree, but every git child that
	// lands in the user's clone hangs - exactly the asymmetry a sandboxed daemon
	// exhibits, and exactly where CopyLocalUserIdentity reads the git identity.
	workingPath, err := filepath.EvalSymlinks(repo.WorkingPath)
	if err != nil {
		t.Fatal(err)
	}
	installHangingGit(t, workingPath)
	setRunSetupTimeout(t, p, 2*time.Second)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	start := time.Now()
	var result ipc.PushReceivedResult
	callErr := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("confined-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result)
	elapsed := time.Since(start)

	if callErr == nil {
		t.Fatal("push_received succeeded; want a failure naming the unreachable repo")
	}
	// The whole point: bounded. Allow generous slack for the worktree checkout
	// that precedes the hanging call, but nothing like an unbounded wait.
	if elapsed > 60*time.Second {
		t.Fatalf("push_received took %s to fail; the setup phase is not bounded", elapsed)
	}

	run := latestRun(t, d, repo.ID, "main")
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %q, want %q (a run the daemon cannot start must not sit in pending)", run.Status, types.RunFailed)
	}
	if run.Error == nil || strings.TrimSpace(*run.Error) == "" {
		t.Fatal("failed run carries no error message; the reason must be readable from the run row")
	}
	msg := *run.Error
	if !strings.Contains(msg, "configure worktree git identity") {
		t.Fatalf("run error does not name the setup stage that failed:\n%s", msg)
	}
	if !strings.Contains(msg, "daemon restart --force") {
		t.Fatalf("run error does not tell the operator how to recover from a confined daemon:\n%s", msg)
	}
}

// TestSetupTimeoutFallsBackToTheDefault guards the one way this deadline could be
// switched off: a non-positive value must not mean "unbounded", because unbounded
// is the bug.
func TestSetupTimeoutFallsBackToTheDefault(t *testing.T) {
	if got := setupTimeout(nil); got != config.DefaultRunSetupTimeout {
		t.Fatalf("setupTimeout(nil) = %s, want %s", got, config.DefaultRunSetupTimeout)
	}
	if got := setupTimeout(&config.GlobalConfig{RunSetupTimeout: 0}); got != config.DefaultRunSetupTimeout {
		t.Fatalf("setupTimeout(0) = %s, want %s", got, config.DefaultRunSetupTimeout)
	}
	if got := setupTimeout(&config.GlobalConfig{RunSetupTimeout: -1}); got != config.DefaultRunSetupTimeout {
		t.Fatalf("setupTimeout(-1) = %s, want %s", got, config.DefaultRunSetupTimeout)
	}
	if got := setupTimeout(&config.GlobalConfig{RunSetupTimeout: 90 * time.Second}); got != 90*time.Second {
		t.Fatalf("setupTimeout(90s) = %s, want 90s", got)
	}
}

// TestExplainSetupFailureNamesTheConfinedDaemon covers the second observed shape:
// the child is not hung but SIGKILLed by the sandbox, which surfaces from git as a
// bare "signal: killed" that says nothing about why.
func TestExplainSetupFailureNamesTheConfinedDaemon(t *testing.T) {
	killed := errors.New("git config --local --get --default  user.name: signal: killed")
	err := explainSetupFailure(context.Background(), context.Background(), "configure worktree git identity", killed)
	if !strings.Contains(err.Error(), "daemon restart --force") {
		t.Fatalf("a SIGKILLed setup child must name the recovery action:\n%s", err)
	}

	ordinary := errors.New("git fetch origin main: exit status 128: fatal: couldn't find remote ref main")
	err = explainSetupFailure(context.Background(), context.Background(), "fetch default branch main", ordinary)
	if strings.Contains(err.Error(), "daemon restart --force") {
		t.Fatalf("an ordinary git error must not be blamed on a sandbox:\n%s", err)
	}
	if !strings.Contains(err.Error(), "couldn't find remote ref") {
		t.Fatalf("the underlying git error must survive wrapping:\n%s", err)
	}

	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-expired.Done()
	err = explainSetupFailure(context.Background(), expired, "create worktree", expired.Err())
	if !strings.Contains(err.Error(), "daemon restart --force") {
		t.Fatalf("a setup phase that blew its deadline must name the recovery action:\n%s", err)
	}

	// A daemon shutting down mid-setup cancels the handler's context, which kills
	// the in-flight git child and produces the identical "signal: killed". Blaming
	// that on a sandbox would send somebody chasing a confinement that is not there.
	shuttingDown, stop := context.WithCancel(context.Background())
	stop()
	err = explainSetupFailure(shuttingDown, shuttingDown, "configure worktree git identity", killed)
	if strings.Contains(err.Error(), "daemon restart --force") {
		t.Fatalf("a child killed because the daemon is shutting down must not be blamed on a sandbox:\n%s", err)
	}
}
