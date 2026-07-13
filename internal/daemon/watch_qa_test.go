package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rendezvousNode blocks until both nodes of the confirm phase are inside Execute.
// A sequential pipeline can never open this barrier, so a test that finishes is
// direct evidence of overlap - not an inference drawn from timings.
type rendezvousNode struct {
	name    types.StepName
	arrived *sync.WaitGroup
	opened  chan struct{}
	hold    time.Duration
	workDir chan string
}

func (s *rendezvousNode) Name() types.StepName { return s.name }

func (s *rendezvousNode) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.workDir != nil {
		select {
		case s.workDir <- sctx.WorkDir:
		default:
		}
	}
	s.arrived.Done()
	select {
	case <-s.opened:
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("%s: the other node never started - the confirm phase is not parallel", s.name)
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
	if s.hold > 0 {
		time.Sleep(s.hold)
	}
	if s.name == types.StepWatch {
		sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{Action: pipeline.WatchConverged, Reason: "test verdict"})
	}
	return &pipeline.StepOutcome{}, nil
}

// The shape the captain asked for: after the PR, ONE run whose CI poll and QA
// pass are two concurrent nodes, holding its worktree until both converge.
//
// The evidence is twofold. The rendezvous makes a sequential pipeline impossible
// (each node waits for the other to start), and the recorded step timeline - the
// same started_at/completed_at a person reads off `axi status` - is asserted to
// overlap.
func TestWatchRunRunsQAAndCIInParallel(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-confirm-parallel")

	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}, &mockPRStep{prURL: testPRURL}}
	}

	var arrived sync.WaitGroup
	arrived.Add(2)
	opened := make(chan struct{})
	go func() {
		arrived.Wait()
		close(opened)
	}()
	qaWorkDir := make(chan string, 1)
	mgr.SetWatchStepFactory(func(selection []types.StepName) []pipeline.Step {
		if !types.SelectsQA(selection) {
			t.Error("the watch run did not inherit the gate run's qa selection")
		}
		return []pipeline.Step{
			&rendezvousNode{name: types.StepQA, arrived: &arrived, opened: opened, hold: 60 * time.Millisecond, workDir: qaWorkDir},
			&rendezvousNode{name: types.StepWatch, arrived: &arrived, opened: opened, hold: 60 * time.Millisecond},
		}
	})

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
		withSteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}
	waitForRunStatus(t, d, gateID, types.RunCompleted)

	watch := waitForRunOfKind(t, d, repo.ID, types.RunKindWatch)
	final := waitForRunStatus(t, d, watch.ID, types.RunCompleted)
	if final.ParentRunID == nil || *final.ParentRunID != gateID {
		t.Fatalf("watch parent = %v, want the gate run", final.ParentRunID)
	}

	// The QA node ran inside the run's own worktree - a QA pass has to boot the
	// product, and it must not do that in the user's clone.
	select {
	case dir := <-qaWorkDir:
		if want := p.WorktreeDir(repo.ID, watch.ID); dir != want {
			t.Fatalf("QA ran in %s, want the run's worktree %s", dir, want)
		}
	default:
		t.Fatal("the QA node never reported its working directory")
	}

	// The timeline: two step rows, overlapping in wall-clock time.
	steps, err := d.GetStepsByRun(watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("the confirm run has %d step rows, want one per node", len(steps))
	}
	byName := map[types.StepName]*db.StepResult{}
	for _, sr := range steps {
		byName[sr.StepName] = sr
	}
	qa, watchStep := byName[types.StepQA], byName[types.StepWatch]
	if qa == nil || watchStep == nil {
		t.Fatalf("confirm run steps = %v, want qa + watch", steps)
	}
	for _, sr := range steps {
		if sr.StartedAt == nil || sr.CompletedAt == nil {
			t.Fatalf("%s has no recorded timeline", sr.StepName)
		}
		if sr.Status != types.StepStatusCompleted {
			t.Fatalf("%s = %s, want completed", sr.StepName, sr.Status)
		}
	}
	if *qa.StartedAt > *watchStep.CompletedAt || *watchStep.StartedAt > *qa.CompletedAt {
		t.Fatalf("the nodes did not overlap: qa %d..%d, watch %d..%d",
			*qa.StartedAt, *qa.CompletedAt, *watchStep.StartedAt, *watchStep.CompletedAt)
	}

	// The worktree the run held is released once both nodes converged.
	waitForWorktreeGone(t, p.WorktreeDir(repo.ID, watch.ID))
}

// waitForWorktreeGone waits for a finished run's worktree to be released. The run
// row reaches `completed` inside the executor, a moment before the daemon's
// cleanup runs, so a bare stat here would race the teardown rather than test it.
func waitForWorktreeGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the confirm run's worktree outlived it: %s", dir)
}

// QA stays on-demand. An ordinary push must not grow a QA node - no agent
// session, no environment bootstrap, no comment on the user's PR.
func TestWatchRunHasNoQANodeUnlessSelected(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-confirm-default")

	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}, &mockPRStep{prURL: testPRURL}}
	}
	workDirs := make(chan string, 1)
	mgr.SetWatchStepFactory(func(selection []types.StepName) []pipeline.Step {
		if types.SelectsQA(selection) {
			t.Error("an ordinary push selected qa")
		}
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchConverged, workDirs: workDirs}}
	})

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}
	waitForRunStatus(t, d, gateID, types.RunCompleted)

	watch := waitForRunOfKind(t, d, repo.ID, types.RunKindWatch)
	waitForRunStatus(t, d, watch.ID, types.RunCompleted)

	steps, err := d.GetStepsByRun(watch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].StepName != types.StepWatch {
		t.Fatalf("an ordinary watch run has steps %v, want just the poller", steps)
	}

	// And with no QA node there is no worktree: a poller holds nothing on disk.
	if _, err := os.Stat(p.WorktreeDir(repo.ID, watch.ID)); !os.IsNotExist(err) {
		t.Fatalf("a plain watch run created a worktree it has no use for: %v", err)
	}
	select {
	case dir := <-workDirs:
		if dir == p.WorktreeDir(repo.ID, watch.ID) {
			t.Fatal("the poller ran in a managed worktree")
		}
	default:
	}
}

