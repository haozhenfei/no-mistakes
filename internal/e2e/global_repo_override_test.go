//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGlobalRepoOverride_AllowRepoCommands proves the deadlock is gone. The
// trusted default branch says allow_repo_commands: false and a repo whose
// default branch is frozen could never say otherwise — the switch that unlocks
// pushed-branch commands used to live behind the very door it opens. Naming the
// repo in the global config (which only the owner of the daemon host can write,
// exactly like the default branch) flips it, and the pushed branch's lint
// command runs.
func TestGlobalRepoOverride_AllowRepoCommands(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	h.AppendGlobalConfig(fmt.Sprintf("repos:\n  %s:\n    allow_repo_commands: true\n", h.WorkDir))

	markerPath := filepath.Join(t.TempDir(), "lint-ran")
	branch := "global-allow"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	repoCfg := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  lint: \"echo ran > %s\"\n", markerPath)
	h.CommitChange(branch, ".no-mistakes.yaml", repoCfg, "configure lint command on the feature branch")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("global allow_repo_commands override did not take effect: pushed-branch lint command never ran (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
	}
}

// TestGlobalRepoOverride_DefaultBranch proves the second hatch. The server's
// HEAD (main here, master at the reporting site) is not the branch the repo
// actually integrates onto. Without an override the daemon reads its trusted
// config — and computes its rebase and diff bases — from that wrong branch. With
// repos.<path>.default_branch pointing at the integration branch, the trusted
// .no-mistakes.yaml comes from THERE: its lint command runs, which it could not
// if the daemon were still reading the server's HEAD (main carries no commands).
func TestGlobalRepoOverride_DefaultBranch(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// The real integration baseline, on origin. It carries the trusted repo
	// config; main does not (the harness writes main's copy with no commands).
	markerPath := filepath.Join(t.TempDir(), "trusted-lint-ran")
	integration := "integration/2026-07"
	trustedCfg := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  lint: \"echo ran > %s\"\n", markerPath)
	h.CommitChange(integration, ".no-mistakes.yaml", trustedCfg, "trusted lint command on the integration branch")
	h.PushToUpstream(integration)
	h.Checkout("main")

	h.AppendGlobalConfig(fmt.Sprintf("repos:\n  %s:\n    default_branch: %s\n", h.WorkDir, integration))

	branch := "global-default-branch"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("default_branch override did not take effect: the trusted config was not read from %s (marker %s missing); run status=%s err=%v", integration, markerPath, run.Status, deref(run.Error))
	}

	// Sanity: lint actually reached a terminal status, so the marker is a real
	// signal rather than a pipeline that stopped before lint.
	lintStep, ok := findStep(run.Steps, types.StepLint)
	if !ok {
		t.Fatalf("lint step missing from run results")
	}
	switch lintStep.Status {
	case types.StepStatusCompleted, types.StepStatusSkipped, types.StepStatusFailed:
	default:
		t.Fatalf("lint step did not reach a terminal status: %s", lintStep.Status)
	}
}

// TestGlobalRepoOverride_PushedBranchCannotSetEither is the security red line:
// the two maintainer stances live in the global config and on the trusted
// default branch, never on the pushed branch. A contributor who ships both keys
// (and a repos: block, in case the loader were sloppy) alongside a hostile
// command must change nothing.
func TestGlobalRepoOverride_PushedBranchCannotSetEither(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	// No repos: block in the global config: the maintainer has said nothing.

	markerPath := filepath.Join(t.TempDir(), "pwned")
	branch := "pushed-self-enable"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	hostile := fmt.Sprintf(`ignore_patterns:
  - 'vendor/**'
allow_repo_commands: true
default_branch: %s
repos:
  %s:
    allow_repo_commands: true
    default_branch: %s
commands:
  lint: "echo pwned > %s"
`, branch, h.WorkDir, branch, markerPath)
	h.CommitChange(branch, ".no-mistakes.yaml", hostile, "try to self-enable from the pushed branch")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("SECURITY REGRESSION: the pushed branch enabled its own commands (marker %s exists); allow_repo_commands and default_branch must come only from the global config or the trusted default branch", markerPath)
	}
}
