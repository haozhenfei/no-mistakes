package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rendezvousStep blocks until every step of the phase has entered Execute. If the
// executor ran the phase sequentially, the first step would wait for a partner
// that has not started and the barrier would never open - so a passing test is
// proof of overlap, not an inference from timing.
type rendezvousStep struct {
	name    types.StepName
	arrived *sync.WaitGroup
	opened  chan struct{}
	hold    time.Duration
	outcome *StepOutcome
	fail    bool

	startedAt time.Time
	endedAt   time.Time
}

func (s *rendezvousStep) Name() types.StepName { return s.name }

func (s *rendezvousStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.startedAt = time.Now()
	s.arrived.Done()
	select {
	case <-s.opened:
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("step %s: the other node of the phase never started; the phase is not parallel", s.name)
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
	if s.hold > 0 {
		time.Sleep(s.hold)
	}
	s.endedAt = time.Now()
	if s.fail {
		return nil, fmt.Errorf("step %s: killed mid-flight", s.name)
	}
	if s.outcome != nil {
		return s.outcome, nil
	}
	return &StepOutcome{}, nil
}

// The confirm phase's two nodes must actually run at the same time. This is the
// whole reason QA lives next to the CI poll rather than in front of it: a PR whose
// CI is not watched for the ~25 minutes a QA pass takes is a PR whose failing
// checks derive no fix round and whose blocked approval escalates to nobody.
func TestExecutor_ParallelPhaseRunsItsStepsAtTheSameTime(t *testing.T) {
	database, p, run, repo := setupTest(t)

	var arrived sync.WaitGroup
	arrived.Add(2)
	opened := make(chan struct{})
	go func() {
		arrived.Wait()
		close(opened) // both nodes are inside Execute at once
	}()

	qa := &rendezvousStep{name: types.StepQA, arrived: &arrived, opened: opened, hold: 40 * time.Millisecond}
	watch := &rendezvousStep{name: types.StepWatch, arrived: &arrived, opened: opened, hold: 40 * time.Millisecond}

	exec := NewExecutor(database, p, nil, nil, []Step{qa, watch}, nil)
	exec.SetParallelPhase([]types.StepName{types.StepQA, types.StepWatch})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// The rendezvous already proves overlap; assert the recorded timeline says so
	// too, because that timeline is what a person reads off `axi status`.
	if !qa.startedAt.Before(watch.endedAt) || !watch.startedAt.Before(qa.endedAt) {
		t.Fatalf("the two nodes did not overlap: qa %s..%s, watch %s..%s",
			qa.startedAt, qa.endedAt, watch.startedAt, watch.endedAt)
	}

	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("step rows = %d, want one per node", len(results))
	}
	for _, sr := range results {
		if sr.Status != types.StepStatusCompleted {
			t.Fatalf("%s = %s, want completed", sr.StepName, sr.Status)
		}
	}
}

// The run must not finish until BOTH nodes have converged - that is what "the run
// holds its worktree until the confirm phase is done" means in code. A poll that
// converges in milliseconds must not tear the worktree out from under a QA pass
// that is still booting the product.
func TestExecutor_ParallelPhaseWaitsForEveryNode(t *testing.T) {
	database, p, run, repo := setupTest(t)

	var arrived sync.WaitGroup
	arrived.Add(2)
	opened := make(chan struct{})
	go func() {
		arrived.Wait()
		close(opened)
	}()

	slow := &rendezvousStep{name: types.StepQA, arrived: &arrived, opened: opened, hold: 150 * time.Millisecond}
	fast := &rendezvousStep{name: types.StepWatch, arrived: &arrived, opened: opened}

	exec := NewExecutor(database, p, nil, nil, []Step{slow, fast}, nil)
	exec.SetParallelPhase([]types.StepName{types.StepQA, types.StepWatch})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if slow.endedAt.IsZero() {
		t.Fatal("the run finished before its slow node did")
	}
	if !fast.endedAt.Before(slow.endedAt) {
		t.Fatal("the test did not exercise the case it means to: the fast node was not first")
	}

	updated, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", updated.Status)
	}
}

