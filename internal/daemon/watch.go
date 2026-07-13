package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// startWatchRun launches the PR's confirm phase: a poller over the PR a gate run
// opened, plus - when the caller selected qa - a QA pass running concurrently
// with it.
//
// The run inherits the parent gate run's PR URL, head SHA, and step selection,
// and points parent_run_id back at it. The selection is what decides the shape:
//
//   - no qa: the run is a pure poller. It adds no worktree and holds nothing
//     local, so it can be thrown away and rebuilt at any time.
//   - qa: the run also owns a worktree, because a QA pass has to boot the product
//     from real code. It still writes nothing anyone else can observe - no ref, no
//     push, no PR content beyond a comment - so it stays disposable in the way
//     that matters; and it is resumable, so a daemon restart re-uses a QA pass
//     that already finished instead of paying for it again (see rearmWatchRuns).
func (m *RunManager) startWatchRun(ctx context.Context, repo *db.Repo, parent *db.Run) (string, error) {
	if m.shuttingDown.Load() {
		return "", fmt.Errorf("daemon is shutting down")
	}
	if parent == nil || parent.PRURL == nil || strings.TrimSpace(*parent.PRURL) == "" {
		return "", fmt.Errorf("cannot watch a run with no PR")
	}

	branch := parent.Branch
	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()

	// A watch run replaces the branch's previous watcher and nothing else. It
	// must never cancel a gate run: a fix round IS a gate run derived from a
	// watch run, and a gate run derives the watch run that would then kill it.
	m.cancelActiveRuns(repo.ID, branch, types.RunKindWatch)

	// The PR URL is written WITH the row, not by a follow-up UPDATE: a watch run
	// that briefly exists with no PR URL is a watch run that looks - to a reader,
	// or to crash recovery - like it is watching nothing.
	run, err := m.db.InsertRunWithOptions(repo.ID, branch, parent.HeadSHA, parent.BaseSHA, db.RunOptions{
		Kind:        types.RunKindWatch,
		ParentRunID: parent.ID,
		PRURL:       strings.TrimSpace(*parent.PRURL),
		// The selection travels with the handoff. It is what tells this run - and
		// any later recovery of it - that it carries a QA node.
		OnlySteps: parent.OnlySteps,
	})
	if err != nil {
		return "", fmt.Errorf("create watch run: %w", err)
	}

	if err := m.launchWatchRun(ctx, repo, run, false); err != nil {
		if dbErr := m.db.UpdateRunError(run.ID, err.Error()); dbErr != nil {
			slog.Error("failed to record watch run start failure", "run_id", run.ID, "error", dbErr)
		}
		return "", err
	}
	return run.ID, nil
}

