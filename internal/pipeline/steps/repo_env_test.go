package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func envMap(t *testing.T, entries []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", entry)
		}
		m[key] = value
	}
	return m
}

// TestRepoCommandEnv_ExposesRunRefs pins the public contract: a repo command
// gets the run's base and head refs, so a monorepo can scope an incremental
// build without hardcoding a branch name.
func TestRepoCommandEnv_ExposesRunRefs(t *testing.T) {
	workDir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, nil, workDir, baseSHA, headSHA, config.Commands{})

	env := envMap(t, repoCommandEnv(sctx))

	if env[EnvHeadRef] != "feature" {
		t.Errorf("NM_HEAD_REF = %q, want feature (branch name without refs/heads/)", env[EnvHeadRef])
	}
	if env[EnvHeadSHA] != headSHA {
		t.Errorf("NM_HEAD_SHA = %q, want the worktree HEAD %q", env[EnvHeadSHA], headSHA)
	}
	if env[EnvDefaultBranch] != "main" {
		t.Errorf("NM_DEFAULT_BRANCH = %q, want main", env[EnvDefaultBranch])
	}
	// The template repo has no remote, so the base ref falls back to the local
	// default branch; either way it must resolve in the worktree.
	if env[EnvBaseRef] != "main" {
		t.Errorf("NM_BASE_REF = %q, want main", env[EnvBaseRef])
	}
	if env[EnvBaseSHA] != baseSHA {
		t.Errorf("NM_BASE_SHA = %q, want the merge-base with the default branch %q", env[EnvBaseSHA], baseSHA)
	}
	if env[EnvRunID] != "run-1" {
		t.Errorf("NM_RUN_ID = %q, want run-1", env[EnvRunID])
	}
}

// TestRepoCommandEnv_TracksConfiguredDefaultBranch is the coze case: the base
// is a dated release branch, and the exported ref must follow the repo's
// configured default branch rather than a name baked into no-mistakes.
func TestRepoCommandEnv_TracksConfiguredDefaultBranch(t *testing.T) {
	workDir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, workDir, "branch", "release/20260713", baseSHA)

	sctx := newTestContext(t, nil, workDir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.DefaultBranch = "release/20260713"

	env := envMap(t, repoCommandEnv(sctx))
	if env[EnvBaseRef] != "release/20260713" {
		t.Errorf("NM_BASE_REF = %q, want release/20260713", env[EnvBaseRef])
	}
	if env[EnvBaseSHA] != baseSHA {
		t.Errorf("NM_BASE_SHA = %q, want %q", env[EnvBaseSHA], baseSHA)
	}
}

// TestLintStep_RepoCommandSeesRunRefs proves the variables actually reach the
// process that runs a repo command, not just the helper that builds them.
func TestLintStep_RepoCommandSeesRunRefs(t *testing.T) {
	workDir, baseSHA, headSHA := setupGitRepo(t)
	out := filepath.Join(t.TempDir(), "env.txt")
	sctx := newTestContext(t, nil, workDir, baseSHA, headSHA, config.Commands{
		Lint: "printf '%s %s %s %s\\n' \"$NM_BASE_REF\" \"$NM_BASE_SHA\" \"$NM_HEAD_REF\" \"$NM_HEAD_SHA\" > " + out,
	})

	step := &LintStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatalf("lint step: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("lint command did not write its env dump: %v", err)
	}
	fields := strings.Fields(string(data))
	want := []string{"main", baseSHA, "feature", headSHA}
	if len(fields) != len(want) {
		t.Fatalf("lint command saw %q, want %q", fields, want)
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("field %d = %q, want %q", i, fields[i], w)
		}
	}
}
