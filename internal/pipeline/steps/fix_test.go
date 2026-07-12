package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// seedParentWatchRun creates the watch run the fix run descends from, with the
// findings on its watch step - the exact shape the fix step reads through
// parent_run_id.
func seedParentWatchRun(t *testing.T, d *db.DB, child *db.Run, findingsJSON string) {
	t.Helper()
	parent, err := d.InsertRunWithOptions(child.RepoID, child.Branch, child.HeadSHA, child.BaseSHA, db.RunOptions{
		Kind: types.RunKindWatch,
	})
	if err != nil {
		t.Fatalf("insert parent watch run: %v", err)
	}
	sr, err := d.InsertStepResult(parent.ID, types.StepWatch)
	if err != nil {
		t.Fatalf("insert watch step: %v", err)
	}
	if findingsJSON != "" {
		if err := d.SetStepFindings(sr.ID, findingsJSON); err != nil {
			t.Fatalf("set watch findings: %v", err)
		}
	}
	child.ParentRunID = &parent.ID
}

func TestFixStep_SkipsWhenRunHasNoParent(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&FixStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("an ordinary gate run carries no findings to fix; the step must skip")
	}
}

// TestFixStep_AppliesSeedFindingsFromParentWatchRun is the load-bearing half of
// the watch->gate loop: the derived run picks the findings up from its parent
// and turns them into a commit, which the rest of the gate then reviews.
func TestFixStep_AppliesSeedFindingsFromParentWatchRun(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	var prompt string
	agentStep := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("fixed\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix the failing build"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, agentStep, dir, baseSHA, headSHA, config.Commands{})
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL

	seed := `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing on the PR"}`
	seedParentWatchRun(t, sctx.DB, sctx.Run, seed)

	outcome, err := (&FixStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if outcome.Skipped {
		t.Fatal("the step must not skip when its parent handed it findings")
	}
	if outcome.NeedsApproval {
		t.Fatal("a fix that produced changes proceeds through the gate; it does not park")
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Fatal("the fix produced no commit")
	}
	if !strings.Contains(prompt, "CI check failing: build") {
		t.Fatalf("the fix prompt never named the finding:\n%s", prompt)
	}
	if !strings.Contains(prompt, prURL) {
		t.Fatalf("the fix prompt never named the PR:\n%s", prompt)
	}
}

// TestFixStep_NoChangesParksInsteadOfLooping: if the agent cannot fix the
// finding, pushing on would re-open the same PR, fail the same check, derive the
// same watch run, and come back here. Park instead.
func TestFixStep_NoChangesParksInsteadOfLooping(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	// The agent writes nothing.
	quiet := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"nothing to do"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, quiet, dir, baseSHA, headSHA, config.Commands{})

	seed := `{"findings":[{"severity":"error","description":"CI check failing: flaky","action":"auto-fix"}],"summary":"CI failing"}`
	seedParentWatchRun(t, sctx.DB, sctx.Run, seed)

	outcome, err := (&FixStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a fix that changed nothing must park, or the run loop spins forever")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("parked findings = %d, want the original finding carried through", len(findings.Items))
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatal("nothing was committed, so the head must not move")
	}
}

// TestFixStep_AdoptsCommitTheAgentMadeItself pins the contract the fix prompt
// states out loud ("Do not amend or rewrite existing commits; add new ones"):
// an agent that follows it leaves a CLEAN worktree, and the step must still see
// the fix. Before this was fixed, "did the agent do anything" was decided by
// `git status --porcelain`, so a fully fixed and committed run was declared to
// have produced nothing and parked - and every retry parked again, because the
// worktree stayed clean.
func TestFixStep_AdoptsCommitTheAgentMadeItself(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	committer := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("fixed\n"), 0o644); err != nil {
			return nil, err
		}
		// The agent commits its own work, exactly as the prompt asks.
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-m", "fix(probe): make the failing check pass")
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix the failing check"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, committer, dir, baseSHA, headSHA, config.Commands{})

	seed := `{"findings":[{"severity":"error","description":"CI check failing: build","action":"auto-fix"}],"summary":"CI failing"}`
	seedParentWatchRun(t, sctx.DB, sctx.Run, seed)

	outcome, err := (&FixStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("the agent fixed the finding and committed it; the run must carry on through the gate, not park")
	}

	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if agentHead == headSHA {
		t.Fatal("test bug: the agent did not actually commit")
	}
	if sctx.Run.HeadSHA != agentHead {
		t.Fatalf("run head = %s, want the agent's commit %s", sctx.Run.HeadSHA, agentHead)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.HeadSHA != agentHead {
		t.Fatalf("persisted head = %s, want %s", stored.HeadSHA, agentHead)
	}
	// The push step pushes the branch ref, so an unadvanced ref means the fix
	// never reaches origin even if the run carries on.
	branchRef := gitCmd(t, dir, "rev-parse", "refs/heads/feature")
	if branchRef != agentHead {
		t.Fatalf("branch ref = %s, want the agent's commit %s", branchRef, agentHead)
	}
}

// TestCommitAgentFixes_CommitsUncommittedAgentEdits is the other shape, and the
// one every other fix step (review, lint, document) relies on: the agent leaves
// its edits in the worktree and the pipeline commits them.
func TestCommitAgentFixes_CommitsUncommittedAgentEdits(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	if err := os.WriteFile(filepath.Join(dir, "linted.txt"), []byte("linted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepLint, "fix lint issues", "fallback"); err != nil {
		t.Fatalf("commit agent fixes: %v", err)
	}

	head := gitCmd(t, dir, "rev-parse", "HEAD")
	if head == headSHA {
		t.Fatal("uncommitted agent edits must be committed")
	}
	if sctx.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, head)
	}
	if got := gitCmd(t, dir, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("branch ref = %s, want %s", got, head)
	}
	subject := gitCmd(t, dir, "log", "-1", "--format=%s")
	if !strings.Contains(subject, "fix lint issues") {
		t.Fatalf("commit subject = %q, want the agent summary", subject)
	}
}

// TestCommitAgentFixes_UntouchedWorktreeMovesNothing keeps the park path honest:
// a genuinely idle agent must still leave the head where it was.
func TestCommitAgentFixes_UntouchedWorktreeMovesNothing(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	if err := commitAgentFixes(sctx, types.StepFix, "", "fallback"); err != nil {
		t.Fatalf("commit agent fixes: %v", err)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want it unmoved at %s", sctx.Run.HeadSHA, headSHA)
	}
}

func TestFixStep_SkipsWhenParentHasNoFindings(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	seedParentWatchRun(t, sctx.DB, sctx.Run, "")

	outcome, err := (&FixStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("fix: %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("no seed findings means nothing to fix")
	}
}

func TestFailingCheckNamesFromFindings(t *testing.T) {
	t.Parallel()
	seed := Findings{Items: []Finding{
		{Description: "CI check failing: build (ubuntu)"},
		{Description: "PR has merge conflicts with the base branch"},
		{Description: "CI check failing: lint"},
	}}
	got := failingCheckNamesFromFindings(seed)
	want := []string{"build (ubuntu)", "lint"}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
