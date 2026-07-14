//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/park"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestParkedRunIsImpossibleToMiss is the end-to-end proof of the wake mechanism
// against a REAL parked run: a real daemon, a real git repo, a real gate.
//
// A run parks at the review gate with an ask-user finding. Three things must
// then be true, and they cover three different ways a supervisor can fail to
// find out:
//
//  1. The notify hook fired - the supervisor is TOLD, without having to poll.
//  2. <NM_HOME>/parked.json names the run, step, gate, findings, and the
//     commands that answer it - so a supervisor who was never listening, or
//     whose listener died, can still find out. This is the layer that does not
//     depend on anyone having been there at the time.
//  3. `no-mistakes parked` reads that record with no daemon dependency and exits
//     0 while something is parked, 1 when nothing is.
//
// And when the gate is answered, all three go quiet: the record empties, the
// unpark hook fires, and `parked` exits 1. A record that outlives its wait sends
// people to answer a question that is already gone.
func TestParkedRunIsImpossibleToMiss(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	inbox := filepath.Join(t.TempDir(), "inbox.txt")
	h.AppendGlobalConfig(fmt.Sprintf(`
notify:
  on_park: 'printf "PARK run=%%s step=%%s gate=%%s reminder=%%s findings=%%s\n" "$NM_RUN_ID" "$NM_STEP" "$NM_GATE" "$NM_REMINDER" "$NM_FINDINGS" >> %s'
  on_unpark: 'printf "UNPARK run=%%s\n" "$NM_RUN_ID" >> %s'
  reminder_interval: "10m"
`, inbox, inbox))

	h.CommitChange("init-wake", "seed.txt", "seed\n", "seed for wake test")
	initWorktree := h.AddWorktree("init-wake")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/wake", "feature.txt", "change\n", "add feature change")
	fw := h.AddWorktree("feature/wake")

	// Nothing is parked yet, and `parked` says so with exit 1.
	if out, err := h.RunInDir(fw, "parked"); err == nil {
		t.Fatalf("`parked` exited 0 with nothing parked:\n%s", out)
	} else if !strings.Contains(out, "No run is parked") {
		t.Fatalf("`parked` with nothing parked printed:\n%s", out)
	}

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run (expected to stop at the review gate, exit 0): %v\n%s", err, out)
	}

	gated := waitForStepStatus(t, h, "feature/wake", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if gated == nil {
		t.Fatal("expected the feature/wake run to park at the review gate")
	}

	// (1) The durable record. It must name what the run is waiting for.
	parkedFile := filepath.Join(h.NMHome, "parked.json")
	records := waitForParkedRecords(t, parkedFile, 1, 30*time.Second)
	rec := records[0]
	if rec.RunID != gated.ID {
		t.Errorf("parked.json names run %q, want the parked run %q", rec.RunID, gated.ID)
	}
	if rec.Step != string(types.StepReview) {
		t.Errorf("parked.json does not name the step: %q", rec.Step)
	}
	if rec.Gate != string(types.StepStatusAwaitingApproval) {
		t.Errorf("parked.json does not name the gate: %q", rec.Gate)
	}
	if rec.Branch != "feature/wake" {
		t.Errorf("parked.json does not name the branch: %q", rec.Branch)
	}
	if len(rec.Findings) != 1 || rec.Findings[0].ID != "axi-1" {
		t.Fatalf("parked.json does not name the findings the gate is waiting on: %+v", rec.Findings)
	}
	if rec.Findings[0].Action != "ask-user" {
		t.Errorf("finding lost its action in the record: %+v", rec.Findings[0])
	}
	if strings.Join(rec.Actions, ",") != "approve,fix,skip" {
		t.Errorf("parked.json does not name the acceptable answers: %v", rec.Actions)
	}
	if len(rec.Respond) == 0 || !strings.Contains(rec.Respond[0], "axi respond --action approve") {
		t.Errorf("parked.json does not carry the commands that answer the gate: %v", rec.Respond)
	}
	if rec.Since.IsZero() {
		t.Error("parked.json does not say since when the run has been waiting")
	}

	// (2) The notify hook fired - nobody had to go looking.
	notice := waitForInbox(t, inbox, "PARK run=", 30*time.Second)
	for _, want := range []string{
		"PARK run=" + gated.ID,
		"step=review",
		"gate=awaiting_approval",
		"reminder=0",
		"axi-1",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("on_park hook did not report %q:\n%s", want, notice)
		}
	}

	// (3) `no-mistakes parked` surfaces the same wait, needs no daemon RPC, and
	// exits 0 while something is parked.
	parkedOut, err := h.RunInDir(fw, "parked")
	if err != nil {
		t.Fatalf("`parked` exited non-zero while a run was parked: %v\n%s", err, parkedOut)
	}
	for _, want := range []string{gated.ID, "review", "awaiting_approval", "axi-1", "axi respond --action approve"} {
		if !strings.Contains(parkedOut, want) {
			t.Errorf("`parked` did not surface %q:\n%s", want, parkedOut)
		}
	}

	// Answering the gate ends the wait - and the wake mechanism goes quiet.
	if out, err := h.RunInDir(fw, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("axi respond approve: %v\n%s", err, out)
	}

	waitForParkedRecords(t, parkedFile, 0, 60*time.Second)
	unparked := waitForInbox(t, inbox, "UNPARK run="+gated.ID, 30*time.Second)
	if !strings.Contains(unparked, "UNPARK run="+gated.ID) {
		t.Errorf("on_unpark hook did not fire when the gate was answered:\n%s", unparked)
	}
	if out, err := h.RunInDir(fw, "parked"); err == nil {
		t.Fatalf("`parked` still exits 0 after the gate was answered:\n%s", out)
	}
}

// waitForParkedRecords polls parked.json until it holds want records.
func waitForParkedRecords(t *testing.T, path string, want int, timeout time.Duration) []park.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []park.Record
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var recs []park.Record
			if err := json.Unmarshal(data, &recs); err == nil {
				last = recs
				if len(recs) == want {
					return recs
				}
			}
		} else if want == 0 && os.IsNotExist(err) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("parked.json holds %d records after %v, want %d: %+v", len(last), timeout, want, last)
	return nil
}

// waitForInbox polls the notify hook's output file until it contains want.
func waitForInbox(t *testing.T, path, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			last = string(data)
			if strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("notify hook output never contained %q after %v:\n%s", want, timeout, last)
	return ""
}
