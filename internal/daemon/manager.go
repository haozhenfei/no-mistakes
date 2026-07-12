package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

var recoveredConfigFetchTimeout = 10 * time.Second

var fetchRecoveredRemoteBranch = git.FetchRemoteBranch

// RunManager tracks active pipeline executors and manages run lifecycle.
type RunManager struct {
	mu           sync.Mutex
	executors    map[string]*pipeline.Executor      // runID → executor
	cancels      map[string]context.CancelCauseFunc // runID → cancel function with cause
	dones        map[string]chan struct{}           // runID → closed when goroutine exits
	wg           sync.WaitGroup                     // tracks background run goroutines
	shuttingDown atomic.Bool                        // prevents new runs during shutdown
	db           *db.DB
	paths        *paths.Paths
	steps        StepFactory
	// watchStepFactory overrides the watch run's steps (tests only).
	watchStepFactory StepFactory

	// watchFixRequests holds the findings a driving agent selected when it
	// answered a parked watch run with `fix`. The fix cannot happen inside the
	// watch run (it owns no worktree), so the request waits here until the watch
	// run is terminal and the manager can derive a gate run from it.
	watchFixMu       sync.Mutex
	watchFixRequests map[string]string

	branchLocks sync.Map // repoID+"/"+branch → *sync.Mutex

	subMu          sync.RWMutex
	subscribers    map[string][]chan<- ipc.Event // runID → subscriber channels
	completedRuns  map[string]bool               // runIDs whose goroutines have finished
	completedOrder []string                      // insertion order for FIFO eviction
}

// NewRunManager creates a RunManager. Pass nil for stepFactory to use default steps.
func NewRunManager(database *db.DB, p *paths.Paths, stepFactory StepFactory) *RunManager {
	if stepFactory == nil {
		stepFactory = func() []pipeline.Step { return steps.AllSteps() }
	}
	return &RunManager{
		executors:        make(map[string]*pipeline.Executor),
		cancels:          make(map[string]context.CancelCauseFunc),
		dones:            make(map[string]chan struct{}),
		db:               database,
		paths:            p,
		steps:            stepFactory,
		watchFixRequests: make(map[string]string),
		subscribers:      make(map[string][]chan<- ipc.Event),
		completedRuns:    make(map[string]bool),
	}
}

// SetWatchStepFactory overrides the steps a watch run executes. Tests use it to
// substitute a fake poller; production always uses steps.WatchSteps.
func (m *RunManager) SetWatchStepFactory(factory StepFactory) {
	m.watchStepFactory = factory
}

type recoveredRunPlan struct {
	run     *db.Run
	repo    *db.Repo
	workDir string
	gateDir string
	cfg     *config.Config
	agent   agent.Agent
	steps   []pipeline.Step
}

func (m *RunManager) recoverableParkedRuns(ctx context.Context) []recoveredRunPlan {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		slog.Error("failed to list active runs for recovery", "error", err)
		return nil
	}
	plans := make([]recoveredRunPlan, 0, len(runs))
	branchCounts := make(map[string]int, len(runs))
	for _, run := range runs {
		if run.Kind.Watch() {
			// Watch runs are not resumed from a parked gate; they are re-armed
			// from scratch (rearmWatchRuns). They also hold no worktree, so they
			// must not make a gate run on the same branch look ambiguous.
			continue
		}
		branchCounts[run.RepoID+"\x00"+run.Branch]++
	}
	for _, run := range runs {
		if run.Kind.Watch() {
			continue
		}
		if branchCounts[run.RepoID+"\x00"+run.Branch] != 1 {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", "conflicting active run for branch")
			continue
		}
		plan, err := m.prepareRecoveredRun(ctx, run)
		if err != nil {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", err)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans
}

// loadRepo reads a repo record and applies the maintainer-owned per-repo
// overrides from the global config before anyone reads it. Today that is
// repos.<working_path>.default_branch: it must be applied here, at the single
// point where the record enters the daemon, because repo.DefaultBranch drives
// the trusted-config fetch, the rebase base, every diff base, and the PR base.
// The DB row is never rewritten — see config.GlobalConfig.EffectiveDefaultBranch
// for why the override is resolved on each read.
// An unreadable global config is NOT reported here: the callers own the run
// lifecycle, and startRun/loadRecoveredConfig load the same config again and
// fail the run with a "parse global config" error that the user can see on the
// run row. Failing earlier would lose that row entirely, and the override is
// moot on a run that cannot start.
func (m *RunManager) loadRepo(id string) (*db.Repo, error) {
	repo, err := m.db.GetRepo(id)
	if err != nil || repo == nil {
		return repo, err
	}
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		slog.Warn("failed to load global config while reading repo; per-repo overrides not applied", "repo_id", id, "error", err)
		return repo, nil
	}
	repo.DefaultBranch = globalCfg.EffectiveDefaultBranch(repo.WorkingPath, repo.DefaultBranch)
	return repo, nil
}

