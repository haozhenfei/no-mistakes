package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// RunStatus represents the lifecycle state of a pipeline run.
type RunStatus string

const (
	RunPending     RunStatus = "pending"
	RunRunning     RunStatus = "running"
	RunCompleted   RunStatus = "completed"
	RunFailed      RunStatus = "failed"
	RunCancelled   RunStatus = "cancelled"
	RunInterrupted RunStatus = "interrupted"
)

const (
	RunCancelReasonAbortedByUser         = "cancelled: aborted by user"
	RunCancelReasonSuperseded            = "cancelled: superseded by new push"
	RunInterruptReasonDaemonShuttingDown = "daemon shutting down"
	RunInterruptReasonDaemonCrashed      = "daemon crashed during execution"
)

// RunKind splits the pipeline at the PR boundary, which is where state
// ownership changes hands. A gate run owns local state (worktree, git index,
// agent sessions) and terminates when the PR exists. A watch run owns nothing
// local: it is a poller over state the SCM server owns (PR head, check runs,
// review threads, approval), so it needs no worktree and can be re-armed from
// scratch after any restart.
//
// The watch run is also the "confirm phase": when the caller selected qa, it
// runs the QA pass and the CI polling as two CONCURRENT nodes and holds a
// worktree until both converge. QA has to boot the product (worktree, browser,
// dev server), so the run does own local state in that case - but it still owns
// no state that anyone else can observe: it moves no ref, pushes nothing, and
// touches the PR only by commenting. That is what keeps it disposable.
type RunKind string

const (
	RunKindGate  RunKind = "gate"
	RunKindWatch RunKind = "watch"
)

// Watch reports whether the kind is a post-PR watch run.
func (k RunKind) Watch() bool { return k == RunKindWatch }

// Gate reports whether the kind is a gate run - the only kind that moves the
// branch head, the remote, or the PR.
func (k RunKind) Gate() bool { return k == RunKindGate }

// Valid reports whether the kind is one this build knows how to execute.
func (k RunKind) Valid() bool {
	return k == RunKindGate || k == RunKindWatch
}

// StepName identifies a pipeline step.
type StepName string

const (
	StepIntent   StepName = "intent"
	StepRebase   StepName = "rebase"
	StepFix      StepName = "fix"
	StepReview   StepName = "review"
	StepTest     StepName = "test"
	StepVerify   StepName = "verify"
	StepDocument StepName = "document"
	StepLint     StepName = "lint"
	StepPush     StepName = "push"
	StepPR       StepName = "pr"

	// StepQA boots the product and exercises the PR's entry points. It is
	// on-demand: it runs only when the caller named qa (`--with qa`, or
	// `--only qa`). It is a step of the post-PR watch run, where it runs
	// CONCURRENTLY with StepWatch - see WatchStepsFor.
	StepQA StepName = "qa"

	// StepWatch polls the PR the parent gate run opened and converges on
	// merged/closed, a fix round, or an escalation to a human. It is one of the
	// two concurrent nodes of the watch run; qa is the other.
	StepWatch StepName = "watch"

	// StepCI is the pre-split blocking CI monitor. It no longer runs: watch
	// runs took over post-PR monitoring. The name is kept so historical
	// step_results rows still scan and render.
	StepCI StepName = "ci"
)

func normalizeStepName(s StepName) StepName {
	if s == "babysit" {
		return StepCI
	}
	return s
}

func (s *StepName) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = normalizeStepName(StepName(raw))
	return nil
}

func (s *StepName) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*s = normalizeStepName(StepName(v))
		return nil
	case []byte:
		*s = normalizeStepName(StepName(v))
		return nil
	case nil:
		*s = ""
		return nil
	default:
		return fmt.Errorf("scan StepName from %T", src)
	}
}

func (s StepName) Value() (driver.Value, error) {
	return string(s), nil
}

// StepOrder returns the fixed execution order for a step (1-indexed). Gate and
// watch steps are ordered within their own run kind; the two sequences never
// share a run.
func (s StepName) Order() int {
	switch s {
	case StepIntent:
		return 1
	case StepRebase:
		return 2
	case StepFix:
		return 3
	case StepReview:
		return 4
	case StepTest:
		return 5
	case StepVerify:
		return 6
	case StepDocument:
		return 7
	case StepLint:
		return 8
	case StepPush:
		return 9
	case StepPR:
		return 10
	// qa and watch are the watch run's two CONCURRENT nodes. The order between
	// them is not an execution order - it is the order a resume walks, and qa
	// comes first so a completed QA pass is reused instead of paid for twice.
	case StepQA:
		return 12
	case StepWatch:
		return 13
	case StepCI:
		return 11 // legacy; never executed
	default:
		return 0
	}
}

