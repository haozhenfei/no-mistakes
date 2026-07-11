package codebase

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRepoSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https with .git", "https://code.byted.org/owner/repo.git", "owner/repo"},
		{"https without .git", "https://code.byted.org/owner/repo", "owner/repo"},
		{"https nested namespace", "https://code.byted.org/group/sub/repo.git", "group/sub/repo"},
		{"https trailing slash", "https://code.byted.org/owner/repo/", "owner/repo"},
		{"code-tx https", "https://code-tx.byted.org/owner/repo.git", "owner/repo"},
		{"scp ssh", "git@code.byted.org:owner/repo.git", "owner/repo"},
		{"scp ssh nested", "git@code.byted.org:group/sub/repo.git", "group/sub/repo"},
		{"scp ssh no user", "code.byted.org:owner/repo.git", "owner/repo"},
		{"code-tx scp", "git@code-tx.byted.org:owner/repo.git", "owner/repo"},
		{"ssh url", "ssh://git@code.byted.org:29418/owner/repo.git", "owner/repo"},
		{"empty", "", ""},
		{"host only", "https://code.byted.org", ""},
		{"windows drive path backslash", `C:\Users\me\repo`, ""},
		{"windows drive path forward slash", "C:/Users/me/repo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoSlug(tc.in); got != tc.want {
				t.Fatalf("RepoSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAvailableParsesAuthenticated(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json auth status": {
			stdout: `{"status":"success","data":{"authenticated":true}}` + "\n",
		},
	}), func() bool { return true }, "code.byted.org", "owner/repo")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil", err)
	}
}

func TestAvailableSkipsBannerBeforeJSON(t *testing.T) {
	t.Parallel()

	// bytedcli prints its node runtime warning (which contains '[UNDICI-EHPA]')
	// on stderr; runJSON parses stdout only, so the JSON is read cleanly.
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json auth status": {
			stdout: `{"status":"success","data":{"authenticated":true}}` + "\n",
			stderr: "(node:1) [UNDICI-EHPA] Warning: EnvHttpProxyAgent is experimental\n",
		},
	}), func() bool { return true }, "code.byted.org", "owner/repo")

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v, want nil (banner should be skipped)", err)
	}
}

func TestAvailableFailsWhenUnauthenticated(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json auth status": {
			stdout: `{"status":"success","data":{"authenticated":false}}` + "\n",
		},
	}), func() bool { return true }, "", "")

	if err := host.Available(context.Background()); err == nil {
		t.Fatal("Available() error = nil, want unauthenticated error")
	}
}

func TestAvailableFailsWhenCLIMissing(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(nil), func() bool { return false }, "", "")
	err := host.Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("Available() error = %v, want not-installed error", err)
	}
}

func TestFindPRSynthesizesURLFromNumber(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr list --state open --head feature/x --base master -L 20 -R owner/repo": {
			stdout: `{"data":{"merge_requests":[{"Number":42,"Status":"open","SourceBranchName":"feature/x","TargetBranchName":"master","Title":"t","URL":""}]}}` + "\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindPR() = nil, want PR")
	}
	if pr.Number != "42" {
		t.Fatalf("FindPR() number = %q, want 42", pr.Number)
	}
	if pr.URL != "https://code.byted.org/owner/repo/merge_requests/42" {
		t.Fatalf("FindPR() URL = %q, want synthesized URL", pr.URL)
	}
}

func TestFindPRReturnsNilWhenNoMatch(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr list --state open --head feature/x --base master -L 20 -R owner/repo": {
			stdout: `{"data":{"merge_requests":[]}}` + "\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil", pr)
	}
}

func TestFindPRSkipsMismatchedSourceBranch(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr list --state open --head feature/x --base master -L 20 -R owner/repo": {
			stdout: `{"data":{"merge_requests":[{"Number":9,"SourceBranchName":"feature/other"}]}}` + "\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() = %+v, want nil (branch mismatch)", pr)
	}
}

func TestFindPRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr list --state open --head feature/x --base master -L 20 -R owner/repo": {
			stderr: "codebase unavailable\n",
			code:   1,
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.FindPR(context.Background(), "feature/x", "master")
	if err == nil {
		t.Fatal("FindPR() error = nil, want CLI error")
	}
	if !strings.Contains(err.Error(), "bytedcli codebase mr list") {
		t.Fatalf("FindPR() error = %v, want mr list context", err)
	}
	if pr != nil {
		t.Fatalf("FindPR() PR = %+v, want nil", pr)
	}
}