func (m *RunManager) prepareRecoveredRun(ctx context.Context, run *db.Run) (*recoveredRunPlan, error) {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Branch == "" {
		return nil, fmt.Errorf("run is not a parked running run")
	}
	repo, err := m.loadRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("run repository is missing")
	}
	workDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("worktree is missing")
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil || headSHA != run.HeadSHA {
		return nil, fmt.Errorf("worktree head does not match run head")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree common git dir: %w", err)
	}
	if !samePath(resolveGitPath(workDir, commonDir), gateDir) {
		return nil, fmt.Errorf("worktree does not belong to its gate repository")
	}

	execSteps := m.steps()
	if err := pipeline.ValidateRecoveredRun(m.db, run, execSteps); err != nil {
		return nil, err
	}
	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	ag, err := newPipelineAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.SessionReuse {
		if err := validateRecoveredSessionProviders(m.db, run.ID, ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return &recoveredRunPlan{
		run:     run,
		repo:    repo,
		workDir: workDir,
		gateDir: gateDir,
		cfg:     cfg,
		agent:   ag,
		steps:   execSteps,
	}, nil
}

func validateRecoveredSessionProviders(database *db.DB, runID string, ag agent.Agent) error {
	sessions, err := database.GetRunAgentSessions(runID)
	if err != nil {
		return fmt.Errorf("get run sessions: %w", err)
	}
	for _, session := range sessions {
		if session.Role != string(pipeline.SessionRoleReviewer) && session.Role != string(pipeline.SessionRoleFixer) {
			return fmt.Errorf("recovered run has unknown session role %q", session.Role)
		}
		if session.Agent == "" || session.SessionID == "" {
			return fmt.Errorf("recovered run has incomplete session metadata")
		}
		if !agent.SupportsSessionProvider(ag, session.Agent) {
			return fmt.Errorf("session provider %q is no longer configured", session.Agent)
		}
	}
	return nil
}

func (m *RunManager) loadRecoveredConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) (*config.Config, error) {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if err := fetchRecoveredRemoteBranch(fetchCtx, workDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, workDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, run.ID)
	allowRepoCommands := resolveAllowRepoCommands(globalCfg, repo, trustedRepoCfg)
	return config.Merge(globalCfg, config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)), nil
}

// resolveAllowRepoCommands decides whether the pushed branch's commands/agent
// may run. Both inputs are maintainer-controlled and the pushed branch is not
// among them: the global config's per-repo override wins when configured (only
// the owner of the daemon host can write ~/.no-mistakes/config.yaml), otherwise
// the trusted default-branch copy of .no-mistakes.yaml decides.
//
// The global override exists because the default-branch-only switch was a
// deadlock for repos whose default branch is frozen: enabling pushed-branch
// commands required a commit to the very branch nobody may commit to. It is not
// a weakening — the global config is exactly as unreachable to a contributor as
// the default branch is.
func resolveAllowRepoCommands(globalCfg *config.GlobalConfig, repo *db.Repo, trusted *config.RepoConfig) bool {
	trustedAllow := trusted != nil && trusted.AllowRepoCommands
	if repo == nil {
		return trustedAllow
	}
	return globalCfg.AllowRepoCommandsFor(repo.WorkingPath, trustedAllow)
}

func newPipelineAgent(ctx context.Context, cfg *config.Config) (agent.Agent, error) {
	if steps.IsDemoMode() {
		return agent.NewNoop(), nil
	}
	if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
		return nil, err
	}
	agents := cfg.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{cfg.Agent}
	}
	created := make([]agent.Agent, 0, len(agents))
	for _, name := range agents {
		next, err := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
			ACPRegistryOverrides: cfg.ACPRegistryOverrides,
		})
		if err != nil {
			for _, existing := range created {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create agent %s: %w", name, err)
		}
		created = append(created, agent.WithSteering(next))
	}
	return agent.NewFallback(created), nil
}

