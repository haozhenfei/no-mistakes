package daemon

import (
	"context"

	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSupersedes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		incoming, existing types.RunKind
		want               bool
	}{
		// A new push moves the head: the old gate run and the watcher of the
		// old head are both obsolete.
		{types.RunKindGate, types.RunKindGate, true},
		{types.RunKindGate, types.RunKindWatch, true},
		// A watch run replaces the branch's previous watcher only. If it
		// cancelled gate runs, a gate run could never derive one (it would be
		// killed the instant it succeeded), and a watch run could never derive a
		// fix round (it would kill the run it just asked for).
		{types.RunKindWatch, types.RunKindWatch, true},
		{types.RunKindWatch, types.RunKindGate, false},
	}
	for _, tc := range cases {
		if got := supersedes(tc.incoming, tc.existing); got != tc.want {
			t.Errorf("supersedes(%s, %s) = %v, want %v", tc.incoming, tc.existing, got, tc.want)
		}
	}
}

// TestNewGateRunCancelsActiveWatchRun: a push invalidates the head the watcher
// was polling, so the watcher must die with it.
func TestNewGateRunCancelsActiveWatchRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-cancel-watch")

	watchStarted := make(chan struct{})
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepWatch, started: watchStarted}}
	})
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}}
	}

	parent, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert parent gate run: %v", err)
	}
	if err := d.UpdateRunPRURL(parent.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	parent, _ = d.GetRun(parent.ID)

	watchID, err := mgr.startWatchRun(context.Background(), repo, parent)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}
	select {
	case <-watchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("watch run never started")
	}

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}

	watch := waitForRunStatus(t, d, watchID, types.RunCancelled)
	if watch.Error == nil || *watch.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("watch run error = %v, want %q", watch.Error, types.RunCancelReasonSuperseded)
	}
	waitForRunStatus(t, d, gateID, types.RunCompleted)
}

// TestNewWatchRunDoesNotCancelGateRun is the invariant the whole design rests
// on. A fix round is a gate run derived from a watch run, and a gate run derives
// a watch run when it succeeds - so if a starting watch run cancelled gate runs,
// the loop would eat itself. The pre-split code cancelled by branch alone.
func TestNewWatchRunDoesNotCancelGateRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-keep-gate")

	gateStarted := make(chan struct{})
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepPush, started: gateStarted}}
	}
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepWatch}}
	})

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}
	select {
	case <-gateStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("gate run never started")
	}

	parent, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert parent run: %v", err)
	}
	if err := d.UpdateRunPRURL(parent.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	parent, _ = d.GetRun(parent.ID)

	watchID, err := mgr.startWatchRun(context.Background(), repo, parent)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}
	waitForRunStatus(t, d, watchID, types.RunCompleted)

	// The gate run is untouched: still running, and never given a cancel cause.
	gate, err := d.GetRun(gateID)
	if err != nil {
		t.Fatalf("get gate run: %v", err)
	}
	if gate.Status != types.RunRunning {
		t.Fatalf("gate run status = %s, want running: a starting watch run must not cancel a gate run", gate.Status)
	}
	if gate.Error != nil {
		t.Fatalf("gate run error = %q, want none", *gate.Error)
	}

	mgr.Shutdown()
}

