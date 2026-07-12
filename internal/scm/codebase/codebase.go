// Package codebase implements scm.Host backed by the bytedcli CLI, targeting
// ByteDance Codebase (code.byted.org / code-tx.byted.org). All calls prefer
// `bytedcli --json codebase ...` so the machine-readable payload can be parsed,
// except FetchFailedCheckLogs, which streams raw log text.
package codebase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to ByteDance Codebase through the bytedcli CLI.
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	host         string // repo's Codebase hostname; used to build MR URLs
	repo         string // "owner/repo" path (nested namespaces allowed)
}

// New builds a Host. cliAvailable reports whether the bytedcli binary is
// resolvable on the caller's PATH (possibly overridden by env). host is the
// repo's Codebase hostname (code.byted.org or code-tx.byted.org); it is used to
// synthesize MR web URLs since `mr list`/`mr create` do not always return one.
// repo is the "owner/repo" path (subgroups/nested namespaces allowed) passed to
// bytedcli via -R. Both are optional; empty host defaults to code.byted.org for
// URL construction, and an empty repo lets bytedcli infer it from the git origin.
func New(cmd CmdFactory, cliAvailable func() bool, host, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		host:         strings.TrimSpace(host),
		repo:         strings.TrimSpace(repo),
	}
}

// RepoSlug extracts the "owner/repo" path (no host, no trailing .git) from a
// Codebase remote URL. Codebase projects can live under nested namespaces, so
// the full path - not just the last two segments - is returned. It handles
// HTTPS/ssh:// URLs and scp-style SSH (git@code.byted.org:owner/repo.git).
// Returns "" when no path can be determined; callers treat that as "unknown"
// and let bytedcli infer -R from the git origin.
func RepoSlug(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var path string
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil {
			path = u.Path
		}
	} else if colon := strings.Index(raw, ":"); colon >= 0 && !isWindowsDrivePath(raw) {
		// scp-style: [user@]host:owner/repo.git -> owner/repo. The first ':'
		// separates host from path, recovered whether or not a "user@" prefix
		// is present. A Windows drive-letter path (C:\...) carries a colon too
		// but is a local filesystem path, not a remote URL, so it is excluded.
		path = raw[colon+1:]
	}
	path = strings.Trim(path, "/")
	return strings.TrimSuffix(path, ".git")
}

// isWindowsDrivePath reports whether raw begins with a Windows drive specifier
// like "C:\..." or "C:/...". Such a path's drive-letter colon must not be
// mistaken for the host:path separator of scp-style SSH syntax.
func isWindowsDrivePath(raw string) bool {
	if len(raw) < 2 || raw[1] != ':' {
		return false
	}
	c := raw[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	return len(raw) == 2 || raw[2] == '\\' || raw[2] == '/'
}

func (h *Host) Provider() scm.Provider { return scm.ProviderCodebase }

func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
}

// repoArgs appends -R <repo> when a repo slug is known. When it is empty,
// bytedcli infers the repo from the current git origin.
func (h *Host) repoArgs(args []string) []string {
	if h.repo != "" {
		args = append(args, "-R", h.repo)
	}
	return args
}

// hostName returns the Codebase hostname used to build MR URLs, defaulting to
// code.byted.org when unknown.
func (h *Host) hostName() string {
	if h.host != "" {
		return h.host
	}
	return "code.byted.org"
}

// mrURL builds the web URL for an MR number from the configured host and repo.
// Returns "" when the repo slug is unknown (nothing to point at).
func (h *Host) mrURL(number string) string {
	if h.repo == "" || strings.TrimSpace(number) == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/merge_requests/%s", h.hostName(), h.repo, number)
}

// runJSON runs a bytedcli command, capturing stdout and stderr separately.
// bytedcli emits its node runtime warnings (e.g. the UNDICI-EHPA experimental
// notice) on stderr, so parsing stdout alone yields clean JSON without a
// preamble. The returned error, on non-zero exit, carries the trimmed stderr.
func (h *Host) runJSON(ctx context.Context, args ...string) ([]byte, error) {
	cmd := h.cmd(ctx, "bytedcli", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("%s: %w", msg, err)
	}
	return stdout.Bytes(), nil
}