// GateSteps returns the gate pipeline in execution order. It terminates at the
// PR: everything after that boundary is a watch run's business.
func GateSteps() []StepName {
	return []StepName{StepIntent, StepRebase, StepFix, StepReview, StepTest, StepVerify, StepDocument, StepLint, StepPush, StepPR}
}

// WatchSteps returns the watch pipeline in execution order.
func WatchSteps() []StepName {
	return []StepName{StepWatch}
}

// WatchStepsFor returns the watch run's steps for a run with this selection.
// The two are CONCURRENT nodes, not a sequence: qa is listed first only so that
// a resume reuses a completed QA pass (the expensive one) and re-executes the
// poll (the cheap one) - see pipeline.Executor's parallel phase.
func WatchStepsFor(selection []StepName) []StepName {
	if SelectsQA(selection) {
		return []StepName{StepQA, StepWatch}
	}
	return WatchSteps()
}

// OnDemandSteps returns the steps that are OFF by default and run only when a
// caller names them (`--with qa`, or `--only qa`). They are deliberately not
// part of GateSteps: a push, a rerun, or a watch-derived fix round must never
// pay for them.
//
// Today the only member is qa. It is not a gate step: naming it adds a QA node
// to the post-PR watch run, where it runs concurrently with the CI poll, so the
// PR is watched from the moment it opens instead of ~25 minutes later. QA costs
// an agent session, an environment bootstrap, and a real browser; measured on one
// CSS-only MR that was +24min and +400k tokens for one finding, so it is
// on-demand by design.
func OnDemandSteps() []StepName {
	return []StepName{StepQA}
}

// SelectableSteps returns every step a caller may name in --only: the gate
// sequence plus the on-demand steps. Watch steps are excluded - a watch run is
// derived by the daemon, never requested step-by-step.
func SelectableSteps() []StepName {
	return append(GateSteps(), OnDemandSteps()...)
}

// SelectsQA reports whether a run's persisted selection (runs.only_steps) names
// the qa step, which is what puts a QA node in the watch run it hands off to. A
// nil or empty selection - every ordinary run, and every row written before qa
// existed - selects nothing.
func SelectsQA(selection []StepName) bool {
	for _, step := range selection {
		if step == StepQA {
			return true
		}
	}
	return false
}

// IsOnDemandStep reports whether a step only runs when explicitly selected.
func IsOnDemandStep(s StepName) bool {
	for _, step := range OnDemandSteps() {
		if step == s {
			return true
		}
	}
	return false
}

// StepsForKind returns the step sequence a run of the given kind executes.
func StepsForKind(kind RunKind) []StepName {
	if kind.Watch() {
		return WatchSteps()
	}
	return GateSteps()
}

// KnownSteps returns every step name this build recognizes, across both run
// kinds. Use it to validate user-supplied step names and to enumerate steps for
// reporting; use GateSteps/WatchStepsFor when you mean one run's sequence.
func KnownSteps() []StepName {
	return append(SelectableSteps(), WatchSteps()...)
}

// StepStatus represents the lifecycle state of a pipeline step.
type StepStatus string

const (
	StepStatusPending          StepStatus = "pending"
	StepStatusRunning          StepStatus = "running"
	StepStatusAwaitingApproval StepStatus = "awaiting_approval"
	StepStatusFixing           StepStatus = "fixing"
	StepStatusFixReview        StepStatus = "fix_review"
	StepStatusCompleted        StepStatus = "completed"
	StepStatusSkipped          StepStatus = "skipped"
	StepStatusFailed           StepStatus = "failed"
)

// ApprovalAction represents user responses at approval points.
type ApprovalAction string

const (
	ActionApprove ApprovalAction = "approve"
	ActionFix     ApprovalAction = "fix"
	ActionSkip    ApprovalAction = "skip"
	ActionAbort   ApprovalAction = "abort"
)

// AgentName identifies a supported agent backend.
// ACP agent names use dynamic acp:<target> values instead of constants.
type AgentName string

const (
	AgentAuto     AgentName = "auto"
	AgentClaude   AgentName = "claude"
	AgentCodex    AgentName = "codex"
	AgentRovoDev  AgentName = "rovodev"
	AgentOpenCode AgentName = "opencode"
	AgentPi       AgentName = "pi"
	AgentCopilot  AgentName = "copilot"
)