func resolveGitPath(workDir, value string) string {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	return filepath.Clean(value)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func (m *RunManager) resumeRecoveredRuns(plans []recoveredRunPlan) {
	for _, plan := range plans {
		m.resumeRecoveredRun(plan)
	}
}

func (m *RunManager) resumeRecoveredRun(plan recoveredRunPlan) {
	if m.shuttingDown.Load() {
		_ = plan.agent.Close()
		return
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, plan.cfg, plan.agent, plan.steps, m.broadcast)
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[plan.run.ID] = executor
	m.cancels[plan.run.ID] = cancel
	m.dones[plan.run.ID] = done
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				errMsg := fmt.Sprintf("internal panic: %v", recovered)
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if err := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); err != nil {
					slog.Error("failed to update recovered run after panic", "run_id", plan.run.ID, "error", err)
				}
			}
			cancel(nil)
			_ = plan.agent.Close()
			m.closeSubscribers(plan.run.ID)
			if err := preserveRunWorktreeHead(context.Background(), plan.gateDir, plan.workDir, plan.run.ID, plan.run.Branch); err != nil {
				slog.Warn("failed to preserve recovered worktree head", "run_id", plan.run.ID, "path", plan.workDir, "error", err)
			}
			if err := git.WorktreeRemove(context.Background(), plan.gateDir, plan.workDir); err != nil {
				slog.Warn("failed to remove recovered worktree", "path", plan.workDir, "error", err)
			}
			m.mu.Lock()
			delete(m.executors, plan.run.ID)
			delete(m.cancels, plan.run.ID)
			delete(m.dones, plan.run.ID)
			m.mu.Unlock()

			// A recovered run that reached its PR hands it off like any other.
			m.deriveWatchRun(plan.run, plan.repo)
		}()

		if err := executor.Resume(runCtx, plan.run, plan.repo, plan.workDir); err != nil {
			if plan.run.Status == types.RunRunning {
				errMsg := err.Error()
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to mark recovered run failed", "run_id", plan.run.ID, "error", dbErr)
				}
			}
			slog.Error("recovered pipeline failed", "run_id", plan.run.ID, "error", err)
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "recovery",
			"agent":       string(plan.cfg.Agent),
			"branch_role": telemetryBranchRole(plan.run.Branch, plan.repo.DefaultBranch),
			"status":      string(plan.run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(plan.steps),
			"pr_created":  plan.run.PRURL != nil && *plan.run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, plan.run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, plan.run.ID, fields)
		telemetry.Track("run", fields)
	}()
}

func agentListsEqual(a, b []types.AgentName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HandleResume resumes a previous terminal run for the current gate branch
// head. Completed steps are reused only when their persisted validation inputs
// match the head/config for this resume attempt; otherwise execution restarts
// at the first invalidated step.
func (m *RunManager) HandleResume(ctx context.Context, repoID, branch, headSHA, oldRunID string) (*ipc.ResumeResult, error) {
	if m.shuttingDown.Load() {
		return nil, fmt.Errorf("daemon is shutting down")
	}
	repo, err := m.loadRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown repo %s", repoID)
	}
	if branch == "" {
		return nil, fmt.Errorf("branch is required")
	}

	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()

	if active, err := m.db.GetActiveRun(repo.ID, branch); err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	} else if active != nil {
		return nil, fmt.Errorf("run %s is already active for branch %s", active.ID, branch)
	}

	gateDir := m.paths.RepoDir(repo.ID)
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("resolve gate head: %w", err)
	}
	if headSHA == "" {
		headSHA = gateHead
	}
	if headSHA != gateHead {
		return nil, fmt.Errorf("branch %s gate head is %s, not requested head %s", branch, gateHead, headSHA)
	}

	run, err := m.resumeSourceRun(repo.ID, branch, headSHA, oldRunID)
	if err != nil {
		return nil, err
	}
	run.HeadSHA = headSHA

	wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if _, statErr := os.Stat(wtDir); statErr == nil {
		if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
			if removeErr := os.RemoveAll(wtDir); removeErr != nil {
				return nil, fmt.Errorf("remove stale resume worktree: %w", removeErr)
			}
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect resume worktree: %w", statErr)
	}
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		return nil, fmt.Errorf("create resume worktree: %w", err)
	}
	bgOwnsWorktree := false
	defer func() {
		if !bgOwnsWorktree {
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove resume worktree during setup cleanup", "path", wtDir, "error", rmErr)
			}
		}
	}()
	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		return nil, fmt.Errorf("configure resume worktree git identity: %w", err)
	}

	cfg, err := m.loadRecoveredConfig(ctx, run, repo, wtDir)
	if err != nil {
		return nil, err
	}
	ag, err := newPipelineAgent(ctx, cfg)
	if err != nil {
		return nil, err
	}
	execSteps := m.steps()
	// The resumed run's skip set is whatever the original run was started with.
	// It is not re-supplied by the caller (`axi resume` has no --skip flag), so
	// it comes back off the run row; without it, resume would revive skipped
	// steps and execute them for real.
	skipSteps := run.SkipSteps
	skipped := reusableResumeStepNames(m.db, run.ID, execSteps, headSHA, pipeline.ConfigHash(cfg), skipSteps)

	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetSkippedSteps(skipSteps)
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()
	m.markRunActiveForSubscribers(run.ID)
	bgOwnsWorktree = true

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in resumed pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to update resumed run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			cancel(nil)
			_ = ag.Close()
			m.closeSubscribers(run.ID)
			if err := preserveRunWorktreeHead(context.Background(), gateDir, wtDir, run.ID, run.Branch); err != nil {
				slog.Warn("failed to preserve resumed worktree head", "run_id", run.ID, "path", wtDir, "error", err)
			}
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove resume worktree", "path", wtDir, "error", rmErr)
			}
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()

			// The common reason to resume is a run that died at or before the pr
			// step. When the resumed run does reach a PR, it hands it off exactly
			// like a fresh one - otherwise the PR that resume just opened would be
			// the one PR nobody watches.
			m.deriveWatchRun(run, repo)
		}()

		if err := executor.ResumeFrom(runCtx, run, repo, wtDir); err != nil {
			slog.Error("resumed pipeline failed", "run_id", run.ID, "error", err)
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "resume",
			"agent":       string(cfg.Agent),
			"branch_role": telemetryBranchRole(run.Branch, repo.DefaultBranch),
			"status":      string(run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(execSteps),
			"pr_created":  run.PRURL != nil && *run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, run.ID, fields)
		telemetry.Track("run", fields)
	}()

	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     "resume",
		"agent":       string(cfg.Agent),
		"branch_role": telemetryBranchRole(branch, repo.DefaultBranch),
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	return &ipc.ResumeResult{RunID: run.ID, FromRunID: run.ID, Skipped: skipped}, nil
}

