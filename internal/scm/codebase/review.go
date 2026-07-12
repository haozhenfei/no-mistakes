package codebase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Review threads and approval state on Codebase.
//
// Both payloads were captured from a live MR (obric/coze-monorepo!6800 for a
// thread, !6951 for a pending-approval review block); the field names below are
// what bytedcli actually returns, not a guess:
//
//   - `mr comment list` puts threads under data.threads with CapitalCase item
//     fields (Id, Status, Outdated, Comments[]), the same casing quirk `mr
//     list`/`mr get` have. Status is the thread's own lifecycle
//     (open/resolved/closed), which is what --status filters on.
//   - `mr status` carries the approval block under data.review with lowercase
//     fields (status, approvals_required, approved_by[]). A fully-green MR that
//     is only waiting on an owner shows review.status "pending" and
//     mergeability.reason "review_not_passed" - that pairing is exactly the
//     "green but nobody can merge it" state, and it is invisible unless
//     approval is read as its own signal.
type commentListResponse struct {
	Data struct {
		Threads []threadItem `json:"threads"`
	} `json:"data"`
}

type threadItem struct {
	ID       string        `json:"Id"`
	Status   string        `json:"Status"`
	Outdated bool          `json:"Outdated"`
	Comments []commentItem `json:"Comments"`
}

type commentItem struct {
	Content   string `json:"Content"`
	CreatedBy struct {
		Username string `json:"Username"`
	} `json:"CreatedBy"`
	Position *struct {
		NewPath string `json:"NewPath"`
		OldPath string `json:"OldPath"`
		NewLine int    `json:"NewLine"`
		OldLine int    `json:"OldLine"`
	} `json:"Position"`
}

// ListReviewThreads returns every comment thread on the MR. Resolution is read
// from the thread's own Status; a thread whose status is not "resolved" (and
// not "closed") still needs someone's attention, whoever opened it.
func (h *Host) ListReviewThreads(ctx context.Context, pr *scm.PR) ([]scm.ReviewThread, error) {
	id := prSelector(pr)
	if id == "" {
		return nil, errors.New("bytedcli codebase mr comment list: no MR number")
	}
	args := h.repoArgs([]string{"--json", "codebase", "mr", "comment", "list", id})
	out, err := h.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr comment list: %w", err)
	}
	trimmed := trimToJSON(out)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("bytedcli codebase mr comment list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var resp commentListResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr comment list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	threads := make([]scm.ReviewThread, 0, len(resp.Data.Threads))
	for _, t := range resp.Data.Threads {
		thread := scm.ReviewThread{
			ID:       t.ID,
			Resolved: threadResolved(t.Status),
			Outdated: t.Outdated,
		}
		if len(t.Comments) > 0 {
			first := t.Comments[0]
			thread.Author = first.CreatedBy.Username
			thread.Body = first.Content
			if first.Position != nil {
				thread.File = first.Position.NewPath
				thread.Line = first.Position.NewLine
				if thread.File == "" {
					thread.File = first.Position.OldPath
					thread.Line = first.Position.OldLine
				}
			}
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

// threadResolved maps a Codebase thread status onto "does anyone still owe this
// thread a response". An unknown status counts as unresolved: a thread the
// watch run cannot classify must escalate, never silently pass.
func threadResolved(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved", "closed":
		return true
	default:
		return false
	}
}

// GetReviewState reports whether the MR has the approvals it needs. It reads
// the same `mr status` call GetPRState/GetChecks use.
func (h *Host) GetReviewState(ctx context.Context, pr *scm.PR) (scm.ReviewState, error) {
	st, err := h.mrStatus(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizeReviewState(st.Data.Review.Status), nil
}

func normalizeReviewState(status string) scm.ReviewState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved", "passed":
		return scm.ReviewStateApproved
	case "pending", "reviewing", "not_passed", "review_not_passed":
		return scm.ReviewStatePending
	case "disapproved", "rejected", "changes_requested":
		return scm.ReviewStateChangesRequested
	default:
		return scm.ReviewStateUnknown
	}
}
