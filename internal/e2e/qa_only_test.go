//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// qaPromptMarker is a line only the qa step's prompt carries.
const qaPromptMarker = "PHASE 3 - Exercise the changed behavior."

// TestQAStepIsOnDemand pins the on-demand contract through the real daemon and a
// real gate push:
//
//   - an ordinary push runs the pipeline exactly as before: no qa step in it, no
//     QA agent invocation, nothing paid for a step nobody asked for;
//   - `axi run --only qa` runs the qa step and skips every other one, and the
//     selection lands on the run row, where a resume reads it.
//
// What the qa step then does with the PR - reads runs.pr_url, falls back to
// asking the host, fails when there is no PR, and carries the repo's
// qa.instructions into its prompt - is covered by the step's own tests in
// internal/pipeline/steps, which fake the PR host directly. A PR host cannot be
// faked inside the daemon here: the daemon rebuilds its PATH from the login
// shell, and the harness's stub bin dir does not survive that on every machine
// (the same reason TestForkRouting and TestPRHandsOffToWatchRun fail locally).
func TestQAStepIsOnDemand(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: qaScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/qa-on-demand"
	h.CommitChange(branch, "feature.txt", "a user-visible change\n", "add a user-visible change")

	// 1. An ordinary push must not pay for QA.
	h.PushToGate(branch)
	gate := h.WaitForRun(branch, 120*time.Second)

	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	gateSteps, err := database.GetStepsByRun(gate.ID)
	if err != nil {
		t.Fatalf("get gate steps: %v", err)
	}
	for _, sr := range gateSteps {
		if sr.StepName == types.StepQA {
			t.Fatalf("an ordinary push carries a qa step (status %s); QA must be on-demand\nsteps:\n%s",
				sr.Status, dumpSteps(gateSteps))
		}
	}
	if got := qaPrompts(h); got != 0 {
		t.Fatalf("an ordinary push invoked the QA agent %d time(s); QA must be on-demand", got)
	}
	if len(gateSteps) != len(types.GateSteps()) {
		t.Fatalf("an ordinary push ran %d steps, want the unchanged %d-step gate sequence\nsteps:\n%s",
			len(gateSteps), len(types.GateSteps()), dumpSteps(gateSteps))
	}
	if gate.Status != types.RunCompleted {
		t.Fatalf("gate run status = %s, error = %v\nsteps:\n%s", gate.Status, deref(gate.Error), dumpSteps(gateSteps))
	}

	// 2. --only qa runs the qa step and skips every other one. Whether that run
	// then succeeds depends on a reachable PR host, which is not what this
	// assertion is about: the contract here is the step SELECTION.
	out, _ := h.Run("axi", "run", "--only", "qa", "--intent", "QA the branch against its open PR")

	qaRun := latestRunForBranch(t, h, database, branch, gate.ID)
	qaSteps, err := database.GetStepsByRun(qaRun.ID)
	if err != nil {
		t.Fatalf("get qa run steps: %v", err)
	}
	if len(qaSteps) == 0 {
		t.Fatalf("--only qa produced a run with no steps\naxi output:\n%s", out)
	}
	ran := map[types.StepName]types.StepStatus{}
	for _, sr := range qaSteps {
		if sr.Status != types.StepStatusSkipped {
			ran[sr.StepName] = sr.Status
		}
	}
	if _, ok := ran[types.StepQA]; !ok {
		t.Fatalf("--only qa did not run the qa step\nsteps:\n%s\naxi output:\n%s", dumpSteps(qaSteps), out)
	}
	delete(ran, types.StepQA)
	if len(ran) != 0 {
		t.Fatalf("--only qa also ran %v; every other step must be skipped\nsteps:\n%s", ran, dumpSteps(qaSteps))
	}
	// The selection is a property of the run: it is persisted as the
	// complementary skip set, which is what a later resume reads back.
	for _, step := range []types.StepName{types.StepReview, types.StepPush, types.StepPR} {
		if !containsStepName(qaRun.SkipSteps, step) {
			t.Fatalf("--only qa did not persist %q in the run's skip set: %v", step, qaRun.SkipSteps)
		}
	}
}

// --only with a step name that does not exist must fail loudly. Silently running
// the full pipeline instead would review, test, push, and open a PR that the
// caller never asked for.
func TestOnlyRejectsUnknownStep(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	h.CommitChange("feature/only-unknown-step", "unknown.txt", "change\n", "add change")

	out, err := h.Run("axi", "run", "--only", "quality-assurance", "--intent", "typo in the step name")
	if err == nil {
		t.Fatalf("axi run --only quality-assurance succeeded; want an error\n%s", out)
	}
	if !strings.Contains(out, "unknown step") {
		t.Fatalf("error output does not name the unknown step:\n%s", out)
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("a rejected --only still started %d run(s); it must start none", len(runs))
	}
}

// --skip and --only describe the same set two ways, and honoring both would make
// the result depend on an evaluation order the caller cannot see.
func TestOnlyAndSkipAreMutuallyExclusive(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	h.CommitChange("feature/only-and-skip", "both.txt", "change\n", "add change")

	out, err := h.Run("axi", "run", "--only", "qa", "--skip", "lint", "--intent", "both flags")
	if err == nil {
		t.Fatalf("axi run --only qa --skip lint succeeded; want an error\n%s", out)
	}
	if !strings.Contains(out, "cannot be combined") {
		t.Fatalf("error output does not explain the conflict:\n%s", out)
	}
}

func qaPrompts(h *Harness) int {
	count := 0
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, qaPromptMarker) {
			count++
		}
	}
	return count
}

func containsStepName(steps []types.StepName, want types.StepName) bool {
	for _, step := range steps {
		if step == want {
			return true
		}
	}
	return false
}

// latestRunForBranch returns the newest run for a branch other than excludeID.
func latestRunForBranch(t *testing.T, h *Harness, database *db.DB, branch, excludeID string) *db.Run {
	t.Helper()
	// repoID mirrors the gate's own hashing, which resolves symlinks first
	// (/var -> /private/var on macOS); looking the repo up by raw path misses.
	repoID := h.repoID()
	deadline := time.Now().Add(30 * time.Second)
	for {
		runs, err := database.GetRunsByRepoBranch(repoID, branch)
		if err != nil {
			t.Fatalf("get runs: %v", err)
		}
		for _, run := range runs {
			if run.ID != excludeID {
				return run
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run for branch %s other than %s", branch, excludeID)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func dumpSteps(steps []*db.StepResult) string {
	var b strings.Builder
	for _, sr := range steps {
		fmt.Fprintf(&b, "  %-9s %-10s err=%q\n", sr.StepName, sr.Status, deref(sr.Error))
	}
	return b.String()
}

// qaScenario answers the QA prompt with a report, so a run that does reach the
// QA agent has something to parse. Everything else falls to the clean catch-all.
func qaScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qa-scenario.yaml")
	content := `actions:
  - match: "` + qaPromptMarker + `"
    text: "QA complete"
    structured:
      verdict: PASS
      summary: "booted the app and exercised the changed page"
      achieves_goal: "Yes - the page renders the new field."
      report_markdown: "## QA Report: PASS"
      issues: []
  - match: "whose job is to REFUTE"
    text: "the evidence supports the claim"
    structured:
      verdict: CONFIRMED
      rationale: "fakeagent: the captured evidence supports the claim"
  - text: "clean"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "fakeagent: nothing risky in this change"
      title: "feat: a user-visible change"
      body: "## What Changed\n\nA user-visible change."
      tested: []
      testing_summary: "fakeagent: no tests to run"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}
