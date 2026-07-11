package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// inRunContext bundles what an in-worktree command (evidence, claim) needs: the
// worktree root, the owning run, and its branch/commit. These commands are
// invoked by the agent that runs INSIDE the pipeline's isolated worktree, so
// they resolve the run from the worktree path rather than from the user's
// original clone.
type inRunContext struct {
	p        *paths.Paths
	d        *db.DB
	repoRoot string
	branch   string
	headSHA  string
	run      *db.Run
}

func (c *inRunContext) close() {
	if c.d != nil {
		c.d.Close()
	}
}

// runID returns the owning run's id, or "" when no run could be resolved.
func (c *inRunContext) runID() string {
	if c.run == nil {
		return ""
	}
	return c.run.ID
}

// openInRunContext resolves paths, opens the DB, finds the worktree root, and
// resolves the owning run. The daemon checks out each run into
// <NM_HOME>/worktrees/<repoID>/<runID>, so when the current directory is under
// that tree the run id is literally a path component — the most reliable signal
// available to a command spawned by the in-run agent (which sits at a detached
// HEAD where `git branch` is empty). Outside a managed worktree it falls back to
// the active run for the current repo/branch so the commands also work when an
// author drives a run from their own clone.
func openInRunContext(ctx context.Context) (*inRunContext, error) {
	p, d, err := openResources()
	if err != nil {
		return nil, err
	}
	repoRoot, err := git.FindGitRoot(".")
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("not in a git repository")
	}
	c := &inRunContext{p: p, d: d, repoRoot: repoRoot}
	c.headSHA, _ = git.HeadSHA(ctx, repoRoot)

	if runID := runIDFromWorktreePath(p, repoRoot); runID != "" {
		run, err := d.GetRun(runID)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("get run %s: %w", runID, err)
		}
		c.run = run
	}
	if c.run == nil {
		// Fallback: resolve via the repo + active run.
		if repo, err := findRepo(d); err == nil {
			branch := currentBranchForRunResolve(ctx)
			if active, err := d.GetActiveRun(repo.ID, branch); err == nil && active != nil {
				c.run = active
			}
		}
	}
	if c.run != nil {
		c.branch = c.run.Branch
	} else {
		c.branch = currentBranchForRunResolve(ctx)
	}
	return c, nil
}

// runIDFromWorktreePath extracts the run id when repoRoot is a daemon-managed
// worktree (<worktrees>/<repoID>/<runID>). Symlinks are resolved on both sides
// because NM_HOME often lives under /var (a symlink to /private/var on macOS).
func runIDFromWorktreePath(p *paths.Paths, repoRoot string) string {
	worktrees := resolveSymlinks(p.WorktreesDir())
	root := resolveSymlinks(repoRoot)
	rel, err := filepath.Rel(worktrees, root)
	if err != nil {
		return ""
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return ""
	}
	return parts[1] // <repoID>/<runID>[/...]
}

func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