func (m *RunManager) resumeSourceRun(repoID, branch, headSHA, oldRunID string) (*db.Run, error) {
	if oldRunID != "" {
		run, err := m.db.GetRun(oldRunID)
		if err != nil {
			return nil, fmt.Errorf("get resume run: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("run not found: %s", oldRunID)
		}
		if run.RepoID != repoID || run.Branch != branch {
			return nil, fmt.Errorf("run %s belongs to %s/%s, not %s/%s", oldRunID, run.RepoID, run.Branch, repoID, branch)
		}
		if !pipeline.ResumableStatus(run.Status) {
			return nil, fmt.Errorf("run %s is %s, not resumable", oldRunID, run.Status)
		}
		return run, nil
	}
	runs, err := m.db.GetRunsByRepoBranch(repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("get branch runs: %w", err)
	}
	var fallback *db.Run
	for _, run := range runs {
		if !pipeline.ResumableStatus(run.Status) {
			continue
		}
		if run.HeadSHA == headSHA {
			return run, nil
		}
		if fallback == nil {
			fallback = run
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("no resumable run for branch %s", branch)
}

// reusableResumeStepNames reports the leading steps a resume will not re-execute:
// steps already completed for this head/config, plus steps the run's persisted
// skip set keeps skipped. It mirrors the executor's own prefix scan
// (pipeline.ResumeStepReusable) so the "resume: reused ..." line the CLI prints
// matches what actually happens.
func reusableResumeStepNames(database *db.DB, runID string, steps []pipeline.Step, headSHA, configHash string, skipSteps []types.StepName) []string {
	results, err := database.GetStepsByRun(runID)
	if err != nil || len(results) != len(steps) {
		return nil
	}
	skips := pipeline.SkipSet(skipSteps)
	var out []string
	for i, result := range results {
		if result.StepName != steps[i].Name() || !pipeline.ResumeStepReusable(result, headSHA, configHash, skips) {
			break
		}
		out = append(out, string(result.StepName))
	}
	return out
}

func preserveRunWorktreeHead(ctx context.Context, gateDir, workDir, runID, branch string) error {
	if branch == "" {
		return nil
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil || headSHA == "" {
		return err
	}
	backupRef := "refs/no-mistakes/runs/" + runID + "/head"
	if _, err := git.Run(ctx, gateDir, "update-ref", backupRef, headSHA); err != nil {
		return fmt.Errorf("backup run head: %w", err)
	}
	branchRef := "refs/heads/" + branch
	branchSHA, err := git.ResolveRef(ctx, gateDir, branchRef)
	if err == nil {
		if branchSHA == headSHA {
			return nil
		}
		if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", branchSHA, headSHA); err != nil {
			return fmt.Errorf("branch %s is not an ancestor of run head; preserved only at %s", branchRef, backupRef)
		}
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", branchRef, headSHA); err != nil {
		return fmt.Errorf("update branch ref: %w", err)
	}
	return nil
}

// Subscribe registers a channel to receive events for a run.
// Returns the channel and an unsubscribe function.
// If the run has already completed, the returned channel is immediately closed.
func (m *RunManager) Subscribe(runID string) (<-chan ipc.Event, func()) {
	ch := make(chan ipc.Event, 64)
	m.subMu.Lock()
	if m.completedRuns[runID] {
		m.subMu.Unlock()
		close(ch)
		return ch, func() {}
	}
	m.subscribers[runID] = append(m.subscribers[runID], ch)
	m.subMu.Unlock()

	unsub := func() {
		m.subMu.Lock()
		defer m.subMu.Unlock()
		subs := m.subscribers[runID]
		for i, s := range subs {
			if s == ch {
				m.subscribers[runID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
	return ch, unsub
}

func (m *RunManager) markRunActiveForSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	delete(m.completedRuns, runID)
	for i := 0; i < len(m.completedOrder); i++ {
		if m.completedOrder[i] == runID {
			m.completedOrder = append(m.completedOrder[:i], m.completedOrder[i+1:]...)
			i--
		}
	}
}

// broadcast sends an event to all subscribers of the event's run.
func (m *RunManager) broadcast(event ipc.Event) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()
	for _, ch := range m.subscribers[event.RunID] {
		select {
		case ch <- event:
		default:
			slog.Debug("dropped event for slow subscriber", "run_id", event.RunID, "type", event.Type)
		}
	}
}

// closeSubscribers closes all subscriber channels for a run and marks it
// as completed so future Subscribe calls return an immediately-closed channel.
func (m *RunManager) closeSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, ch := range m.subscribers[runID] {
		close(ch)
	}
	delete(m.subscribers, runID)
	m.completedRuns[runID] = true
	m.completedOrder = append(m.completedOrder, runID)
	if len(m.completedOrder) > 1000 {
		half := len(m.completedOrder) / 2
		for _, id := range m.completedOrder[:half] {
			delete(m.completedRuns, id)
		}
		m.completedOrder = m.completedOrder[half:]
	}
}

// repoIDFromGatePath extracts the repo ID from a gate bare repo path.
// Gate paths look like: <root>/repos/<id>.git
func repoIDFromGatePath(gatePath string) (string, error) {
	base := filepath.Base(gatePath)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gatePath)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

// branchFromRef extracts the branch name from a full git ref.
// "refs/heads/main" → "main", "main" → "main"
func branchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// loadTrustedRepoConfig reads .no-mistakes.yaml from the trusted
// default-branch commit (trustedSHA — the exact SHA startRun just fetched and
// resolved) in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve — every one of those failure modes returns nil
// here so the caller (EffectiveRepoConfig) fails closed: the pushed branch's
// commands and agent are dropped and the run proceeds on built-in defaults.
// None of these are fatal, since the pushed-branch copy is still read for
// non-executing fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent — the secure default — instead of falling back to a
		// potentially stale origin/<defaultBranch> ref.
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		// Path absent on the default branch is the common "repo has no
		// trusted commands" case; log at debug so it isn't noisy. Other
		// errors are surfaced at warn so a genuinely broken read isn't
		// silent. Either way trusted is nil → fail closed.
		slog.Debug("trusted repo config: not present on default branch", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	trusted, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		slog.Warn("trusted repo config: parse failed; commands/agent from pushed branch will be disabled", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	return trusted
}

// HandlePushReceived processes a push notification from the post-receive hook.
// It creates a run, sets up a worktree, and launches pipeline execution in the background.
func (m *RunManager) HandlePushReceived(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	// Ref deletion (git push remote :branch) sends new SHA as all-zeros.
	// Nothing to validate - skip pipeline.
	if git.IsZeroSHA(params.New) {
		return "", fmt.Errorf("ref deletion push, no pipeline to run")
	}

	repoID, err := repoIDFromGatePath(params.Gate)
	if err != nil {
		return "", err
	}

	repo, err := m.loadRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo for gate %s", params.Gate)
	}

	branch := branchFromRef(params.Ref)
	return m.startRun(ctx, repo, runSpec{
		branch:    branch,
		headSHA:   params.New,
		baseSHA:   params.Old,
		trigger:   "push",
		skipSteps: params.SkipSteps,
		intent:    params.Intent,
	})
}

// HandleRerun creates a new run for the latest gate head on a branch. An
// optional intent is stamped onto the new run.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch string, skipSteps []types.StepName, intent string) (string, error) {
	repo, err := m.loadRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %s", repoID)
	}

	gateDir := m.paths.RepoDir(repo.ID)
	headSHA, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve gate head: %w", err)
	}

	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get runs: %w", err)
	}

	var latestForBranch *db.Run
	var matchingHead *db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if latestForBranch == nil {
			latestForBranch = run
		}
		if run.HeadSHA == headSHA {
			matchingHead = run
			break
		}
	}
	if latestForBranch == nil {
		return "", fmt.Errorf("no previous run for branch %s", branch)
	}

	baseSHA := latestForBranch.BaseSHA
	if matchingHead != nil {
		baseSHA = matchingHead.BaseSHA
	}

	return m.startRun(ctx, repo, runSpec{
		branch:    branch,
		headSHA:   headSHA,
		baseSHA:   baseSHA,
		trigger:   "rerun",
		skipSteps: skipSteps,
		intent:    intent,
	})
}