// authStatus is the shape of `bytedcli --json auth status`.
type authStatus struct {
	Status string `json:"status"`
	Data   struct {
		Authenticated bool `json:"authenticated"`
	} `json:"data"`
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("bytedcli is not installed")
	}
	// `bytedcli --json auth status` reports overall ByteCloud authentication,
	// which is what Codebase API calls rely on. Parse data.authenticated rather
	// than trusting the exit code alone.
	out, err := h.runJSON(ctx, "--json", "auth", "status")
	if err != nil {
		return errors.New("bytedcli is not authenticated")
	}
	trimmed := trimToJSON(out)
	if len(trimmed) == 0 {
		return errors.New("bytedcli auth status returned no JSON")
	}
	var st authStatus
	if json.Unmarshal(trimmed, &st) != nil || !st.Data.Authenticated {
		return errors.New("bytedcli is not authenticated")
	}
	return nil
}

// mrListItem is one entry of `bytedcli --json codebase mr list`. Codebase uses
// CapitalCase field names here and leaves URL empty, so callers synthesize the
// web URL from the number.
type mrListItem struct {
	Number           int    `json:"Number"`
	Status           string `json:"Status"`
	SourceBranchName string `json:"SourceBranchName"`
	TargetBranchName string `json:"TargetBranchName"`
	Title            string `json:"Title"`
	URL              string `json:"URL"`
}

type mrListResponse struct {
	Data struct {
		MergeRequests []mrListItem `json:"merge_requests"`
	} `json:"data"`
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"--json", "codebase", "mr", "list", "--state", "open", "--head", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--base", base)
	}
	args = append(args, "-L", "20")
	args = h.repoArgs(args)
	out, err := h.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr list: %w", err)
	}
	trimmed := trimToJSON(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var resp mrListResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr list: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	// bytedcli filters by --head server-side, but guard defensively so a stale
	// or fuzzy match on another branch cannot be picked up as this branch's MR.
	for _, mr := range resp.Data.MergeRequests {
		if mr.SourceBranchName != "" && mr.SourceBranchName != branch {
			continue
		}
		return h.itemToPR(mr), nil
	}
	return nil, nil
}

func (h *Host) itemToPR(mr mrListItem) *scm.PR {
	number := ""
	if mr.Number > 0 {
		number = fmt.Sprintf("%d", mr.Number)
	}
	prURL := strings.TrimSpace(mr.URL)
	if prURL == "" {
		prURL = h.mrURL(number)
	}
	return &scm.PR{Number: number, URL: prURL}
}

// mrCreateMR is the MR object inside a create/get payload. bytedcli is
// inconsistent about field casing across commands, so both spellings are parsed.
type mrCreateMR struct {
	Number      int    `json:"Number"`
	NumberLower int    `json:"number"`
	URL         string `json:"URL"`
	URLLower    string `json:"url"`
}

// mrCreateResponse covers the create/get payload. The *wrapper key* casing also
// drifts per command, and Go's case-insensitive field match does NOT bridge the
// two spellings because of the underscore: a `json:"merge_request"` tag silently
// parses a `data.MergeRequest` payload as a zero value. Observed against
// bytedcli on code.byted.org:
//
//	mr create → data.MergeRequest  (CapitalCase)
//	mr get    → data.merge_request (snake_case, CapitalCase fields)
//	mr status → data.merge_request (snake_case, lowercase fields)
//
// So accept BOTH wrapper keys rather than betting on one: guessing this shape
// instead of capturing it is what let a successfully created MR be reported as a
// hard failure, which failed the run and meant the `ci` step never ran.
type mrCreateResponse struct {
	Data struct {
		MergeRequest        mrCreateMR `json:"merge_request"`
		MergeRequestCapital mrCreateMR `json:"MergeRequest"`
	} `json:"data"`
}

// mr returns whichever wrapper key the CLI actually populated.
func (r mrCreateResponse) mr() mrCreateMR {
	if r.Data.MergeRequest != (mrCreateMR{}) {
		return r.Data.MergeRequest
	}
	return r.Data.MergeRequestCapital
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := []string{"--json", "codebase", "mr", "create",
		"--head", branch,
		"--base", base,
		"--title", content.Title,
		"--body", content.Body,
	}
	args = h.repoArgs(args)
	out, err := h.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr create: %w", err)
	}
	number, prURL := parseCreatedMR(out)
	if number == "" && prURL == "" {
		return nil, fmt.Errorf("bytedcli codebase mr create: could not resolve created MR from output: %s", strings.TrimSpace(string(out)))
	}
	if prURL == "" {
		prURL = h.mrURL(number)
	}
	if number == "" {
		if n, nerr := scm.ExtractPRNumber(prURL); nerr == nil {
			number = n
		}
	}
	return &scm.PR{Number: number, URL: prURL}, nil
}