// failingNode stands in for a QA pass that could not boot the product.
type failingNode struct{ name types.StepName }

func (s *failingNode) Name() types.StepName { return s.name }
func (s *failingNode) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return nil, fmt.Errorf("could not boot the product: no browser on this machine")
}

// The two nodes are independent, and the run must treat them that way. A QA pass
// that fails - a broken environment, a crashed agent - must not swallow the CI
// verdict the poll already reached, or a bad QA environment would quietly cost
// the PR its fix round and nothing would ever escalate.
func TestFailedQANodeStillLetsTheCIVerdictDeriveAFixRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-qa-fails")

	findings := `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`
	mgr.SetWatchStepFactory(func(_ []types.StepName) []pipeline.Step {
		return []pipeline.Step{
			&failingNode{name: types.StepQA},
			&mockWatchStep{action: pipeline.WatchFix, findings: findings},
		}
	})
	fixRan := make(chan *db.Run, 1)
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&recordingStep{name: types.StepFix, seen: fixRan}}
	}

	gate, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind:      types.RunKindGate,
		PRURL:     testPRURL,
		OnlySteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatalf("insert gate run: %v", err)
	}

	watchID, err := mgr.startWatchRun(context.Background(), repo, gate)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}

	select {
	case fix := <-fixRan:
		if fix.ParentRunID == nil || *fix.ParentRunID != watchID {
			t.Fatalf("fix run parent = %v, want the watch run %s", fix.ParentRunID, watchID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the poll said CI was failing, but the failed QA node swallowed the verdict: no fix run was derived")
	}
}

// The captain's failure mode: a laptop sleeps, or an update restarts the daemon,
// while a confirm run has been holding a worktree for hours. Recovery must RESUME
// it, not re-run it - a QA pass that already finished cost ~25 minutes and ~400k
// tokens, and its verdict is recorded. The poll is the opposite: it is a pure
// function of the PR, so it simply asks again.
func TestRearmedWatchRunResumesQAInsteadOfRerunningIt(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-qa-resume")

	// The state a crash leaves behind: a running watch run that selected qa, whose
	// QA node completed (verdict recorded) and whose poll died mid-flight.
	crashed, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind:      types.RunKindWatch,
		PRURL:     testPRURL,
		OnlySteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatalf("insert crashed watch run: %v", err)
	}
	if err := d.UpdateRunStatus(crashed.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := d.UpdateRunQAVerdict(crashed.ID, "PASS"); err != nil {
		t.Fatalf("record qa verdict: %v", err)
	}
	qaRow, err := d.InsertStepResult(crashed.ID, types.StepQA)
	if err != nil {
		t.Fatalf("insert qa step: %v", err)
	}
	watchRow, err := d.InsertStepResult(crashed.ID, types.StepWatch)
	if err != nil {
		t.Fatalf("insert watch step: %v", err)
	}
	cfg := mgr.loadWatchConfig(context.Background(), repo, crashed)
	if err := d.CompleteStepWithValidation(qaRow.ID, types.StepStatusCompleted, 0, 1000, "", headSHA, pipeline.ConfigHash(cfg)); err != nil {
		t.Fatalf("complete qa step: %v", err)
	}
	if err := d.StartStep(watchRow.ID); err != nil {
		t.Fatalf("start watch step: %v", err)
	}

	qaRan := make(chan struct{}, 1)
	pollRan := make(chan struct{}, 1)
	mgr.SetWatchStepFactory(func(_ []types.StepName) []pipeline.Step {
		return []pipeline.Step{
			&recordingNode{name: types.StepQA, ran: qaRan},
			&recordingNode{name: types.StepWatch, ran: pollRan, converge: true},
		}
	})

	crashed, _ = d.GetRun(crashed.ID)
	if rearmed := mgr.rearmWatchRuns(context.Background(), []*db.Run{crashed}); len(rearmed) != 1 {
		t.Fatalf("rearmWatchRuns() = %v, want the crashed run", rearmed)
	}
	waitForRunStatus(t, d, crashed.ID, types.RunCompleted)

	select {
	case <-pollRan:
	default:
		t.Fatal("the re-armed run never asked the PR again")
	}
	select {
	case <-qaRan:
		t.Fatal("the re-armed run paid for the finished QA pass a second time instead of resuming it")
	default:
	}

	// And the verdict it reached is still the one on the run: nothing was lost.
	run, err := d.GetRun(crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.QAVerdict == nil || *run.QAVerdict != "PASS" {
		t.Fatalf("qa verdict after the restart = %v, want the recorded PASS", run.QAVerdict)
	}
}

// recordingNode reports that it executed.
type recordingNode struct {
	name     types.StepName
	ran      chan struct{}
	converge bool
}

func (s *recordingNode) Name() types.StepName { return s.name }
func (s *recordingNode) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.ran <- struct{}{}:
	default:
	}
	if s.converge {
		sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{Action: pipeline.WatchConverged, Reason: "test verdict"})
	}
	return &pipeline.StepOutcome{}, nil
}