func TestCreatePRParsesNumberAndSynthesizesURL(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr create --head feature/x --base master --title feat: demo --body body text -R owner/repo": {
			stdout: `{"data":{"merge_request":{"Number":101,"URL":""}}}` + "\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "feat: demo", Body: "body text"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.Number != "101" {
		t.Fatalf("CreatePR() number = %q, want 101", pr.Number)
	}
	if pr.URL != "https://code.byted.org/owner/repo/merge_requests/101" {
		t.Fatalf("CreatePR() URL = %q, want synthesized URL", pr.URL)
	}
}

func TestCreatePRUsesReturnedURLWhenPresent(t *testing.T) {
	t.Parallel()

	// When the create payload carries a lowercase url (mr status shape), it is
	// preferred and the number is recovered from it.
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr create --head feature/x --base master --title t --body b -R owner/repo": {
			stdout: `{"data":{"merge_request":{"url":"https://code-tx.byted.org/owner/repo/merge_requests/7"}}}` + "\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v", err)
	}
	if pr.URL != "https://code-tx.byted.org/owner/repo/merge_requests/7" {
		t.Fatalf("CreatePR() URL = %q, want returned URL", pr.URL)
	}
	if pr.Number != "7" {
		t.Fatalf("CreatePR() number = %q, want 7 (from URL)", pr.Number)
	}
}

// realMRCreateStdout is a verbatim (abridged) capture of what
// `bytedcli --json codebase mr create` actually printed when it opened MR 6951
// on code.byted.org/obric/coze-monorepo. The wrapper key is CapitalCase
// (data.MergeRequest), unlike `mr list`/`mr get`/`mr status`, which all nest
// the payload under a snake_case key. Only the commit/branch blobs are elided;
// every key name and its casing is preserved exactly as emitted.
const realMRCreateStdout = `{"status":"success","data":{"MergeRequest":{` +
	`"Id":"784897989989491","Number":6951,"Status":"open",` +
	`"SourceRepoId":"978977","TargetRepoId":"978977",` +
	`"SourceBranchName":"fm/coze-obj-rename-fix-y3","TargetBranchName":"release/20260713",` +
	`"Title":"fix(storage): inline rename row color","Description":"## Intent\n\n..."` +
	`}}}` + "\n"

// The CapitalCase wrapper used to parse as a zero-value struct: Go's
// case-insensitive field match does not bridge `merge_request` and
// `MergeRequest` because of the underscore. CreatePR therefore reported a
// successfully created MR as a hard failure, which failed the run and meant the
// `ci` step never executed.
func TestCreatePRParsesCapitalCaseMergeRequestWrapper(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr create --head fm/coze-obj-rename-fix-y3 --base release/20260713 --title t --body b -R obric/coze-monorepo": {
			stdout: realMRCreateStdout,
		},
	}), nil, "code.byted.org", "obric/coze-monorepo")

	pr, err := host.CreatePR(context.Background(), "fm/coze-obj-rename-fix-y3", "release/20260713", scm.PRContent{Title: "t", Body: "b"})
	if err != nil {
		t.Fatalf("CreatePR() error = %v, want the created MR", err)
	}
	if pr.Number != "6951" {
		t.Fatalf("CreatePR() number = %q, want 6951", pr.Number)
	}
	if pr.URL != "https://code.byted.org/obric/coze-monorepo/merge_requests/6951" {
		t.Fatalf("CreatePR() URL = %q, want synthesized URL", pr.URL)
	}
}

func TestCreatePRReturnsCLIError(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr create --head feature/x --base master --title t --body b -R owner/repo": {
			stderr: "permission denied\n",
			code:   1,
		},
	}), nil, "code.byted.org", "owner/repo")

	if _, err := host.CreatePR(context.Background(), "feature/x", "master", scm.PRContent{Title: "t", Body: "b"}); err == nil {
		t.Fatal("CreatePR() error = nil, want CLI error")
	}
}

func TestUpdatePRTargetsNumberFromURL(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli codebase mr update 55 --title new title --body new body -R owner/repo": {
			stdout: "updated\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	pr := &scm.PR{URL: "https://code.byted.org/owner/repo/merge_requests/55"}
	updated, err := host.UpdatePR(context.Background(), pr, scm.PRContent{Title: "new title", Body: "new body"})
	if err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	if updated != pr {
		t.Fatalf("UpdatePR() returned unexpected PR: %+v", updated)
	}
}