func parseCreatedMR(out []byte) (number, prURL string) {
	trimmed := trimToJSON(out)
	if len(trimmed) == 0 {
		return "", ""
	}
	var resp mrCreateResponse
	if json.Unmarshal(trimmed, &resp) != nil {
		return "", ""
	}
	mr := resp.mr()
	n := mr.Number
	if n == 0 {
		n = mr.NumberLower
	}
	if n > 0 {
		number = fmt.Sprintf("%d", n)
	}
	prURL = strings.TrimSpace(mr.URL)
	if prURL == "" {
		prURL = strings.TrimSpace(mr.URLLower)
	}
	return number, prURL
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := prSelector(pr)
	if id == "" {
		return nil, errors.New("bytedcli codebase mr update: no MR number to update")
	}
	args := []string{"codebase", "mr", "update", id,
		"--title", content.Title,
		"--body", content.Body,
	}
	args = h.repoArgs(args)
	if out, err := h.cmd(ctx, "bytedcli", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("bytedcli codebase mr update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return pr, nil
}

// prSelector returns the MR number to pass to bytedcli, preferring the parsed
// number and falling back to the trailing number in the URL.
func prSelector(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if strings.TrimSpace(pr.Number) != "" {
		return pr.Number
	}
	if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
		return num
	}
	return ""
}

// mrStatusResponse is the payload of `bytedcli --json codebase mr status`,
// which returns MR lifecycle, mergeability, and check runs in one call.
type mrStatusResponse struct {
	Data struct {
		MergeRequest struct {
			Status string `json:"status"`
		} `json:"merge_request"`
		Mergeability struct {
			Mergeable bool   `json:"mergeable"`
			Reason    string `json:"reason"`
			Detail    struct {
				Mergeable         bool   `json:"Mergeable"`
				UnmergeableReason string `json:"UnmergeableReason"`
			} `json:"detail"`
		} `json:"mergeability"`
		CheckRuns struct {
			Items []checkRunItem `json:"items"`
		} `json:"check_runs"`
	} `json:"data"`
}

type checkRunItem struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	Conclusion  string `json:"Conclusion"`
	CompletedAt string `json:"CompletedAt"`
}

func (i checkRunItem) completedAt() time.Time {
	if strings.TrimSpace(i.CompletedAt) == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339, i.CompletedAt); err == nil {
		return parsed
	}
	return time.Time{}
}

