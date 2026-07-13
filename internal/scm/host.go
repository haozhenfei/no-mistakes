package scm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExtractHost returns the lowercased host (without any port) from a git
// remote URL. It handles both scp-like syntax (git@host:group/project) and
// URL forms (https://host/group/project, ssh://git@host:22/group/project).
// It returns "" when no host can be determined.
func ExtractHost(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		// URL form: scheme://[user@]host[:port]/path. Split off the path at the
		// first '/' before scanning for userinfo, so a '@' inside the path
		// (e.g. .../group@prod/repo.git) cannot be mistaken for a "user@" prefix.
		s = s[i+3:]
		if slash := strings.Index(s, "/"); slash >= 0 {
			s = s[:slash]
		}
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		return strings.ToLower(stripPort(s))
	}
	// No scheme. scp-like syntax is [user@]host:path; the first ':' separates
	// the host from the path. Split off the path first, then strip any userinfo
	// prefix from the host segment only, so a '@' in the path (e.g.
	// git@host:group@prod/repo.git) cannot collapse host extraction.
	if c := strings.Index(s, ":"); c >= 0 {
		s = s[:c]
	} else if slash := strings.Index(s, "/"); slash >= 0 {
		s = s[:slash]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.ToLower(s)
}

// stripPort removes a trailing :port from a host, leaving bare hosts and
// bracketed IPv6 literals intact.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: [::1]:22 -> [::1]
		if end := strings.Index(host, "]"); end >= 0 {
			return host[:end+1]
		}
		return host
	}
	if c := strings.LastIndex(host, ":"); c >= 0 {
		port := host[c+1:]
		if port != "" && strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return host[:c]
		}
	}
	return host
}

