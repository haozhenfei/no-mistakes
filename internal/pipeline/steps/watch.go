package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// maxThreadBodyChars bounds how much of a comment body reaches a finding. The
// body is a seed for a fix agent, not an archive; a bot's review comment can
// run to kilobytes of diff suggestion.
const maxThreadBodyChars = 600

// WatchStep is the whole of a watch run: it polls the PR its parent gate run
// opened and converges on one of three verdicts.
//
// It is a pure function of the PR's current state. It holds no worktree, writes
// no files, and touches no git ref, which is exactly why a watch run can be
// killed at any moment and re-armed from nothing: one more poll rebuilds the
// entire verdict. That is the property the old blocking `ci` step did not have,
// and it is why that step could be destroyed by an unrelated push (or a daemon
// restart) without leaving any record that a PR still owed a check.
//
// The signals are deliberately generic. Unresolved comment threads are one
// signal, not three: the same poll covers an automated QA agent's findings, a
// review bot's findings, and a human reviewer's - the watch run does not know
// or care which it is looking at, and must not, because the conservative
// handling is identical.
type WatchStep struct {
	// Test seams, matching CIStep's.
	pollIntervalOverride time.Duration
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
	checksGracePeriod    time.Duration
}

func (s *WatchStep) Name() types.StepName { return types.StepWatch }

func (s *WatchStep) gracePeriod() time.Duration {
	if s.checksGracePeriod > 0 {
		return s.checksGracePeriod
	}
	return defaultChecksGracePeriod
}