func (h *Host) mrStatus(ctx context.Context, pr *scm.PR) (mrStatusResponse, error) {
	id := prSelector(pr)
	if id == "" {
		return mrStatusResponse{}, errors.New("bytedcli codebase mr status: no MR number")
	}
	args := h.repoArgs([]string{"--json", "codebase", "mr", "status", id})
	out, err := h.runJSON(ctx, args...)
	if err != nil {
		return mrStatusResponse{}, fmt.Errorf("bytedcli codebase mr status: %w", err)
	}
	trimmed := trimToJSON(out)
	if len(trimmed) == 0 {
		return mrStatusResponse{}, fmt.Errorf("bytedcli codebase mr status: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	var resp mrStatusResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return mrStatusResponse{}, fmt.Errorf("bytedcli codebase mr status: invalid JSON output: %s", strings.TrimSpace(string(out)))
	}
	return resp, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	st, err := h.mrStatus(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizePRState(st.Data.MergeRequest.Status), nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	st, err := h.mrStatus(ctx, pr)
	if err != nil {
		return "", err
	}
	m := st.Data.Mergeability
	if m.Mergeable {
		return scm.MergeableOK, nil
	}
	reason := strings.ToLower(strings.TrimSpace(m.Reason))
	if reason == "" {
		reason = strings.ToLower(strings.TrimSpace(m.Detail.UnmergeableReason))
	}
	switch {
	case strings.Contains(reason, "conflict"):
		return scm.MergeableConflict, nil
	case reason == "" || strings.Contains(reason, "checking") ||
		strings.Contains(reason, "pending") || strings.Contains(reason, "unchecked") ||
		strings.Contains(reason, "running"):
		return scm.MergeablePending, nil
	default:
		// closed/merged/approvals-required and other non-conflict reasons: not a
		// merge conflict, so do not trigger a rebase. Report OK; PR lifecycle
		// state (GetPRState) governs whether the run should stop.
		return scm.MergeableOK, nil
	}
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	st, err := h.mrStatus(ctx, pr)
	if err != nil {
		return nil, err
	}
	items := st.Data.CheckRuns.Items
	checks := make([]scm.Check, 0, len(items))
	for _, it := range items {
		checks = append(checks, scm.Check{
			Name:        it.Name,
			Bucket:      checkBucket(it.Status, it.Conclusion),
			CompletedAt: it.completedAt(),
		})
	}
	return checks, nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, _ string, _ string, failingNames []string) (string, error) {
	if len(failingNames) == 0 {
		return "", nil
	}
	st, err := h.mrStatus(ctx, pr)
	if err != nil {
		return "", nil
	}
	targets := map[string]struct{}{}
	for _, name := range failingNames {
		if name = strings.TrimSpace(name); name != "" {
			targets[name] = struct{}{}
		}
	}
	var b strings.Builder
	for _, it := range st.Data.CheckRuns.Items {
		if checkBucket(it.Status, it.Conclusion) != scm.CheckBucketFail {
			continue
		}
		if _, ok := targets[it.Name]; !ok && len(targets) > 0 {
			continue
		}
		if strings.TrimSpace(it.ID) == "" {
			continue
		}
		logText := h.fetchCheckLog(ctx, it.ID)
		if logText == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(fmt.Sprintf("=== %s ===\n", it.Name))
		b.WriteString(logText)
	}
	return strings.TrimSpace(b.String()), nil
}

// fetchCheckLog resolves the raw log text for a check run id via
// `bytedcli codebase checks log --check-run-id`. It runs in plain (non-JSON)
// mode because that command streams log lines as text. Best-effort: returns ""
// on any error or when no step logs could be resolved.
func (h *Host) fetchCheckLog(ctx context.Context, checkRunID string) string {
	args := h.repoArgs([]string{"codebase", "checks", "log", "--check-run-id", checkRunID, "--no-limit"})
	out, err := h.cmd(ctx, "bytedcli", args...).Output()
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(out))
	if text == "" || strings.HasPrefix(text, "✗") {
		// bytedcli prints a "✗ No step log pointers ..." notice when a check run
		// has no resolvable step logs (e.g. pipeline atom logs live elsewhere).
		return ""
	}
	return text
}

// checkBucket maps a Codebase check run's Status + Conclusion to a normalized
// bucket. A check that has not completed is pending regardless of conclusion.
// Codebase uses GitHub-style check-run vocabulary, but with "succeeded" instead
// of "success"; both spellings are handled.
func checkBucket(status, conclusion string) scm.CheckBucket {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "queued", "in_progress", "pending", "running", "waiting", "created", "":
		if s == "" {
			// No status field: fall through to conclusion-only mapping below so a
			// bare {conclusion} payload is still classified.
			break
		}
		return scm.CheckBucketPending
	}
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success", "succeeded":
		return scm.CheckBucketPass
	case "failure", "failed", "timed_out", "action_required", "startup_failure", "error":
		return scm.CheckBucketFail
	case "cancelled", "canceled":
		return scm.CheckBucketCancel
	case "skipped", "neutral", "stale":
		return scm.CheckBucketSkip
	case "":
		if s == "completed" {
			// Completed with no conclusion: treat as pending rather than pass so a
			// green verdict is never inferred from a missing conclusion.
			return scm.CheckBucketPending
		}
		return scm.CheckBucketPending
	default:
		return ""
	}
}

func normalizePRState(raw string) scm.PRState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "opened", "open":
		return scm.PRStateOpen
	case "merged":
		return scm.PRStateMerged
	case "closed", "locked":
		return scm.PRStateClosed
	default:
		return scm.PRState(strings.ToUpper(raw))
	}
}

// trimToJSON drops any leading noise before the JSON body by seeking to the
// first '{'. Every bytedcli --json payload this package parses is a top-level
// object, so seeking '{' (rather than '{' or '[') avoids latching onto a '['
// that may appear in a stray preamble line.
func trimToJSON(out []byte) []byte {
	if i := bytes.IndexByte(out, '{'); i >= 0 {
		return out[i:]
	}
	return nil
}
