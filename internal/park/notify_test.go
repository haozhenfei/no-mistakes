package park

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The notify hook is the edge that reaches a human (or their supervising agent)
// while the run is still waiting. This exercises the real shell path, not a
// stub: a configured on_park command must actually run, and must be able to see
// the whole wait - run, step, gate, findings, and how to answer - from its
// environment alone.
func TestShellNotifier_OnParkRunsTheHookWithTheWholeWaitInItsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook shell differs on windows")
	}
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox.txt")

	cfg := Config{
		OnPark: `{ printf "event=%s run=%s step=%s gate=%s reminder=%s\n" "$NM_EVENT" "$NM_RUN_ID" "$NM_STEP" "$NM_GATE" "$NM_REMINDER"; ` +
			`printf "findings=%s\n" "$NM_FINDINGS"; printf "%s\n" "$NM_PARK_SUMMARY"; } >> ` + inbox,
		OnUnpark: `printf "event=%s run=%s\n" "$NM_EVENT" "$NM_RUN_ID" >> ` + inbox,
	}
	n := NewShellNotifier(StaticConfig(cfg), filepath.Join(dir, "parked.json"))

	rec := Record{
		RunID:    "01KXDKC674JR6QPKY21T9VCXBY",
		Repo:     "/Users/me/coze",
		Branch:   "fix/colors",
		Step:     "test",
		Gate:     "awaiting_approval",
		Since:    time.Now(),
		Findings: Findings(gateFindings),
		Actions:  GateActions(),
		Respond:  RespondCommands("/Users/me/coze"),
	}
	n.Notify(context.Background(), Event{Kind: KindPark, Record: rec})

	got := readFile(t, inbox)
	for _, want := range []string{
		"event=park",
		"run=01KXDKC674JR6QPKY21T9VCXBY",
		"step=test",
		"gate=awaiting_approval",
		"reminder=0",
		"v8-lcov-no-da-on-jsx-lines",
		"no-mistakes axi respond --action approve",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook did not see %q:\n%s", want, got)
		}
	}

	// And the gate ending is announced too, so a supervisor is not left holding
	// a park that is no longer true.
	n.Notify(context.Background(), Event{Kind: KindUnpark, Record: rec})
	got = readFile(t, inbox)
	if !strings.Contains(got, "event=unpark") {
		t.Errorf("on_unpark hook did not run:\n%s", got)
	}
}

// A reminder re-sends through the same hook, and tells the hook it is a
// reminder so a downstream inbox can escalate rather than duplicate.
func TestShellNotifier_ReminderReSendsThroughTheOnParkHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook shell differs on windows")
	}
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox.txt")
	n := NewShellNotifier(StaticConfig(Config{OnPark: `printf "reminder=%s\n" "$NM_REMINDER" >> ` + inbox}), "")

	rec := Record{RunID: "run-1", Step: "test", Gate: "awaiting_approval", Since: time.Now()}
	n.Notify(context.Background(), Event{Kind: KindPark, Reminder: 0, Record: rec})
	n.Notify(context.Background(), Event{Kind: KindPark, Reminder: 2, Record: rec})

	got := readFile(t, inbox)
	if !strings.Contains(got, "reminder=0") || !strings.Contains(got, "reminder=2") {
		t.Fatalf("hook did not see the reminder count:\n%s", got)
	}
}

// A hook that fails must not take the run down with it - and must not be able to
// touch the gate. The notification is the loud layer; parked.json is the durable
// one, and the gate is neither's business.
func TestShellNotifier_AFailingHookIsSurvivable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook shell differs on windows")
	}
	n := NewShellNotifier(StaticConfig(Config{OnPark: "exit 7"}), "")
	n.Notify(context.Background(), Event{Kind: KindPark, Record: Record{RunID: "run-1"}})
	// No panic, no error surface: the only contract is that it returns.
}

// No hook configured is not an error, and must not be mistaken for a hook that
// ran.
func TestShellNotifier_NoHookConfiguredIsANoOp(t *testing.T) {
	n := NewShellNotifier(StaticConfig(Config{}), "")
	n.Notify(context.Background(), Event{Kind: KindPark, Record: Record{RunID: "run-1"}})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
