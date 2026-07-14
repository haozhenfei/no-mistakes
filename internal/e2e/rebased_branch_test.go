//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRebasedBranchStillGates is the end-to-end regression for a branch whose
// history was rewritten between two runs: rebasing onto a freshly cut release
// branch (routine where a release branch is cut daily) leaves the branch with
// the same name but a head that is not a descendant of the one the gate mirror
// already holds. `axi run` pushed that head into the mirror fast-forward-only,
// so git refused it ("fetch first"), the run never started, and the only way
// forward was deleting the ref inside the mirror by hand.
//
// It drives the real CLI: gate the branch once, rewrite its history, gate it
// again - the second run must start on the rebased head with no manual repair.
func TestRebasedBranchStillGates(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/rebased", "feature.txt", "v1\n", "add feature")
	if out, err := h.Run("axi", "run", "--intent", "ship the feature"); err != nil {
		t.Fatalf("first axi run: %v\n%s", err, out)
	}
	first := h.WaitForRun("feature/rebased", 120*time.Second)
	if first.Status != types.RunCompleted {
		t.Fatalf("first run did not complete: status=%s error=%v", first.Status, deref(first.Error))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, h.WorkDir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// Take the branch as it landed upstream (the run's own commits included),
	// then rewrite its history on top of a moved default branch.
	mustGit("fetch", "origin", "feature/rebased")
	mustGit("checkout", "feature/rebased")
	mustGit("reset", "--hard", "FETCH_HEAD")
	h.CommitChange("main", "release.txt", "cut\n", "release commit")
	h.PushToUpstream("main")
	mustGit("checkout", "feature/rebased")
	mustGit("rebase", "main")
	rebased := h.WorktreeRefSHA("HEAD")
	if rebased == first.HeadSHA {
		t.Fatal("rebase did not rewrite the branch head; the test proves nothing")
	}

	// The gate mirror still holds the pre-rebase head, which the rebased head is
	// not a descendant of. This is the push that used to be rejected.
	if out, err := h.Run("axi", "run", "--intent", "re-gate the rebased branch"); err != nil {
		t.Fatalf("axi run on the rebased branch: %v\n%s", err, out)
	}
	second := h.WaitForRun("feature/rebased", 120*time.Second)
	if second.ID == first.ID {
		t.Fatalf("no new run started for the rebased head (still run %s)", first.ID)
	}
	if second.Status != types.RunCompleted {
		t.Fatalf("run on the rebased branch did not complete: status=%s error=%v", second.Status, deref(second.Error))
	}
	if second.HeadSHA != rebased {
		t.Fatalf("run head = %s, want the rebased head %s: the pipeline ran on stale history", second.HeadSHA, rebased)
	}
}
