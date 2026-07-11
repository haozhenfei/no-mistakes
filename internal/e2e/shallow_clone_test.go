//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestShallowClonedRepoGates is the end-to-end regression for shallow clones:
// large monorepos are checked out with --depth, and until the gate accepted
// shallow pushes such a repo could not be validated at all - git refused the
// push into the (empty) gate repo with "shallow update not allowed", so the
// pipeline never started.
//
// It drives the real daemon: init, push a branch from the shallow clone through
// the gate, run every pipeline step in a worktree cut from the now-shallow gate
// repo, and land the branch on the (full-history) upstream. That last hop is the
// one that had never been measured before this change.
func TestShallowClonedRepoGates(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), ShallowClone: true})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("shallow/feature", "hello.txt", "hello from a shallow clone\n", "add hello.txt")
	h.PushToGate("shallow/feature")

	run := h.WaitForRun("shallow/feature", 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run on shallow clone did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	// The push step must have landed the branch on the upstream, which holds
	// the full history the shallow clone lacks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "refs/heads/shallow/feature")
	if err != nil {
		t.Fatalf("branch from shallow clone never reached upstream: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("upstream has no SHA for the pushed branch")
	}
}
