package pipeline

// WatchAction is what the daemon must do once a watch run's poller has
// converged on the PR's state.
type WatchAction string

const (
	// WatchConverged: the PR reached a terminal state (merged or closed).
	// Nothing more to do; the watch run is finished.
	WatchConverged WatchAction = "converged"

	// WatchFix: the PR has a problem the pipeline is allowed to fix on its own
	// (today: failing CI checks, within auto_fix.ci). The daemon derives a new
	// gate run seeded with these findings - it does NOT patch the branch from
	// inside the watch run, so the fix re-crosses review/test/lint before it
	// reaches the PR again.
	WatchFix WatchAction = "fix"

	// WatchEscalate: the PR needs a person. This is the conservative default
	// for anything that is not a plain CI failure - unresolved comment threads
	// (which mix bots, QA agents, and human reviewers indistinguishably) and a
	// blocked approval both land here. The watch run parks and the driving
	// agent decides, exactly as auto_fix.review: 0 makes a blocking review
	// finding park today.
	WatchEscalate WatchAction = "escalate"
)

// WatchOutcome is the watch step's verdict about a PR, handed to the daemon
// after the run finishes. It is in-memory only (see RunShared): a crash simply
// re-arms the watch run, which re-derives the same verdict from the live PR.
type WatchOutcome struct {
	Action WatchAction
	// Reason is a one-line, user-facing explanation of the verdict.
	Reason string
	// FindingsJSON is the seed a derived fix gate run works from. It is also
	// persisted on the watch step's row, which is where a fix run actually
	// reads it from (via its parent_run_id) - this copy just saves the daemon a
	// query when it derives the run in-process.
	FindingsJSON string
}
