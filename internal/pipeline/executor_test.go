package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_StepLifecycleEvents verifies the executor emits step_started
// and step_completed IPC events for every step in order. The broader
// happy-path orchestration (DB persistence, run/step status transitions,
// timestamp + duration recording across all 8 real steps) is exercised by
// the e2e journey suite (internal/e2e), so this test focuses solely on
// the IPC event contract that the TUI subscribes to.
func TestExecutor_StepLifecycleEvents(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	stepNames := []types.StepName{types.StepReview, types.StepTest, types.StepLint}
	steps := make([]Step, len(stepNames))
	for i, name := range stepNames {
		steps[i] = newPassStep(name)
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	for _, name := range stepNames {
		if e := events.find(ipc.EventStepStarted, name); e == nil {
			t.Errorf("missing step_started event for %s", name)
		}
		if e := events.find(ipc.EventStepCompleted, name); e == nil {
			t.Errorf("missing step_completed event for %s", name)
		}
	}
}

func TestExecutor_ResumeSkipsCompletedStepsForSameHead(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}
	hash := ConfigHash(cfg)

	review := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("initial execute: %v", err)
	}
	if got := review.callCount(); got != 1 {
		t.Fatalf("initial review calls = %d, want 1", got)
	}
	if got := testStep.callCount(); got != 1 {
		t.Fatalf("initial test calls = %d, want 1", got)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if steps[0].ValidatedHeadSHA == nil || *steps[0].ValidatedHeadSHA != run.HeadSHA {
		t.Fatalf("review validated head = %v, want %s", steps[0].ValidatedHeadSHA, run.HeadSHA)
	}
	if steps[0].ConfigHash == nil || *steps[0].ConfigHash != hash {
		t.Fatalf("review config hash = %v, want %s", steps[0].ConfigHash, hash)
	}
	if err := database.UpdateRunErrorStatus(run.ID, "daemon crashed during execution", types.RunInterrupted); err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	run, _ = database.GetRun(run.ID)

	resumed := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := resumed.ResumeFrom(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := review.callCount(); got != 1 {
		t.Fatalf("review calls after resume = %d, want still 1", got)
	}
	if got := testStep.callCount(); got != 1 {
		t.Fatalf("test calls after resume = %d, want still 1", got)
	}
}

func TestExecutor_ResumeDoesNotSkipWhenHeadMoved(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}

	review := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("initial execute: %v", err)
	}
	if err := database.UpdateRunErrorStatus(run.ID, "daemon crashed during execution", types.RunInterrupted); err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, "moved-head"); err != nil {
		t.Fatalf("move head: %v", err)
	}
	run, _ = database.GetRun(run.ID)

	resumed := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := resumed.ResumeFrom(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := review.callCount(); got != 2 {
		t.Fatalf("review calls after moved-head resume = %d, want 2", got)
	}
	if got := testStep.callCount(); got != 2 {
		t.Fatalf("test calls after moved-head resume = %d, want 2", got)
	}
}

