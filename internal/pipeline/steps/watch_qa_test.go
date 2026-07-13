package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// qaDriftFixture builds the state the staleness check reads: a gate bare repo
// holding the commit QA verified and the commit the PR now carries, a QA verdict
// recorded against the first, and a watch run watching the second.
//
// It is deliberately a real git repository. The whole decision turns on what the
// diff between two commits actually contains, and mocking that away would test
// the mock.
type qaDriftFixture struct {
	sctx    *pipeline.StepContext
	qaSHA   string
	headSHA string
	ghLog   string
}

func newQADriftFixture(t *testing.T, verdict string, secondCommit map[string]string) qaDriftFixture {
	t.Helper()

	// The gate bare repo: what a watch run reads, since it holds no worktree.
	p := paths.WithRoot(t.TempDir())
	work := t.TempDir()
	gitCmd(t, work, "init")
	gitCmd(t, work, "config", "user.email", "t@example.com")
	gitCmd(t, work, "config", "user.name", "t")
	writeRepoFiles(t, work, map[string]string{
		"internal/app/server.go": "package app\n\nfunc Serve() string { return \"v1\" }\n",
		"package-lock.json":      "{\"lockfileVersion\": 1}\n",
	})
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-m", "base")
	qaSHA := gitCmd(t, work, "rev-parse", "HEAD")

	writeRepoFiles(t, work, secondCommit)
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-m", "ci fix")
	headSHA := gitCmd(t, work, "rev-parse", "HEAD")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, work, qaSHA, headSHA, config.Commands{})
	sctx.Paths = p

	// The gate bare repo the watch run diffs against.
	gateDir := p.RepoDir(sctx.Repo.ID)
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(gateDir), "clone", "--bare", work, gateDir)

	// The QA node's verdict, recorded against the commit it exercised. This run
	// stands in for the earlier watch run that carried the QA pass.
	qaRun, err := sctx.DB.InsertRunWithOptions(sctx.Repo.ID, sctx.Run.Branch, qaSHA, qaSHA, db.RunOptions{
		Kind:      types.RunKindWatch,
		OnlySteps: []types.StepName{types.StepQA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunQAVerdict(qaRun.ID, verdict); err != nil {
		t.Fatal(err)
	}

	// The run doing the watching is at the new head - the fix round's commit.
	sctx.Run.HeadSHA = headSHA
	prURL := "https://github.com/test/repo/pull/7"
	setRunPRURL(t, sctx, prURL)
	sctx.Config.CITimeout = time.Hour

	env, log := fakeQAGH(t, prURL)
	sctx.Env = env
	sctx.Ctx = context.Background()

	return qaDriftFixture{sctx: sctx, qaSHA: qaSHA, headSHA: headSHA, ghLog: log}
}

// runStalenessCheck drives just the QA-drift half of a poll and returns the log.
func runStalenessCheck(t *testing.T, f qaDriftFixture) string {
	t.Helper()
	var logs []string
	f.sctx.Log = func(line string) { logs = append(logs, line) }

	host, skip := buildHost(f.sctx, "github")
	if host == nil {
		t.Fatalf("no host: %s", skip)
	}
	step := &WatchStep{}
	step.reportQAStaleness(f.sctx, host, prForTest(f.sctx))
	return strings.Join(logs, "\n")
}

// The captain's case, and the common one: a CI fix round that only re-resolved a
// lockfile changed nothing the QA pass exercised. Forcing a ~25-minute re-run for
// that is pure waste, and so is telling the reviewer the verdict is suspect.
func TestWatchStep_QAVerdictSurvivesALockfileOnlyFixRound(t *testing.T) {
	t.Parallel()
	f := newQADriftFixture(t, "PASS", map[string]string{
		"package-lock.json": "{\"lockfileVersion\": 2}\n",
	})

	logged := runStalenessCheck(t, f)

	if !strings.Contains(logged, "still applies") {
		t.Fatalf("the QA verdict was not carried forward across a lockfile-only change:\n%s", logged)
	}
	if strings.Contains(ghLog(t, f.ghLog), "pr comment") {
		t.Fatal("a lockfile bump made the PR grow a staleness comment; that trains people to ignore them")
	}
}

// The other half: "CI fixes never touch behavior" is an assumption, not a
// guarantee - CI runs the unit tests, and fixing a failing test can mean changing
// the product logic it was pointing at. QA is not re-run (that is a decision with
// a real cost), but the PR must say out loud which commit the verdict covers.
func TestWatchStep_ProductSourceFixRoundMarksTheQAVerdictStaleOnThePR(t *testing.T) {
	t.Parallel()
	f := newQADriftFixture(t, "PASS", map[string]string{
		"internal/app/server.go": "package app\n\nfunc Serve() string { return \"v2\" }\n",
		"package-lock.json":      "{\"lockfileVersion\": 2}\n",
	})

	logged := runStalenessCheck(t, f)
	posted := ghLog(t, f.ghLog)

	if !strings.Contains(posted, "pr comment") {
		t.Fatalf("the PR was never told its QA verdict is older than its code; gh log:\n%s\nstep log:\n%s", posted, logged)
	}
	for _, want := range []string{f.qaSHA[:12], f.headSHA[:12], "internal/app/server.go", "not re-run"} {
		if !strings.Contains(posted, want) {
			t.Fatalf("the staleness comment does not carry %q; gh log:\n%s", want, posted)
		}
	}
	// The lockfile moved too, but it is not what makes the verdict stale, and the
	// comment must not pad the product-file list with it.
	if strings.Contains(posted, "- `package-lock.json`") {
		t.Fatalf("the lockfile was listed as a product-source change; gh log:\n%s", posted)
	}
}

// The comment is posted once per (PR, QA verdict, head), not once per poll: a
// watch run polls for as long as the PR is open, and a re-armed one starts over.
func TestWatchStep_TheStalenessNoteIsPostedOnlyOnce(t *testing.T) {
	t.Parallel()
	f := newQADriftFixture(t, "FAIL", map[string]string{
		"internal/app/server.go": "package app\n\nfunc Serve() string { return \"v2\" }\n",
	})

	runStalenessCheck(t, f)
	// A second poll, and then a fresh step instance - what a daemon restart leaves.
	runStalenessCheck(t, f)

	if got := strings.Count(ghLog(t, f.ghLog), "pr comment"); got != 1 {
		t.Fatalf("the staleness note was posted %d times, want exactly 1", got)
	}
}

// A QA verdict about the very commit being watched is not stale, and saying
// anything at all about it would be noise.
func TestWatchStep_NoNoteWhenQAVerifiedTheWatchedCommit(t *testing.T) {
	t.Parallel()
	f := newQADriftFixture(t, "PASS", map[string]string{
		"internal/app/server.go": "package app\n\nfunc Serve() string { return \"v2\" }\n",
	})
	// Pretend QA ran against the head this run is watching.
	f.sctx.Run.HeadSHA = f.qaSHA

	logged := runStalenessCheck(t, f)

	if strings.Contains(ghLog(t, f.ghLog), "pr comment") {
		t.Fatal("the PR was told its own QA verdict is stale")
	}
	if strings.Contains(logged, "still applies") {
		t.Fatalf("a verdict about the watched commit was reported as drift:\n%s", logged)
	}
}

func prForTest(sctx *pipeline.StepContext) *scm.PR {
	return &scm.PR{Number: "7", URL: *sctx.Run.PRURL}
}

// writeRepoFiles writes (and creates directories for) a set of files.
func writeRepoFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
