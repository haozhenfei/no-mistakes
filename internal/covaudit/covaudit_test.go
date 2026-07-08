package covaudit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
)

func TestMain(m *testing.M) {
	// Agent harnesses inject git config (safe.bareRepository=explicit) via
	// GIT_CONFIG_COUNT; this package shells out to git for diffs, so strip it.
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Exit(m.Run())
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return string(out[:len(out)-1])
}

// TestEndToEnd_GoCoverageBackfillAudit is the full demonstration required by the
// task: a real git repo with a Go change, real `go test -coverprofile` output
// captured as signed coverage evidence, an over-eager agent ledger, backfill
// against instrumentation truth, and the §4.4c audit.
func TestEndToEnd_GoCoverageBackfillAudit(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")

	// Base commit: a package with one function.
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/calc\n\ngo 1.25\n")
	writeFile(t, filepath.Join(repo, "calc.go"), baseCalc)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base")
	base := headSHA(t, repo)

	// Change: add two functions. Only Add is exercised by the test we will run;
	// Sub is not — so an agent that labels both runtime-verified is lying about
	// Sub, and backfill must catch it.
	writeFile(t, filepath.Join(repo, "calc.go"), headCalc)
	writeFile(t, filepath.Join(repo, "calc_test.go"), calcTest)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "add funcs")
	head := headSHA(t, repo)

	// DB + run.
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	dbrepo, err := database.InsertRepo(repo, "https://example.com/calc.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(dbrepo.ID, "fm/calc", head, base)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Capture real coverage with the trusted collector.
	keyPath := filepath.Join(t.TempDir(), "evidence.key")
	key, err := evidence.LoadOrCreateKey(keyPath)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	store, err := evidence.Open(evidence.DirForBranch(repo, "fm/calc"), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	profile := filepath.Join(repo, "cover.out")
	covEntry, err := store.Coverage(context.Background(), evidence.CoverageOpts{
		Label:        "go unit tests",
		Argv:         goTestArgv(profile),
		Format:       coverage.FormatGo,
		Dir:          repo,
		RepoRoot:     repo,
		CoverProfile: profile,
		Commit:       head,
		RunID:        run.ID,
		Branch:       "fm/calc",
	})
	if err != nil {
		t.Fatalf("coverage collection: %v", err)
	}
	if covEntry.Coverage == nil || len(covEntry.Coverage.Files) == 0 {
		t.Fatalf("expected parsed coverage from go test, got %+v", covEntry.Coverage)
	}
	t.Logf("captured coverage: %+v", covEntry.Coverage.Files)

	// The agent optimistically records BOTH changed functions as
	// runtime-verified. We find the changed hunks the same way the audit does.
	diff := runGit(t, repo, "diff", base+".."+head)
	hunks := coverage.ParseDiffHunks(diff)
	var calcHunks []coverage.Hunk
	for _, h := range hunks {
		if h.File == "calc.go" {
			calcHunks = append(calcHunks, h)
		}
	}
	if len(calcHunks) == 0 {
		t.Fatalf("expected changed hunks in calc.go, got %+v", hunks)
	}
	for _, h := range calcHunks {
		if _, err := database.InsertCoverageEntry(coverage.LedgerEntry{
			RunID: run.ID, File: h.File, StartLine: h.Start, EndLine: h.End,
			State: coverage.StateRuntimeVerified, Source: "agent",
		}); err != nil {
			t.Fatalf("insert ledger: %v", err)
		}
	}

	// Run the orchestrated backfill + audit.
	res, err := Run(context.Background(), database, run.ID, repo, keyPath, base, head)
	if err != nil {
		t.Fatalf("covaudit.Run: %v", err)
	}
	t.Logf("audit: %s", res.Report.String())

	// The Add hunk was executed → stays runtime-verified. The Sub hunk was NOT
	// executed → the agent's false runtime-verified label is downgraded.
	if res.Report.RuntimeVerified == 0 {
		t.Fatalf("expected at least one runtime-verified hunk (Add), got report %+v", res.Report)
	}
	if len(res.Downgrades) == 0 {
		t.Fatalf("expected a downgrade for the unexecuted Sub hunk; audit found none.\nledger: %+v", res.Ledger)
	}
	// After backfill the audit must PASS (no false-runtime survives) but coverage
	// is partial.
	if !res.Report.Pass {
		t.Fatalf("audit should pass after backfill; issues: %+v", res.Report.Issues)
	}
	if res.Report.RuntimeVerified >= res.Report.TotalHunks {
		t.Fatalf("expected partial coverage (Sub uncovered), got %d/%d", res.Report.RuntimeVerified, res.Report.TotalHunks)
	}

	// Persistence: the ledger on disk reflects the correction.
	persisted, err := database.GetCoverageEntriesByRun(run.ID)
	if err != nil {
		t.Fatalf("reload ledger: %v", err)
	}
	var downgraded int
	for _, e := range persisted {
		if e.Source == "backfill" && e.State != coverage.StateRuntimeVerified {
			downgraded++
			if e.Reason == "" {
				t.Errorf("downgraded entry must carry a reason: %+v", e)
			}
		}
	}
	if downgraded == 0 {
		t.Error("expected the downgrade to be persisted to the ledger")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// goTestArgv runs the module's tests with coverage into profile. Uses `sh -c` so
// the redirect-free `-coverprofile` flag lands the file where we expect.
func goTestArgv(profile string) []string {
	return []string{"go", "test", "-coverprofile=" + profile, "./..."}
}

const baseCalc = `package calc

func Mul(a, b int) int { return a * b }

// end
`

// headCalc inserts Add BEFORE the "// end" context line and Sub AFTER it, so the
// two additions land in SEPARATE diff hunks (the unchanged "// end" separates
// them). TestAdd exercises Add but not Sub, so instrumentation covers only the
// first hunk — the second must be downgraded from a false runtime-verified
// label.
const headCalc = `package calc

func Mul(a, b int) int { return a * b }

func Add(a, b int) int {
	return a + b
}

// end

func Sub(a, b int) int {
	return a - b
}
`

const calcTest = `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatal("bad")
	}
}
`