func TestExecutor_ResumeClearsRunScopedStateWhenRerunningProducerStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Agent: types.AgentClaude}

	review := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("initial execute: %v", err)
	}

	claim, err := database.InsertClaim(claims.Claim{RunID: run.ID, Step: string(types.StepTest), Text: "old claim", Kind: claims.KindBehavior, Evidence: []string{"ev-old"}})
	if err != nil {
		t.Fatalf("insert stale claim: %v", err)
	}
	if _, err := database.InsertVerifyVerdict(db.VerifyVerdict{RunID: run.ID, ClaimID: claim.ID, Verdict: claims.VerdictConfirmed}); err != nil {
		t.Fatalf("insert stale verify verdict: %v", err)
	}
	if _, err := database.InsertCoverageEntry(coverage.LedgerEntry{RunID: run.ID, File: "foo.go", StartLine: 1, EndLine: 2, State: coverage.StateRuntimeVerified}); err != nil {
		t.Fatalf("insert stale coverage entry: %v", err)
	}

	if err := database.UpdateRunErrorStatus(run.ID, types.RunInterruptReasonDaemonCrashed, types.RunInterrupted); err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if err := database.CompleteStepWithValidation(steps[0].ID, types.StepStatusCompleted, 0, 1, "", "old-head", ConfigHash(cfg)); err != nil {
		t.Fatalf("stale review validation: %v", err)
	}
	if err := database.CompleteStepWithValidation(steps[1].ID, types.StepStatusCompleted, 0, 1, "", run.HeadSHA, ConfigHash(cfg)); err != nil {
		t.Fatalf("revalidate test: %v", err)
	}
	run, _ = database.GetRun(run.ID)

	resumed := NewExecutor(database, p, cfg, nil, []Step{review, testStep}, nil)
	if err := resumed.ResumeFrom(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := review.callCount(); got != 2 {
		t.Fatalf("review calls after resume = %d, want rerun", got)
	}
	if got := testStep.callCount(); got != 2 {
		t.Fatalf("test calls after resume = %d, want rerun", got)
	}
	claims, err := database.GetClaimsByRun(run.ID)
	if err != nil {
		t.Fatalf("get claims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims after resume = %d, want cleared", len(claims))
	}
	verdicts, err := database.GetVerifyVerdictsByRun(run.ID)
	if err != nil {
		t.Fatalf("get verdicts: %v", err)
	}
	if len(verdicts) != 0 {
		t.Fatalf("verify verdicts after resume = %d, want cleared", len(verdicts))
	}
	coverageEntries, err := database.GetCoverageEntriesByRun(run.ID)
	if err != nil {
		t.Fatalf("get coverage entries: %v", err)
	}
	if len(coverageEntries) != 0 {
		t.Fatalf("coverage entries after resume = %d, want cleared", len(coverageEntries))
	}
}

func TestExecutor_SuccessfulStepsDoNotEmitTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	exec := NewExecutor(database, p, nil, nil, []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
	}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if event := recorder.find("step", "", nil); event != nil {
		t.Fatalf("successful steps should not emit step telemetry, got %v", event.fields)
	}
}

func TestExecutor_SkippedStepsDoNotEmitTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	skipStep := &mockStep{
		name:    types.StepRebase,
		outcome: &StepOutcome{ExitCode: 0, SkipRemaining: true},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{
		skipStep,
		newPassStep(types.StepReview),
	}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if event := recorder.find("step", "status", string(types.StepStatusSkipped)); event != nil {
		t.Fatalf("skipped steps should not emit step telemetry, got %v", event.fields)
	}
}

func TestExecutor_RunEventStatusCorrectOnSuccess(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// run_updated event should carry "running" status (not stale "pending")
	updatedEvent := events.findRunEvent(ipc.EventRunUpdated)
	if updatedEvent == nil {
		t.Fatal("expected run_updated event")
	}
	if updatedEvent.Status == nil || *updatedEvent.Status != string(types.RunRunning) {
		got := "<nil>"
		if updatedEvent.Status != nil {
			got = *updatedEvent.Status
		}
		t.Errorf("run_updated event: expected status %q, got %q", types.RunRunning, got)
	}

	// run_completed event should carry "completed" status (not stale "running")
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.Status == nil || *completedEvent.Status != string(types.RunCompleted) {
		got := "<nil>"
		if completedEvent.Status != nil {
			got = *completedEvent.Status
		}
		t.Errorf("run_completed event: expected status %q, got %q", types.RunCompleted, got)
	}
}

func TestExecutor_RunEventStatusCorrectOnFailure(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newFailStep(types.StepReview, fmt.Errorf("boom"))}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// run_completed event should carry "failed" status (not stale "running")
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.Status == nil || *completedEvent.Status != string(types.RunFailed) {
		got := "<nil>"
		if completedEvent.Status != nil {
			got = *completedEvent.Status
		}
		t.Errorf("run_completed event: expected status %q, got %q", types.RunFailed, got)
	}
}

func TestExecutor_StepError_FailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newPassStep(types.StepReview),
		newFailStep(types.StepTest, fmt.Errorf("tests crashed")),
		newPassStep(types.StepLint), // should not run
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Run should be failed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}

	// Second step should be failed, third should be pending
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[1].Status != types.StepStatusFailed {
		t.Errorf("step test: expected %q, got %q", types.StepStatusFailed, dbSteps[1].Status)
	}
	if dbSteps[2].Status != types.StepStatusPending {
		t.Errorf("step lint: expected %q, got %q", types.StepStatusPending, dbSteps[2].Status)
	}
}