func TestGetPRState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want scm.PRState
	}{
		{"open", scm.PRStateOpen},
		{"opened", scm.PRStateOpen},
		{"merged", scm.PRStateMerged},
		{"closed", scm.PRStateClosed},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
				"bytedcli --json codebase mr status 12 -R owner/repo": {
					stdout: fmt.Sprintf(`{"data":{"merge_request":{"status":%q}}}`, tc.raw) + "\n",
				},
			}), nil, "code.byted.org", "owner/repo")

			got, err := host.GetPRState(context.Background(), &scm.PR{Number: "12"})
			if err != nil {
				t.Fatalf("GetPRState() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("GetPRState() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetMergeableState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want scm.MergeableState
	}{
		{"mergeable", `{"data":{"mergeability":{"mergeable":true}}}`, scm.MergeableOK},
		{"conflict via reason", `{"data":{"mergeability":{"mergeable":false,"reason":"merge_conflict"}}}`, scm.MergeableConflict},
		{"conflict via detail", `{"data":{"mergeability":{"mergeable":false,"detail":{"UnmergeableReason":"conflict"}}}}`, scm.MergeableConflict},
		{"checking pending", `{"data":{"mergeability":{"mergeable":false,"reason":"checking"}}}`, scm.MergeablePending},
		{"ci running pending", `{"data":{"mergeability":{"mergeable":false,"reason":"ci_still_running"}}}`, scm.MergeablePending},
		{"closed not a conflict", `{"data":{"mergeability":{"mergeable":false,"reason":"closed"}}}`, scm.MergeableOK},
		{"approvals required not a conflict", `{"data":{"mergeability":{"mergeable":false,"reason":"approvals_required"}}}`, scm.MergeableOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
				"bytedcli --json codebase mr status 12 -R owner/repo": {stdout: tc.body + "\n"},
			}), nil, "code.byted.org", "owner/repo")

			got, err := host.GetMergeableState(context.Background(), &scm.PR{Number: "12"})
			if err != nil {
				t.Fatalf("GetMergeableState() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("GetMergeableState(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestGetChecksMapsStatusAndConclusion(t *testing.T) {
	t.Parallel()

	body := `{"data":{"check_runs":{"items":[
		{"Id":"1","Name":"build","Status":"completed","Conclusion":"succeeded","CompletedAt":"2026-04-24T04:15:00Z"},
		{"Id":"2","Name":"test","Status":"completed","Conclusion":"failed","CompletedAt":"2026-04-24T04:16:00Z"},
		{"Id":"3","Name":"lint","Status":"in_progress","Conclusion":""},
		{"Id":"4","Name":"review","Status":"completed","Conclusion":"neutral"},
		{"Id":"5","Name":"deploy","Status":"completed","Conclusion":"cancelled"}
	]}}}`
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr status 12 -R owner/repo": {stdout: body + "\n"},
	}), nil, "code.byted.org", "owner/repo")

	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "12"})
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	want := map[string]scm.CheckBucket{
		"build":  scm.CheckBucketPass,
		"test":   scm.CheckBucketFail,
		"lint":   scm.CheckBucketPending,
		"review": scm.CheckBucketSkip,
		"deploy": scm.CheckBucketCancel,
	}
	if len(checks) != len(want) {
		t.Fatalf("len(checks) = %d, want %d", len(checks), len(want))
	}
	for _, c := range checks {
		if c.Bucket != want[c.Name] {
			t.Fatalf("check %q bucket = %q, want %q", c.Name, c.Bucket, want[c.Name])
		}
	}
	// CompletedAt is parsed for completed checks.
	for _, c := range checks {
		if c.Name == "build" {
			if !c.CompletedAt.Equal(time.Date(2026, 4, 24, 4, 15, 0, 0, time.UTC)) {
				t.Fatalf("build CompletedAt = %v, want 2026-04-24T04:15:00Z", c.CompletedAt)
			}
		}
		if c.Name == "lint" && !c.CompletedAt.IsZero() {
			t.Fatalf("lint CompletedAt = %v, want zero (still running)", c.CompletedAt)
		}
	}
}