// launchWatchRun wires an executor to an already-created watch run row and runs
// it in the background. It is shared by the first handoff and by crash re-arm
// (which passes resume=true, so a QA node that already finished is reused rather
// than paid for a second time).
func (m *RunManager) launchWatchRun(ctx context.Context, repo *db.Repo, run *db.Run, resume bool) error {
	cfg := m.loadWatchConfig(ctx, repo, run)
	execSteps := m.watchSteps(run.OnlySteps)
	withQA := types.SelectsQA(run.OnlySteps)

	// Only a run with a QA node needs a worktree, and only because QA has to boot
	// the product. A pure poller still gets none: there is nothing for it to read
	// off disk.
	gateDir := m.paths.RepoDir(repo.ID)
	workDir := m.watchWorkDir(repo)
	if withQA {
		wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
		if err := ensureWatchWorktree(ctx, gateDir, wtDir, run.HeadSHA); err != nil {
			return fmt.Errorf("create watch worktree: %w", err)
		}
		if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
			_ = git.WorktreeRemove(context.Background(), gateDir, wtDir)
			return fmt.Errorf("configure watch worktree git identity: %w", err)
		}
		workDir = wtDir
	}

	// A watch run's poll node never invokes an agent, and a run with no QA node
	// therefore needs none: giving it a no-op agent means a machine with no agent
	// binary installed can still watch a PR it already opened, and means the poll
	// can never silently start editing code. A QA node does need a real agent.
	//
	// If one cannot be resolved, the QA node is dropped and the run watches the PR
	// anyway. Failing the whole run instead would answer "QA could not start" with
	// "this PR is no longer being watched" - failing CI would derive no fix round
	// and nothing would escalate - which is a far worse outcome than a missing QA
	// pass, and it is the exact failure the gate/watch split exists to remove.
	ag := agent.NewNoop()
	if withQA && !steps.IsDemoMode() {
		resolved, err := newPipelineAgent(ctx, cfg)
		if err != nil {
			slog.Error("no agent available for the QA node; watching the PR without it", "run_id", run.ID, "error", err)
			if wtErr := m.removeWatchWorktree(gateDir, repo, run); wtErr != nil {
				slog.Warn("failed to remove the unused watch worktree", "run_id", run.ID, "error", wtErr)
			}
			withQA = false
			workDir = m.watchWorkDir(repo)
			execSteps = steps.WatchSteps()
		} else {
			ag = resolved
		}
	}

	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	// The QA node and the poll node are one concurrent phase: the PR is watched
	// from the moment it opens, and the run stays alive - holding its worktree -
	// until both have converged.
	executor.SetParallelPhase(types.WatchStepsFor(run.OnlySteps))
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()
	m.markRunActiveForSubscribers(run.ID)

	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     "watch",
		"kind":        string(types.RunKindWatch),
		"branch_role": telemetryBranchRole(run.Branch, repo.DefaultBranch),
		"step_count":  len(execSteps),
		"qa":          withQA,
	})

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in watch goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to update watch run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			cancel(nil)
			_ = ag.Close()
			m.closeSubscribers(run.ID)
			// The worktree is released as it was created: a watch run makes no
			// commits, and adopting anything a QA agent left behind would move the
			// branch ref on the strength of a run that is not allowed to change code.
			if wtErr := m.removeWatchWorktree(gateDir, repo, run); wtErr != nil {
				slog.Warn("failed to remove watch worktree", "run_id", run.ID, "error", wtErr)
			}
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()
		}()

		var execErr error
		if resume {
			execErr = executor.ResumeFrom(runCtx, run, repo, workDir)
		} else {
			execErr = executor.Execute(runCtx, run, repo, workDir)
		}
		if execErr != nil {
			slog.Error("watch run failed", "run_id", run.ID, "error", execErr)
		}
		telemetry.Track("run", telemetry.Fields{
			"action":      "finished",
			"trigger":     "watch",
			"kind":        string(types.RunKindWatch),
			"branch_role": telemetryBranchRole(run.Branch, repo.DefaultBranch),
			"status":      string(run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(execSteps),
			"qa":          withQA,
		})
		// The poll's verdict is acted on whenever the poll reached one, even if the
		// run as a whole failed. The two nodes are independent: a QA pass that could
		// not boot the product must not swallow a "CI is failing" verdict the poll
		// already reached, or a failing environment would quietly cost the PR its
		// fix round. A cancelled run reaches no verdict at all, so nothing is acted
		// on there.
		outcome := executor.WatchOutcome()
		if outcome == nil || runCtx.Err() != nil {
			return
		}
		if execErr != nil {
			slog.Warn("acting on the poll's verdict even though the run failed", "run_id", run.ID, "verdict", outcome.Reason, "error", execErr)
		}
		m.resolveWatchOutcome(repo, run, outcome)
	}()
	return nil
}

