package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestQAStep_PromptIncludesProtocolConstraints(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "qa",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"qa passed","tested":["qa-ledger"],"testing_summary":"qa cases passed"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "checkout returns a receipt"

	outcome, err := (&QAStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("qa execute: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval for empty qa findings")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one qa agent call, got %d", len(ag.calls))
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Fatal("expected qa step to request structured findings output")
	}

	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Reachability triage",
		"Machine-readable scenario/use-case ledger",
		"Execution with captured evidence",
		"Evidence-backed case results",
		"runtime-pass QA case MUST have captured evidence IDs and corresponding coverage-ledger support",
		"Code-level reasoning alone MUST NOT count as runtime pass",
		"Mark endpoint/runtime reachability as \"deterministic\" only when a command, probe, or captured run established it",
		"Mark data/account reachability and scenario semantics as \"semantic\"",
		"no-mistakes evidence exec",
		"no-mistakes evidence coverage",
		"no-mistakes claim add",
		"no-mistakes coverage add",
		"Use runtime-verified only when a captured coverage evidence entry can support that hunk",
		"Do not create a parallel datastore",
		"Do not implement or simulate green/yellow/red risk routing",
		"checkout returns a receipt",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected qa prompt to contain %q, got:\n%s", want, prompt)
		}
	}
}

func TestQAStep_BlockingFindingsNeedApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "qa",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"runtime evidence missing for checkout case","action":"ask-user"}],"summary":"qa evidence incomplete","tested":["checkout case"],"testing_summary":"case could not be runtime verified"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&QAStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("qa execute: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected ask-user qa finding to require approval")
	}
	if !outcome.AutoFixable {
		t.Fatal("expected blocking qa finding to be auto-fixable")
	}
}

func TestAllStepsIncludesQAAfterTest(t *testing.T) {
	t.Parallel()
	steps := AllSteps()
	want := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepQA,
		types.StepVerify,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	if len(steps) != len(want) {
		t.Fatalf("AllSteps returned %d steps, want %d", len(steps), len(want))
	}
	for i, step := range steps {
		if step.Name() != want[i] {
			t.Fatalf("step[%d] = %s, want %s", i, step.Name(), want[i])
		}
	}
	if _, ok := steps[4].(*QAStep); !ok {
		t.Fatalf("step[4] type = %T, want *QAStep", steps[4])
	}
}
