package steps

import (
	"encoding/json"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Finding represents a single code review or lint finding.
type Finding = types.Finding

// Findings is the structured output from a pipeline step agent call.
type Findings = types.Findings

// findingsSchema is the JSON schema for structured findings output.
var findingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		}
	},
	"required": ["findings", "summary"]
}`)

var testFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"summary": {"type": "string"},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		},
		"artifacts": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "description": "artifact type such as screenshot, gif, image, video, log, command-output, or other"},
					"label": {"type": "string"},
					"path": {"type": "string", "description": "artifact file path, including absolute paths for temporary local evidence files when available"},
					"url": {"type": "string", "description": "artifact URL when available"},
					"content": {"type": "string", "description": "short log, command output, or textual artifact content to show inline"}
				},
				"required": ["label"]
			}
		}
	},
	"required": ["findings", "summary", "tested", "testing_summary", "artifacts"]
}`)

// reviewFindingsSchema is the JSON schema for structured review output with risk assessment.
// Field order matters for chain-of-thought: findings first, then risk level, then rationale.
var reviewFindingsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"findings": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"severity": {"type": "string", "enum": ["error", "warning", "info"]},
					"file": {"type": "string"},
					"line": {"type": "integer"},
					"description": {"type": "string"},
					"action": {"type": "string", "enum": ["no-op", "auto-fix", "ask-user"]}
				},
				"required": ["severity", "description", "action"]
			}
		},
		"tested": {
			"type": "array",
			"items": {"type": "string"}
		},
		"testing_summary": {
			"type": "string"
		},
		"risk_level": {"type": "string", "enum": ["low", "medium", "high"]},
		"risk_rationale": {"type": "string"}
	},
	"required": ["findings", "risk_level", "risk_rationale"]
}`)

// AllSteps returns the gate pipeline: everything up to and including the PR.
// Post-PR monitoring is a watch run's job (see WatchSteps), so this sequence no
// longer ends in a blocking CI poll that holds the worktree for days.
// When NM_DEMO=1, it returns mock steps for demo recordings.
//
// This is the concrete-constructor twin of types.GateSteps() and must list the
// same steps in the same order. &VerifyStep{} is intentionally absent so the
// executor never runs verify by default (the executor iterates this list, not
// GateSteps) - see the reversal note on types.GateSteps. The VerifyStep type
// itself is preserved; to re-enable, add &VerifyStep{} back here and StepVerify
// to types.GateSteps() at the matching position.
func AllSteps() []pipeline.Step {
	if IsDemoMode() {
		return DemoSteps()
	}
	return []pipeline.Step{
		&IntentStep{},
		&RebaseStep{},
		&FixStep{},
		&ReviewStep{},
		&TestStep{},
		&DocumentStep{},
		&LintStep{},
		&PushStep{},
		&PRStep{},
	}
}

// WatchSteps returns the watch pipeline: one poller over the PR the gate run
// opened. It owns no worktree and no local state.
func WatchSteps() []pipeline.Step {
	return []pipeline.Step{&WatchStep{}}
}

// WatchStepsFor returns the watch run's steps for a run with this selection: the
// PR poller, plus a QA node when the caller asked for one. The two are executed
// as ONE CONCURRENT PHASE (pipeline.Executor.SetParallelPhase), so the PR is
// polled from the moment it opens rather than ~25 minutes later, and the run
// holds its worktree until both have converged.
func WatchStepsFor(selection []types.StepName) []pipeline.Step {
	if types.SelectsQA(selection) {
		return []pipeline.Step{&QAStep{}, &WatchStep{}}
	}
	return WatchSteps()
}

// StepsForKind returns the step sequence a run of the given kind executes.
func StepsForKind(kind types.RunKind) []pipeline.Step {
	if kind.Watch() {
		return WatchSteps()
	}
	return AllSteps()
}
