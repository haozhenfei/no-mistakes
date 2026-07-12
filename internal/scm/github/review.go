package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// reviewThreadsQuery reads the PR's review threads with their resolution state.
// `gh pr view` cannot report this - resolution lives only on the GraphQL
// reviewThreads connection - so this is the one place the GitHub host reaches
// past the porcelain commands.
const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!){` +
	`repository(owner:$owner,name:$name){pullRequest(number:$number){` +
	`reviewThreads(first:100){nodes{id isResolved isOutdated ` +
	`comments(first:1){nodes{author{login} body path line}}}}}}}`

type reviewThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Comments   struct {
							Nodes []struct {
								Author *struct {
									Login string `json:"login"`
								} `json:"author"`
								Body string `json:"body"`
								Path string `json:"path"`
								Line *int   `json:"line"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// ListReviewThreads returns the PR's review threads. It needs the owner and
// name separately (GraphQL variables, not the --repo slug), so it degrades to
// ErrUnsupported when the slug is unknown rather than querying the wrong repo.
func (h *Host) ListReviewThreads(ctx context.Context, pr *scm.PR) ([]scm.ReviewThread, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repoPathSlug(h.repo)), "/")
	if !ok || owner == "" || name == "" {
		return nil, scm.ErrUnsupported
	}
	if strings.TrimSpace(pr.Number) == "" {
		return nil, fmt.Errorf("gh api graphql: no PR number")
	}
	cmd := h.cmd(ctx, "gh", "api", "graphql",
		"-F", "owner="+owner,
		"-F", "name="+name,
		"-F", "number="+pr.Number,
		"-f", "query="+reviewThreadsQuery,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api graphql reviewThreads: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var resp reviewThreadsResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("gh api graphql reviewThreads: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	nodes := resp.Data.Repository.PullRequest.ReviewThreads.Nodes
	threads := make([]scm.ReviewThread, 0, len(nodes))
	for _, n := range nodes {
		thread := scm.ReviewThread{
			ID:       n.ID,
			Resolved: n.IsResolved,
			Outdated: n.IsOutdated,
		}
		if len(n.Comments.Nodes) > 0 {
			first := n.Comments.Nodes[0]
			if first.Author != nil {
				thread.Author = first.Author.Login
			}
			thread.Body = first.Body
			thread.File = first.Path
			if first.Line != nil {
				thread.Line = *first.Line
			}
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

// repoPathSlug strips any GHE host prefix ("host/owner/name" -> "owner/name"),
// since GraphQL variables take the owner and name only.
func repoPathSlug(slug string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(slug), "/"), "/")
	if len(parts) <= 2 {
		return strings.Join(parts, "/")
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// GetReviewState reports the PR's approval status. An empty reviewDecision
// means the repo requires no review, which cannot block a merge - that is
// APPROVED for our purposes, not "unknown".
func (h *Host) GetReviewState(ctx context.Context, pr *scm.PR) (scm.ReviewState, error) {
	if strings.TrimSpace(pr.Number) == "" {
		return "", fmt.Errorf("gh pr view: no PR number")
	}
	args := []string{"pr", "view", pr.Number}
	args = append(args, h.repoArgs()...)
	args = append(args, "--json", "reviewDecision")
	cmd := h.cmd(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr view --json reviewDecision: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var payload struct {
		ReviewDecision string `json:"reviewDecision"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("gh pr view --json reviewDecision: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	switch strings.ToUpper(strings.TrimSpace(payload.ReviewDecision)) {
	case "APPROVED", "":
		return scm.ReviewStateApproved, nil
	case "CHANGES_REQUESTED":
		return scm.ReviewStateChangesRequested, nil
	case "REVIEW_REQUIRED":
		return scm.ReviewStatePending, nil
	default:
		return scm.ReviewStateUnknown, nil
	}
}
