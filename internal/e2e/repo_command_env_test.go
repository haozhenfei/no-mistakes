//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRepoCommandsSeeRunRefs is the reason the variables exist. A monorepo
// wants to test only the packages its branch touches, which needs the run's
// base ref. Before this, the ref was known to no-mistakes (the rebase step
// rebases onto it) but never handed to the command, so repos hardcoded a dated
// release branch into .no-mistakes.yaml and it expired every cycle.
//
// The repo here integrates onto release/20260713, not main: the exported
// NM_BASE_REF must follow the configured default branch, and NM_HEAD_SHA must
// be the commit the command is actually running against.
func TestRepoCommandsSeeRunRefs(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	integration := "release/20260713"
	h.CommitChange(integration, "release.txt", "release baseline\n", "start the release branch")
	h.PushToUpstream(integration)
	h.Checkout("main")
	h.AppendGlobalConfig(fmt.Sprintf("repos:\n  %s:\n    default_branch: %s\n", h.WorkDir, integration))

	dumpPath := filepath.Join(t.TempDir(), "lint-env")
	branch := "impacted-packages"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	repoCfg := fmt.Sprintf(`ignore_patterns:
  - 'vendor/**'
commands:
  lint: "printf 'base_ref=%%s\\nbase_sha=%%s\\nhead_ref=%%s\\nhead_sha=%%s\\n' \"$NM_BASE_REF\" \"$NM_BASE_SHA\" \"$NM_HEAD_REF\" \"$NM_HEAD_SHA\" > %s"
`, dumpPath)
	h.CommitChange(branch, ".no-mistakes.yaml", repoCfg, "lint command that reports the run's refs")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)

	data, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("lint command never wrote its env dump (%s): %v; run status=%s err=%v", dumpPath, err, run.Status, deref(run.Error))
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			got[key] = value
		}
	}

	if got["base_ref"] != "origin/"+integration {
		t.Errorf("NM_BASE_REF = %q, want origin/%s (dump: %q)", got["base_ref"], integration, data)
	}
	if got["head_ref"] != branch {
		t.Errorf("NM_HEAD_REF = %q, want %s", got["head_ref"], branch)
	}
	if len(got["base_sha"]) != 40 {
		t.Errorf("NM_BASE_SHA = %q, want a full commit SHA", got["base_sha"])
	}
	if got["head_sha"] != run.HeadSHA {
		t.Errorf("NM_HEAD_SHA = %q, want the run's head %q", got["head_sha"], run.HeadSHA)
	}
	if got["base_sha"] == got["head_sha"] {
		t.Errorf("NM_BASE_SHA and NM_HEAD_SHA are both %q; the base must be the fork point, not the head", got["base_sha"])
	}
}
