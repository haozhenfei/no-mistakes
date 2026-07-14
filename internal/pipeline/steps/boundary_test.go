package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/boundary"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The incident this whole boundary exists for: a fix agent decided the repo's
// gate config was wrong and committed a change to the shared team
// .no-mistakes.yaml, rewriting the rules it was being judged by, mid-run.
// Nothing stopped it - "do not touch the gate config" was a sentence in a
// prompt. Now it is a mechanism: the run fails, and the message names the path.
func TestFixStep_EditingTheGateConfigFailsTheRunWithThePathNamed(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	selfModifier := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		writeFile(t, dir, ".no-mistakes.yaml", "commands:\n  test: echo skip\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"relax the gate config"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, selfModifier, dir, baseSHA, headSHA, config.Commands{})
	seedParentWatchRun(t, sctx.DB, sctx.Run, `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`)

	_, err := (&FixStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("a fix agent that rewrites the gate's own config must fail the run, not pass it")
	}
	if !strings.Contains(err.Error(), ".no-mistakes.yaml") {
		t.Fatalf("the failure must name the offending path, got: %v", err)
	}
	var boundaryErr *boundary.Error
	if !asBoundaryError(err, &boundaryErr) {
		t.Fatalf("want a boundary error, got %T: %v", err, err)
	}

	// The run's head must not have moved: nothing the agent wrote was adopted,
	// so no later step reviews, tests, or pushes it.
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head moved to %s; a refused change must not be committed", sctx.Run.HeadSHA)
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatalf("worktree head moved to %s", head)
	}
	// ...and the agent's work is still on disk. A silently dropped change is its
	// own failure mode: the agent would believe it made a fix the run validated
	// without.
	if _, statErr := os.Stat(filepath.Join(dir, ".no-mistakes.yaml")); statErr != nil {
		t.Fatalf("the agent's work must be left in the worktree, not discarded: %v", statErr)
	}
}

// The other shape an agent leaves work in: it commits the change itself (which
// the fix prompt asks for). The boundary has to hold there too - by the time the
// pipeline looks, the offending edit is already a commit.
func TestFixStep_GateConfigCommittedByTheAgentItselfAlsoFailsTheRun(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	selfCommitter := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		writeFile(t, dir, ".no-mistakes.yaml", "commands:\n  test: echo skip\n")
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "chore: move the test baseline")
		return &agent.Result{Output: json.RawMessage(`{"summary":"move the test baseline"}`)}, nil
	}}
	// The daemon runs every step in a detached worktree (git.WorktreeAdd passes
	// --detach), so an agent's own commit moves nothing but the detached HEAD:
	// the branch ref the push step pushes advances ONLY when commitAgentFixes
	// adopts the work with update-ref. Detach here so the test models that.
	gitCmd(t, dir, "checkout", "--detach")
	sctx := newTestContextWithDBRecords(t, selfCommitter, dir, baseSHA, headSHA, config.Commands{})
	seedParentWatchRun(t, sctx.DB, sctx.Run, `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`)

	_, err := (&FixStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("an agent's own commit to the gate config must fail the run")
	}
	if !strings.Contains(err.Error(), ".no-mistakes.yaml") {
		t.Fatalf("the failure must name the offending path, got: %v", err)
	}
	// The agent's commit exists in the worktree, but the run never adopted it:
	// the run head and the branch ref (what the push step pushes) both stayed put.
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want the pre-agent head %s", sctx.Run.HeadSHA, headSHA)
	}
	if ref := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); ref != headSHA {
		t.Fatalf("branch ref = %s, want %s: a refused commit must never reach origin", ref, headSHA)
	}
}