// ExtractPRNumber returns the trailing numeric segment from a PR/MR URL.
// Supports GitHub (/pull/N), GitLab (/-/merge_requests/N), Bitbucket
// (/pull-requests/N), and Azure DevOps (/pullrequest/N) URLs; all of them
// end in a digit path segment.
func ExtractPRNumber(prURL string) (string, error) {
	trimmed := strings.TrimRight(prURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	num := parts[len(parts)-1]
	if num == "" {
		return "", fmt.Errorf("invalid PR URL: %s", prURL)
	}
	if _, err := strconv.Atoi(num); err != nil {
		return "", fmt.Errorf("invalid PR number %q in URL: %s", num, prURL)
	}
	return num, nil
}

// PR identifies a pull/merge request on a provider.
type PR struct {
	Number string
	URL    string
}

// PRContent is the title + body for creating or updating a PR.
type PRContent struct {
	Title string
	Body  string
}

// PRState is the normalized lifecycle state of a PR.
type PRState string

const (
	PRStateOpen   PRState = "OPEN"
	PRStateMerged PRState = "MERGED"
	PRStateClosed PRState = "CLOSED"
)

// MergeableState is the normalized merge-conflict status of a PR.
type MergeableState string

const (
	MergeableOK       MergeableState = "MERGEABLE"
	MergeableConflict MergeableState = "CONFLICTING"
	MergeablePending  MergeableState = "PENDING"
	MergeableUnknown  MergeableState = "UNKNOWN"
)

// Conflict reports whether the state indicates a known merge conflict.
func (s MergeableState) Conflict() bool { return s == MergeableConflict }

// Resolved reports whether the state is final (MERGEABLE or CONFLICTING).
func (s MergeableState) Resolved() bool {
	return s == MergeableOK || s == MergeableConflict
}

// CheckBucket is the normalized outcome of a CI check.
type CheckBucket string

const (
	CheckBucketPass    CheckBucket = "pass"
	CheckBucketFail    CheckBucket = "fail"
	CheckBucketPending CheckBucket = "pending"
	CheckBucketCancel  CheckBucket = "cancel"
	CheckBucketSkip    CheckBucket = "skipping"
)

// Check is a single CI check result on a PR.
type Check struct {
	Name        string
	Bucket      CheckBucket
	CompletedAt time.Time // zero when unknown; used to detect CI re-runs between polls
}

// Failing reports whether the check is in a failed bucket.
func (c Check) Failing() bool { return c.Bucket == CheckBucketFail }

// Pending reports whether the check is still running or queued.
func (c Check) Pending() bool { return c.Bucket == CheckBucketPending }

// ReviewThread is one comment thread on a PR. It is deliberately
// provider-agnostic and deliberately says nothing about who opened it: an
// unresolved thread from a bot, from an automated QA agent, and from a human
// reviewer are the same signal, and the watch run treats them the same.
type ReviewThread struct {
	ID       string
	Resolved bool
	Outdated bool // the thread's code has since changed; still unresolved
	Author   string
	File     string
	Line     int
	Body     string // first comment's text
}

// ReviewState is the normalized approval status of a PR.
type ReviewState string

const (
	// ReviewStateApproved means the PR has the approvals it needs to merge.
	ReviewStateApproved ReviewState = "APPROVED"
	// ReviewStatePending means the PR is waiting on a required approval. This
	// is the state a fully-green PR sits in while it waits for a human, and it
	// is invisible unless approval is treated as a first-class signal.
	ReviewStatePending ReviewState = "PENDING"
	// ReviewStateChangesRequested means a reviewer asked for changes.
	ReviewStateChangesRequested ReviewState = "CHANGES_REQUESTED"
	// ReviewStateUnknown means the provider did not report a state.
	ReviewStateUnknown ReviewState = "UNKNOWN"
)

// Blocked reports whether the review state, on its own, prevents a merge.
func (s ReviewState) Blocked() bool {
	return s == ReviewStatePending || s == ReviewStateChangesRequested
}

// UnresolvedThreads returns the threads that still need someone's attention.
func UnresolvedThreads(threads []ReviewThread) []ReviewThread {
	var out []ReviewThread
	for _, t := range threads {
		if !t.Resolved {
			out = append(out, t)
		}
	}
	return out
}

// Capabilities declares which optional Host methods return meaningful data.
// Callers must consult Capabilities before invoking optional methods.
type Capabilities struct {
	MergeableState  bool
	FailedCheckLogs bool
	ReviewThreads   bool
	ReviewState     bool
	// PRComments reports whether the host can post a top-level comment on a PR.
	// The qa step needs it to publish its report where reviewers already look;
	// without it QA still records its report on the run, and says so rather than
	// pretending it published.
	PRComments bool
}

// ErrUnsupported is returned by optional Host methods that the provider
// cannot fulfil. Callers should gate calls on Capabilities rather than
// relying on this error, but implementations return it as a fallback.
var ErrUnsupported = errors.New("operation not supported by this provider")

// Host is the provider-agnostic interface to a PR-hosting service.
// Transport (CLI vs HTTP API) is an implementation detail.
type Host interface {
	Provider() Provider
	Capabilities() Capabilities

	// Available returns nil when the host is ready to use, or a descriptive
	// error explaining why it is not (missing CLI, unauthenticated, etc).
	Available(ctx context.Context) error

	// FindPR returns the open PR for the source branch, or nil if none exists.
	FindPR(ctx context.Context, branch, base string) (*PR, error)
	CreatePR(ctx context.Context, branch, base string, content PRContent) (*PR, error)
	UpdatePR(ctx context.Context, pr *PR, content PRContent) (*PR, error)

	GetPRState(ctx context.Context, pr *PR) (PRState, error)
	GetChecks(ctx context.Context, pr *PR) ([]Check, error)

	// GetMergeableState is optional; implementations without Capabilities().MergeableState
	// must return ErrUnsupported. Callers should consult Capabilities first.
	GetMergeableState(ctx context.Context, pr *PR) (MergeableState, error)

	// FetchFailedCheckLogs is optional; returns "" when no logs can be retrieved
	// and ErrUnsupported when the provider has no log-fetching support at all.
	FetchFailedCheckLogs(ctx context.Context, pr *PR, branch, headSHA string, failingNames []string) (string, error)

	// ListReviewThreads is optional; implementations without
	// Capabilities().ReviewThreads must return ErrUnsupported.
	ListReviewThreads(ctx context.Context, pr *PR) ([]ReviewThread, error)

	// GetReviewState is optional; implementations without
	// Capabilities().ReviewState must return ErrUnsupported.
	GetReviewState(ctx context.Context, pr *PR) (ReviewState, error)

	// PostPRComment posts a top-level comment on the PR. It is optional;
	// implementations without Capabilities().PRComments must return
	// ErrUnsupported.
	//
	// A posted comment is an unresolved review thread on the hosts that model
	// threads (see ListReviewThreads), which a watch run reads as "someone is
	// waiting on a human". That is deliberate for a QA report that found
	// something, and the reason the qa step does not post a clean PASS.
	PostPRComment(ctx context.Context, pr *PR, body string) error
}