// TestRearmWatchRunsAfterCrash: a watch run holds no local state, so a crash
// costs nothing and the daemon must simply ask the PR again. Marking it
// `interrupted` and waiting for a human is what left a PR with nobody watching
// it - the failure mode the split exists to remove.
func TestRearmWatchRunsAfterCrash(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-rearm")

	// A watch run left `running` by a daemon that died mid-poll, with the
	// half-written step row that poll had already inserted.
	crashed, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindWatch,
	})
	if err != nil {
		t.Fatalf("insert crashed watch run: %v", err)
	}
	if err := d.UpdateRunPRURL(crashed.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	if err := d.UpdateRunStatus(crashed.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	stale, err := d.InsertStepResult(crashed.ID, types.StepWatch)
	if err != nil {
		t.Fatalf("insert stale step: %v", err)
	}
	if err := d.StartStep(stale.ID); err != nil {
		t.Fatalf("start stale step: %v", err)
	}

	seen := make(chan *db.Run, 1)
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchConverged, seen: seen}}
	})

	// This is the startup sequence: select the re-armable watch runs, keep them
	// out of the interrupted sweep, then re-arm.
	candidates := mgr.resumableWatchRuns()
	if len(candidates) != 1 || candidates[0].ID != crashed.ID {
		t.Fatalf("resumableWatchRuns() = %v, want the crashed watch run", candidates)
	}
	preserved := map[string]struct{}{crashed.ID: {}}
	if _, err := d.RecoverStaleRunsExcept(types.RunInterruptReasonDaemonCrashed, preserved); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}

	afterSweep, err := d.GetRun(crashed.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if afterSweep.Status == types.RunInterrupted {
		t.Fatal("the stale sweep interrupted a watch run: its PR now has nobody watching it")
	}

	rearmed := mgr.rearmWatchRuns(context.Background(), candidates)
	if len(rearmed) != 1 || rearmed[0] != crashed.ID {
		t.Fatalf("rearmWatchRuns() = %v, want [%s]", rearmed, crashed.ID)
	}

	select {
	case run := <-seen:
		if run.ID != crashed.ID {
			t.Fatalf("re-armed the wrong run: %s", run.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the re-armed watch run never polled")
	}

	final := waitForRunStatus(t, d, crashed.ID, types.RunCompleted)
	if final.Kind != types.RunKindWatch {
		t.Fatalf("kind = %q, want watch", final.Kind)
	}

	// The dead poll's step row is gone, replaced by the re-armed run's own.
	steps, err := d.GetStepsByRun(crashed.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("watch step rows = %d, want exactly 1 (the re-armed poll's)", len(steps))
	}
	if steps[0].ID == stale.ID {
		t.Fatal("the crashed poll's step row survived the re-arm")
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("re-armed step status = %s, want completed", steps[0].Status)
	}
}

// TestRearmSkipsWatchRunWithoutPR: there is nothing to poll, so normal stale-run
// recovery should interrupt it instead of re-arming an empty watcher.
func TestRearmSkipsWatchRunWithoutPR(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-rearm-nopr")

	orphan, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindWatch,
	})
	if err != nil {
		t.Fatalf("insert watch run: %v", err)
	}
	if err := d.UpdateRunStatus(orphan.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	if got := mgr.resumableWatchRuns(); len(got) != 0 {
		t.Fatalf("resumableWatchRuns() = %v, want none: a watch run with no PR has nothing to poll", got)
	}
	if _, err := d.RecoverStaleRunsExcept(types.RunInterruptReasonDaemonCrashed, nil); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	run, err := d.GetRun(orphan.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != types.RunInterrupted {
		t.Fatalf("status = %s, want interrupted", run.Status)
	}
}

// A QA-only gate run must NOT cancel the branch's watcher. It skips push and pr,
// so it cannot move origin or touch the PR the watcher is polling - and it will
// not derive a replacement watcher either, because deriving one needs a PR its
// own pr step opened. Cancelling would leave the PR permanently unwatched:
// failing CI would derive no fix round and nothing would escalate.
func TestQAOnlyGateRunDoesNotCancelActiveWatchRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-qa-keeps-watch")

	watchStarted := make(chan struct{})
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepWatch, started: watchStarted}}
	})
	// The gate factory stands in for the pipeline; the qa step the run selects is
	// appended to it by execStepsFor.
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}}
	}

	parent, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert parent gate run: %v", err)
	}
	if err := d.UpdateRunPRURL(parent.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	parent, _ = d.GetRun(parent.ID)

	watchID, err := mgr.startWatchRun(context.Background(), repo, parent)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}
	select {
	case <-watchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("watch run never started")
	}

	// `axi run --only qa`.
	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "rerun",
		onlySteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatalf("start qa-only gate run: %v", err)
	}
	// The qa step itself fails here (this fixture has no PR host); what matters
	// is what the run did to the watcher on its way in.
	waitForRunTerminalState(t, d, gateID)

	watch, err := d.GetRun(watchID)
	if err != nil {
		t.Fatalf("get watch run: %v", err)
	}
	if watch.Status == types.RunCancelled {
		t.Fatalf("a --only qa run cancelled the branch's watch run (error=%v); the PR is now unwatched and no replacement watcher is derived", watch.Error)
	}
}
