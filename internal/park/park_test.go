package park

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a Notifier that captures the edges instead of shelling out.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Notify(_ context.Context, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// waitForEvents blocks until the recorder has at least n events. Notifications
// are fired off the caller's goroutine so a park never blocks the pipeline.
func (r *recorder) waitForEvents(t *testing.T, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if evs := r.snapshot(); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d notifications, got %d", n, len(r.snapshot()))
	return nil
}

func newTestStore(t *testing.T, interval time.Duration) (*Store, *recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parked.json")
	rec := &recorder{}
	s := New(path, StaticConfig(Config{ReminderInterval: interval}), rec)
	return s, rec, path
}

const gateFindings = `{"findings":[
  {"id":"v8-lcov-no-da-on-jsx-lines","severity":"warning","action":"ask-user","file":"src/App.tsx","description":"coverage downgraded 3 of 4 hunks to static-verified"},
  {"id":"noise","severity":"info","action":"no-op","description":"nothing to decide"}
]}`

// A run that parks must produce BOTH the durable record and the notification,
// and the record must name the step, the gate, and the findings - that is the
// whole contract: a supervisor who was not listening can still find out what is
// waiting on them and what answers it takes.
func TestPark_WritesDurableRecordAndNotifies(t *testing.T) {
	s, rec, path := newTestStore(t, DefaultReminderInterval)
	since := time.Now().Add(-3 * time.Minute)

	s.Park(Record{
		RunID:    "01KXDKC674JR6QPKY21T9VCXBY",
		Repo:     "/Users/me/coze",
		Branch:   "fix/colors",
		Step:     "test",
		Gate:     "awaiting_approval",
		Since:    since,
		Findings: Findings(gateFindings),
		Actions:  GateActions(),
		Respond:  RespondCommands("/Users/me/coze"),
	})

	// The durable record: on disk, readable with no daemon and no listener.
	records, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("parked.json holds %d records, want 1", len(records))
	}
	got := records[0]
	if got.RunID != "01KXDKC674JR6QPKY21T9VCXBY" {
		t.Errorf("run = %q", got.RunID)
	}
	if got.Step != "test" {
		t.Errorf("record does not name the step: step = %q", got.Step)
	}
	if got.Gate != "awaiting_approval" {
		t.Errorf("record does not name the gate: gate = %q", got.Gate)
	}
	if !got.Since.Equal(since.Truncate(time.Nanosecond)) && got.Since.Unix() != since.Unix() {
		t.Errorf("record does not preserve the park time: since = %v, want %v", got.Since, since)
	}
	if len(got.Findings) != 1 || got.Findings[0].ID != "v8-lcov-no-da-on-jsx-lines" {
		t.Fatalf("record does not name the findings: %+v", got.Findings)
	}
	if got.Findings[0].Action != "ask-user" || got.Findings[0].Severity != "warning" {
		t.Errorf("finding lost its action/severity: %+v", got.Findings[0])
	}
	// A no-op finding is not something a person has to decide, so it is not
	// what the run is waiting for.
	for _, f := range got.Findings {
		if f.ID == "noise" {
			t.Errorf("no-op finding leaked into the record as something to answer")
		}
	}
	// What answers are acceptable, and the exact commands that send them.
	if strings.Join(got.Actions, ",") != "approve,fix,skip" {
		t.Errorf("record does not name the acceptable answers: %v", got.Actions)
	}
	if len(got.Respond) == 0 || !strings.Contains(got.Respond[0], "axi respond --action approve") {
		t.Errorf("record does not carry the respond commands: %v", got.Respond)
	}
	if !strings.Contains(got.Respond[0], "/Users/me/coze") {
		t.Errorf("respond command does not say where to run it: %q", got.Respond[0])
	}

	// The edge notification.
	evs := rec.waitForEvents(t, 1)
	if evs[0].Kind != KindPark || evs[0].Reminder != 0 {
		t.Fatalf("first notification = %+v, want an initial park edge", evs[0])
	}
	summary := Summary(evs[0])
	for _, want := range []string{"PARKED", "test", "awaiting_approval", "v8-lcov-no-da-on-jsx-lines", "axi respond --action approve"} {
		if !strings.Contains(summary, want) {
			t.Errorf("park summary does not mention %q:\n%s", want, summary)
		}
	}
}