// ensureWatchWorktree creates the run's worktree, replacing a stale directory
// left by a previous incarnation of the same run (a re-armed run reuses its run
// ID, and therefore its worktree path).
func ensureWatchWorktree(ctx context.Context, gateDir, wtDir, headSHA string) error {
	if _, err := os.Stat(wtDir); err == nil {
		if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
			if err := os.RemoveAll(wtDir); err != nil {
				return fmt.Errorf("remove stale watch worktree: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect watch worktree: %w", err)
	}
	return git.WorktreeAdd(ctx, gateDir, wtDir, headSHA)
}

func (m *RunManager) removeWatchWorktree(gateDir string, repo *db.Repo, run *db.Run) error {
	if !types.SelectsQA(run.OnlySteps) {
		return nil
	}
	return git.WorktreeRemove(context.Background(), gateDir, m.paths.WorktreeDir(repo.ID, run.ID))
}

// resolveWatchOutcome acts on the verdict a finished watch run reached.
//
// The only action that changes code is WatchFix, and even that changes nothing
// here: it derives a gate run, which rebases, applies the findings, and then
// re-crosses review, test, and lint before the PR sees anything. Everything
// else - unresolved threads, a blocked approval, a timeout - has already parked
// the run for the driving agent, and the daemon deliberately does nothing more.
func (m *RunManager) resolveWatchOutcome(repo *db.Repo, run *db.Run, outcome *pipeline.WatchOutcome) {
	// An agent that responded `fix` to a parked watch run wants the same fix
	// round the CI path takes automatically; honor that over the step's own
	// (escalate) verdict.
	if findings, ok := m.takeWatchFixRequest(run.ID); ok {
		m.deriveFixRun(repo, run, findings, "agent requested a fix round")
		return
	}
	if outcome == nil {
		return
	}
	switch outcome.Action {
	case pipeline.WatchFix:
		m.deriveFixRun(repo, run, outcome.FindingsJSON, outcome.Reason)
	case pipeline.WatchConverged:
		slog.Info("watch run converged", "run_id", run.ID, "reason", outcome.Reason)
	case pipeline.WatchEscalate:
		slog.Info("watch run needs a human", "run_id", run.ID, "reason", outcome.Reason, "pr_url", derefString(run.PRURL))
	}
}

// deriveFixRun starts a gate run that fixes what the watch run found. It is the
// watch->gate half of the loop: fix -> full gate -> push -> PR update -> a fresh
// watch run.
//
// maxFixRounds bounds the loop. Without it, a finding the agent cannot actually
// fix would spin forever: fix run produces a change, PR updates, the same check
// fails, a new watch run finds it, and around again. The bound is auto_fix.ci,
// the same budget the pre-split CI auto-fix spent, counted over the run's
// ancestry rather than within one step.
func (m *RunManager) deriveFixRun(repo *db.Repo, watchRun *db.Run, findingsJSON, reason string) {
	if m.shuttingDown.Load() {
		slog.Info("daemon shutting down; not deriving fix run", "watch_run_id", watchRun.ID)
		return
	}
	if strings.TrimSpace(findingsJSON) == "" {
		slog.Warn("watch run asked for a fix with no findings; not deriving a run", "watch_run_id", watchRun.ID)
		return
	}
	cfg := m.loadWatchConfig(context.Background(), repo, watchRun)
	limit := cfg.AutoFix.CI
	if limit <= 0 {
		limit = 1
	}
	if depth := m.fixRoundDepth(watchRun); depth >= limit {
		slog.Warn("fix-round budget exhausted; leaving the PR for a human",
			"watch_run_id", watchRun.ID, "rounds", depth, "limit", limit, "reason", reason)
		msg := fmt.Sprintf("%s; %d fix round(s) already attempted (limit %d) - a person needs to look at this PR", reason, depth, limit)
		if err := m.db.UpdateRunErrorStatus(watchRun.ID, msg, types.RunCompleted); err != nil {
			slog.Warn("failed to record exhausted fix budget on watch run", "run_id", watchRun.ID, "error", err)
		}
		return
	}

	ctx := context.Background()
	gateDir := m.paths.RepoDir(repo.ID)
	headSHA, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+watchRun.Branch+"^{commit}")
	if err != nil {
		slog.Error("failed to resolve gate head for fix run", "watch_run_id", watchRun.ID, "branch", watchRun.Branch, "error", err)
		return
	}

	runID, err := m.startRun(ctx, repo, runSpec{
		branch:      watchRun.Branch,
		headSHA:     headSHA,
		baseSHA:     watchRun.BaseSHA,
		trigger:     "watch_fix",
		intent:      fixRunIntent(reason, derefString(watchRun.PRURL)),
		parentRunID: watchRun.ID,
	})
	if err != nil {
		slog.Error("failed to derive fix run from watch run", "watch_run_id", watchRun.ID, "error", err)
		return
	}
	slog.Info("derived fix run from watch run", "watch_run_id", watchRun.ID, "fix_run_id", runID, "reason", reason)
}

