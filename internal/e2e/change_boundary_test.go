//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// documentPromptMarker is a phrase only the document step's prompt carries, in
// both of its modes (documentation-only and the combined documentation+lint
// housekeeping pass, which is what a repo with no lint command gets). The
// document step runs an agent on every gate run and commits whatever it wrote
// through commitAgentFixes - the same choke point every fix agent goes through -
// so it is the cheapest way to put an agent-written change in front of the
// boundary in a real daemon run.
const documentPromptMarker = "Analyze what the change made stale"

// TestGateConfigIsImmutableToAgents drives the real daemon, a real gate push and
// a real agent process, and pins both halves of the change boundary:
//
//   - Default-deny: an agent that writes the gate's own config (.no-mistakes.yaml)
//     fails the run, and the failure names the path. This is the incident - a fix
//     agent rewrote the shared team gate config mid-run, and nothing stopped it,
//     because "do not touch the gate config" was only a sentence in a prompt.
//   - The explicit opt-in still permits it: a task whose whole purpose IS to
//     change the gate config passes `no-mistakes.allow-gate-config` with the push,
//     and the same agent edit sails through to the PR.
func TestGateConfigIsImmutableToAgents(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: gateConfigEditScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// 1. Default-deny.
	denied := "feature/agent-rewrites-the-gate"
	h.CommitChange(denied, "feature.txt", "a user-visible change\n", "add a user-visible change")
	h.PushToGate(denied)

	run := h.WaitForRun(denied, 180*time.Second)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed: an agent rewriting the gate's own config must stop the run\nsteps:\n%s",
			run.Status, dumpSteps(steps))
	}
	failure := deref(run.Error)
	t.Logf("the run's failure, as the user sees it:\n%s", failure)
	if !strings.Contains(failure, ".no-mistakes.yaml") {
		t.Fatalf("the failure must name the offending path, got %q\nsteps:\n%s", failure, dumpSteps(steps))
	}
	if !strings.Contains(failure, "change boundary") {
		t.Fatalf("the failure must say what refused the change, got %q", failure)
	}
	// Nothing the agent wrote reached the branch: the run failed before the
	// push step, so the gate config on the shared remote is untouched.
	if h.UpstreamBranchExists(denied) {
		t.Fatal("a run refused by the change boundary must not have pushed the branch")
	}

	// 2. The explicit opt-in permits the very same edit.
	allowed := "feature/retune-the-gate-on-purpose"
	h.CommitChange(allowed, "feature2.txt", "another user-visible change\n", "add another user-visible change")
	h.PushToGateWithOptions(allowed, "no-mistakes.allow-gate-config")

	run = h.WaitForRun(allowed, 180*time.Second)
	steps, err = database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("opted-in run status = %s, error = %q: the legitimate case must stay possible\nsteps:\n%s",
			run.Status, deref(run.Error), dumpSteps(steps))
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !stored.AllowGateConfig {
		t.Fatal("the push option must be recorded on the run: a resume and a derived fix round read the permission back from there")
	}
	// And the agent's gate-config edit actually shipped: the opt-in is not a
	// flag that merely silences the check, it lets the change through the gate.
	if !h.UpstreamBranchExists(allowed) {
		t.Fatal("the opted-in run must have pushed its branch")
	}
	pushedConfig := h.UpstreamFileAtBranch(allowed, ".no-mistakes.yaml")
	if !strings.Contains(pushedConfig, "the agent decided the baseline was stale") {
		t.Fatalf("the opted-in gate-config change never reached the remote; upstream .no-mistakes.yaml =\n%s", pushedConfig)
	}
}

// gateConfigEditScenario makes the document agent - which every gate run invokes,
// and whose output goes through commitAgentFixes - write the repository's gate
// config, exactly as the fix agent did during the incident.
func gateConfigEditScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate-config-edit-scenario.yaml")
	content := `actions:
  - match: "` + documentPromptMarker + `"
    text: "the gate config looked wrong to me, so I fixed it"
    edits:
      - path: .no-mistakes.yaml
        new: |
          commands:
            test: echo "the agent decided the baseline was stale"
    structured:
      findings: []
      summary: "docs are accurate"
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