// Unparking clears the record and announces the edge. A record that outlives the
// wait it describes sends a supervisor to answer a gate that is already gone.
func TestUnpark_ClearsTheRecordAndAnnouncesTheEdge(t *testing.T) {
	s, rec, path := newTestStore(t, DefaultReminderInterval)
	s.Park(Record{RunID: "run-1", Step: "review", Gate: "awaiting_approval", Since: time.Now()})
	rec.waitForEvents(t, 1)

	s.Unpark("run-1")

	records, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("parked.json still holds %d records after unpark", len(records))
	}
	evs := rec.waitForEvents(t, 2)
	if evs[1].Kind != KindUnpark {
		t.Fatalf("second notification = %v, want an unpark edge", evs[1].Kind)
	}

	// Idempotent: unparking a run that is not parked announces nothing.
	s.Unpark("run-1")
	time.Sleep(20 * time.Millisecond)
	if evs := rec.snapshot(); len(evs) != 2 {
		t.Fatalf("a redundant unpark produced %d notifications, want 2", len(evs))
	}
}

// The reminder is the layer that makes a MISSED edge recoverable: the park is
// re-sent, on a backing-off cadence, until the gate is answered.
func TestReminder_ResendsUnansweredParkOnABackingOffCadence(t *testing.T) {
	const interval = 10 * time.Minute
	s, rec, path := newTestStore(t, interval)

	parkedAt := time.Date(2026, 7, 13, 21, 2, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return parkedAt })
	s.Park(Record{RunID: "run-1", Step: "test", Gate: "awaiting_approval", Since: parkedAt})
	rec.waitForEvents(t, 1)

	// Not due yet: a tick before the interval must not nudge anybody.
	s.Tick(parkedAt.Add(9 * time.Minute))
	if evs := rec.snapshot(); len(evs) != 1 {
		t.Fatalf("reminder fired early: %d notifications after 9m", len(evs))
	}

	// Reminder 1 at +10m, then the gaps back off: 3x (40m), then 6x (100m),
	// and stay at 6x. A wait nobody answers settles to about one message an
	// hour instead of one every ten minutes - that is what keeps this from
	// becoming spam while never going silent.
	wantAt := []time.Duration{10 * time.Minute, 40 * time.Minute, 100 * time.Minute, 160 * time.Minute}
	for i, at := range wantAt {
		// A tick one minute before each due time must do nothing.
		s.Tick(parkedAt.Add(at - time.Minute))
		if evs := rec.snapshot(); len(evs) != i+1 {
			t.Fatalf("reminder %d fired early at %v: %d notifications", i+1, at-time.Minute, len(evs))
		}
		s.Tick(parkedAt.Add(at))
		evs := rec.waitForEvents(t, i+2)
		last := evs[len(evs)-1]
		if last.Kind != KindPark {
			t.Fatalf("reminder %d is a %s event, want a re-sent park", i+1, last.Kind)
		}
		if last.Reminder != i+1 {
			t.Fatalf("reminder count = %d at %v, want %d", last.Reminder, at, i+1)
		}
		if !strings.Contains(Summary(last), "reminder") {
			t.Errorf("re-sent park does not say it is a reminder:\n%s", Summary(last))
		}
	}

	// The escalation is visible in the durable record too, not only in whatever
	// the hook happens to talk to.
	records, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != 1 || records[0].Reminders != len(wantAt) {
		t.Fatalf("record does not carry the reminder count: %+v", records)
	}

	// Answering the gate stops the re-sends. It must never keep nagging about a
	// question that has been answered.
	s.Unpark("run-1")
	before := len(rec.waitForEvents(t, len(wantAt)+2))
	s.Tick(parkedAt.Add(24 * time.Hour))
	time.Sleep(20 * time.Millisecond)
	if got := len(rec.snapshot()); got != before {
		t.Fatalf("reminders kept firing after the gate was answered: %d -> %d", before, got)
	}
}