// runSpec is everything a new gate run is born with. parentRunID is set only
// for a fix round derived from a watch run; the fix step reads its seed findings
// through it.
type runSpec struct {
	branch      string
	headSHA     string
	baseSHA     string
	trigger     string
	skipSteps   []types.StepName
	intent      string
	parentRunID string
}

// startRun creates a gate run, sets up a worktree, and launches pipeline
// execution. A non-empty intent is stamped onto the run as agent-supplied, so
// the intent step uses it instead of inferring from transcripts.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, spec runSpec) (string, error) {
	branch, headSHA, baseSHA, trigger := spec.branch, spec.headSHA, spec.baseSHA, spec.trigger
	skipSteps, intent := spec.skipSteps, spec.intent
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}

	if m.shuttingDown.Load() {
		trackStartFailure("daemon_shutdown")
		return "", fmt.Errorf("daemon is shutting down")
	}

	// Serialize per repo+branch to prevent two concurrent pushes from both
	// passing cancelActiveRuns and creating duplicate pipelines.
	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()

	// Supersede the branch's previous gate run and its watcher: this run is
	// about to move the head both of them were built on.
	m.cancelActiveRuns(repo.ID, branch, types.RunKindGate)

	// Create run record. The skip set is stored on the row so a later resume of
	// this run skips the same steps: `axi resume` carries no --skip flag, and a
	// skipped step is not `completed`, so without the persisted set resume would
	// re-execute exactly the steps the caller asked to skip.
	run, err := m.db.InsertRunWithOptions(repo.ID, branch, headSHA, baseSHA, db.RunOptions{
		Kind:        types.RunKindGate,
		ParentRunID: spec.parentRunID,
		SkipSteps:   skipSteps,
	})
	if err != nil {
		trackStartFailure("create_run")
		return "", fmt.Errorf("create run: %w", err)
	}

	// Stamp an agent-supplied intent onto the run before the pipeline starts,
	// so the intent step finds it already present and skips transcript-based
	// inference. A persist failure is non-fatal: the intent step would simply
	// fall back to inference.
	if trimmed := strings.TrimSpace(intent); trimmed != "" {
		if err := m.db.UpdateRunIntent(run.ID, db.RunIntent{Summary: trimmed, Source: "agent", Score: 1}); err != nil {
			slog.Warn("failed to persist agent-supplied intent", "run_id", run.ID, "error", err)
		} else {
			run.Intent = &trimmed
			source := "agent"
			run.IntentSource = &source
			score := 1.0
			run.IntentScore = &score
		}
	}

	// Create worktree from the gate bare repo.
	gateDir := m.paths.RepoDir(repo.ID)
	wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create worktree: %s", err))
		trackStartFailure("create_worktree")
		return "", fmt.Errorf("create worktree: %w", err)
	}
	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("configure worktree git identity: %s", err))
		trackStartFailure("configure_worktree_identity")
		return "", fmt.Errorf("configure worktree git identity: %w", err)
	}
	// Fetch the trusted default branch and resolve it to an exact commit SHA
	// before any read. Reading the trusted config at this pinned SHA (rather
	// than the origin/<defaultBranch> remote-tracking ref) is what makes a
	// fetch failure fail closed: if the fetch errors or the ref does not
	// resolve, trustedSHA stays empty, loadTrustedRepoConfig returns nil, and
	// EffectiveRepoConfig drops the pushed branch's commands/agent. Without
	// the resolve, a stale origin/<defaultBranch> left in the shared bare
	// repo by a previous run could serve a trusted copy that the live default
	// branch has already removed — silently running stale shell.
	var trustedSHA string
	if repo.DefaultBranch != "" {
		if err := git.FetchRemoteBranch(ctx, wtDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch into worktree; trusted config disabled (commands/agent from pushed branch will be dropped)", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, wtDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve fetched default-branch ref; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}

	// Track whether the background goroutine takes ownership of worktree cleanup.
	// If setup fails before the goroutine launches, we must clean up here.
	bgOwnsWorktree := false
	defer func() {
		if !bgOwnsWorktree {
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree during setup cleanup", "path", wtDir, "error", rmErr)
			}
		}
	}()

	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_global_config")
		return "", fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_repo_config")
		return "", fmt.Errorf("load repo config: %w", err)
	}
	// SECURITY: load the code-executing selection fields (commands.* and
	// agent) from the trusted default-branch copy of .no-mistakes.yaml rather
	// than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent and let any pushed SHA run arbitrary shell
	// (sh -c) or pick the launched agent (incl. acp: targets) on the daemon
	// host with the maintainer's env (GH_TOKEN, SSH agent, ...).
	// EffectiveRepoConfig replaces commands + agent with the trusted
	// default-branch values unless the maintainer has explicitly opted in.
	//
	// allow_repo_commands is read ONLY from maintainer-controlled sources —
	// the global config's per-repo override, else the trusted default-branch
	// copy (resolveAllowRepoCommands). A contributor cannot self-enable it from
	// the pushed branch. With no trusted copy (fetch failed, no default branch,
	// or no file on it) and no override, the opt-in is false and commands/agent
	// are forced empty — fail closed.
	trustedRepoCfg := loadTrustedRepoConfig(ctx, wtDir, trustedSHA, run.ID)
	allowRepoCommands := resolveAllowRepoCommands(globalCfg, repo, trustedRepoCfg)
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled by the maintainer: honoring commands/agent from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !agentListsEqual(repoCfg.Agents, effectiveRepoCfg.Agents) {
		// Surface the silent override so a maintainer who shipped a commands.*
		// or agent change on a feature branch understands why it did not run.
		// This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.Merge(globalCfg, effectiveRepoCfg)

	// Create agent. In demo mode, skip resolution and use a no-op agent.
	var ag agent.Agent
	if steps.IsDemoMode() {
		ag = agent.NewNoop()
	} else {
		if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("resolve_agent")
			return "", err
		}
		agents := cfg.Agents
		if len(agents) == 0 {
			agents = []types.AgentName{cfg.Agent}
		}
		created := make([]agent.Agent, 0, len(agents))
		for _, name := range agents {
			next, agErr := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
				ACPRegistryOverrides: cfg.ACPRegistryOverrides,
			})
			if agErr != nil {
				m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent %s: %s", name, agErr))
				trackStartFailure("create_agent")
				return "", fmt.Errorf("create agent %s: %w", name, agErr)
			}
			// Steer every pipeline agent to keep writes inside the worktree and
			// avoid mutating system state (e.g. brew/Homebrew touching
			// /Applications), which triggers macOS App Management prompts.
			created = append(created, agent.WithSteering(next))
		}
		ag = agent.NewFallback(created)
	}

	execSteps := m.steps()
	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     trigger,
		"agent":       string(cfg.Agent),
		"branch_role": branchRole,
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	// Create executor with event broadcast.
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetSkippedSteps(skipSteps)

	// Track executor.
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()

	// Background goroutine now owns worktree cleanup.
	bgOwnsWorktree = true

	// Launch pipeline in background.
	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		// handOffPR is set when the pipeline completes with a PR. The handoff
		// itself happens at the very end of the cleanup below, after the
		// worktree is released: a watch run must never coexist with the gate
		// worktree it replaced, or "the PR no longer pins a worktree" would be
		// true only eventually.
		handOffPR := false
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				fields := telemetry.Fields{
					"action":      "finished",
					"trigger":     trigger,
					"agent":       string(cfg.Agent),
					"branch_role": branchRole,
					"status":      string(run.Status),
					"duration_ms": time.Since(startedAt).Milliseconds(),
					"step_count":  len(execSteps),
					"pr_created":  run.PRURL != nil && *run.PRURL != "",
				}
				if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
					fields["failed_step"] = failedStep
				}
				addRunPerformanceSummary(m.db, run.ID, fields)
				telemetry.Track("run", fields)
				if dbErr := m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to update run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			cancel(nil)
			ag.Close()
			// Close subscriber channels for this run.
			m.closeSubscribers(run.ID)
			// Clean up worktree.
			if err := preserveRunWorktreeHead(context.Background(), gateDir, wtDir, run.ID, run.Branch); err != nil {
				slog.Warn("failed to preserve worktree head", "run_id", run.ID, "path", wtDir, "error", err)
			}
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree", "path", wtDir, "error", rmErr)
			}
			// Remove tracking.
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()

			if handOffPR {
				m.deriveWatchRun(run, repo)
			}
		}()

		if err := executor.Execute(runCtx, run, repo, wtDir); err != nil {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
				fields["failed_step"] = failedStep
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Info("pipeline completed", "run_id", run.ID)
			handOffPR = true
		}
	}()

	return run.ID, nil
}