// The legitimate case has to stay possible: a task whose whole purpose IS to
// change the gate config, run with the explicit opt-in.
func TestFixStep_GateConfigOptInPermitsIt(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	editor := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		writeFile(t, dir, ".no-mistakes.yaml", "commands:\n  test: go test ./...\n")
		return &agent.Result{Output: json.RawMessage(`{"summary":"point the gate at the new test command"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, editor, dir, baseSHA, headSHA, config.Commands{})
	// The opt-in the caller asked for with `axi run --allow-gate-config`, as the
	// daemon persisted it on the run row.
	sctx.Run.AllowGateConfig = true
	seedParentWatchRun(t, sctx.DB, sctx.Run, `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`)

	if _, err := (&FixStep{}).Execute(sctx); err != nil {
		t.Fatalf("the explicit opt-in must permit the change: %v", err)
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Fatal("the opted-in change must be committed and the run head advanced")
	}
	changed := gitCmd(t, dir, "show", "--name-only", "--pretty=format:", "HEAD")
	if !strings.Contains(changed, ".no-mistakes.yaml") {
		t.Fatalf("the commit must carry the gate config change, got: %s", changed)
	}
}

// The maintainer's own immutable_paths list, declared in the trusted repo
// config. Unlike the built-in gate-config default, no run-level opt-in lifts it.
func TestCommitAgentFixes_DeclaredImmutablePathIsRefused(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Boundary = boundary.Policy{ImmutablePaths: []string{"ci/**"}}
	sctx.Run.AllowGateConfig = true // does not apply to a declared immutable path

	writeFile(t, dir, "ci/pipeline.yaml", "steps: []\n")
	err := commitAgentFixes(sctx, types.StepLint, "reformat the pipeline", "fallback")
	if err == nil {
		t.Fatal("a declared immutable path must be refused")
	}
	if !strings.Contains(err.Error(), "ci/pipeline.yaml") || !strings.Contains(err.Error(), "immutable_paths") {
		t.Fatalf("the failure must name the path and the rule, got: %v", err)
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatalf("head moved to %s after a refused change", head)
	}
}

func TestCommitAgentFixes_AllowedPathsIsAWhitelist(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Boundary = boundary.Policy{AllowedPaths: []string{"src/**"}}

	writeFile(t, dir, "src/a.txt", "in bounds\n")
	if err := commitAgentFixes(sctx, types.StepLint, "fix lint", "fallback"); err != nil {
		t.Fatalf("a path inside allowed_paths must be committed: %v", err)
	}

	writeFile(t, dir, "elsewhere/b.txt", "out of bounds\n")
	err := commitAgentFixes(sctx, types.StepLint, "fix lint", "fallback")
	if err == nil {
		t.Fatal("a path outside allowed_paths must be refused")
	}
	if !strings.Contains(err.Error(), "elsewhere/b.txt") {
		t.Fatalf("the failure must name the path, got: %v", err)
	}
}

// The boundary bounds the AGENT, not the branch. A human's own commit changing
// the gate config is the legitimate way to change it, and a run that merely
// carries such a commit must gate normally - otherwise every gate-config change
// would need the opt-in just to be reviewed.
func TestCommitAgentFixes_HumanCommitToTheGateConfigDoesNotFailTheRun(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	writeFile(t, dir, ".no-mistakes.yaml", "commands:\n  test: go test ./...\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "chore: retune the gate")
	humanHead := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, humanHead, config.Commands{})

	// The agent then does something entirely ordinary on top.
	writeFile(t, dir, "linted.txt", "linted\n")
	if err := commitAgentFixes(sctx, types.StepLint, "fix lint", "fallback"); err != nil {
		t.Fatalf("the human's gate-config commit is not the agent's change and must not fail the run: %v", err)
	}
	if sctx.Run.HeadSHA == humanHead {
		t.Fatal("the agent's own change should have been committed")
	}
}

// commitAgentFixes is not the only adoption point: the push step sweeps up work
// an agent left uncommitted (a step that runs an agent outside a fix path leaves
// no commit) and pushes it. Without the boundary there, an agent could reach the
// shared remote by simply not committing.
func TestPushStep_RefusesUncommittedAgentWorkOutsideTheBoundary(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	// An agent that ran outside a fix path (a reviewer, a skeptic) left this
	// behind. The push step is about to commit and push it.
	writeFile(t, dir, ".no-mistakes.yaml", "commands:\n  test: echo skip\n")

	_, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("the push step must refuse uncommitted agent work that crosses the boundary")
	}
	if !strings.Contains(err.Error(), ".no-mistakes.yaml") {
		t.Fatalf("the failure must name the offending path, got: %v", err)
	}
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != headSHA {
		t.Fatalf("head moved to %s: the refused work must not be committed", head)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func asBoundaryError(err error, target **boundary.Error) bool {
	if e, ok := err.(*boundary.Error); ok {
		*target = e
		return true
	}
	return false
}