// A park that happened hours ago and was never answered is due for its reminder
// the moment a restarted daemon re-announces it. This is the whole reason the
// record carries the ORIGINAL park time rather than "now": the 859-minute wait
// in the 6951 run must not get a fresh 10-minute grace period every time the
// daemon bounces.
func TestReminder_AnOldParkIsDueImmediatelyAfterARestart(t *testing.T) {
	s, rec, _ := newTestStore(t, 10*time.Minute)
	now := time.Now()
	parkedAt := now.Add(-3 * time.Hour)

	s.Park(Record{RunID: "run-1", Step: "test", Gate: "awaiting_approval", Since: parkedAt})
	rec.waitForEvents(t, 1)

	s.Tick(now)
	evs := rec.waitForEvents(t, 2)
	if evs[1].Reminder != 1 {
		t.Fatalf("a 3h-old park was not reminded on the first tick after restart: %+v", evs[1])
	}
}

// Reset is what a starting daemon calls: the previous process's file is not this
// process's truth.
func TestReset_TruncatesTheRecordToTheEmptySet(t *testing.T) {
	s, _, path := newTestStore(t, DefaultReminderInterval)
	s.Park(Record{RunID: "run-1", Step: "review", Gate: "awaiting_approval", Since: time.Now()})

	s.Reset()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parked.json: %v", err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("parked.json after Reset = %q, want []", strings.TrimSpace(string(data)))
	}
	if recs := s.Records(); len(recs) != 0 {
		t.Fatalf("Reset left %d in-memory records", len(recs))
	}
}

// The durable record must not depend on a notify hook being configured: a user
// with no hooks still gets the file, which is the layer a supervisor can always
// fall back to.
func TestPark_WritesTheRecordWithNoNotifierConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parked.json")
	s := New(path, StaticConfig(Config{}), nil)
	s.Park(Record{RunID: "run-1", Step: "verify", Gate: "fix_review", Since: time.Now()})

	records, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(records) != 1 || records[0].Gate != "fix_review" {
		t.Fatalf("record not written without a notifier: %+v", records)
	}
}

// Re-parking the same run (a later gate in the same run) replaces the record and
// restarts the reminder clock: it is a new question.
func TestPark_ASecondGateReplacesTheRecordAndRestartsTheClock(t *testing.T) {
	s, rec, path := newTestStore(t, 10*time.Minute)
	first := time.Now().Add(-2 * time.Hour)
	s.Park(Record{RunID: "run-1", Step: "test", Gate: "awaiting_approval", Since: first})
	rec.waitForEvents(t, 1)

	second := time.Now()
	s.Park(Record{RunID: "run-1", Step: "verify", Gate: "awaiting_approval", Since: second})
	rec.waitForEvents(t, 2)

	records, _ := Load(path)
	if len(records) != 1 {
		t.Fatalf("re-park produced %d records, want 1", len(records))
	}
	if records[0].Step != "verify" || records[0].Reminders != 0 {
		t.Fatalf("re-park did not replace the record: %+v", records[0])
	}
	s.Tick(second.Add(time.Minute))
	time.Sleep(20 * time.Millisecond)
	if evs := rec.snapshot(); len(evs) != 2 {
		t.Fatalf("the new gate inherited the old gate's overdue reminder: %d notifications", len(evs))
	}
}

// Load of a file that has never been written is "nothing is parked", not an
// error: on a fresh machine those are the same fact.
func TestLoad_MissingFileIsAnEmptySet(t *testing.T) {
	recs, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("Load of a missing file returned %d records", len(recs))
	}
}

// Reminders are opt-out, but opting out must not cost the durable record.
func TestReminder_DisabledByConfigStillWritesTheRecord(t *testing.T) {
	s, rec, path := newTestStore(t, 0)
	parkedAt := time.Now().Add(-24 * time.Hour)
	s.Park(Record{RunID: "run-1", Step: "test", Gate: "awaiting_approval", Since: parkedAt})
	rec.waitForEvents(t, 1)

	s.Tick(time.Now())
	time.Sleep(20 * time.Millisecond)
	if evs := rec.snapshot(); len(evs) != 1 {
		t.Fatalf("reminder fired with reminder_interval disabled: %d notifications", len(evs))
	}
	if records, _ := Load(path); len(records) != 1 {
		t.Fatalf("disabling reminders cost the durable record: %+v", records)
	}
}