// deriveWatchRun hands a finished gate run's PR to a watch run. It is the whole
// gate->watch handoff: the gate run is complete and its worktree is about to be
// released, and from here on the PR's state lives on the server, so what takes
// over holds nothing locally.
//
// A gate run that ended without a PR (no changes, PR step skipped, run failed)
// derives nothing - there is nothing to watch.
func (m *RunManager) deriveWatchRun(run *db.Run, repo *db.Repo) {
	if run == nil || repo == nil || run.Kind.Watch() {
		return
	}
	if run.Status != types.RunCompleted {
		return
	}
	if run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" {
		return
	}
	if m.shuttingDown.Load() {
		slog.Info("daemon shutting down; not deriving watch run", "gate_run_id", run.ID)
		return
	}
	watchID, err := m.startWatchRun(context.Background(), repo, run)
	if err != nil {
		slog.Error("failed to derive watch run for PR", "gate_run_id", run.ID, "pr_url", *run.PRURL, "error", err)
		return
	}
	slog.Info("watch run took over the PR", "gate_run_id", run.ID, "watch_run_id", watchID, "pr_url", *run.PRURL)
}

// addRunPerformanceSummary attaches the bounded per-run performance rollup
// to the terminal "run finished" event: low-cardinality counts only. The
// detailed per-invocation evidence (session keys, models, timings, tokens)
// stays in the local agent_invocations table and is never sent remotely.
func addRunPerformanceSummary(database *db.DB, runID string, fields telemetry.Fields) {
	summary, err := database.AgentInvocationSummaryForRun(runID)
	if err != nil {
		return
	}
	fields["agent_invocations"] = summary.Count
	fields["resumed_invocations"] = summary.Resumed
	fields["fallback_invocations"] = summary.Fallback
}