func (s *WatchStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		// A watch run exists because a PR exists. No URL means the daemon
		// derived it wrongly; fail loudly rather than silently watching
		// nothing.
		return nil, errors.New("watch run has no PR URL")
	}

	provider := scm.DetectProvider(sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown {
		provider = scm.DetectProvider(prURL)
	}
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping watch: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping watch: %v", err))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}

	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}
	if unlimited {
		sctx.Log(fmt.Sprintf("watching PR #%s (no timeout, until merged or closed)...", prNumber))
	} else {
		sctx.Log(fmt.Sprintf("watching PR #%s (idle timeout: %s)...", prNumber, timeout))
	}

	now := s.now
	if now == nil {
		now = time.Now
	}
	started := now()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		elapsed := now().Sub(started)
		if !unlimited && elapsed >= timeout {
			sctx.Log("watch idle timeout reached")
			return s.escalate(sctx, watchFindings(
				"watch timed out before the PR was merged or closed",
				[]Finding{{
					Severity:    "warning",
					Description: fmt.Sprintf("PR %s was neither merged nor closed within %s", prURL, timeout),
					Action:      types.ActionAskUser,
				}},
			), "watch timed out"), nil
		}

		signals, err := s.poll(sctx, host, pr)
		if err != nil {
			return nil, err
		}

		if outcome, done := s.converge(sctx, signals, prURL, elapsed); done {
			return outcome, nil
		}

		interval := s.pollIntervalOverride
		if interval == 0 {
			interval = pollInterval(elapsed)
		}
		if !unlimited {
			if remaining := timeout - now().Sub(started); remaining < interval {
				interval = remaining
			}
		}
		wait := s.waitForNextPoll
		if wait == nil {
			wait = func(ctx context.Context, d time.Duration) error {
				select {
				case <-time.After(d):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err := wait(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// watchSignals is everything one poll learned about the PR. Every field has an
// explicit "known" companion: a signal the provider could not report is never
// laundered into "fine" - it just holds the watch run open for another poll.
type watchSignals struct {
	state      scm.PRState
	stateKnown bool

	checks      []scm.Check
	checksKnown bool

	mergeConflict     bool
	mergeabilityKnown bool

	unresolved      []scm.ReviewThread
	threadsKnown    bool
	review          scm.ReviewState
	reviewKnown     bool
	unsupportedSigs []string
}

func (s *WatchStep) poll(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) (watchSignals, error) {
	ctx := sctx.Ctx
	var sig watchSignals

	state, err := host.GetPRState(ctx, pr)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
	} else {
		sig.state, sig.stateKnown = state, true
	}
	if sig.stateKnown && (state == scm.PRStateMerged || state == scm.PRStateClosed) {
		// Terminal. Skip the remaining calls; nothing they report can change
		// the verdict.
		return sig, nil
	}

	checks, err := host.GetChecks(ctx, pr)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
	} else {
		sig.checks, sig.checksKnown = checks, true
	}

	if host.Capabilities().MergeableState {
		mergeState, err := host.GetMergeableState(ctx, pr)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", err))
		} else if mergeState.Resolved() {
			sig.mergeConflict, sig.mergeabilityKnown = mergeState.Conflict(), true
		} else {
			sctx.Log(fmt.Sprintf("mergeable state still pending: %s", mergeState))
		}
	} else {
		sig.mergeabilityKnown = true // nothing to learn; do not hold the run open for it
	}

	if host.Capabilities().ReviewThreads {
		threads, err := host.ListReviewThreads(ctx, pr)
		if err != nil && !errors.Is(err, scm.ErrUnsupported) {
			sctx.Log(fmt.Sprintf("warning: could not list review threads: %v", err))
		} else if err == nil {
			sig.unresolved, sig.threadsKnown = scm.UnresolvedThreads(threads), true
		}
	} else {
		sig.unsupportedSigs = append(sig.unsupportedSigs, "review threads")
	}

	if host.Capabilities().ReviewState {
		review, err := host.GetReviewState(ctx, pr)
		if err != nil && !errors.Is(err, scm.ErrUnsupported) {
			sctx.Log(fmt.Sprintf("warning: could not check review state: %v", err))
		} else if err == nil {
			sig.review, sig.reviewKnown = review, true
		}
	} else {
		sig.unsupportedSigs = append(sig.unsupportedSigs, "approval state")
	}

	return sig, nil
}

// converge maps one poll's signals onto a verdict, or reports that the watch
// run should keep polling. The ordering is the policy:
//
//  1. merged/closed wins over everything: the PR is gone, nothing else matters.
//  2. Still-pending checks mean "not yet" - never judge a half-finished CI run.
//  3. Failing checks (or a merge conflict) are the machine's own mess and are
//     the one thing the pipeline may fix unattended, bounded by auto_fix.ci.
//  4. Unresolved comment threads escalate. Never auto-fix them: the same signal
//     carries a human reviewer's opinion, and silently rewriting code in
//     response to a person's comment - then resolving nothing - is the kind of
//     thing this tool exists to not do.
//  5. A green PR blocked only on approval escalates too. It is the state that
//     is invisible today and the one that actually strands PRs for hours.
func (s *WatchStep) converge(sctx *pipeline.StepContext, sig watchSignals, prURL string, elapsed time.Duration) (*pipeline.StepOutcome, bool) {
	if sig.stateKnown && sig.state == scm.PRStateMerged {
		sctx.Log("PR has been merged")
		return s.converged(sctx, "PR merged"), true
	}
	if sig.stateKnown && sig.state == scm.PRStateClosed {
		sctx.Log("PR has been closed")
		return s.converged(sctx, "PR closed"), true
	}

	if !sig.checksKnown || !sig.stateKnown || !sig.mergeabilityKnown {
		// A signal we could not read is not a signal that says "fine".
		return nil, false
	}

	failing := failingCheckNames(sig.checks)
	sort.Strings(failing)
	pending := hasPendingChecks(sig.checks)

	if pending {
		if len(failing) > 0 {
			sctx.Log("CI failures present but checks still running, waiting for all checks to complete...")
		} else {
			sctx.Log(ciChecksRunningMsg)
		}
		return nil, false
	}

	if len(failing) > 0 || sig.mergeConflict {
		desc := strings.Join(failing, ", ")
		if sig.mergeConflict {
			if desc != "" {
				desc += " + merge conflict"
			} else {
				desc = "merge conflict"
			}
		}
		findings := ciFailureFindings(failing, sig.mergeConflict, fmt.Sprintf("PR %s has failing CI: %s", prURL, desc))
		if sctx.Config.AutoFix.CI <= 0 {
			sctx.Log(fmt.Sprintf("CI issues detected: %s - auto-fix disabled, escalating for a human decision", desc))
			return s.escalate(sctx, findings, "CI failing; auto-fix disabled"), true
		}
		sctx.Log(fmt.Sprintf("CI issues detected: %s - deriving a fix run", desc))
		return s.fix(sctx, findings, fmt.Sprintf("CI failing: %s", desc)), true
	}

	if len(sig.unresolved) > 0 {
		sctx.Log(fmt.Sprintf("%d unresolved review thread(s) on the PR - escalating for a decision", len(sig.unresolved)))
		return s.escalate(sctx, threadFindings(sig.unresolved, prURL), fmt.Sprintf("%d unresolved review thread(s)", len(sig.unresolved))), true
	}

	if len(sig.checks) == 0 && elapsed < s.gracePeriod() {
		sctx.Log("no CI checks reported yet, waiting for checks to register...")
		return nil, false
	}

	if sig.reviewKnown && sig.review.Blocked() {
		sctx.Log(fmt.Sprintf("PR is green but blocked on review (%s) - escalating for a human", sig.review))
		return s.escalate(sctx, watchFindings(
			fmt.Sprintf("PR %s passed every check and is waiting on a human approval (%s)", prURL, sig.review),
			[]Finding{{
				Severity:    "info",
				Description: fmt.Sprintf("PR %s is mergeable and green, but review state is %s. Only a person can unblock it.", prURL, sig.review),
				Action:      types.ActionAskUser,
			}},
		), fmt.Sprintf("awaiting approval (%s)", sig.review)), true
	}

	// Green, no threads, approval either satisfied or not reported: nothing to
	// do but wait for the merge.
	if len(sig.checks) == 0 {
		sctx.Log(ciNoChecksPassedMsg)
	} else {
		sctx.Log(ciChecksPassedMsg)
	}
	if len(sig.unsupportedSigs) > 0 {
		sctx.Log(fmt.Sprintf("note: this provider does not report %s; the watch run cannot see them", strings.Join(sig.unsupportedSigs, " or ")))
	}
	return nil, false
}

func (s *WatchStep) converged(sctx *pipeline.StepContext, reason string) *pipeline.StepOutcome {
	sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{Action: pipeline.WatchConverged, Reason: reason})
	return &pipeline.StepOutcome{}
}

