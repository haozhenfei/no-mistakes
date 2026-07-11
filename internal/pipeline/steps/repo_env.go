package steps

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Environment variables no-mistakes exports to every repo command it runs
// (`commands.test`, `commands.lint`, `commands.format`, and
// `test.evidence.upload_cmd`). They are a public contract for
// .no-mistakes.yaml authors: a monorepo can scope an incremental build to the
// run's real base ref instead of hardcoding a branch name that expires.
//
// Semantics are documented in
// docs/src/content/docs/reference/environment.md; keep the two in sync.
const (
	// EnvRunID is the pipeline run's ID.
	EnvRunID = "NM_RUN_ID"
	// EnvDefaultBranch is the repo's integration branch name (no remote
	// prefix), e.g. "main" or "release/20260713".
	EnvDefaultBranch = "NM_DEFAULT_BRANCH"
	// EnvBaseRef is the ref the branch is gated against - the
	// remote-tracking ref of the default branch, e.g. "origin/main". This is
	// the ref the rebase step rebases onto, so it resolves inside the run's
	// worktree.
	EnvBaseRef = "NM_BASE_REF"
	// EnvBaseSHA is the commit the branch forked from: the merge-base of the
	// run's HEAD and the base ref.
	EnvBaseSHA = "NM_BASE_SHA"
	// EnvHeadRef is the pushed branch name (no "refs/heads/" prefix). The
	// managed worktree runs on a detached HEAD, so this names the branch under
	// review, not a local ref that resolves there; use HEAD or NM_HEAD_SHA for
	// the commit.
	EnvHeadRef = "NM_HEAD_REF"
	// EnvHeadSHA is the commit the repo command is running against (the
	// worktree HEAD, post-rebase).
	EnvHeadSHA = "NM_HEAD_SHA"
)

// repoCommandEnv builds the NM_* ref variables exported to repo commands.
// Every value comes from what the run already knows (the run row plus the
// worktree's git state); a value that cannot be resolved is omitted rather
// than guessed, so a command can test for it.
func repoCommandEnv(sctx *pipeline.StepContext) []string {
	if sctx == nil || sctx.Run == nil {
		return nil
	}
	ctx := sctx.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var env []string
	add := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			env = append(env, key+"="+value)
		}
	}

	add(EnvRunID, sctx.Run.ID)

	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	add(EnvHeadRef, branch)

	headSHA := sctx.Run.HeadSHA
	if sha, err := git.HeadSHA(ctx, sctx.WorkDir); err == nil && strings.TrimSpace(sha) != "" {
		headSHA = strings.TrimSpace(sha)
	}
	add(EnvHeadSHA, headSHA)

	defaultBranch := ""
	upstreamURL := ""
	if sctx.Repo != nil {
		defaultBranch = strings.TrimSpace(sctx.Repo.DefaultBranch)
		upstreamURL = sctx.Repo.UpstreamURL
	}
	if defaultBranch == "" {
		return env
	}
	add(EnvDefaultBranch, defaultBranch)
	add(EnvBaseRef, baseRefForCommands(ctx, sctx.WorkDir, upstreamURL, defaultBranch))
	add(EnvBaseSHA, resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, defaultBranch))
	return env
}

// baseRefForCommands names the base the run is gated against, preferring the
// remote-tracking ref the rebase step fetched. It returns "" when neither the
// remote-tracking ref nor a local branch of that name exists in the worktree,
// so repo commands never receive a ref that git cannot resolve.
func baseRefForCommands(ctx context.Context, workDir, upstreamURL, defaultBranch string) string {
	remote := resolveUpstreamRemoteName(ctx, workDir, upstreamURL)
	for _, ref := range []string{remote + "/" + defaultBranch, defaultBranch} {
		if _, err := git.Run(ctx, workDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return ""
}

// withPWD appends PWD to a subprocess environment. os/exec only injects
// PWD=Cmd.Dir when Cmd.Env is nil, so any command that carries extra env must
// thread the working directory through itself; otherwise the daemon's own PWD
// leaks into the shell (see git.NonInteractiveEnv for the same reasoning).
func withPWD(dir string, env []string) []string {
	if dir == "" || runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		return env
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return env
	}
	return append(env, "PWD="+abs)
}