func TestFetchFailedCheckLogsFetchesMatchingFailedChecks(t *testing.T) {
	t.Parallel()

	status := `{"data":{"check_runs":{"items":[
		{"Id":"111","Name":"unit","Status":"completed","Conclusion":"failed"},
		{"Id":"222","Name":"lint","Status":"completed","Conclusion":"succeeded"}
	]}}}`
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr status 12 -R owner/repo": {stdout: status + "\n"},
		"bytedcli codebase checks log --check-run-id 111 --no-limit -R owner/repo": {
			stdout: "not ok 1 - failing assertion\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "12"}, "", "", []string{"unit"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if !strings.Contains(logs, "not ok 1 - failing assertion") {
		t.Fatalf("FetchFailedCheckLogs() = %q, want failing log body", logs)
	}
	if !strings.Contains(logs, "=== unit ===") {
		t.Fatalf("FetchFailedCheckLogs() = %q, want check-name header", logs)
	}
}

func TestFetchFailedCheckLogsSkipsUnresolvableLogs(t *testing.T) {
	t.Parallel()

	// A check whose logs cannot be resolved prints the "✗" notice; it must be
	// treated as empty rather than returned as log content.
	status := `{"data":{"check_runs":{"items":[
		{"Id":"111","Name":"unit","Status":"completed","Conclusion":"failed"}
	]}}}`
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr status 12 -R owner/repo": {stdout: status + "\n"},
		"bytedcli codebase checks log --check-run-id 111 --no-limit -R owner/repo": {
			stdout: "✗ No step log pointers could be resolved from the check run.\n",
		},
	}), nil, "code.byted.org", "owner/repo")

	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "12"}, "", "", []string{"unit"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty (unresolvable logs)", logs)
	}
}

func TestFetchFailedCheckLogsNoNamesReturnsEmpty(t *testing.T) {
	t.Parallel()

	host := New(codebaseTestCmdFactory(nil), nil, "code.byted.org", "owner/repo")
	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "12"}, "", "", nil)
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if logs != "" {
		t.Fatalf("FetchFailedCheckLogs() = %q, want empty", logs)
	}
}

func TestRepoInferredWhenSlugEmpty(t *testing.T) {
	t.Parallel()

	// With no repo slug, -R is omitted so bytedcli infers the repo from origin.
	host := New(codebaseTestCmdFactory(map[string]codebaseTestResponse{
		"bytedcli --json codebase mr status 12": {
			stdout: `{"data":{"merge_request":{"status":"open"}}}` + "\n",
		},
	}), nil, "", "")

	got, err := host.GetPRState(context.Background(), &scm.PR{Number: "12"})
	if err != nil {
		t.Fatalf("GetPRState() error = %v", err)
	}
	if got != scm.PRStateOpen {
		t.Fatalf("GetPRState() = %q, want OPEN", got)
	}
}

func TestCheckBucket(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status     string
		conclusion string
		want       scm.CheckBucket
	}{
		{"completed", "success", scm.CheckBucketPass},
		{"completed", "succeeded", scm.CheckBucketPass},
		{"completed", "failure", scm.CheckBucketFail},
		{"completed", "timed_out", scm.CheckBucketFail},
		{"completed", "cancelled", scm.CheckBucketCancel},
		{"completed", "skipped", scm.CheckBucketSkip},
		{"completed", "neutral", scm.CheckBucketSkip},
		{"in_progress", "", scm.CheckBucketPending},
		{"queued", "", scm.CheckBucketPending},
		{"completed", "", scm.CheckBucketPending},
	}
	for _, tc := range cases {
		if got := checkBucket(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("checkBucket(%q,%q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

type codebaseTestResponse struct {
	stdout string
	stderr string
	code   int
}

func codebaseTestCmdFactory(responses map[string]codebaseTestResponse) CmdFactory {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		response, ok := responses[key]
		if !ok {
			response = codebaseTestResponse{stderr: "unexpected command: " + key, code: 1}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCodebaseHelperProcess", "--", key)
		cmd.Env = append(os.Environ(),
			"CODEBASE_TEST_HELPER=1",
			"CODEBASE_TEST_STDOUT="+response.stdout,
			"CODEBASE_TEST_STDERR="+response.stderr,
			fmt.Sprintf("CODEBASE_TEST_EXIT_CODE=%d", response.code),
		)
		return cmd
	}
}

func TestCodebaseHelperProcess(t *testing.T) {
	if os.Getenv("CODEBASE_TEST_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, os.Getenv("CODEBASE_TEST_STDOUT")); err != nil {
		os.Exit(1)
	}
	if _, err := fmt.Fprint(os.Stderr, os.Getenv("CODEBASE_TEST_STDERR")); err != nil {
		os.Exit(1)
	}
	if code := os.Getenv("CODEBASE_TEST_EXIT_CODE"); code != "" && code != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}