func telemetryBranchRole(branch, defaultBranch string) string {
	if branch == "" {
		return "unknown"
	}
	if defaultBranch != "" && branch == defaultBranch {
		return "default"
	}
	return "feature"
}

func telemetryFailedStepName(database *db.DB, runID string) string {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, step := range steps {
		if step.Status == types.StepStatusFailed {
			return string(step.StepName)
		}
	}
	return ""
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return m.HandleRespondWithOverrides(runID, step, action, findingIDs, nil, nil)
}

// HandleRespondWithOverrides is like HandleRespond but also forwards user
// instructions and user-authored findings to the executor.
func (m *RunManager) HandleRespondWithOverrides(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active executor for run %s", runID)
	}

	// A `fix` on a parked watch run cannot be executed by the watch run: it owns
	// no worktree, and a fix that bypassed review/test/lint is exactly what this
	// split removed. Record the request, let the watch step complete, and derive
	// a gate run from it - the same path a failing CI check takes.
	if step == types.StepWatch && action == types.ActionFix {
		findings, err := m.selectedWatchFindings(runID, findingIDs, instructions, addedFindings)
		if err != nil {
			return err
		}
		m.requestWatchFix(runID, findings)
		return exec.RespondWithOverrides(step, types.ActionApprove, nil, nil, nil)
	}

	return exec.RespondWithOverrides(step, action, findingIDs, instructions, addedFindings)
}

