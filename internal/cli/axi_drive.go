package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// drivePollInterval is how often the drive loop re-reads run state. Short
// enough to feel responsive to an agent, long enough to avoid hammering the
// daemon during long agent steps.
const drivePollInterval = 250 * time.Millisecond

// triggerWaitTimeout bounds how long we wait for the daemon to register a run
// after pushing to the gate before falling back to a rerun.
const triggerWaitTimeout = 5 * time.Second

// terminalStatus reports whether a run has reached a final state.
func terminalStatus(status string) bool {
	switch types.RunStatus(status) {
	case types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunInterrupted:
		return true
	default:
		return false
	}
}

// outcomeFor maps a terminal run status onto an agent-facing outcome word.
func outcomeFor(status string) string {
	switch types.RunStatus(status) {
	case types.RunCompleted:
		return "passed"
	case types.RunFailed:
		return "failed"
	case types.RunCancelled:
		return "cancelled"
	case types.RunInterrupted:
		return "interrupted"
	default:
		return status
	}
}

func newAxiRunCmd() *cobra.Command {
	var autoYes bool
	var skipValue string
	var onlyValue string
	var withValue string
	var allowGateConfig bool
	var intent string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Validate your code changes, blocking until a decision point or the outcome",
		Long: "Triggers a pipeline run for the current branch and drives it. Without\n" +
			"--yes it blocks until the first approval gate, CI-ready point, or final outcome and\n" +
			"prints it. With --yes it auto-resolves every gate (fixing actionable\n" +
			"findings - including ask-user findings, with no escalation - then\n" +
			"accepting the result) until a decision point or outcome.\n\n" +
			"--intent is required when starting a new run: pass what the user set out\n" +
			"to accomplish (the goal behind the change, not a description of the diff)\n" +
			"so no-mistakes uses it directly instead of inferring it from transcripts.\n\n" +
			"--only runs exactly the named steps and skips the rest (`--only qa` runs the\n" +
			"QA pass alone against the branch's existing PR; `--only review` reviews without\n" +
			"testing or pushing). It cannot be combined with --skip.\n\n" +
			"--with adds an on-demand step to an otherwise normal run: `--with qa` runs the\n" +
			"full pipeline and then, once the PR exists, starts a product-level QA pass that\n" +
			"runs alongside the CI watcher instead of delaying it. QA is never run unless\n" +
			"named.\n\n" +
			"--allow-gate-config lets this run's agents change the gate's own config\n" +
			"(.no-mistakes.yaml). Without it, an agent that writes that file fails the run\n" +
			"with the path named: an agent must not rewrite the rules it is judged by.\n" +
			"Pass it only when changing the gate config IS the task.\n\n" +
			"The calling agent drives AXI approval gates but does not become the pipeline\n" +
			"agent. The daemon requires a supported native agent binary or a configured\n" +
			"ACP target through acpx, and fails before the first step when none can run.\n\n" +
			preserveGateFixCommitsGuidance,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-run", "/axi/run", telemetry.Fields{
				"auto_yes":   autoYes,
				"has_intent": strings.TrimSpace(intent) != "",
				"has_skip":   strings.TrimSpace(skipValue) != "",
				"has_only":   strings.TrimSpace(onlyValue) != "",
				"has_with":   strings.TrimSpace(withValue) != "",
				// The gate-config opt-in is rare and consequential; count how
				// often it is actually asked for.
				"allow_gate_config": allowGateConfig,
			}, func() error {
				selection, err := parseStepSelectionWith(skipValue, onlyValue, withValue)
				if err != nil {
					return emitError(cmd, 2, err.Error(), stepSelectionHelp())
				}
				return runAxiRun(cmd, autoYes, selection, allowGateConfig, intent)
			})
		},
	}
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "auto-resolve every gate (fix findings, then accept) until a decision point or outcome")
	cmd.Flags().StringVar(&skipValue, "skip", "", "comma-separated pipeline steps to skip")
	cmd.Flags().StringVar(&onlyValue, "only", "", "comma-separated pipeline steps to run exclusively (skips every other step; not combinable with --skip)")
	cmd.Flags().StringVar(&withValue, "with", "", "comma-separated on-demand steps to add to a normal run (qa); QA runs after the PR exists, in parallel with the CI watcher")
	cmd.Flags().BoolVar(&allowGateConfig, "allow-gate-config", false, "let this run's agents change the gate's own config (.no-mistakes.yaml); off by default, so an agent cannot rewrite the rules it is judged by")
	cmd.Flags().StringVar(&intent, "intent", "", "what the user set out to accomplish (not a description of the diff); used instead of inferring from transcripts (required to start a run)")
	return cmd
}