func TestExecutor_FailedStepEmitsTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	exec := NewExecutor(database, p, nil, nil, []Step{
		newFailStep(types.StepReview, fmt.Errorf("review crashed")),
	}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err == nil {
		t.Fatal("expected error, got nil")
	}

	event := recorder.find("step", "status", string(types.StepStatusFailed))
	if event == nil {
		t.Fatal("expected failed step telemetry event")
	}
	if got := event.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("step telemetry step = %v, want %q", got, types.StepReview)
	}
}

func TestExecutor_FailedStepRecordsDuration(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newFailStep(types.StepReview, fmt.Errorf("review crashed")),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Failed step should still have duration_ms recorded.
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].DurationMS == nil {
		t.Error("expected failed step to have duration_ms recorded, got nil")
	}
}

func TestExecutor_FastCompletedStepRecordsPositiveDuration(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepVerify)}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if dbSteps[0].DurationMS == nil {
		t.Fatal("duration_ms = nil, want positive value")
	}
	if *dbSteps[0].DurationMS <= 0 {
		t.Fatalf("duration_ms = %d, want positive value", *dbSteps[0].DurationMS)
	}
}

func TestExecutor_EmptySteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, nil, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error for empty steps, got: %v", err)
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}
}

func TestExecutor_StepResultUsesDurationOverride(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &mockStep{
		name: types.StepReview,
		outcome: &StepOutcome{
			ExitCode:           0,
			DurationOverrideMS: 45000,
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	if dbSteps[0].DurationMS == nil {
		t.Fatal("expected duration_ms to be set")
	}
	if got := *dbSteps[0].DurationMS; got != 45000 {
		t.Fatalf("duration_ms = %d, want %d", got, 45000)
	}
}

func TestExecutor_StepOutcomePRURL_EmitsRunUpdated(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	prURL := "https://github.com/test/repo/pull/99"
	prStep := &mockStep{
		name:    types.StepPR,
		outcome: &StepOutcome{ExitCode: 0, PRURL: prURL},
	}
	steps := []Step{newPassStep(types.StepReview), prStep}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have a run_updated event with the PRURL after the PR step.
	found := false
	for _, e := range events.all() {
		if e.Type == ipc.EventRunUpdated && e.PRURL != nil && *e.PRURL == prURL {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a run_updated event with PRURL after PR step")
	}

	// The run_completed event should also carry the PRURL.
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.PRURL == nil || *completedEvent.PRURL != prURL {
		t.Errorf("expected run_completed PRURL %q, got %v", prURL, completedEvent.PRURL)
	}
}

func TestExecutor_SkippedOutcome_EmitsSkippedEvent(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &mockStep{
		name:    types.StepPR,
		outcome: &StepOutcome{Skipped: true},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	event := events.find(ipc.EventStepCompleted, types.StepPR)
	if event == nil {
		t.Fatal("expected step_completed event")
	}
	if event.Status == nil || *event.Status != string(types.StepStatusSkipped) {
		got := "<nil>"
		if event.Status != nil {
			got = *event.Status
		}
		t.Fatalf("expected skipped event status, got %q", got)
	}
}

func TestExecutor_ConfiguredSkippedStepDoesNotExecuteAndContinues(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	review := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, nil, nil, []Step{review, testStep}, nil)
	exec.SetSkippedSteps([]types.StepName{types.StepReview})
	events := collectEvents(exec)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := review.callCount(); got != 0 {
		t.Fatalf("skipped step executed %d times, want 0", got)
	}
	if got := testStep.callCount(); got != 1 {
		t.Fatalf("next step executed %d times, want 1", got)
	}
	if event := events.find(ipc.EventStepStarted, types.StepReview); event != nil {
		t.Fatal("configured skipped step should not emit step_started")
	}
	event := events.find(ipc.EventStepCompleted, types.StepReview)
	if event == nil || event.Status == nil || *event.Status != string(types.StepStatusSkipped) {
		t.Fatalf("expected skipped completion event, got %+v", event)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status != types.StepStatusSkipped {
			t.Fatalf("review status = %s, want %s", step.Status, types.StepStatusSkipped)
		}
	}
}
