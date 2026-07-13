package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/qadrift"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// reportQAStaleness makes the age of a QA verdict visible on the PR.
//
// QA runs against ONE commit - the node right next to this one in the same watch
// run. The PR's head then moves: a failing check derives a fix round, which
// pushes a new commit, and the PR is watched again by a NEW watch run at that new
// head. From that moment the QA report sitting on the PR is a statement about a
// commit that is no longer what would merge, and nobody reading the PR can tell:
// an hour-old QA comment looks exactly as authoritative as a fresh one.
//
// The two easy answers are both wrong. Invalidating QA on any new commit throws
// away a ~25-minute pass because a lockfile moved, which is what most CI fixes
// touch. Assuming a CI fix cannot change behavior is an assumption, not a
// guarantee: CI runs the unit tests too, and fixing a failing test can mean
// changing the product logic the test was about. So the DIFF decides
// (internal/qadrift), and the decision is always visible:
//
//   - Only infrastructure moved (lockfiles, CI config, linter config, docs, or a
//     pure reformat): the QA verdict still describes the product, and it stands.
//     Logged, not commented - a comment for every lockfile bump would train people
//     to ignore these.
//   - Product source moved: the PR gets a comment naming both commits and every
//     product file that changed between them. QA is deliberately NOT re-run: it is
//     expensive, and whether the change plausibly affects what QA exercised is a
//     judgment call. What is not optional is that the reader knows.
//
// It never fails the watch run: a QA verdict this run cannot read is a verdict it
// says nothing about, which is the honest outcome - but it must not also cost the
// PR its CI monitoring.
func (s *WatchStep) reportQAStaleness(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) {
	if sctx.DB == nil || sctx.Paths == nil || sctx.Repo == nil || sctx.Run == nil {
		return
	}
	qaRun, err := sctx.DB.LatestQAVerdict(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not read the branch's QA verdict: %v", err))
		return
	}
	if qaRun == nil || qaRun.HeadSHA == "" || qaRun.HeadSHA == sctx.Run.HeadSHA {
		// No QA verdict for this branch, or QA verified exactly the commit this
		// run is watching. Nothing to say either way.
		return
	}

	drift, err := s.qaDrift(sctx, qaRun)
	if err != nil {
		// An unreadable diff cannot be reported as "no drift": that would pass off
		// a verdict about one commit as a verdict about another on the strength of
		// a git failure. Say what happened and try again on the next poll.
		sctx.Log(fmt.Sprintf("warning: QA verified %s but this PR is at %s, and the diff between them could not be read (%v) - the QA verdict cannot be trusted for this head",
			qadrift.ShortSHA(qaRun.HeadSHA), qadrift.ShortSHA(sctx.Run.HeadSHA), err))
		return
	}

	if !drift.Stale() {
		if s.notedFreshQA == qaRun.ID {
			return
		}
		s.notedFreshQA = qaRun.ID
		sctx.Log(qadrift.FreshNote(drift))
		return
	}

	posted, err := sctx.DB.QANoticePosted(pr.URL, qaRun.ID, sctx.Run.HeadSHA)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not check whether the QA staleness note was already posted: %v", err))
		return
	}
	if posted {
		return
	}
	if !host.Capabilities().PRComments {
		// The note cannot reach the PR. It must still reach the run's log, or a
		// stale verdict passes silently - which is the one outcome this whole
		// mechanism exists to prevent.
		if s.notedStaleQA != qaRun.ID {
			s.notedStaleQA = qaRun.ID
			sctx.Log(fmt.Sprintf("QA verified %s but this PR is at %s and %d product file(s) changed in between; %s cannot post PR comments, so this could not be published: %s",
				qadrift.ShortSHA(drift.QAHeadSHA), qadrift.ShortSHA(drift.HeadSHA), len(drift.Product), host.Provider(), strings.Join(drift.Product, ", ")))
		}
		return
	}
	if err := host.PostPRComment(sctx.Ctx, pr, qadrift.StaleNote(drift)); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not publish the QA staleness note to %s: %v", pr.URL, err))
		return
	}
	if err := sctx.DB.RecordQANotice(pr.URL, qaRun.ID, sctx.Run.HeadSHA); err != nil {
		// The comment is already on the PR. Failing to record it means a re-armed
		// watch run could post it once more - noisy, but not a lie. Log and move on.
		sctx.Log(fmt.Sprintf("warning: posted the QA staleness note but could not record it: %v", err))
	}
	sctx.Log(fmt.Sprintf("QA verified %s, this PR is at %s, and %d product file(s) changed in between - said so on the PR (QA was not re-run)",
		qadrift.ShortSHA(drift.QAHeadSHA), qadrift.ShortSHA(drift.HeadSHA), len(drift.Product)))
}

// qaDrift classifies what changed between the commit QA verified and the commit
// this watch run is watching.
//
// The diff is read from the gate bare repository, because a watch run owns no
// worktree and must not grow one: both commits were pushed to the gate (that is
// how the runs that produced them started), so the gate holds everything needed.
func (s *WatchStep) qaDrift(sctx *pipeline.StepContext, qaRun *db.Run) (qadrift.Drift, error) {
	gateDir := sctx.Paths.RepoDir(sctx.Repo.ID)
	changed, err := git.DiffNameOnly(sctx.Ctx, gateDir, qaRun.HeadSHA, sctx.Run.HeadSHA)
	if err != nil {
		return qadrift.Drift{}, err
	}
	behavioral, err := git.DiffNameOnlyIgnoringWhitespace(sctx.Ctx, gateDir, qaRun.HeadSHA, sctx.Run.HeadSHA)
	if err != nil {
		return qadrift.Drift{}, err
	}
	verdict := ""
	if qaRun.QAVerdict != nil {
		verdict = *qaRun.QAVerdict
	}
	return qadrift.Analyze(qaRun.HeadSHA, sctx.Run.HeadSHA, verdict, changed, behavioral), nil
}
