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

func testManager() *RunManager {
	return &RunManager{steps: func() []pipeline.Step { return steps.AllSteps() }}
}

// A default run must not pay for an on-demand step. The skip set the run is born
// with carries qa, and it selects nothing - so nothing downstream (execution, a
// resume, or the PR handoff) can revive it.
func TestResolveRunSteps_OnDemandStepsAreOffByDefault(t *testing.T) {
	skip, selection := resolveRunSteps(nil, nil, nil)
	if !hasSteps(skip, types.StepQA) {
		t.Fatalf("default skip set = %v, want [qa]", skip)
	}
	if len(selection) != 0 {
		t.Fatalf("default run selected %v, want nothing", selection)
	}

	skip, selection = resolveRunSteps([]types.StepName{types.StepLint}, nil, nil)
	if !hasSteps(skip, types.StepLint, types.StepQA) {
		t.Fatalf("--skip lint skip set = %v, want [lint qa]", skip)
	}
	if types.SelectsQA(selection) {
		t.Fatalf("--skip lint must not select qa: %v", selection)
	}
}

// --with qa keeps the whole pipeline and adds the QA node. The run must not
// record qa as skipped: it selected it.
func TestResolveRunSteps_WithAddsTheOnDemandStepWithoutSkippingAnything(t *testing.T) {
	skip, selection := resolveRunSteps(nil, nil, []types.StepName{types.StepQA})
	if len(skip) != 0 {
		t.Fatalf("--with qa skipped %v, want nothing skipped", skip)
	}
	if !types.SelectsQA(selection) {
		t.Fatalf("--with qa did not select qa: %v", selection)
	}

	// It composes with --skip: skipping a gate step does not un-select qa.
	skip, selection = resolveRunSteps([]types.StepName{types.StepLint}, nil, []types.StepName{types.StepQA})
	if !hasSteps(skip, types.StepLint) {
		t.Fatalf("--skip lint --with qa skip set = %v, want [lint]", skip)
	}
	if !types.SelectsQA(selection) {
		t.Fatalf("--skip lint --with qa did not select qa: %v", selection)
	}
}

// --only qa must skip every gate step, and select qa so the PR handoff carries a
// QA node.
func TestResolveRunSteps_OnlySelectsExactlyTheNamedSteps(t *testing.T) {
	skip, selection := resolveRunSteps(nil, []types.StepName{types.StepQA}, nil)
	if slices.Contains(skip, types.StepQA) {
		t.Fatalf("--only qa skipped qa itself: %v", skip)
	}
	if !types.SelectsQA(selection) {
		t.Fatalf("--only qa did not select qa: %v", selection)
	}
	for _, step := range types.GateSteps() {
		if !slices.Contains(skip, step) {
			t.Fatalf("--only qa did not skip gate step %q: %v", step, skip)
		}
	}

	// review is a normal gate step and could not be run alone before --only
	// existed: skipping the other nine by hand was the only way.
	skip, selection = resolveRunSteps(nil, []types.StepName{types.StepReview}, nil)
	if slices.Contains(skip, types.StepReview) {
		t.Fatalf("--only review skipped review itself: %v", skip)
	}
	if types.SelectsQA(selection) {
		t.Fatalf("--only review must not select qa: %v", selection)
	}
	if !slices.Contains(skip, types.StepQA) {
		t.Fatalf("--only review must still skip the on-demand qa step: %v", skip)
	}
	if len(skip) != len(types.SelectableSteps())-1 {
		t.Fatalf("--only review skip set = %v, want every selectable step but review", skip)
	}
}

// qa is not a gate step, whatever the run selected: it is a node of the watch run
// the gate run hands off to. A gate run's pipeline never grows a QA pass.
func TestExecStepsFor_GateRunNeverCarriesTheOnDemandStep(t *testing.T) {
	m := testManager()

	for _, selection := range [][]types.StepName{nil, {types.StepQA}, {types.StepQA, types.StepReview}} {
		got := stepNames(m.execStepsFor(&db.Run{Kind: types.RunKindGate, OnlySteps: selection}))
		if slices.Contains(got, types.StepQA) {
			t.Fatalf("a gate run selecting %v carries qa in its pipeline: %v", selection, got)
		}
		if len(got) != len(types.GateSteps()) {
			t.Fatalf("gate pipeline = %v, want the unchanged gate sequence", got)
		}
	}
}

// The watch run is the confirm phase: it carries the QA node exactly when the run
// that handed off to it selected qa, and the two nodes are qa + watch.
func TestExecStepsFor_WatchRunCarriesTheQANodeOnlyWhenSelected(t *testing.T) {
	m := testManager()

	plain := stepNames(m.execStepsFor(&db.Run{Kind: types.RunKindWatch}))
	if !hasSteps(plain, types.StepWatch) {
		t.Fatalf("watch run without a selection = %v, want just the poller", plain)
	}

	withQA := stepNames(m.execStepsFor(&db.Run{Kind: types.RunKindWatch, OnlySteps: []types.StepName{types.StepQA}}))
	if !hasSteps(withQA, types.StepQA, types.StepWatch) {
		t.Fatalf("watch run selecting qa = %v, want both nodes", withQA)
	}
	if withQA[0] != types.StepQA {
		t.Fatalf("qa must come first so a resume reuses a finished QA pass: %v", withQA)
	}
}

// The bug this guards: qa is absent from the skip set both when a run selected
// it AND on every row written before qa existed. A run started on the old binary
// with `--skip review` has skip_steps=["review"] and no only_steps, so inferring
// selection from the skip set would bolt a whole QA pass - an agent session, a
// browser, a comment on the user's PR - onto that run the moment a daemon restart
// recovered it or `axi resume` continued it.
func TestExecStepsFor_LegacyRunWithSkipSetNeverRevivesOnDemandStep(t *testing.T) {
	m := testManager()

	// Every shape a pre-upgrade / directly-inserted row can have: no selection.
	for _, legacy := range []*db.Run{
		{Kind: types.RunKindGate, SkipSteps: nil, OnlySteps: nil},
		{Kind: types.RunKindGate, SkipSteps: []types.StepName{}, OnlySteps: nil},
		{Kind: types.RunKindGate, SkipSteps: []types.StepName{types.StepReview}, OnlySteps: nil},
		{Kind: types.RunKindWatch, SkipSteps: []types.StepName{types.StepPush, types.StepPR}, OnlySteps: nil},
	} {
		got := stepNames(m.execStepsFor(legacy))
		if slices.Contains(got, types.StepQA) {
			t.Fatalf("a run with skip=%v and no recorded selection picked up qa: %v", legacy.SkipSteps, got)
		}
	}
}

// A gate run that skips both push and pr cannot touch origin or the PR, so the
// branch's watcher is still watching the right thing. Cancelling it there would
// leave the PR unwatched for as long as the inert run takes. `--only review` is
// that shape.
func TestRunCanChangePR(t *testing.T) {
	for _, only := range [][]types.StepName{
		{types.StepQA},
		{types.StepReview},
		{types.StepQA, types.StepReview},
	} {
		skip, _ := resolveRunSteps(nil, only, nil)
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
