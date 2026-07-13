package daemon

import (
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// hasSteps reports whether got holds exactly the wanted step names, in any order.
func hasSteps(got []types.StepName, want ...types.StepName) bool {
	if len(got) != len(want) {
		return false
	}
	for _, step := range want {
		if !slices.Contains(got, step) {
			return false
		}
	}
	return true
}

func stepNames(list []pipeline.Step) []types.StepName {
	names := make([]types.StepName, 0, len(list))
	for _, step := range list {
		names = append(names, step.Name())
	}
	return names
}

// A default run must not pay for an on-demand step. The skip set the run is born
// with carries qa, so nothing downstream (execution or a later resume) can
// revive it.
func TestResolveSkipSteps_OnDemandStepsAreOffByDefault(t *testing.T) {
	got := resolveSkipSteps(nil, nil)
	if !hasSteps(got, types.StepQA) {
		t.Fatalf("default skip set = %v, want [qa]", got)
	}

	got = resolveSkipSteps([]types.StepName{types.StepLint}, nil)
	if !hasSteps(got, types.StepLint, types.StepQA) {
		t.Fatalf("--skip lint skip set = %v, want [lint qa]", got)
	}
}

// --only qa must skip every other gate step, and nothing else.
func TestResolveSkipSteps_OnlySelectsExactlyTheNamedSteps(t *testing.T) {
	got := resolveSkipSteps(nil, []types.StepName{types.StepQA})
	if slices.Contains(got, types.StepQA) {
		t.Fatalf("--only qa skipped qa itself: %v", got)
	}
	for _, step := range types.GateSteps() {
		if !slices.Contains(got, step) {
			t.Fatalf("--only qa did not skip gate step %q: %v", step, got)
		}
	}

	// review is a normal gate step and could not be run alone before --only
	// existed: skipping the other nine by hand was the only way.
	got = resolveSkipSteps(nil, []types.StepName{types.StepReview})
	if slices.Contains(got, types.StepReview) {
		t.Fatalf("--only review skipped review itself: %v", got)
	}
	if !slices.Contains(got, types.StepQA) {
		t.Fatalf("--only review must still skip the on-demand qa step: %v", got)
	}
	if len(got) != len(types.SelectableSteps())-1 {
		t.Fatalf("--only review skip set = %v, want every selectable step but review", got)
	}
}

// An on-demand step joins a run's pipeline only when the run SELECTED it, and
// the selection is read from the run's own only_steps - never inferred from the
// step's absence in the skip set.
func TestExecStepsFor_AppendsOnDemandStepOnlyWhenSelected(t *testing.T) {
	m := &RunManager{steps: func() []pipeline.Step { return steps.AllSteps() }}

	def := stepNames(m.execStepsFor(nil))
	if slices.Contains(def, types.StepQA) {
		t.Fatalf("default run's pipeline contains qa: %v", def)
	}
	if len(def) != len(types.GateSteps()) {
		t.Fatalf("default run's pipeline = %v, want the unchanged gate sequence", def)
	}

	only := stepNames(m.execStepsFor([]types.StepName{types.StepQA}))
	if !slices.Contains(only, types.StepQA) {
		t.Fatalf("--only qa run's pipeline lacks qa: %v", only)
	}
	if only[len(only)-1] != types.StepQA {
		t.Fatalf("qa must run last (it needs the PR the pr step opened): %v", only)
	}
}

// The bug this guards: qa is absent from the skip set both when a run selected
// it AND on every row written before qa existed. A run started on the old binary
// with `--skip review` has skip_steps=["review"] and no only_steps, so inferring
// selection from the skip set would append a whole QA pass - an agent session, a
// browser, a comment on the user's PR - to that run the moment a daemon restart
// recovered it or `axi resume` continued it.
func TestExecStepsFor_LegacyRunWithSkipSetNeverRevivesOnDemandStep(t *testing.T) {
	m := &RunManager{steps: func() []pipeline.Step { return steps.AllSteps() }}

	// Every shape a pre-upgrade / directly-inserted row can have: no selection.
	for _, legacy := range []*db.Run{
		{SkipSteps: nil, OnlySteps: nil},
		{SkipSteps: []types.StepName{}, OnlySteps: nil},
		{SkipSteps: []types.StepName{types.StepReview}, OnlySteps: nil},
		{SkipSteps: []types.StepName{types.StepPush, types.StepPR}, OnlySteps: nil},
	} {
		got := stepNames(m.execStepsFor(legacy.OnlySteps))
		if slices.Contains(got, types.StepQA) {
			t.Fatalf("a run with skip=%v and no recorded selection picked up qa: %v", legacy.SkipSteps, got)
		}
	}
}

// A gate run that skips both push and pr cannot touch origin or the PR, so the
// branch's watcher is still watching the right thing - and this run will not
// derive a replacement (that needs a PR its own pr step opened). Cancelling the
// watcher would leave the PR permanently unwatched. `--only qa` is that shape.
func TestRunCanChangePR(t *testing.T) {
	// Any --only selection that leaves out both push and pr is inert from the
	// PR's point of view, so the branch's watcher must survive it.
	for _, only := range [][]types.StepName{
		{types.StepQA},
		{types.StepReview},
		{types.StepQA, types.StepReview},
	} {
		skip := resolveSkipSteps(nil, only)
		if runCanChangePR(skip) {
			t.Fatalf("--only %v (skip=%v) cannot reach origin or the PR, so it must not supersede the watcher", only, skip)
		}
	}

	for _, skip := range [][]types.StepName{
		nil,
		{types.StepQA},                 // an ordinary run
		{types.StepQA, types.StepPush}, // skips push but still opens/updates the PR
		{types.StepQA, types.StepPR},   // skips pr but still pushes the branch
	} {
		if !runCanChangePR(skip) {
			t.Fatalf("a run with skip=%v can still reach origin or the PR; it must supersede the watcher", skip)
		}
	}
}
