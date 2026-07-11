//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEvidenceCommandsWorkWhileDaemonRuns pins the invariant the Evidence Vault
// depends on: the `evidence` and `claim` commands are worktree-side clients that
// open the DB and the evidence store directly, and they must never contend for
// the daemon's singleton NM_HOME lock (internal/daemon/lock.go) or try to start
// a daemon of their own. A daemon is always running when an in-run agent reaches
// the test/verify steps, so a command that needs the daemon gone is a command an
// agent can never call.
//
// The regression it guards: a report that every `evidence` subcommand — down to
// `--help` — aborted with "a no-mistakes daemon is already running for this
// NM_HOME (pid N): resource temporarily unavailable" whenever a daemon held the
// lock, which is exactly when an agent needs the vault.
//
// It runs the commands twice: from the user's own clone, and from inside the
// daemon-managed worktree of a live, gate-parked run, which is where the in-run
// agent actually calls them from.
func TestEvidenceCommandsWorkWhileDaemonRuns(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// init leaves a live daemon holding the singleton lock. Everything below
	// runs against it.
	if out, err := h.Run("daemon", "status"); err != nil || !strings.Contains(out, "daemon running") {
		t.Fatalf("daemon should be running after init: %v\n%s", err, out)
	}

	// Park a real run at the review gate so a daemon-managed worktree exists
	// with an active run in it — the in-run agent's exact situation.
	h.CommitChange("feature/evidence", "feature.txt", "evidence\n", "add feature.txt")
	h.PushToGate("feature/evidence")
	parked := waitForStepStatus(t, h, "feature/evidence", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if parked == nil {
		t.Fatal("expected feature/evidence run to park at the review gate")
	}
	runWorktree := filepath.Join(h.NMHome, "worktrees", h.repoID(), parked.ID)

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "from_user_clone", dir: h.WorkDir},
		{name: "from_run_worktree", dir: runWorktree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// --help must never touch NM_HOME state at all.
			out, err := h.RunInDir(tc.dir, "evidence", "--help")
			assertNoDaemonLockAbort(t, "evidence --help", out, err)
			if !strings.Contains(out, "Evidence Vault client") {
				t.Errorf("evidence --help did not print help:\n%s", out)
			}

			out, err = h.RunInDir(tc.dir, "evidence", "list")
			assertNoDaemonLockAbort(t, "evidence list", out, err)

			out, err = h.RunInDir(tc.dir, "evidence", "exec", "--label", "smoke-"+tc.name, "--", "echo", "hello")
			assertNoDaemonLockAbort(t, "evidence exec", out, err)
			if !strings.Contains(out, "captured") {
				t.Fatalf("evidence exec did not record captured evidence:\n%s", out)
			}
			evidenceID := strings.SplitN(strings.TrimSpace(out), "\t", 2)[0]

			out, err = h.RunInDir(tc.dir, "evidence", "list")
			assertNoDaemonLockAbort(t, "evidence list (after exec)", out, err)
			if !strings.Contains(out, evidenceID) {
				t.Errorf("evidence list does not show %s:\n%s", evidenceID, out)
			}
			if strings.Contains(out, "signature invalid") {
				t.Errorf("evidence recorded by the CLI failed its own signature check:\n%s", out)
			}

			// claim add shares the same in-run context plumbing, and only
			// resolves a run from inside the daemon-managed worktree.
			out, err = h.RunInDir(tc.dir, "claim", "add", "--text", "echo prints hello", "--evidence", evidenceID)
			assertNoDaemonLockAbort(t, "claim add", out, err)
			if tc.dir == runWorktree {
				if err != nil {
					t.Fatalf("claim add in the run worktree: %v\n%s", err, out)
				}
				out, err = h.RunInDir(tc.dir, "claim", "list")
				assertNoDaemonLockAbort(t, "claim list", out, err)
				if !strings.Contains(out, "echo prints hello") {
					t.Errorf("claim list does not show the registered claim:\n%s", out)
				}
			}
		})
	}

	// The daemon is still the same live daemon: nothing above stole or broke
	// its lock.
	if out, err := h.Run("daemon", "status"); err != nil || !strings.Contains(out, "daemon running") {
		t.Fatalf("daemon should still be running after the evidence commands: %v\n%s", err, out)
	}

	// Clear the gate and let the run finish. Leaving a run active would make
	// the harness's `daemon stop` refuse (the lifecycle guard) and leak this
	// test's daemon into the rest of the suite.
	h.Respond(parked.ID, types.StepReview, types.ActionApprove)
	waitForRunIDStatus(t, h, parked.ID, types.RunCompleted, 60*time.Second)
}

// assertNoDaemonLockAbort fails when a command aborted on the daemon's singleton
// NM_HOME lock — the failure mode this test exists to prevent.
func assertNoDaemonLockAbort(t *testing.T, cmd, out string, err error) {
	t.Helper()
	if strings.Contains(out, "daemon is already running for this NM_HOME") ||
		strings.Contains(out, "resource temporarily unavailable") {
		t.Fatalf("%s contended for the daemon singleton lock: %v\n%s", cmd, err, out)
	}
	if err != nil && strings.Contains(out, "daemon") {
		t.Fatalf("%s failed with a daemon-related error: %v\n%s", cmd, err, out)
	}
}