// selectedWatchFindings narrows a parked watch run's findings to the ones the
// agent selected, carrying over any instructions and user-authored findings, and
// re-serializes them as the seed for the derived fix run.
func (m *RunManager) selectedWatchFindings(runID string, findingIDs []string, instructions map[string]string, added []types.Finding) (string, error) {
	results, err := m.db.GetStepsByRun(runID)
	if err != nil {
		return "", fmt.Errorf("load watch findings: %w", err)
	}
	raw := ""
	for _, sr := range results {
		if sr.StepName == types.StepWatch && sr.FindingsJSON != nil {
			raw = *sr.FindingsJSON
			break
		}
	}
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("watch run %s has no findings to fix", runID)
	}
	return pipeline.SelectFindingsJSON(raw, findingIDs, instructions, added), nil
}

// Shutdown cancels all active runs. Called during daemon shutdown to prevent
// orphaned goroutines from continuing agent calls and git operations.
func (m *RunManager) Shutdown() {
	m.shuttingDown.Store(true)

	m.mu.Lock()
	cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels[id] = cancel
	}
	m.mu.Unlock()

	for id, cancel := range cancels {
		cancel(fmt.Errorf(types.RunInterruptReasonDaemonShuttingDown))
		slog.Info("cancelled run on shutdown", "run_id", id)
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for runs to finish during shutdown")
	}
}

// HandleCancel stops an active run and propagates cancellation to the executor.
func (m *RunManager) HandleCancel(runID string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
	return nil
}

// cancelActiveRuns cancels the in-progress runs on repo+branch that the
// incoming run of kind supersedes, and waits for their goroutines to exit.
//
// Which runs a new run supersedes depends on its kind, and getting this wrong
// is not cosmetic:
//
//   - A new GATE run supersedes both the old gate run (two pipelines must never
//     push the same branch concurrently) and any watch run on that branch: the
//     new gate run is about to move the branch head, so the PR head the watch
//     run is polling is already stale.
//
//   - A new WATCH run supersedes only other watch runs. It must NOT cancel gate
//     runs. A fix round is a gate run derived from a watch run: if a watch run
//     cancelled gate runs, deriving one would kill it, and if a gate run's own
//     derived watch run cancelled it, the gate run would be cancelled the
//     instant it succeeded. The pre-split code cancelled by branch alone, and a
//     push during CI monitoring silently destroyed the monitor - the only CI
//     step this database has ever run died exactly that way, as
//     `context canceled`, with no record that the PR still owed a check.
func (m *RunManager) cancelActiveRuns(repoID, branch string, kind types.RunKind) {
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		slog.Error("failed to query active runs for cancellation", "repo", repoID, "branch", branch, "error", err)
		return
	}

	var toWait []chan struct{}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.Status != types.RunPending && run.Status != types.RunRunning {
			continue
		}
		if !supersedes(kind, run.Kind) {
			continue
		}

		m.mu.Lock()
		cancel, ok := m.cancels[run.ID]
		done := m.dones[run.ID]
		m.mu.Unlock()
		if !ok {
			continue
		}

		cancel(fmt.Errorf(types.RunCancelReasonSuperseded))
		slog.Info("cancelled active run", "run_id", run.ID, "run_kind", string(run.Kind), "superseded_by", string(kind), "repo_id", repoID, "branch", branch)
		if done != nil {
			toWait = append(toWait, done)
		}
	}

	timeout := time.After(30 * time.Second)
	for _, done := range toWait {
		select {
		case <-done:
		case <-timeout:
			slog.Warn("timed out waiting for cancelled runs to finish")
			return
		}
	}
}

// supersedes reports whether a starting run of kind incoming takes over from an
// active run of kind existing on the same branch.
func supersedes(incoming, existing types.RunKind) bool {
	if incoming.Watch() {
		// A watch run replaces the branch's previous watcher and nothing else.
		return existing.Watch()
	}
	// A gate run moves the branch head, invalidating both the previous gate run
	// and whatever was watching the old head.
	return true
}
