package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/park"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeParkNotifier records what the executor announced.
type fakeParkNotifier struct {
	mu       sync.Mutex
	parked   []park.Record
	unparked []string
}

func (f *fakeParkNotifier) Park(rec park.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = append(f.parked, rec)
}

func (f *fakeParkNotifier) Unpark(runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unparked = append(f.unparked, runID)
}

func (f *fakeParkNotifier) parks() []park.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]park.Record(nil), f.parked...)
}

func (f *fakeParkNotifier) unparks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unparked...)
}

const parkTestFindings = `{"findings":[{"id":"verify-3","severity":"error","action":"ask-user","file":"internal/coverage/behavior.go","description":"behavior claim has no runtime evidence"}]}`

// A run that parks at a gate must announce it, and the announcement must name
// what the run is waiting for: which step, which gate, which findings, and what
// answers are acceptable. Without this, the pipeline asks and nobody finds out -
// which is what turned one question at 21:02 into 859 parked minutes.
func TestExecutor_ParkingAGateAnnouncesTheWaitAndWhatItIsWaitingFor(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	notifier := &fakeParkNotifier{}

	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepVerify, parkTestFindings)}, nil)
	exec.SetParkNotifier(notifier)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepVerify, types.StepStatusAwaitingApproval)

	var parks []park.Record
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if parks = notifier.parks(); len(parks) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(parks) != 1 {
		t.Fatalf("a parked gate produced %d park announcements, want 1", len(parks))
	}
	rec := parks[0]
	if rec.RunID != run.ID {
		t.Errorf("park names run %q, want %q", rec.RunID, run.ID)
	}
	if rec.Step != string(types.StepVerify) {
		t.Errorf("park does not name the step: %q", rec.Step)
	}
	if rec.Gate != string(types.StepStatusAwaitingApproval) {
		t.Errorf("park does not name the gate: %q", rec.Gate)
	}
	if rec.Branch != run.Branch || rec.Repo != repo.WorkingPath {
		t.Errorf("park does not say where to answer it: repo=%q branch=%q", rec.Repo, rec.Branch)
	}
	if len(rec.Findings) != 1 || rec.Findings[0].ID != "verify-3" {
		t.Fatalf("park does not name the findings: %+v", rec.Findings)
	}
	if rec.Findings[0].Action != "ask-user" {
		t.Errorf("finding lost its action: %+v", rec.Findings[0])
	}
	if strings.Join(rec.Actions, ",") != "approve,fix,skip" {
		t.Errorf("park does not name the acceptable answers: %v", rec.Actions)
	}
	if rec.Since.IsZero() {
		t.Error("park does not say since when")
	}
	if got := notifier.unparks(); len(got) != 0 {
		t.Fatalf("unpark announced while the gate was still parked: %v", got)
	}

	// Answering the gate ends the wait, and the end is announced too: a
	// supervisor must not be left believing the last park it heard still holds.
	exec.Respond(types.StepVerify, types.ActionApprove, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
	if got := notifier.unparks(); len(got) != 1 || got[0] != run.ID {
		t.Fatalf("answered gate produced unparks %v, want [%s]", got, run.ID)
	}
}

// The park must be readable with no live listener at all. This is the acceptance
// case: a supervising agent that died, restarted, or never watched still has to
// be able to ask "is anything waiting on me, since when, for what" and get an
// answer - so the executor's park is driven through a real park.Store here, and
// the assertion is made against the file it leaves on disk.
func TestExecutor_ParkedGateIsDiscoverableFromTheDurableRecordAlone(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	store := park.New(p.ParkedFile(), park.StaticConfig(park.Config{ReminderInterval: park.DefaultReminderInterval}), nil)
	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepVerify, parkTestFindings)}, nil)
	exec.SetParkNotifier(store)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepVerify, types.StepStatusAwaitingApproval)

	var records []park.Record
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, _ = park.Load(p.ParkedFile())
		if len(records) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(records) != 1 {
		t.Fatalf("parked.json holds %d records while a gate is parked, want 1", len(records))
	}
	rec := records[0]
	if rec.RunID != run.ID || rec.Step != string(types.StepVerify) || rec.Gate != string(types.StepStatusAwaitingApproval) {
		t.Fatalf("durable record does not name the run/step/gate: %+v", rec)
	}
	if len(rec.Findings) != 1 || rec.Findings[0].ID != "verify-3" {
		t.Fatalf("durable record does not name the findings: %+v", rec.Findings)
	}
	if len(rec.Respond) == 0 || !strings.Contains(rec.Respond[0], "axi respond --action approve") {
		t.Fatalf("durable record does not say how to answer: %v", rec.Respond)
	}

	exec.Respond(types.StepVerify, types.ActionApprove, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// And the record is gone the moment the wait is: it is state, not a log.
	records, err := park.Load(p.ParkedFile())
	if err != nil {
		t.Fatalf("load parked.json: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("parked.json still names %d parked runs after the gate was answered", len(records))
	}
}

// The record mirrors runs.awaiting_agent_since exactly - it must never claim a
// gate is waiting when the DB says nothing is awaiting an answer.
//
// A daemon shutdown is the case that makes this concrete, and it is easy to get
// backwards (I did, and a real daemon SIGTERM against a real parked run is what
// corrected it): the shutdown path clears awaiting_agent_since and marks the run
// interrupted, so the run is NOT parked afterwards - it needs `axi resume`
// before it has a gate to answer at all. A record that survived here would send
// a supervisor to `axi respond` a gate that no longer exists, and would go on
// saying so forever, because nothing is left running to retract it.
func TestExecutor_RecordTracksTheAwaitingAgentColumnOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause string
	}{
		{"daemon shutting down", types.RunInterruptReasonDaemonShuttingDown},
		{"aborted by user", types.RunCancelReasonAbortedByUser},
		{"superseded", types.RunCancelReasonSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := t.TempDir()
			notifier := &fakeParkNotifier{}

			exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, parkTestFindings)}, nil)
			exec.SetParkNotifier(notifier)

			ctx, cancel := context.WithCancelCause(context.Background())
			done := make(chan error, 1)
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

			cancel(errors.New(tc.cause))
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("executor timed out")
			}

			// The DB stopped awaiting an answer...
			row, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if row.AwaitingAgentSince != nil {
				t.Fatalf("runs.awaiting_agent_since is still set after %q", tc.cause)
			}
			// ...so the record must have stopped saying somebody owes one.
			if got := notifier.unparks(); len(got) != 1 {
				t.Fatalf("after %q the record was not retracted: unparks %v", tc.cause, got)
			}
		})
	}
}
