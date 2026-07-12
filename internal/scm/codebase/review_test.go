package codebase

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// realCommentListPayload is a real `bytedcli --json codebase mr comment list`
// response, captured from obric/coze-monorepo!6800 and trimmed to the fields
// this package reads. It is a fixture, not a guess: the CapitalCase item keys
// (Id/Status/Outdated/Comments) alongside the snake_case wrapper (data.threads)
// are exactly the casing split that has bitten this backend before, and only a
// captured payload keeps a hand-written struct honest about it.
const realCommentListPayload = `{"status":"success","data":{
  "repository":{"path":"obric/coze-monorepo","id":"978977"},
  "threads":[
    {"Id":"784300349852436","CommentableType":"merge_request","RepoId":"978977","Status":"resolved","Outdated":false,
     "Comments":[{"Id":"784300349852436","Content":"[P1] logic error in createTextNode","CreatedBy":{"Username":"Bits CodeGuard","Email":"x@bytedance.com"},"Position":null}]},
    {"Id":"784300349852999","CommentableType":"merge_request","RepoId":"978977","Status":"open","Outdated":true,
     "Comments":[{"Id":"784300349852999","Content":"this drops the error","CreatedBy":{"Username":"fanwenjie.fe"},"Position":{"NewPath":"apps/space/src/node-factory.ts","NewLine":332,"OldPath":"","OldLine":0}}]}
  ],
  "thread_count":2,"total_thread_count":2}}`

// realMRStatusReviewPayload is the review block of a real `bytedcli --json
// codebase mr status`, captured from obric/coze-monorepo!6951: every check
// green, and the MR still unmergeable because one person has not approved it.
const realMRStatusReviewPayload = `{"status":"success","data":{
  "merge_request":{"status":"open"},
  "mergeability":{"mergeable":false,"reason":"review_not_passed"},
  "review":{"status":"pending","approvals_required":0,"approved_by":[],"reviewers":[]},
  "check_runs":{"items":[{"Id":"1","Name":"Aime CodeReview","Status":"completed","Conclusion":"succeeded"}]}}}`

// fakeBytedcli returns a CmdFactory that replays payload on stdout, plus the
// node runtime warning bytedcli really does print on stderr (it contains a '['
// and would defeat brace-seeking if stderr were merged in).
func fakeBytedcli(t *testing.T, payload string) CmdFactory {
	t.Helper()
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sh", "-c",
			`printf '%s' "$PAYLOAD"; printf '[UNDICI-EHPA] Warning: experimental\n' >&2`)
		cmd.Env = append(os.Environ(), "PAYLOAD="+payload)
		return cmd
	}
}

func TestListReviewThreads_ParsesRealPayload(t *testing.T) {
	h := New(fakeBytedcli(t, realCommentListPayload), func() bool { return true }, "code.byted.org", "obric/coze-monorepo")

	threads, err := h.ListReviewThreads(context.Background(), &scm.PR{Number: "6800"})
	if err != nil {
		t.Fatalf("list review threads: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(threads))
	}
	if !threads[0].Resolved {
		t.Error("thread with Status=resolved must read as resolved")
	}
	if threads[0].Author != "Bits CodeGuard" {
		t.Errorf("author = %q, want the bot's username", threads[0].Author)
	}

	open := threads[1]
	if open.Resolved {
		t.Error("thread with Status=open must read as unresolved")
	}
	if !open.Outdated {
		t.Error("Outdated must be carried through: the thread still counts, but its code moved")
	}
	if open.Author != "fanwenjie.fe" {
		t.Errorf("author = %q, want fanwenjie.fe", open.Author)
	}
	if open.File != "apps/space/src/node-factory.ts" || open.Line != 332 {
		t.Errorf("location = %s:%d, want the Position's NewPath/NewLine", open.File, open.Line)
	}
	if open.Body != "this drops the error" {
		t.Errorf("body = %q", open.Body)
	}

	unresolved := scm.UnresolvedThreads(threads)
	if len(unresolved) != 1 || unresolved[0].ID != "784300349852999" {
		t.Fatalf("unresolved = %+v, want only the open thread", unresolved)
	}
}

// TestThreadResolved_UnknownStatusIsUnresolved: a thread the backend cannot
// classify must escalate, never silently pass.
func TestThreadResolved_UnknownStatusIsUnresolved(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{
		"resolved":   true,
		"RESOLVED":   true,
		"closed":     true,
		"open":       false,
		"":           false,
		"some_state": false,
	} {
		if got := threadResolved(status); got != want {
			t.Errorf("threadResolved(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestGetReviewState_ParsesRealPayload(t *testing.T) {
	h := New(fakeBytedcli(t, realMRStatusReviewPayload), func() bool { return true }, "code.byted.org", "obric/coze-monorepo")

	state, err := h.GetReviewState(context.Background(), &scm.PR{Number: "6951"})
	if err != nil {
		t.Fatalf("get review state: %v", err)
	}
	if state != scm.ReviewStatePending {
		t.Fatalf("review state = %q, want PENDING", state)
	}
	if !state.Blocked() {
		t.Fatal("a pending review blocks the merge; that is the whole point of reading it")
	}

	// The same payload still reports its checks green and its mergeability
	// blocked - the pairing that makes "green, but waiting on a person" visible.
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "6951"})
	if err != nil {
		t.Fatalf("get checks: %v", err)
	}
	if len(checks) != 1 || checks[0].Failing() {
		t.Fatalf("checks = %+v, want one passing check", checks)
	}
}

func TestNormalizeReviewState(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]scm.ReviewState{
		"approved":          scm.ReviewStateApproved,
		"pending":           scm.ReviewStatePending,
		"review_not_passed": scm.ReviewStatePending,
		"disapproved":       scm.ReviewStateChangesRequested,
		"":                  scm.ReviewStateUnknown,
		"weird":             scm.ReviewStateUnknown,
	} {
		if got := normalizeReviewState(status); got != want {
			t.Errorf("normalizeReviewState(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestCodebaseCapabilitiesDeclareReviewSignals(t *testing.T) {
	t.Parallel()
	caps := New(nil, func() bool { return true }, "code.byted.org", "o/r").Capabilities()
	if !caps.ReviewThreads || !caps.ReviewState {
		t.Fatalf("capabilities = %+v, want both review signals declared", caps)
	}
}