// The approval gate is single-occupancy by design: one waitingStep, one
// approvalCh, one runs.awaiting_agent_since. Two concurrent nodes that both want
// to park must therefore take turns, or the second would overwrite the gate the
// driving agent is answering and its findings would vanish.
func TestExecutor_ParallelPhaseSerializesTheApprovalGate(t *testing.T) {
	database, p, run, repo := setupTest(t)

	var arrived sync.WaitGroup
	arrived.Add(2)
	opened := make(chan struct{})
	go func() {
		arrived.Wait()
		close(opened)
	}()

	qa := &rendezvousStep{
		name: types.StepQA, arrived: &arrived, opened: opened,
		outcome: &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"qa"}`},
	}
	watch := &rendezvousStep{
		name: types.StepWatch, arrived: &arrived, opened: opened,
		outcome: &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"watch"}`},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{qa, watch}, nil)
	exec.SetParallelPhase([]types.StepName{types.StepQA, types.StepWatch})

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	// Answer whichever node reached the gate first, then the other. At no point
	// may both be parked at once.
	for i := 0; i < 2; i++ {
		parked := waitForGate(t, exec)
		if err := exec.Respond(parked, types.ActionApprove, nil); err != nil {
			t.Fatalf("respond to %s: %v", parked, err)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished after both gates were answered")
	}
}

// A daemon restart (a laptop that sleeps, an update) interrupts a confirm run
// that may have been polling for hours. Re-arming it must RESUME: a QA pass that
// already finished cost ~25 minutes and ~400k tokens and its verdict is recorded,
// so it is reused; the poll is a pure function of the PR, so it simply runs again.
// Re-running QA on every restart is the failure this ordering prevents.
func TestExecutor_ResumeReusesAFinishedQANodeAndRepollsTheWatch(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}
	workDir := t.TempDir()

	var arrived sync.WaitGroup
	arrived.Add(2)
	opened := make(chan struct{})
	go func() {
		arrived.Wait()
		close(opened)
	}()
	qa := &rendezvousStep{name: types.StepQA, arrived: &arrived, opened: opened}
	// The poll dies with the daemon; the QA node had already finished. This is the
	// state a crash leaves behind (the QA row completed, the poll's row failed).
	watch := &rendezvousStep{name: types.StepWatch, arrived: &arrived, opened: opened, fail: true}

	exec := NewExecutor(database, p, cfg, nil, []Step{qa, watch}, nil)
	exec.SetParallelPhase([]types.StepName{types.StepQA, types.StepWatch})
	if err := exec.Execute(context.Background(), run, repo, workDir); err == nil {
		t.Fatal("the first run was supposed to die with its poll")
	}

	// The daemon comes back and re-arms the same run at the same head.
	qaAgain := newPassStep(types.StepQA)
	watchAgain := newPassStep(types.StepWatch)
	resumed := NewExecutor(database, p, cfg, nil, []Step{qaAgain, watchAgain}, nil)
	resumed.SetParallelPhase([]types.StepName{types.StepQA, types.StepWatch})
	if err := resumed.ResumeFrom(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if qaAgain.callCount() != 0 {
		t.Fatal("the re-armed run paid for the QA pass again instead of reusing the finished one")
	}
	if watchAgain.callCount() != 1 {
		t.Fatalf("the poll ran %d times on resume, want exactly 1: it must ask the PR again", watchAgain.callCount())
	}
}

// waitForGate blocks until exactly one step is parked and returns it.
func waitForGate(t *testing.T, exec *Executor) types.StepName {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		exec.mu.Lock()
		waiting, step := exec.waiting, exec.waitingStep
		exec.mu.Unlock()
		if waiting {
			return step
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no step reached the approval gate")
	return ""
}