func newAxiResumeCmd() *cobra.Command {
	var autoYes bool
	var runID string
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume a previous run for the current branch head",
		Long: "Resumes a failed, cancelled, or interrupted run for the current branch\n" +
			"head. Completed steps are reused only when they recorded the exact same\n" +
			"head commit and effective config; the first invalidated step and every\n" +
			"later step rerun. Steps the original run skipped (--skip) stay skipped:\n" +
			"the skip set belongs to the run, so it does not need repeating here.\n\n" +
			preserveGateFixCommitsGuidance,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-resume", "/axi/resume", telemetry.Fields{
				"auto_yes": autoYes,
				"has_run":  strings.TrimSpace(runID) != "",
			}, func() error {
				return runAxiResume(cmd, strings.TrimSpace(runID), autoYes)
			})
		},
	}
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "auto-resolve every gate (fix findings, then accept) until a decision point or outcome")
	cmd.Flags().StringVar(&runID, "run", "", "resume this run id instead of the newest resumable run on the current branch")
	return cmd
}

func runAxiResume(cmd *cobra.Command, runID string, autoYes bool) error {
	ctx := cmd.Context()
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}
	if branch == "HEAD" {
		return emitError(cmd, 1, "detached HEAD: check out the branch before resuming",
			"Run `git switch <branch>` and retry")
	}
	headSHA, err := git.Run(ctx, ".", "rev-parse", "HEAD")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current HEAD: %v", err))
	}
	if guard := preflightGuard(ctx, env, branch); guard != nil {
		return guard(cmd)
	}

	var result ipc.ResumeResult
	if err := env.client.Call(ipc.MethodResume, &ipc.ResumeParams{RepoID: env.repo.ID, Branch: branch, HeadSHA: headSHA, OldRunID: runID}, &result); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("resume run: %v", err),
			"Resume only reuses completed steps for the exact same HEAD and config.",
			"Run `no-mistakes axi run --intent \"...\"` to start over intentionally.")
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "  resume: reused %s\n", strings.Join(result.Skipped, ", "))
	}
	final, ciReady, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, result.RunID, autoYes, ciLogReader(env.p))
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, final, ciReady)
}

func runAxiRun(cmd *cobra.Command, autoYes bool, selection stepSelection, allowGateConfig bool, intent string) error {
	ctx := cmd.Context()
	env, err := openAxiRunEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()

	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}
	if branch == "HEAD" {
		return emitError(cmd, 1, "detached HEAD: check out a branch before validating",
			"Run `git switch -c <branch>` to put your commits on a branch")
	}

	headSHA, err := git.Run(ctx, ".", "rev-parse", "HEAD")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current HEAD: %v", err))
	}

	// An active run for this head is normally the run to drive: `axi run` is the
	// agent's re-attach. But a command that CARRIES a step selection is a
	// different request, and driving a run that resolved to a different set of
	// steps would report that run's outcome as if the requested steps had run -
	// the `--only qa` that never ran QA. So a selection the active run does not
	// carry is never silently swallowed: it either starts its own run or refuses.
	var runID, bypassRunID string
	if active := activeRunForHead(env, branch, headSHA); active != nil {
		switch {
		case selection.empty() || runCarriesSelection(active, selection):
			runID = active.ID
		case !active.Kind.Watch():
			// A gate run. (Not `Kind.Gate()`: an older daemon still serving this
			// socket sends no kind at all, and the safe reading of "unknown" is
			// the one that refuses rather than the one that supersedes.)
			// Starting the requested run would supersede this one (see
			// cancelActiveRuns), throwing away in-flight local work - a worktree,
			// agent sessions, findings already parked at a gate. That is the
			// caller's call to make, not ours.
			return emitError(cmd, 1,
				fmt.Sprintf("run %s is already active on %q and was not started with the requested steps (%s)", active.ID, branch, describeSelection(selection)),
				fmt.Sprintf("It runs: skip=%s selection=%s. Driving it would report its outcome as if your steps had run.", formatSteps(active.SkipSteps), formatSteps(active.OnlySteps)),
				"Wait for it (`no-mistakes axi status`), or cancel it (`no-mistakes axi abort`) and re-run with the selection.")
		default:
			// A watch run owns nothing a new run would destroy: it polls the PR,
			// and the daemon's supersede rules keep the PR watched (a PR-inert
			// run leaves it alone; a `--only qa` run replaces it with a watcher
			// that carries the QA node). So the requested run starts, and this
			// one must not be re-attached to below.
			bypassRunID = active.ID
			fmt.Fprintf(cmd.ErrOrStderr(), "  selection: watch run %s does not carry %s; starting a new run for it\n", active.ID, describeSelection(selection))
		}
	}
	if runID == "" {
		if err := configErrorForFreshAxiRun(env, runID); err != nil {
			return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
		}
		// Intent is mandatory when starting a run: the agent driving this knows
		// the change's intent, so we take it directly instead of inferring it
		// from transcripts. Reattaching to an in-flight run does not need it.
		if strings.TrimSpace(intent) == "" {
			return emitError(cmd, 2, "--intent is required to start a run",
				`Pass what the user set out to accomplish: no-mistakes axi run --intent "the user's goal"`)
		}
		// Starting a fresh run: apply the same pre-flight the human wizard
		// enforces, but as structured errors the agent acts on rather than
		// silent auto-branching/auto-committing. The gate validates committed
		// history, so a wrong branch or uncommitted work would otherwise be
		// validated incorrectly or not at all.
		if guard := preflightGuard(ctx, env, branch); guard != nil {
			return guard(cmd)
		}
		var err error
		runID, err = triggerRun(ctx, env, branch, headSHA, selection, allowGateConfig, intent, bypassRunID)
		if err != nil {
			return emitError(cmd, 1, err.Error(), gatePushHelp(err)...)
		}
	}

	run, ciReady, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, runID, autoYes, ciLogReader(env.p))
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, run, ciReady)
}

