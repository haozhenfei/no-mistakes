package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newFailThenPassStep fails until its err is cleared, then passes. It models the
// step that interrupted the first run and succeeds on resume.
func newFailThenPassStep(name types.StepName) *mockStep {
	return &mockStep{name: name, outcome: &StepOutcome{ExitCode: 0}, err: errors.New("boom")}
}

// TestExecutor_ResumeDoesNotReviveSkippedStep is the regression for the bug
// where a step skipped with --skip came back to life on resume and ran for
// real. A skipped row is not `completed`, so the resume prefix scan broke at it
// and re-executed it. The skip set now travels with the run, so resume must
// keep the step at zero executions.
func TestExecutor_ResumeDoesNotReviveSkippedStep(t *testing.T) {
	database, p, _, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}

	// Start the run the way the daemon does: the skip set is stored on the row.
	run, err := database.InsertRunWithSkipSteps(repo.ID, "feature", "abc123", "def456", []types.StepName{types.StepReview})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	review := newPassStep(types.StepReview)
	testStep := newFailThenPassStep(types.StepTest)
	steps := []Step{review, testStep}

	exec := NewExecutor(database, p, cfg, nil, steps, nil)
	exec.SetSkippedSteps(run.SkipSteps)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("initial execute: want failure from test step, got nil")
	}
	if got := review.callCount(); got != 0 {
		t.Fatalf("review calls after run with --skip review = %d, want 0", got)
	}
	if got := testStep.callCount(); got != 1 {
		t.Fatalf("test calls after initial run = %d, want 1", got)
	}

	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if !ResumableStatus(run.Status) {
		t.Fatalf("run status = %s, want a resumable status", run.Status)
	}

	// The daemon reconstructs the skip set from the run row; do the same here.
	testStep.err = nil // the transient failure is gone; resume should get past it
	resumed := NewExecutor(database, p, cfg, nil, steps, nil)
	resumed.SetSkippedSteps(run.SkipSteps)
	if err := resumed.ResumeFrom(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := review.callCount(); got != 0 {
		t.Fatalf("review calls after resume = %d, want still 0 (skipped step was revived)", got)
	}
	if got := testStep.callCount(); got != 2 {
		t.Fatalf("test calls after resume = %d, want 2", got)
	}

	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if results[0].Status != types.StepStatusSkipped {
		t.Fatalf("review status after resume = %s, want %s", results[0].Status, types.StepStatusSkipped)
	}
	if results[1].Status != types.StepStatusCompleted {
		t.Fatalf("test status after resume = %s, want %s", results[1].Status, types.StepStatusCompleted)
	}
}

// TestExecutor_ResumeKeepsCompletedTailAfterSkippedStep guards the cost side of
// the fix: a skipped leading step must not invalidate the completed steps that
// follow it, or resume would re-run work it already paid for.
func TestExecutor_ResumeKeepsCompletedTailAfterSkippedStep(t *testing.T) {
	database, p, _, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}

	// Start the run the way the daemon does: the skip set is stored on the row.
	run, err := database.InsertRunWithSkipSteps(repo.ID, "feature", "abc123", "def456", []types.StepName{types.StepReview})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	review := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)
	lint := newFailThenPassStep(types.StepLint)
	steps := []Step{review, testStep, lint}

	exec := NewExecutor(database, p, cfg, nil, steps, nil)
	exec.SetSkippedSteps(run.SkipSteps)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("initial execute: want failure from lint step, got nil")
	}

	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	lint.err = nil // the transient failure is gone; resume should get past it
	resumed := NewExecutor(database, p, cfg, nil, steps, nil)
	resumed.SetSkippedSteps(run.SkipSteps)
	if err := resumed.ResumeFrom(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := review.callCount(); got != 0 {
		t.Fatalf("review calls after resume = %d, want 0", got)
	}
	if got := testStep.callCount(); got != 1 {
		t.Fatalf("test calls after resume = %d, want still 1 (completed tail was re-run)", got)
	}
	if got := lint.callCount(); got != 2 {
		t.Fatalf("lint calls after resume = %d, want 2", got)
	}
}