// fix records a machine-fixable verdict. The step itself completes: the daemon
// derives a new gate run from it, so the fix goes back through review, test,
// and lint before it can touch the PR again.
func (s *WatchStep) fix(sctx *pipeline.StepContext, findingsJSON, reason string) *pipeline.StepOutcome {
	sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{
		Action:       pipeline.WatchFix,
		Reason:       reason,
		FindingsJSON: findingsJSON,
	})
	return &pipeline.StepOutcome{Findings: findingsJSON}
}

// escalate parks the watch run awaiting the driving agent, with the findings
// attached. This is the same park the gate pipeline performs for a blocking
// review finding under auto_fix.review: 0, and it is deliberately the default
// for every non-CI signal.
func (s *WatchStep) escalate(sctx *pipeline.StepContext, findingsJSON, reason string) *pipeline.StepOutcome {
	sctx.Shared.SetWatchOutcome(pipeline.WatchOutcome{
		Action:       pipeline.WatchEscalate,
		Reason:       reason,
		FindingsJSON: findingsJSON,
	})
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   true, // a `fix` response derives a gate run; see RunManager.HandleRespondWithOverrides
		Findings:      findingsJSON,
	}
}

func watchFindings(summary string, items []Finding) string {
	data, _ := json.Marshal(Findings{Summary: summary, Items: items})
	return string(data)
}

// ciFailureFindings is the findings shape a failing-CI verdict seeds a fix run
// with. It mirrors what the old blocking ci step assembled, so a fix agent sees
// the same information it always did.
func ciFailureFindings(failing []string, mergeConflict bool, summary string) string {
	items := make([]Finding, 0, len(failing)+1)
	for _, name := range failing {
		items = append(items, Finding{
			Severity:    "error",
			Description: ciCheckFindingPrefix + name,
			Action:      types.ActionAutoFix,
		})
	}
	if mergeConflict {
		items = append(items, Finding{
			Severity:    "error",
			Description: "PR has merge conflicts with the base branch",
			Action:      types.ActionAutoFix,
		})
	}
	return watchFindings(summary, items)
}

// threadFindings turns unresolved comment threads into findings. They are
// ask-user, never auto-fix: see converge's rule 4.
func threadFindings(threads []scm.ReviewThread, prURL string) string {
	items := make([]Finding, 0, len(threads))
	for _, t := range threads {
		desc := fmt.Sprintf("Unresolved review thread")
		if t.Author != "" {
			desc += fmt.Sprintf(" from %s", t.Author)
		}
		if t.Outdated {
			desc += " (on code that has since changed)"
		}
		if body := truncateThreadBody(t.Body); body != "" {
			desc += ": " + body
		}
		items = append(items, Finding{
			Severity:    "warning",
			File:        t.File,
			Line:        t.Line,
			Description: desc,
			Action:      types.ActionAskUser,
		})
	}
	return watchFindings(
		fmt.Sprintf("PR %s has %d unresolved review thread(s). They may be from a person: decide, do not bulk-apply.", prURL, len(threads)),
		items,
	)
}

func truncateThreadBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	runes := []rune(body)
	if len(runes) <= maxThreadBodyChars {
		return body
	}
	return string(runes[:maxThreadBodyChars]) + "..."
}