// fixRunIntent is what the fix run's steps see as the user's intent. The
// findings themselves reach the fix step through parent_run_id; this is the
// one-line "why does this run exist" every other step needs.
func fixRunIntent(reason, prURL string) string {
	if prURL == "" {
		return fmt.Sprintf("Fix problems found on the open pull request after it was opened: %s", reason)
	}
	return fmt.Sprintf("Fix problems found on %s after it was opened: %s", prURL, reason)
}

// fixRoundDepth counts how many fix rounds already happened for this PR, by
// walking the run's ancestry. Each round contributes one gate run that carries a
// parent (the watch run that ordered it).
func (m *RunManager) fixRoundDepth(run *db.Run) int {
	depth := 0
	current := run
	// The chain is gate -> watch -> gate -> watch ...; bound the walk so a
	// cycle from a corrupt row cannot hang the daemon.
	for i := 0; i < 100; i++ {
		if current == nil || current.ParentRunID == nil || *current.ParentRunID == "" {
			return depth
		}
		parent, err := m.db.GetRun(*current.ParentRunID)
		if err != nil || parent == nil {
			return depth
		}
		if !parent.Kind.Watch() && parent.ParentRunID != nil {
			// A gate run with a parent is a fix round.
			depth++
		}
		current = parent
	}
	slog.Warn("fix-round ancestry walk hit its bound; treating the budget as exhausted", "run_id", run.ID)
	return depth + 1
}

// RequestWatchFix records that the driving agent answered a parked watch run
// with `fix`. The manager cannot fix from inside the watch run (it holds no
// worktree), so it remembers the request, lets the watch step finish, and
// derives a gate run once the watch run is terminal - the same path the CI
// verdict takes.
func (m *RunManager) requestWatchFix(runID, findingsJSON string) {
	m.watchFixMu.Lock()
	defer m.watchFixMu.Unlock()
	if m.watchFixRequests == nil {
		m.watchFixRequests = make(map[string]string)
	}
	m.watchFixRequests[runID] = findingsJSON
}

func (m *RunManager) takeWatchFixRequest(runID string) (string, bool) {
	m.watchFixMu.Lock()
	defer m.watchFixMu.Unlock()
	findings, ok := m.watchFixRequests[runID]
	delete(m.watchFixRequests, runID)
	return findings, ok
}

// resumableWatchRuns returns the watch runs that were live when the daemon died
// and can be re-armed. Startup calls this BEFORE stale-run recovery so it can
// keep these rows out of the interrupted sweep, then hands them to
// rearmWatchRuns.
func (m *RunManager) resumableWatchRuns() []*db.Run {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		slog.Error("failed to list active runs for watch re-arm", "error", err)
		return nil
	}
	var out []*db.Run
	for _, run := range runs {
		if !run.Kind.Watch() {
			continue
		}
		if run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" {
			// Nothing to poll. Let normal stale-run recovery interrupt it.
			continue
		}
		if repo, err := m.loadRepo(run.RepoID); err != nil || repo == nil {
			slog.Warn("watch run cannot be re-armed", "run_id", run.ID, "error", err)
			continue
		}
		out = append(out, run)
	}
	return out
}

