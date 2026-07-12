package daemon

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testPRURL = "https://github.com/test/repo/pull/7"

// mockPRStep completes the gate pipeline the way a real pr step does: it
// reports the PR it opened, which is what makes the run eligible to hand off to
// a watch run.
type mockPRStep struct {
	prURL string
}

func (s *mockPRStep) Name() types.StepName { return types.StepPR }
func (s *mockPRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	// The real pr step persists the URL on the run row itself; mirror that, or
	// the test would prove the handoff works off state production never writes.
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, s.prURL); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{PRURL: s.prURL}, nil
}

// mockWatchStep stands in for the real poller. It reports a fixed verdict and
// records the run it saw, so tests can assert the daemon's reaction to a verdict
// without a provider.
type mockWatchStep struct {
	action   pipeline.WatchAction
	findings string
	park     bool

	seen     chan *db.Run
	workDirs chan string
}

func (s *mockWatchStep) Name() types.StepName { return types.StepWatch }

func (s *mockWatchStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.seen != nil {
		select {
		case s.seen <- sctx.Run:
		default:
		}
	}
	if s.workDirs != nil {
		select {
		case s.workDirs <- sctx.WorkDir:
		default:
		}
	}
	sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{
		Action:       s.action,
		Reason:       "test verdict",
		FindingsJSON: s.findings,
	})
	return &pipeline.StepOutcome{
		NeedsApproval: s.park,
		Findings:      s.findings,
	}, nil
}

func newTestManager(t *testing.T) (*RunManager, *paths.Paths, *db.DB) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// startRun resolves a real agent binary before it launches a pipeline, so a
	// manager test needs one on disk even when its mock steps never call it.
	// Without this the tests only pass on a machine that happens to have claude
	// or codex installed.
	writeManagerGlobalConfig(t, p, "")
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewRunManager(d, p, nil), p, d
}

// writeManagerGlobalConfig writes a hermetic global config pointing the agent at
// a mock binary, plus any extra YAML the test needs.
func writeManagerGlobalConfig(t *testing.T, p *paths.Paths, extraYAML string) {
	t.Helper()
	mockClaude := writeMockClaude(t, t.TempDir())
	content := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n" + extraYAML
	if err := os.WriteFile(p.ConfigFile(), []byte(content), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
}

func waitForRunStatus(t *testing.T, d *db.DB, runID string, want types.RunStatus) *db.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run != nil && run.Status == want {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := d.GetRun(runID)
	status := types.RunStatus("<missing>")
	if run != nil {
		status = run.Status
	}
	t.Fatalf("run %s status = %s, want %s", runID, status, want)
	return nil
}

// waitForRunOfKind waits for a run of the given kind to appear on the branch,
// excluding the runs named in exclude.
func waitForRunOfKind(t *testing.T, d *db.DB, repoID string, kind types.RunKind, exclude ...string) *db.Run {
	t.Helper()
	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := d.GetRunsByRepo(repoID)
		if err != nil {
			t.Fatalf("get runs: %v", err)
		}
		for _, run := range runs {
			if run.Kind == kind && !skip[run.ID] {
				return run
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s run appeared for repo %s", kind, repoID)
	return nil
}

// TestGateRunDerivesWatchRunOnPR is the gate->watch half of the loop: a gate run
// that opens a PR hands it to a watch run and lets go of its worktree.
func TestGateRunDerivesWatchRunOnPR(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-derive")

	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}, &mockPRStep{prURL: testPRURL}}
	}
	seen := make(chan *db.Run, 1)
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchConverged, seen: seen}}
	})

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}

	gate := waitForRunStatus(t, d, gateID, types.RunCompleted)
	if gate.Kind != types.RunKindGate {
		t.Fatalf("gate run kind = %q, want gate", gate.Kind)
	}
	if gate.PRURL == nil || *gate.PRURL != testPRURL {
		t.Fatalf("gate run pr_url = %v, want %s", gate.PRURL, testPRURL)
	}

	watch := waitForRunOfKind(t, d, repo.ID, types.RunKindWatch)
	if watch.ParentRunID == nil || *watch.ParentRunID != gateID {
		t.Fatalf("watch parent = %v, want the gate run %s", watch.ParentRunID, gateID)
	}
	if watch.PRURL == nil || *watch.PRURL != testPRURL {
		t.Fatalf("watch run pr_url = %v, want %s", watch.PRURL, testPRURL)
	}

	select {
	case run := <-seen:
		if run.ID != watch.ID {
			t.Fatalf("watch step ran for %s, want %s", run.ID, watch.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch step never executed")
	}

	waitForRunStatus(t, d, watch.ID, types.RunCompleted)

	// The gate run's worktree is released the moment the PR exists. This is the
	// point of the split: a PR must not pin a worktree for the days a review
	// might take.
	wtDir := p.WorktreeDir(repo.ID, gateID)
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("gate worktree %s still exists after the run completed (err=%v)", wtDir, err)
	}
	// And a watch run never made one.
	watchWT := p.WorktreeDir(repo.ID, watch.ID)
	if _, err := os.Stat(watchWT); !os.IsNotExist(err) {
		t.Fatalf("watch run created a worktree at %s; it must hold no local state", watchWT)
	}
}