func configErrorForFreshAxiRun(env *axiEnv, runID string) error {
	if runID != "" {
		return nil
	}
	return env.globalConfigErr
}

// activeRunForHead returns the non-terminal run for branch and head, or nil if none.
func activeRunForHead(env *axiEnv, branch, headSHA string) *ipc.RunInfo {
	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return nil
	}
	return activeRunInfoForHead(active.Run, headSHA)
}

// runCarriesSelection reports whether run was started with exactly the steps this
// invocation asks for. The comparison is on the RESOLVED pair the daemon persists
// (skip set + selection), through the same resolver the daemon uses, so `--only
// qa` matches a run started with `--only qa` and nothing else - and an agent that
// re-issues the identical command after a disconnect re-attaches instead of
// starting a second run.
func runCarriesSelection(run *ipc.RunInfo, selection stepSelection) bool {
	skip, named := types.ResolveRunSteps(selection.skip, selection.only, selection.with)
	return types.SameStepSet(run.SkipSteps, skip) && types.SameStepSet(run.OnlySteps, named)
}

// describeSelection renders the flags the caller passed, for the error that
// refuses to swallow them.
func describeSelection(selection stepSelection) string {
	var parts []string
	if len(selection.only) > 0 {
		parts = append(parts, "--only "+formatSteps(selection.only))
	}
	if len(selection.skip) > 0 {
		parts = append(parts, "--skip "+formatSteps(selection.skip))
	}
	if len(selection.with) > 0 {
		parts = append(parts, "--with "+formatSteps(selection.with))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func formatSteps(steps []types.StepName) string {
	if len(steps) == 0 {
		return "none"
	}
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, string(step))
	}
	return strings.Join(names, ",")
}

func activeRunInfoForHead(run *ipc.RunInfo, headSHA string) *ipc.RunInfo {
	if run == nil || terminalStatus(string(run.Status)) || run.HeadSHA != headSHA {
		return nil
	}
	return run
}

// preflightGuard returns an emitter for the first unmet pre-flight condition
// when starting a new run, or nil when the branch is ready to validate. It
// mirrors the wizard's branch/commit hygiene as detect-and-guide: refuse the
// default branch, and refuse an uncommitted working tree, each with the
// command the agent should run.
func preflightGuard(ctx context.Context, env *axiEnv, branch string) func(*cobra.Command) error {
	if env.repo.DefaultBranch != "" && branch == env.repo.DefaultBranch {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, fmt.Sprintf("refusing to validate %q: it is the default branch", branch),
				"Put your changes on a feature branch: `git switch -c <branch>`, then re-run")
		}
	}
	dirty, err := git.HasUncommittedChanges(ctx, ".")
	if err != nil {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, fmt.Sprintf("inspect working tree: %v", err),
				"Run `git status` to check the repository state, then re-run")
		}
	}
	if dirty {
		return func(cmd *cobra.Command) error {
			return emitError(cmd, 1, "uncommitted changes in the working tree",
				"Commit your work before validating: `git add -A && git commit -m \"...\"`, then re-run",
				"Run `git status` to see what is uncommitted")
		}
	}
	return nil
}

