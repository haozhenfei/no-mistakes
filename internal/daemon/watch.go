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

// startWatchRun launches a poller over the PR a gate run opened.
//
// The whole point of the kind split shows up here as what this function does
// NOT do: it adds no worktree, copies no git identity, resolves no agent
// binary. A watch run's entire state is the PR, which the SCM server owns, so
// the daemon holds nothing on its behalf and can throw the run away and rebuild
// it at any time (see rearmWatchRuns).
//
// parent is the run whose pr step produced the PR - a gate run on the first
// handoff. The watch run inherits its PR URL and head SHA and points
// parent_run_id back at it.
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

	run, err := m.db.InsertRunWithOptions(repo.ID, branch, parent.HeadSHA, parent.BaseSHA, db.RunOptions{
		Kind:        types.RunKindWatch,
		ParentRunID: parent.ID,
	})
	if err != nil {
		return "", fmt.Errorf("create watch run: %w", err)
	}
	if err := m.db.UpdateRunPRURL(run.ID, *parent.PRURL); err != nil {
		return "", fmt.Errorf("record watch run PR: %w", err)
	}
	prURL := *parent.PRURL
	run.PRURL = &prURL

	m.launchWatchRun(ctx, repo, run)
	return run.ID, nil
}

// launchWatchRun wires an executor to an already-created watch run row and runs
// it in the background. It is shared by the first handoff and by crash re-arm.
func (m *RunManager) launchWatchRun(ctx context.Context, repo *db.Repo, run *db.Run) {
	cfg := m.loadWatchConfig(ctx, repo, run)
	execSteps := m.watchSteps()

	// A watch run never invokes an agent - it reads the PR and decides. Giving
	// it a no-op agent means a machine with no agent binary installed can still
	// watch a PR it already opened, and means a watch run cannot silently start
	// editing code.
	ag := agent.NewNoop()

	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
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
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()
		}()

		execErr := executor.Execute(runCtx, run, repo, m.watchWorkDir(repo))
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
		})
		if execErr != nil {
			return
		}
		m.resolveWatchOutcome(repo, run, executor.WatchOutcome())
	}()
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
// This is the direct dividend of a watch run holding no local state: there is
// nothing to reconstruct and nothing that can be lost, so the right recovery is
// not to mark the run `interrupted` and wait for a human to resume it - it is to
// ask the PR again. A watch run that a crash turned into an `interrupted` row is
// a PR nobody is watching, which is the failure this whole split exists to
// remove.
//
// The dead run's step rows are dropped first: the re-armed run re-executes from
// nothing, and the half-written rows of the interrupted poll would otherwise sit
// alongside the fresh ones.
func (m *RunManager) rearmWatchRuns(ctx context.Context, runs []*db.Run) []string {
	var rearmed []string
	for _, run := range runs {
		repo, err := m.loadRepo(run.RepoID)
		if err != nil || repo == nil {
			slog.Warn("watch run cannot be re-armed", "run_id", run.ID, "error", err)
			continue
		}
		if err := m.db.DeleteStepsForRun(run.ID); err != nil {
			slog.Warn("watch run cannot be re-armed", "run_id", run.ID, "error", err)
			continue
		}
		if err := m.db.ClearRunAwaitingAgent(run.ID); err != nil {
			slog.Warn("failed to clear parked marker before re-arming watch run", "run_id", run.ID, "error", err)
		}
		run.AwaitingAgentSince = nil
		m.launchWatchRun(ctx, repo, run)
		rearmed = append(rearmed, run.ID)
		slog.Info("re-armed watch run after restart", "run_id", run.ID, "pr_url", derefString(run.PRURL))
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

func (m *RunManager) watchSteps() []pipeline.Step {
	if m.watchStepFactory != nil {
		return m.watchStepFactory()
	}
	return steps.WatchSteps()
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
