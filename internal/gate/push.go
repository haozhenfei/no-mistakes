package gate

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// PushHead pushes the working repo's HEAD onto the gate's branch ref, taking the
// gate's ref wherever it currently points. It is the single way a branch enters
// the pipeline (the CLI's `axi run` and the interactive wizard both call it), and
// the push options carry the run's selection and opt-ins to the post-receive hook.
//
// The update is forced, and that is the whole point. The gate is no-mistakes' own
// private mirror (`<NM_HOME>/repos/<id>.git`); its one job is to carry the
// branch's CURRENT head into the daemon. A branch whose history was legitimately
// rewritten - a rebase onto a fresh release branch, routine in a repo that cuts
// one daily - is not a descendant of the head the mirror already holds, so a
// fast-forward-only push is rejected ("fetch first" / "non-fast-forward"), the
// run never starts, and the branch stays ungateable until somebody deletes the
// ref inside the mirror by hand. Refusing here protects nothing; it only means
// the gate cannot look at the code the user actually has.
//
// Forcing the mirror's branch ref cannot destroy work:
//   - The gate holds refs/heads/<branch> (pushed heads), refs/remotes/origin/*
//     (fetched by run worktrees), and refs/no-mistakes/runs/<id>/head. Only the
//     first is touched.
//   - Commits an agent made inside a run's worktree are preserved on the gate at
//     refs/no-mistakes/runs/<id>/head (daemon.preserveRunWorktreeHead) before the
//     worktree goes away, so an overwritten branch ref never makes them
//     unreachable, and the push step has already sent them to origin.
//   - The branch on origin - the ref that actually holds people's code - is
//     protected separately and unchanged: every force-push to it still goes
//     through resolveForcePushDecision (internal/pipeline/steps/forcepush.go).
//
// Regression: TestPushHead_RebasedBranchStillEntersTheGate.
func PushHead(ctx context.Context, workDir, branch string, pushOptions []string) error {
	return git.ForcePushWithOptions(ctx, workDir, RemoteName, "refs/heads/"+branch, pushOptions)
}