// triggerRun starts a fresh run for branch: it pushes the current HEAD through
// the gate to trigger a pipeline, and falls back to a rerun when the push was a
// no-op (the gate already had this commit). Callers must check for an existing
// active run first (see activeRunForHead) and apply pre-flight guards.
//
// bypassRunID names an active run the caller deliberately declined to drive
// (it does not carry the requested step selection). It must never be handed back
// as "the run this trigger started": the gate already has this commit, so the
// push is a no-op, and without excluding it the poll below would return that same
// run - swallowing the selection through the back door.
func triggerRun(ctx context.Context, env *axiEnv, branch, headSHA string, selection stepSelection, allowGateConfig bool, intent, bypassRunID string) (string, error) {
	pushOptions := selection.pushOptions()
	if opt := formatAllowGateConfigPushOption(allowGateConfig); opt != "" {
		pushOptions = append(pushOptions, opt)
	}
	if opt := formatIntentPushOption(intent); opt != "" {
		pushOptions = append(pushOptions, opt)
	}
	priorRunIDs, err := runIDsForHead(env.client, env.repo.ID, branch, headSHA)
	if err != nil {
		// An active run can still be found below. Without a baseline, however,
		// a matching terminal run may predate this push, so do not attach to it.
		priorRunIDs = nil
	}
	pushErr := git.PushWithOptions(ctx, ".", gate.RemoteName, "refs/heads/"+branch, "", false, pushOptions)

	if run, _ := waitForTriggeredRunForHead(ctx, env.client, env.repo.ID, branch, headSHA, priorRunIDs, bypassRunID, triggerWaitTimeout); run != nil {
		return run.ID, nil
	}
	if !shouldRerunAfterNoActiveRun(pushErr) {
		return "", fmt.Errorf("push %q to gate: %w", branch, gate.ExplainPushError(pushErr))
	}

	// No run appeared: the push was likely up-to-date. Rerun the latest gate
	// head so `axi run` is still useful when there are no new commits.
	var rr ipc.RerunResult
	if err := env.client.Call(ipc.MethodRerun, rerunParams(env.repo.ID, branch, selection, allowGateConfig, intent), &rr); err != nil {
		return "", fmt.Errorf("no run started for %q: %v", branch, err)
	}
	return rr.RunID, nil
}

// runIDsForHead snapshots the run IDs already present for a repo's exact branch
// and head SHA before a push, so waitForTriggeredRunForHead can tell a run this
// push created apart from a terminal run an earlier push left behind. Scoping to
// the head keeps this lookup, and the poll that reuses the same method, bounded
// to the handful of runs for one head rather than the repo's whole history.
func runIDsForHead(client *ipc.Client, repoID, branch, headSHA string) (map[string]struct{}, error) {
	runs, err := runsForHead(client, repoID, branch, headSHA)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		ids[run.ID] = struct{}{}
	}
	return ids, nil
}

func runsForHead(client *ipc.Client, repoID, branch, headSHA string) ([]ipc.RunInfo, error) {
	var result ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRunsForHead, &ipc.GetRunsForHeadParams{RepoID: repoID, Branch: branch, HeadSHA: headSHA}, &result); err != nil {
		return nil, err
	}
	return result.Runs, nil
}

// waitForTriggeredRunForHead waits for the run created by this trigger. The
// active-run lookup handles normal execution; the head lookup catches a run
// that fails before it can be observed as active. priorRunIDs prevents an
// up-to-date push from attaching to a terminal run created by an earlier one,
// and bypassRunID (see triggerRun) excludes the active run the caller declined.
//
// What a trigger produces is always a GATE run. The branch's newest active run
// can be a WATCH run - the gate run's own handoff derives one the moment it opens
// (or, for `--only qa`, adopts) a PR, and a short gate run wins that race often
// enough to matter. Handing that watcher back as "the run this trigger started"
// would make the drive loop follow a different run than the one the caller's
// flags created, so a watch run is never a candidate here.
func waitForTriggeredRunForHead(ctx context.Context, client *ipc.Client, repoID, branch, headSHA string, priorRunIDs map[string]struct{}, bypassRunID string, timeout time.Duration) (*ipc.RunInfo, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}, &result); err != nil {
			return nil, err
		}
		if run := activeRunInfoForHead(result.Run, headSHA); run != nil && triggerProduct(run, bypassRunID) {
			return run, nil
		}
		if priorRunIDs != nil {
			runs, err := runsForHead(client, repoID, branch, headSHA)
			if err != nil {
				return nil, err
			}
			for i := range runs {
				run := &runs[i]
				if _, existed := priorRunIDs[run.ID]; existed {
					continue
				}
				if triggerProduct(run, bypassRunID) {
					return run, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-poll.C:
		}
	}
}

// triggerProduct reports whether run could be what this trigger created: a gate
// run, and not the active run the caller declined to drive (bypassRunID). A watch
// run is never a trigger's product - it is derived by the daemon from a gate run
// that finished - so following one here would silently drive a different run than
// the caller's flags asked for.
func triggerProduct(run *ipc.RunInfo, bypassRunID string) bool {
	return run.ID != bypassRunID && !run.Kind.Watch()
}

func shouldRerunAfterNoActiveRun(pushErr error) bool {
	return pushErr == nil
}

// gatePushHelp returns the recovery steps for a failed gate push, so the agent
// gets the fix instead of git's raw refusal.
func gatePushHelp(err error) []string {
	if gate.ShallowPushRejected(err) {
		return gate.ShallowPushHelp()
	}
	return nil
}

func activeRunLookupParams(repoID, branch string) *ipc.GetActiveRunParams {
	return &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}
}

func rerunParams(repoID, branch string, selection stepSelection, allowGateConfig bool, intent string) *ipc.RerunParams {
	return &ipc.RerunParams{
		RepoID:          repoID,
		Branch:          branch,
		SkipSteps:       selection.skip,
		OnlySteps:       selection.only,
		WithSteps:       selection.with,
		AllowGateConfig: allowGateConfig,
		Intent:          intent,
	}
}

