package types

import (
	"encoding/json"
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
		{StepName("qa"), 0},
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
