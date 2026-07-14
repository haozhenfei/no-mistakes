package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A run whose caller named steps must never be answered with a run that does not
// execute them.
//
// The bug this pins: `axi run --only qa` looked up the branch's active run, found
// the watch run that had been watching the PR since it opened - the state EVERY
// open PR is in - drove that, printed its watch step, and returned. The qa step
// never executed. Not "failed", not "skipped": no run on the branch ever carried
// the selection, so nothing ran and the caller was told it passed.
//
// The watch run is not a run the caller may be handed here, whatever its state,
// so it starts the run it asked for instead: a gate run carrying the selection,
// which is what puts the QA node in the watcher that takes the PR over.
func TestAxiRun_OnlyQAIsNotSwallowedByAnActiveWatchRun(t *testing.T) {
	env := setupAxiRunRepo(t, "feature/qa-selection")

	// The PR is open and a watch run is watching it. This is the ordinary state
	// of a branch that has reached a PR, not an edge case.
	watch, err := env.db.InsertRunWithOptions(env.repoID, env.branch, env.headSHA, env.headSHA, db.RunOptions{
		Kind:  types.RunKindWatch,
		PRURL: "https://example.test/pr/1",
	})
	if err != nil {
		t.Fatalf("insert watch run: %v", err)
	}
	if err := env.db.UpdateRunStatus(watch.ID, types.RunRunning); err != nil {
		t.Fatalf("mark watch run running: %v", err)
	}

	// The command is bounded because the bug's symptom is not an error: it
	// re-attached to the watch run and sat there polling a PR that no one had
	// asked it to watch, which is why "the qa step never ran" looked like "QA is
	// slow". A hang here is a failure, not a timeout to wait out.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, _ := executeCmdWithContext(ctx, "axi", "run", "--only", "qa", "--intent", "QA the branch against its open PR")

	qaRun := waitForRunCarryingQA(t, env, watch.ID)
	if qaRun == nil {
		t.Fatalf("`axi run --only qa` started no run carrying the qa selection: the step never ran\naxi output:\n%s", out)
	}
	// And it is that run the command drove and reported, not the watch run it
	// found already active.
	if !strings.Contains(out, qaRun.ID) {
		t.Fatalf("`axi run --only qa` did not report the run it started (%s)\naxi output:\n%s", qaRun.ID, out)
	}
	if !types.ContainsStep(qaRun.SkipSteps, types.StepPR) {
		t.Fatalf("the run started for --only qa did not resolve to the exclusive skip set: skip=%v", qaRun.SkipSteps)
	}
}

// The same command re-issued while the run it started is still going must
// re-attach, not spawn a second run: an agent that lost its connection re-runs
// the identical command, and the run it asked for is already running.
func TestAxiRun_MatchingSelectionReattachesToTheActiveRun(t *testing.T) {
	env := setupAxiRunRepo(t, "feature/qa-reattach")

	skip, only := types.ResolveRunSteps(nil, []types.StepName{types.StepQA}, nil)
	active, err := env.db.InsertRunWithOptions(env.repoID, env.branch, env.headSHA, env.headSHA, db.RunOptions{
		Kind:      types.RunKindGate,
		SkipSteps: skip,
		OnlySteps: only,
	})
	if err != nil {
		t.Fatalf("insert active run: %v", err)
	}
	if err := env.db.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run running: %v", err)
	}

	selection, err := parseStepSelectionWith("", "qa", "")
	if err != nil {
		t.Fatalf("parse selection: %v", err)
	}
	info := &ipc.RunInfo{ID: active.ID, Kind: types.RunKindGate, SkipSteps: skip, OnlySteps: only}
	if !runCarriesSelection(info, selection) {
		t.Fatal("a run started with --only qa does not carry --only qa; the identical command would start a second run")
	}
}

// A gate run owns local state - a worktree, agent sessions, findings parked at a
// gate - and starting the requested run would supersede it. Refusing is the
// caller's cue to wait or abort; what must not happen is driving it and calling
// its outcome the answer to a selection it never carried.
func TestAxiRun_SelectionAgainstAnActiveGateRunRefusesLoudly(t *testing.T) {
	env := setupAxiRunRepo(t, "feature/qa-gate-conflict")

	gateRun, err := env.db.InsertRun(env.repoID, env.branch, env.headSHA, env.headSHA)
	if err != nil {
		t.Fatalf("insert gate run: %v", err)
	}
	if err := env.db.UpdateRunStatus(gateRun.ID, types.RunRunning); err != nil {
		t.Fatalf("mark gate run running: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := executeCmdWithContext(ctx, "axi", "run", "--only", "review", "--intent", "review only")
	if err == nil {
		t.Fatalf("`axi run --only review` succeeded while a full gate run was active; it must refuse\n%s", out)
	}
	if !strings.Contains(out, gateRun.ID) {
		t.Fatalf("the refusal does not name the active run %s:\n%s", gateRun.ID, out)
	}
	runs, rerr := env.db.GetRunsByRepoBranch(env.repoID, env.branch)
	if rerr != nil {
		t.Fatalf("get runs: %v", rerr)
	}
	if len(runs) != 1 {
		t.Fatalf("the refusal started %d extra run(s); it must start none", len(runs)-1)
	}
}

type axiRunEnv struct {
	db      *db.DB
	repoID  string
	branch  string
	headSHA string
}

// setupAxiRunRepo brings up a real repo, gate, and daemon, with a feature branch
// checked out and committed - everything `axi run` needs to start a real run.
func setupAxiRunRepo(t *testing.T, branch string) axiRunEnv {
	t.Helper()

	repoDir := setupTestRepo(t)
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}
	startTestDaemon(t, p, d)

	run(t, repoDir, "git", "checkout", "-b", branch)
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "a change worth validating")

	head, err := git.Run(context.Background(), repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}
	if repo == nil {
		t.Fatalf("repo %s was not registered by gate.Init", gitRoot)
	}
	return axiRunEnv{db: d, repoID: repo.ID, branch: branch, headSHA: head}
}

// waitForRunCarryingQA returns the GATE run (other than excludeID) whose
// persisted selection names qa, or nil if none appears. It must be the gate run:
// the watch run that gate run hands off to inherits the selection too, and it is
// the gate run's existence that proves the command started the work it was asked
// for rather than re-attaching to something already running.
func waitForRunCarryingQA(t *testing.T, env axiRunEnv, excludeID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := env.db.GetRunsByRepoBranch(env.repoID, env.branch)
		if err != nil {
			t.Fatalf("get runs: %v", err)
		}
		for _, r := range runs {
			if r.ID != excludeID && r.Kind.Gate() && types.SelectsQA(r.OnlySteps) {
				return r
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