// driveRun polls a run until it reaches an approval gate, a terminal state, or
// CI checks pass, streaming step transitions to progress (stderr). When
// autoApprove is set it resolves each gate and continues; otherwise it returns
// at the first gate so the caller can surface it for a human/agent decision.
//
// Auto-resolution means "agree to fix every finding": a gate with actionable
// findings is fixed (every finding selected), and the resulting fix_review is
// accepted; gates with only non-actionable findings are approved. Each step is
// fixed at most once so a finding the fix cannot clear converges to an approval
// instead of looping forever.
//
// The CI step monitors an open PR until a human merges or closes it (a live
// status the TUI shows), so it never reaches a terminal state on its own. An
// agent driving the run must not block on that human action, so once CI checks
// pass driveRun returns with ciReady=true: the change is validated and the PR is
// ready for a human to merge. The daemon keeps monitoring in the background.
// readCILog reads the CI step's log lines for runID; it may be nil (no early
// stop) and returns nil when no log exists yet.
func driveRun(ctx context.Context, progress io.Writer, client *ipc.Client, runID string, autoApprove bool, readCILog func(string) []string) (run *ipc.RunInfo, ciReady bool, err error) {
	pp := &progressPrinter{w: progress, seen: map[string]string{}}
	fixedSteps := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		run, err := getRunInfo(client, runID)
		if err != nil {
			return nil, false, err
		}
		if run == nil {
			return nil, false, fmt.Errorf("run %s not found", runID)
		}
		pp.update(run)

		rv := runViewFromIPC(run)
		if terminalStatus(rv.Status) {
			return run, false, nil
		}
		if gate, ok := rv.awaitingStep(); ok {
			if !autoApprove {
				return run, false, nil
			}
			action, findingIDs := gateResolution(gate, fixedSteps[gate.Name])
			if action == types.ActionFix {
				fixedSteps[gate.Name] = true
			}
			if err := sendRespond(client, runID, types.StepName(gate.Name), action, findingIDs, nil, nil); err != nil {
				return nil, false, fmt.Errorf("auto-resolve %s: %w", gate.Name, err)
			}
			if err := waitStepLeavesGate(ctx, client, runID, gate.Name, gate.Status); err != nil {
				return nil, false, err
			}
			continue
		}
		// CI is green but the PR is unmerged: hand control back rather than
		// waiting on a human merge. This holds even under autoApprove, since
		// the agent cannot approve away a human's merge.
		if readCILog != nil && ciReadyToMerge(rv, readCILog(runID)) {
			return run, true, nil
		}
		if err := sleepCtx(ctx, drivePollInterval); err != nil {
			return nil, false, err
		}
	}
}

// monitorStepName returns the name of a run's PR-monitoring step: `watch` for a
// watch run, and `ci` for a run recorded before the gate/watch split. It returns
// "" for a run that monitors no PR (a gate run, which now ends at the pr step).
func monitorStepName(rv runView) string {
	for _, s := range rv.Steps {
		if s.Name == string(types.StepWatch) || s.Name == string(types.StepCI) {
			return s.Name
		}
	}
	return ""
}

// ciReadyToMerge reports whether the run's PR monitor is actively watching and
// its logs show all checks have passed, meaning the PR is ready for a human to
// merge. It reads that state through the same parser the TUI uses (see
// cimonitor) so the two surfaces never disagree about when a run is "done" from
// the agent's view.
func ciReadyToMerge(rv runView, ciLogs []string) bool {
	for _, s := range rv.Steps {
		if s.Name == string(types.StepWatch) || s.Name == string(types.StepCI) {
			return s.Status == string(types.StepStatusRunning) && cimonitor.ChecksPassed(ciLogs)
		}
	}
	return false
}

// ciLogReader returns a reader of a run's monitor-step log lines, sourced from
// the same on-disk log the daemon writes and `axi logs` reads.
func ciLogReader(p *paths.Paths) func(string) []string {
	return func(runID string) []string {
		for _, step := range []types.StepName{types.StepWatch, types.StepCI} {
			data, err := os.ReadFile(filepath.Join(p.RunLogDir(runID), string(step)+".log"))
			if err == nil {
				return splitLogLines(string(data))
			}
		}
		return nil
	}
}

