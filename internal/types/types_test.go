package types

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestGateStepsOrder(t *testing.T) {
	steps := GateSteps()
	expected := []StepName{StepIntent, StepRebase, StepFix, StepReview, StepTest, StepVerify, StepDocument, StepLint, StepPush, StepPR}
	if len(steps) != len(expected) {
		t.Fatalf("expected %d gate steps, got %d", len(expected), len(steps))
	}
	for i, s := range steps {
		if s != expected[i] {
			t.Errorf("step[%d] = %q, want %q", i, s, expected[i])
		}
	}
	// The gate pipeline ends at the PR: post-PR monitoring is a watch run.
	for _, s := range steps {
		if s == StepCI || s == StepWatch {
			t.Fatalf("gate pipeline must not contain %q", s)
		}
	}
}

func TestStepsForKind(t *testing.T) {
	if got := StepsForKind(RunKindWatch); len(got) != 1 || got[0] != StepWatch {
		t.Fatalf("StepsForKind(watch) = %v, want [watch]", got)
	}
	if got := StepsForKind(RunKindGate); len(got) != len(GateSteps()) {
		t.Fatalf("StepsForKind(gate) = %v, want the gate sequence", got)
	}
	// An empty kind (a row written before the split) is a gate run.
	if got := StepsForKind(""); len(got) != len(GateSteps()) {
		t.Fatalf("StepsForKind(\"\") = %v, want the gate sequence", got)
	}
}

func TestStepNameOrder(t *testing.T) {
	tests := []struct {
		step StepName
		want int
	}{
		{StepIntent, 1},
		{StepRebase, 2},
		{StepFix, 3},
		{StepReview, 4},
		{StepTest, 5},
		{StepVerify, 6},
		{StepDocument, 7},
		{StepLint, 8},
		{StepPush, 9},
		{StepPR, 10},
		{StepWatch, 1},
		// qa runs after the PR exists, so it sorts after every gate step. The
		// name was used once before, by a pre-split step that was folded into
		// test; a historical step_results row named "qa" now sorts here, which
		// only affects how that stale row renders.
		{StepQA, 12},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestStepNameUnmarshalJSON_LegacyBabysit(t *testing.T) {
	var step StepName
	if err := json.Unmarshal([]byte(`"babysit"`), &step); err != nil {
		t.Fatalf("unmarshal step name: %v", err)
	}
	if step != StepCI {
		t.Fatalf("step = %q, want %q", step, StepCI)
	}
}

// The gate sequence must not grow an on-demand step: GateSteps is what an
// ordinary push executes, and qa is off unless a caller names it.
func TestGateStepsExcludesOnDemandSteps(t *testing.T) {
	for _, step := range GateSteps() {
		if IsOnDemandStep(step) {
			t.Fatalf("GateSteps() contains the on-demand step %q; an ordinary run would pay for it", step)
		}
	}
	if len(GateSteps()) != 10 {
		t.Fatalf("GateSteps() = %v, want the unchanged ten-step gate sequence", GateSteps())
	}
}

// SelectableSteps is what --only validates against: every step a gate run can
// execute, on-demand ones included. Watch is not selectable - a watch run is
// derived by the daemon, never requested step by step.
func TestSelectableStepsCoversGateAndOnDemandButNotWatch(t *testing.T) {
	selectable := SelectableSteps()
	for _, step := range append(GateSteps(), OnDemandSteps()...) {
		if !slices.Contains(selectable, step) {
			t.Fatalf("SelectableSteps() is missing %q", step)
		}
	}
	for _, step := range WatchSteps() {
		if slices.Contains(selectable, step) {
			t.Fatalf("SelectableSteps() contains the watch step %q", step)
		}
	}
}

// KnownSteps validates --skip, and must still accept every name a run can carry.
func TestKnownStepsIncludesOnDemandSteps(t *testing.T) {
	if !slices.Contains(KnownSteps(), StepQA) {
		t.Fatalf("KnownSteps() = %v, want it to include qa", KnownSteps())
	}
}