// rearmWatchRuns restarts the watch runs that were live when the daemon died.
//
// A watch run that a crash turned into an `interrupted` row is a PR nobody is
// watching, which is the failure this whole split exists to remove - so the right
// recovery is not to wait for a human, it is to ask the PR again.
//
// It RESUMES rather than restarts. The poll node is a pure function of the PR, so
// re-running it costs one HTTP call and loses nothing. The QA node is not: it
// costs ~25 minutes and ~400k tokens, and it may well have finished before the
// daemon died (a laptop that sleeps, an update that restarts the daemon). Its step
// row records the head SHA and config it validated, so the ordinary resume rules
// (pipeline.ResumeStepReusable) reuse a completed QA pass at the same head and
// re-execute only the poll. Dropping the step rows - which this used to do - would
// throw that away and re-run QA on every restart.
//
// A QA node that had NOT finished (still running, or parked at its gate) is not
// reusable and does run again. That is the honest outcome: a QA pass with no
// recorded verdict verified nothing.
func (m *RunManager) rearmWatchRuns(ctx context.Context, runs []*db.Run) []string {
	var rearmed []string
	for _, run := range runs {
		repo, err := m.loadRepo(run.RepoID)
		if err != nil || repo == nil {
			slog.Warn("watch run cannot be re-armed", "run_id", run.ID, "error", err)
			continue
		}
		if err := m.db.ClearRunAwaitingAgent(run.ID); err != nil {
			slog.Warn("failed to clear parked marker before re-arming watch run", "run_id", run.ID, "error", err)
		}
		run.AwaitingAgentSince = nil
		if err := m.launchWatchRun(ctx, repo, run, true); err != nil {
			slog.Warn("watch run cannot be re-armed", "run_id", run.ID, "error", err)
			continue
		}
		rearmed = append(rearmed, run.ID)
		slog.Info("re-armed watch run after restart", "run_id", run.ID, "pr_url", derefString(run.PRURL), "qa", types.SelectsQA(run.OnlySteps))
	}
	return rearmed
}

// watchWorkDir is the working directory the watch run's provider CLIs run from.
// A watch run owns no worktree; the provider commands are read-only PR queries
// scoped by an explicit repo slug, and they run from the user's clone only
// because glab still infers its project from the git remote in cwd. When the
// clone is gone, the CLIs fall back to the daemon's own cwd (and the
// slug-carrying providers still work).
func (m *RunManager) watchWorkDir(repo *db.Repo) string {
	if repo == nil || repo.WorkingPath == "" {
		return ""
	}
	if info, err := os.Stat(repo.WorkingPath); err != nil || !info.IsDir() {
		return ""
	}
	return repo.WorkingPath
}

// loadWatchConfig builds the config a watch run reads (ci_timeout, auto_fix.ci).
// Neither is a code-executing field, so the pushed branch's copy is honored -
// but a watch run has no worktree to read it from, so the file is read straight
// out of the gate repository at the run's head.
func (m *RunManager) loadWatchConfig(ctx context.Context, repo *db.Repo, run *db.Run) *config.Config {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		slog.Warn("failed to load global config for watch run; using defaults", "run_id", run.ID, "error", err)
		globalCfg = config.DefaultGlobalConfig()
	}
	repoCfg := &config.RepoConfig{}
	gateDir := m.paths.RepoDir(repo.ID)
	if content, err := git.ShowFile(ctx, gateDir, run.HeadSHA, ".no-mistakes.yaml"); err == nil {
		if parsed, err := config.LoadRepoFromBytes([]byte(content)); err != nil {
			slog.Warn("failed to parse repo config for watch run; using global config", "run_id", run.ID, "error", err)
		} else {
			repoCfg = parsed
		}
	}
	// A watch run runs no commands and launches no agent, so the trusted-config
	// split does not apply: pass no trusted copy and no opt-in, which forces
	// every code-executing field empty.
	return config.Merge(globalCfg, config.EffectiveRepoConfig(repoCfg, nil, false))
}

// watchSteps returns the confirm phase's nodes for this selection. The test seam
// replaces the whole set, so a test can substitute a fake poller and a fake QA
// pass together.
func (m *RunManager) watchSteps(selection []types.StepName) []pipeline.Step {
	if m.watchStepFactory != nil {
		return m.watchStepFactory(selection)
	}
	return steps.WatchStepsFor(selection)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