// gateResolution decides how --yes answers an approval gate. A gate with
// actionable findings (anything other than purely informational "no-op") is
// fixed with every finding selected, unless this step was already fixed once -
// in which case the gate is approved so the run converges instead of looping on
// a finding the fix cannot clear. Gates with only non-actionable findings, no
// findings, or actionable findings that carry no IDs (which a fix would resolve
// to zero selections) are approved.
func gateResolution(gate stepView, alreadyFixed bool) (types.ApprovalAction, []string) {
	if alreadyFixed || gate.Status == string(types.StepStatusFixReview) {
		return types.ActionApprove, nil
	}
	parsed, err := types.ParseFindingsJSON(gate.FindingsJSON)
	if err != nil || !types.HasActionableFindings(parsed) {
		return types.ActionApprove, nil
	}
	ids := make([]string, 0, len(parsed.Items))
	for _, f := range parsed.Items {
		if f.ID != "" {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return types.ActionApprove, nil
	}
	return types.ActionFix, ids
}

// waitStepLeavesGate blocks until the named step's status changes away from the
// gate status we just answered, or the run terminates. This prevents a
// double-approve race: respond is asynchronous, so without waiting the next
// poll could still observe the same gate and approve it twice.
func waitStepLeavesGate(ctx context.Context, client *ipc.Client, runID, step, gateStatus string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		run, err := getRunInfo(client, runID)
		if err != nil {
			return err
		}
		if run == nil || terminalStatus(string(run.Status)) {
			return nil
		}
		for _, s := range run.Steps {
			if string(s.StepName) == step {
				if string(s.Status) != gateStatus {
					return nil
				}
				break
			}
		}
		if err := sleepCtx(ctx, drivePollInterval); err != nil {
			return err
		}
	}
}

func getRunInfo(client *ipc.Client, runID string) (*ipc.RunInfo, error) {
	var result ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
		return nil, err
	}
	return result.Run, nil
}

// sendRespond issues an approval action to the daemon for a step.
func sendRespond(client *ipc.Client, runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, added []types.Finding) error {
	params := &ipc.RespondParams{
		RunID:         runID,
		Step:          step,
		Action:        action,
		FindingIDs:    findingIDs,
		Instructions:  instructions,
		AddedFindings: added,
	}
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, params, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("daemon rejected the response")
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// renderDriveResult prints the run snapshot plus one of: the active gate (exit
// 0, a normal decision point), a checks-passed outcome (exit 0, CI is green and
// the PR is ready for a human to merge), or the terminal outcome (exit 0 when
// passed, exit 1 when blocked, failed, or cancelled). Successful outcomes also
// carry the fixes the pipeline applied and reporting instructions, so the agent
// closes the loop with the user instead of stopping at "it passed".
func renderDriveResult(cmd *cobra.Command, run *ipc.RunInfo, ciReady bool) error {
	rv := runViewFromIPC(run)
	fields := []toon.Field{runObjectField(rv)}

	// CI passed but the run is intentionally still monitoring for a human
	// merge. Report it as a distinct, successful outcome so the agent stops
	// and asks the user to review and merge instead of waiting.
	if ciReady {
		fields = append(fields, toon.Field{Key: "outcome", Value: "checks-passed"})
		merge := "CI checks passed - the PR is ready. Ask the user to review and merge it."
		if rv.PRURL != "" {
			merge = fmt.Sprintf("CI checks passed - the PR is ready. Ask the user to review and merge it: %s", rv.PRURL)
		}
		fixes := rv.fixRows()
		fields = appendFixesField(fields, fixes)
		help := append([]string{merge}, successReportHelp(fixes)...)
		help = append(help, staleMonitorGuidance)
		fields = append(fields, toon.Field{Key: "help", Value: help})
		emitDoc(cmd, fields...)
		return nil
	}

	if gate, ok := rv.awaitingStep(); ok {
		fields = append(fields, gateFields(gate)...)
		emitDoc(cmd, fields...)
		return nil
	}

	fields = append(fields, toon.Field{Key: "outcome", Value: outcomeFor(rv.Status)})
	if run.Error != nil && *run.Error != "" {
		fields = append(fields, toon.Field{Key: "error", Value: *run.Error})
	}

	if rv.Status == string(types.RunCompleted) {
		fixes := rv.fixRows()
		fields = appendFixesField(fields, fixes)
		var help []string
		if rv.PRURL != "" {
			help = append(help, fmt.Sprintf("Open the PR: %s", rv.PRURL))
		}
		help = append(help, successReportHelp(fixes)...)
		// A gate run that opened a PR is finished, and a watch run has taken the
		// PR over. Say so, or the agent is left guessing whether it should sit
		// and poll CI itself - which is exactly what it must not do.
		if rv.PRURL != "" && monitorStepName(rv) == "" {
			help = append(help, staleMonitorGuidance)
		}
		// The run selected qa but ran no qa step: the QA pass belongs to the
		// watch run this hands off to. Say so, or "passed" reads as "QA passed".
		if types.SelectsQA(run.OnlySteps) && monitorStepName(rv) == "" {
			help = append(help, qaHandoffGuidance)
		}
		fields = append(fields, toon.Field{Key: "help", Value: help})
		emitDoc(cmd, fields...)
		return nil
	}

	help := []string{preserveGateFixCommitsGuidance}
	if rv.PRURL != "" {
		help = append([]string{fmt.Sprintf("Open the PR: %s", rv.PRURL)}, help...)
	}
	fields = append(fields, toon.Field{Key: "help", Value: help})
	emitDoc(cmd, fields...)
	return &exitError{code: 1}
}

// appendFixesField adds a fixes table when the pipeline applied any fixes.
func appendFixesField(fields []toon.Field, fixes []fixRow) []toon.Field {
	if len(fixes) == 0 {
		return fields
	}
	return append(fields, toon.Field{Key: "fixes", Value: fixes})
}

// successReportHelp returns the reporting instructions for a successful
// outcome: always summarize the run for the user, and when the pipeline
// applied fixes, own the misses and list every fix for the user's review.
func successReportHelp(fixes []fixRow) []string {
	help := []string{"Summarize this pipeline run for the user in a concise, easily readable format: what was validated and what was found."}
	if len(fixes) > 0 {
		help = append(help, "The pipeline fixed findings the original change missed (see `fixes`) - acknowledge the misses and list each fix so the user can review them.")
	}
	help = append(help, preserveGateFixCommitsGuidance)
	return help
}

func newAxiRespondCmd() *cobra.Command {
	var action, step, findings, instructions, addFinding string
	var autoYes bool

	cmd := &cobra.Command{
		Use:   "respond",
		Short: "Answer the current approval gate and continue the run",
		Long: "Sends approve/fix/skip for the step currently awaiting approval, then\n" +
			"blocks until the next gate, CI-ready decision point, or final outcome.\n\n" +
			preserveGateFixCommitsGuidance,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-respond", "/axi/respond", telemetry.Fields{
				"action":   sanitizeAxiTelemetryAction(action),
				"auto_yes": autoYes,
			}, func() error {
				return runAxiRespond(cmd, respondArgs{
					action:       action,
					step:         step,
					findings:     findings,
					instructions: instructions,
					addFinding:   addFinding,
					autoYes:      autoYes,
				})
			})
		},
	}
	cmd.Flags().StringVar(&action, "action", "", "approve | fix | skip (required)")
	cmd.Flags().StringVar(&step, "step", "", "step to respond to (default: the step awaiting approval)")
	cmd.Flags().StringVar(&findings, "findings", "", "comma-separated finding IDs to fix (with --action fix)")
	cmd.Flags().StringVar(&instructions, "instructions", "", "guidance applied to the selected findings (with --action fix)")
	cmd.Flags().StringVar(&addFinding, "add-finding", "", "JSON finding object to add and fix (with --action fix)")
	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "auto-resolve every subsequent gate until a decision point or outcome")
	return cmd
}