// TestGateRunWithoutPRDerivesNoWatchRun: there is nothing to watch.
func TestGateRunWithoutPRDerivesNoWatchRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-nopr")

	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}}
	}
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchConverged}}
	})

	gateID, err := mgr.startRun(context.Background(), repo, runSpec{
		branch: "main", headSHA: headSHA, baseSHA: headSHA, trigger: "push",
	})
	if err != nil {
		t.Fatalf("start gate run: %v", err)
	}
	waitForRunStatus(t, d, gateID, types.RunCompleted)

	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	for _, run := range runs {
		if run.Kind.Watch() {
			t.Fatalf("run %s watches a PR that was never opened", run.ID)
		}
	}
}

// TestWatchRunDerivesFixGateRun is the watch->gate half: a watch run that finds
// failing CI derives a gate run rather than patching the branch itself, and the
// derived run carries the findings through parent_run_id.
func TestWatchRunDerivesFixGateRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-fix")

	findings := `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchFix, findings: findings}}
	})
	fixRan := make(chan *db.Run, 1)
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&recordingStep{name: types.StepFix, seen: fixRan}}
	}

	gate, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert gate run: %v", err)
	}
	if err := d.UpdateRunPRURL(gate.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	gate, _ = d.GetRun(gate.ID)

	watchID, err := mgr.startWatchRun(context.Background(), repo, gate)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}
	waitForRunStatus(t, d, watchID, types.RunCompleted)

	fixRun := waitForRunOfKind(t, d, repo.ID, types.RunKindGate, gate.ID)
	if fixRun.ParentRunID == nil || *fixRun.ParentRunID != watchID {
		t.Fatalf("fix run parent = %v, want the watch run %s", fixRun.ParentRunID, watchID)
	}
	if fixRun.Intent == nil || *fixRun.Intent == "" {
		t.Fatal("fix run carries no intent explaining why it exists")
	}

	select {
	case run := <-fixRan:
		if run.ID != fixRun.ID {
			t.Fatalf("fix step ran for %s, want %s", run.ID, fixRun.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the derived gate run never executed")
	}

	// The seed is reachable from the fix run: this is the durable link the fix
	// step reads, not an in-memory hand-off.
	steps, err := d.GetStepsByRun(watchID)
	if err != nil {
		t.Fatalf("get watch steps: %v", err)
	}
	var seed string
	for _, sr := range steps {
		if sr.StepName == types.StepWatch && sr.FindingsJSON != nil {
			seed = *sr.FindingsJSON
		}
	}
	if seed == "" {
		t.Fatal("watch run persisted no findings for the fix run to read")
	}
}

// TestWatchRunFixRoundBudgetIsBounded: a finding the agent cannot actually fix
// must not spin the loop forever.
func TestWatchRunFixRoundBudgetIsBounded(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-budget")
	writeGlobalAutoFixCI(t, p, 2)

	// Build an ancestry that already spent the budget: gate -> watch -> gate.
	root, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert root gate: %v", err)
	}
	w1, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindWatch, ParentRunID: root.ID,
	})
	if err != nil {
		t.Fatalf("insert watch 1: %v", err)
	}
	fix1, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindGate, ParentRunID: w1.ID,
	})
	if err != nil {
		t.Fatalf("insert fix 1: %v", err)
	}
	fix2, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindGate, ParentRunID: fix1.ID,
	})
	if err != nil {
		t.Fatalf("insert fix 2: %v", err)
	}
	w2, err := d.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{
		Kind: types.RunKindWatch, ParentRunID: fix2.ID,
	})
	if err != nil {
		t.Fatalf("insert watch 2: %v", err)
	}
	prURL := testPRURL
	w2.PRURL = &prURL

	if depth := mgr.fixRoundDepth(w2); depth != 2 {
		t.Fatalf("fixRoundDepth = %d, want 2 (two fix rounds in the ancestry)", depth)
	}

	before, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	mgr.deriveFixRun(repo, w2, `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"still failing"}`, "CI failing")
	after, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("derived a fix run past the budget: runs went %d -> %d", len(before), len(after))
	}

	exhausted, err := d.GetRun(w2.ID)
	if err != nil || exhausted == nil {
		t.Fatalf("get watch run: %v", err)
	}
	if exhausted.Error == nil || *exhausted.Error == "" {
		t.Fatal("an exhausted fix budget must be recorded on the watch run, not swallowed")
	}
}

// recordingStep completes and records the run it executed for.
type recordingStep struct {
	name types.StepName
	seen chan *db.Run
}

func (s *recordingStep) Name() types.StepName { return s.name }
func (s *recordingStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.seen != nil {
		select {
		case s.seen <- sctx.Run:
		default:
		}
	}
	return &pipeline.StepOutcome{}, nil
}

// writeGlobalAutoFixCI sets the CI fix-round budget, keeping the agent override
// the manager needs to start a run.
func writeGlobalAutoFixCI(t *testing.T, p *paths.Paths, limit int) {
	t.Helper()
	writeManagerGlobalConfig(t, p, fmt.Sprintf("auto_fix:\n  ci: %d\n", limit))
}

// TestParkedWatchRunFixResponseDerivesGateRun covers the conservative path end
// to end: an unresolved comment thread parks the watch run instead of rewriting
// anyone's code, and only when the driving agent answers `fix` does a gate run
// get derived - carrying exactly the findings the agent selected.
func TestParkedWatchRunFixResponseDerivesGateRun(t *testing.T) {
	mgr, p, d := newTestManager(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "repo-parked-fix")

	findings := `{"findings":[` +
		`{"id":"F1","severity":"warning","description":"Unresolved review thread from alice: this drops the error","action":"ask-user"},` +
		`{"id":"F2","severity":"warning","description":"Unresolved review thread from bot: nit","action":"ask-user"}` +
		`],"summary":"2 unresolved review threads"}`
	mgr.SetWatchStepFactory(func() []pipeline.Step {
		return []pipeline.Step{&mockWatchStep{action: pipeline.WatchEscalate, findings: findings, park: true}}
	})
	fixRan := make(chan *db.Run, 1)
	mgr.steps = func() []pipeline.Step {
		return []pipeline.Step{&recordingStep{name: types.StepFix, seen: fixRan}}
	}

	gate, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatalf("insert gate run: %v", err)
	}
	if err := d.UpdateRunPRURL(gate.ID, testPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	gate, _ = d.GetRun(gate.ID)

	watchID, err := mgr.startWatchRun(context.Background(), repo, gate)
	if err != nil {
		t.Fatalf("start watch run: %v", err)
	}

	// The run parks: an unresolved thread may be a person's opinion, so nothing
	// happens until someone decides.
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := d.GetRun(watchID)
		if err != nil {
			t.Fatalf("get watch run: %v", err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch run never parked awaiting the agent")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Nothing has been derived while it sits parked.
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs while parked = %d, want just the gate and watch runs: an escalation must not act on its own", len(runs))
	}

	// The agent picks one finding and asks for a fix.
	if err := mgr.HandleRespondWithOverrides(watchID, types.StepWatch, types.ActionFix, []string{"F1"}, nil, nil); err != nil {
		t.Fatalf("respond fix: %v", err)
	}
	waitForRunStatus(t, d, watchID, types.RunCompleted)

	fixRun := waitForRunOfKind(t, d, repo.ID, types.RunKindGate, gate.ID)
	if fixRun.ParentRunID == nil || *fixRun.ParentRunID != watchID {
		t.Fatalf("fix run parent = %v, want the watch run", fixRun.ParentRunID)
	}
	select {
	case <-fixRan:
	case <-time.After(10 * time.Second):
		t.Fatal("the derived gate run never ran")
	}
}
