package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/boundary"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// runBoundary is the change boundary in force for this run: the repository's
// declared policy (from the trusted config) plus the run's own gate-config
// opt-in (from the run row, where a caller had to ask for it explicitly).
func runBoundary(sctx *pipeline.StepContext) boundary.Policy {
	policy := boundary.Policy{}
	if sctx.Config != nil {
		policy = sctx.Config.Boundary
	}
	if sctx.Run != nil {
		policy.AllowGateConfig = sctx.Run.AllowGateConfig
	}
	return policy
}

// agentChangedPaths lists every repo-relative path the step's agent wrote,
// covering BOTH shapes an agent can leave its work in - uncommitted edits and
// commits it made itself - because the boundary has to hold for both. A single
// `git diff <previousHead>` does that: it compares the working tree against the
// head the agent started from, so a file the agent edited and a file the agent
// edited and committed both appear. Untracked files are read separately, since
// diff cannot see a file git does not know about yet.
func agentChangedPaths(ctx context.Context, workDir, previousHead string) ([]string, error) {
	base := strings.TrimSpace(previousHead)
	if base == "" {
		base = "HEAD"
	}
	tracked, err := git.Run(ctx, workDir, "diff", "--name-only", base, "--")
	if err != nil {
		return nil, fmt.Errorf("list changed paths since %s: %w", shortSHA(base), err)
	}
	untracked, err := git.Run(ctx, workDir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("list untracked paths: %w", err)
	}
	return dedupePaths(append(nonEmptyLines(tracked), nonEmptyLines(untracked)...)), nil
}

// enforceChangeBoundary fails the step when the agent wrote a path the run's
// boundary refuses.
//
// It runs BEFORE the pipeline adopts the agent's work, and it is a hard error:
// never a warning, and never a silent drop of the offending file. Dropping it
// would leave the agent believing it made a change the run then validated
// without - and would leave the rest of the gate reviewing, testing and pushing
// something nobody wrote. Failing here leaves the work in the worktree, names
// the path, and stops the run.
//
// The boundary is checked on what the agent WROTE this step, not on the branch
// diff: a human's own commit changing the gate config is the legitimate path and
// needs no permission. This constrains the agent, which is the thing that had
// none.
func enforceChangeBoundary(sctx *pipeline.StepContext, stepName types.StepName, previousHead string) error {
	policy := runBoundary(sctx)
	paths, err := agentChangedPaths(sctx.Ctx, sctx.WorkDir, previousHead)
	if err != nil {
		// Fail closed: an unreadable change set is not an empty one, and a
		// boundary that gives up when it cannot look is not a boundary.
		return fmt.Errorf("check %s change boundary: %w", stepName, err)
	}
	violations := policy.Check(paths)
	if len(violations) == 0 {
		return nil
	}
	err = &boundary.Error{Actor: boundaryActor(stepName), Violations: violations}
	sctx.Log(err.Error())
	return err
}

// boundaryActor names what wrote the refused paths. Every step that adopts agent
// work ran the agent itself and can say so - except push, which sweeps up work
// left uncommitted by whichever earlier step ran an agent outside a fix path.
func boundaryActor(stepName types.StepName) string {
	if stepName == types.StepPush {
		return "an agent in this run left uncommitted work that"
	}
	return fmt.Sprintf("the %s agent", stepName)
}

func nonEmptyLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}