type respondArgs struct {
	action       string
	step         string
	findings     string
	instructions string
	addFinding   string
	autoYes      bool
}

func runAxiRespond(cmd *cobra.Command, ra respondArgs) error {
	ctx := cmd.Context()

	act := types.ApprovalAction(strings.TrimSpace(ra.action))
	switch act {
	case types.ActionApprove, types.ActionFix, types.ActionSkip:
	case "":
		return emitError(cmd, 2, "--action is required",
			"Run `no-mistakes axi respond --action approve|fix|skip`")
	default:
		return emitError(cmd, 2, fmt.Sprintf("unknown action %q", ra.action),
			"Valid actions: approve, fix, skip")
	}

	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}

	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
	}
	if active.Run == nil {
		return emitError(cmd, 1, "no active run to respond to",
			"Run `no-mistakes axi run --intent \"...\"` to start one")
	}
	runID := active.Run.ID

	run, err := getRunInfo(env.client, runID)
	if err != nil || run == nil {
		return emitError(cmd, 1, fmt.Sprintf("load run: %v", err))
	}
	rv := runViewFromIPC(run)

	stepName := types.StepName(strings.TrimSpace(ra.step))
	if stepName == "" {
		gate, ok := rv.awaitingStep()
		if !ok {
			return emitError(cmd, 1, "no step is awaiting approval",
				"Run `no-mistakes axi status` to see the run state")
		}
		stepName = types.StepName(gate.Name)
	}

	findingIDs := splitCSV(ra.findings)
	var instructions map[string]string
	var added []types.Finding

	if act == types.ActionFix {
		if len(findingIDs) == 0 && ra.addFinding == "" {
			return emitError(cmd, 2, "--action fix requires --findings <id,...> or --add-finding <json>",
				"Run `no-mistakes axi status` to list finding IDs")
		}
		if note := strings.TrimSpace(ra.instructions); note != "" && len(findingIDs) > 0 {
			instructions = make(map[string]string, len(findingIDs))
			for _, id := range findingIDs {
				instructions[id] = note
			}
		}
		if ra.addFinding != "" {
			f, err := parseAddFinding(ra.addFinding)
			if err != nil {
				return emitError(cmd, 2, fmt.Sprintf("invalid --add-finding: %v", err),
					`Expected a JSON object, e.g. {"description":"...","action":"auto-fix"}`)
			}
			added = append(added, f)
		}
	}

	if err := sendRespond(env.client, runID, stepName, act, findingIDs, instructions, added); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("respond to %s: %v", stepName, err))
	}

	// Let the executor consume the response before we re-read state, so we
	// don't immediately observe the same gate we just answered.
	if err := waitStepLeavesGate(ctx, env.client, runID, string(stepName), gateStatusFor(rv, string(stepName))); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("wait for %s: %v", stepName, err))
	}

	final, ciReady, err := driveRun(ctx, cmd.ErrOrStderr(), env.client, runID, ra.autoYes, ciLogReader(env.p))
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, final, ciReady)
}

