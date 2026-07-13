package daemon

import (
	"context"
	"sync/atomic"
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
	mgr.SetWatchStepFactory(func(_ []types.StepName) []pipeline.Step {
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
	mgr.SetWatchStepFactory(func(_ []types.StepName) []pipeline.Step {
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
	mgr.SetWatchStepFactory(func(_ []types.StepName) []pipeline.Step {
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

	// The re-arm RESUMES the run rather than rebuilding it from nothing: the dead
	// poll's row is reset and re-executed in place, not dropped and recreated.
	// That is what lets a QA node that had already finished be reused instead of
	// paid for again on every daemon restart - the poll itself loses nothing
	// either way, since one more poll rebuilds its whole verdict.
	steps, err := d.GetStepsByRun(crashed.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("watch step rows = %d, want exactly 1 (the poll's, reset in place)", len(steps))
	}
	if steps[0].ID != stale.ID {
		t.Fatalf("the re-arm replaced the poll's step row (%s -> %s) instead of resuming it", stale.ID, steps[0].ID)
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
// `axi run --only qa` on a branch whose PR is already being watched must never
// leave that PR unwatched. It is PR-inert (it skips push and pr), so it does not
// supersede the watcher on its way in - and on its way out it hands off to a
// REPLACEMENT watcher that carries the QA node, which is the only way QA can be
// added to a PR that is already open. What must never happen is the branch ending
// up with no live watcher at all: failing CI would derive no fix round and nothing
// would escalate.
func TestQAOnlyGateRunKeepsThePRWatched(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-qa-keeps-watch")

	// Each watcher gets its OWN start signal: mockSlowStep closes the channel it
	// is given, and this test starts two watchers (the original, then the
	// replacement the qa-only run hands off to). Sharing one channel would panic
	// the second run on `close of closed channel` - which the daemon recovers into
	// a failed run, and the test would then be measuring its own bug.
	firstStarted := make(chan struct{})
	replacementStarted := make(chan struct{})
	var watchers atomic.Int32
	mgr.SetWatchStepFactory(func(selection []types.StepName) []pipeline.Step {
		started := firstStarted
		if watchers.Add(1) > 1 {
			started = replacementStarted
		}
		return []pipeline.Step{&mockSlowStep{name: types.StepWatch, started: started}}
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
	case <-firstStarted:
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
	waitForRunTerminalState(t, d, gateID)

	// The replacement watcher inherits the PR from the branch's last run that
	// reached one - the qa-only run skipped the pr step, so it has none itself -
	// and it carries the QA node.
	select {
	case <-replacementStarted:
	case <-time.After(15 * time.Second):
		t.Fatal("the qa-only run left the branch's PR with no live watcher")
	}
	replacement := waitForActiveWatchRun(t, d, repo.ID, "main", watchID)
	if !types.SelectsQA(replacement.OnlySteps) {
		t.Fatalf("the replacement watcher has no QA node (only_steps = %v); --only qa did nothing", replacement.OnlySteps)
	}
	if replacement.PRURL == nil || *replacement.PRURL != testPRURL {
		t.Fatalf("the replacement watcher is watching %v, want the branch's PR %s", replacement.PRURL, testPRURL)
	}
}

// waitForActiveWatchRun waits for an active watch run on the branch other than
// excludeID, and fails if the branch is left with no watcher at all.
func waitForActiveWatchRun(t *testing.T, d *db.DB, repoID, branch, excludeID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := d.GetRunsByRepoBranch(repoID, branch)
		if err != nil {
			t.Fatalf("get runs: %v", err)
		}
		for _, run := range runs {
			if run.ID == excludeID || !run.Kind.Watch() {
				continue
			}
			if run.Status == types.RunPending || run.Status == types.RunRunning {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the branch's PR was left with no live watcher")
	return nil
}