// gateStatusFor returns the current status of step in rv, defaulting to the
// awaiting-approval status so the post-respond wait still functions if the step
// was not found.
func gateStatusFor(rv runView, step string) string {
	for _, s := range rv.Steps {
		if s.Name == step {
			return s.Status
		}
	}
	return string(types.StepStatusAwaitingApproval)
}

func newAxiAbortCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "abort",
		Short: "Cancel the active pipeline run",
		Long: "Cancel a pipeline run. With no flags, cancels the active run on the\n" +
			"current branch. Pass --run <id> to cancel a specific run by its id from\n" +
			"anywhere - including outside its worktree - so an orphaned CI monitor\n" +
			"(e.g. after a worktree was torn down) can be reaped deterministically.\n\n" +
			"While a run is active, do NOT abort (or rerun) to go fix a finding\n" +
			"yourself - abort preserves committed pipeline fixes, but it still stops\n" +
			"in-flight work and leaves the branch needing resume or re-validation.\n" +
			"abort and rerun are for between runs (after a failed or cancelled\n" +
			"outcome), never to circumvent a gate.\n\n" +
			preserveGateFixCommitsGuidance,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-abort", "/axi/abort", nil, func() error {
				return runAxiAbort(cmd, strings.TrimSpace(runID))
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "cancel this run id directly, without resolving the current branch or worktree")
	return cmd
}

func runAxiAbort(cmd *cobra.Command, runID string) error {
	if runID != "" {
		return runAxiAbortByRunID(cmd, runID)
	}

	ctx := cmd.Context()
	env, err := openAxiDaemonEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get current branch: %v", err))
	}

	var active ipc.GetActiveRunResult
	if err := env.client.Call(ipc.MethodGetActiveRun, activeRunLookupParams(env.repo.ID, branch), &active); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("get active run: %v", err))
	}

	if active.Run == nil {
		// Idempotent: nothing to abort is a successful no-op.
		emitDoc(cmd,
			toon.Field{Key: "aborted", Value: false},
			toon.Field{Key: "detail", Value: "no active run (no-op)"},
		)
		return nil
	}

	var result ipc.CancelRunResult
	if err := env.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: active.Run.ID}, &result); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("abort run: %v", err))
	}
	emitDoc(cmd,
		toon.Field{Key: "aborted", Value: true},
		toon.Field{Key: "run", Value: active.Run.ID},
		toon.Field{Key: "branch", Value: active.Run.Branch},
	)
	return nil
}

// runAxiAbortByRunID cancels a run by its id directly via the daemon, without
// resolving a repo, branch, or worktree. This is how an orphaned monitor run -
// one whose worktree was torn down before the PR merged - gets reaped from
// outside. A run lives only in the running daemon's memory, so if the daemon is
// not running, or the id is not an active run, there is nothing to cancel and
// we report a successful no-op (the desired end state is already reached).
func runAxiAbortByRunID(cmd *cobra.Command, runID string) error {
	p, err := paths.New()
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("resolve paths: %v", err))
	}
	if err := p.EnsureDirs(); err != nil {
		return emitError(cmd, 1, fmt.Sprintf("create directories: %v", err))
	}

	if alive, _ := daemon.IsRunning(p); !alive {
		emitDoc(cmd,
			toon.Field{Key: "aborted", Value: false},
			toon.Field{Key: "run", Value: runID},
			toon.Field{Key: "detail", Value: "daemon not running, so no active run to cancel (no-op)"},
		)
		return nil
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("connect to daemon: %v", err))
	}
	defer client.Close()

	var result ipc.CancelRunResult
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: runID}, &result); err != nil {
		// The daemon reports an unknown/inactive run id as "no active run
		// <id>". Treat that as an idempotent no-op: the run is already gone.
		if strings.Contains(err.Error(), "no active run") {
			emitDoc(cmd,
				toon.Field{Key: "aborted", Value: false},
				toon.Field{Key: "run", Value: runID},
				toon.Field{Key: "detail", Value: "no active run with that id (no-op)"},
			)
			return nil
		}
		return emitError(cmd, 1, fmt.Sprintf("abort run: %v", err))
	}
	emitDoc(cmd,
		toon.Field{Key: "aborted", Value: true},
		toon.Field{Key: "run", Value: runID},
	)
	return nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
