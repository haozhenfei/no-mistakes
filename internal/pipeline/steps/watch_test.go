package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// watchFixture wires a WatchStep to a fake gh whose four signals the test
// controls. The step is driven through exactly one poll: waitForNextPoll fails
// the test if the step asks for a second one when it should already have
// converged.
type watchFixture struct {
	state    string
	checks   string
	review   string
	threads  string
	autoFix  int
	pollOnce bool
}

func runWatch(t *testing.T, f watchFixture) (*pipeline.StepOutcome, *pipeline.WatchOutcome, []string, error) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)

	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "gh")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":    "ci-gh",
		"FAKE_CLI_STATE":   f.state,
		"FAKE_CLI_CHECKS":  f.checks,
		"FAKE_CLI_REVIEW":  f.review,
		"FAKE_CLI_THREADS": f.threads,
	})

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Hour
	sctx.Config.AutoFix.CI = f.autoFix
	sctx.Shared = &pipeline.RunShared{}

	var logs []string
	baseLog := sctx.Log
	sctx.Log = func(line string) {
		logs = append(logs, line)
		if baseLog != nil {
			baseLog(line)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &WatchStep{
		checksGracePeriod: time.Nanosecond, // no checks means no checks, not "too early to tell"
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls > 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	return outcome, sctx.Shared.WatchOutcome(), logs, err
}

func TestWatchStep_MergedPRConverges(t *testing.T) {
	outcome, verdict, _, err := runWatch(t, watchFixture{
		state:  "MERGED",
		checks: `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatal("a merged PR must not park anyone")
	}
	if verdict == nil || verdict.Action != pipeline.WatchConverged {
		t.Fatalf("verdict = %+v, want converged", verdict)
	}
}

func TestWatchStep_ClosedPRConverges(t *testing.T) {
	_, verdict, _, err := runWatch(t, watchFixture{state: "CLOSED"})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if verdict == nil || verdict.Action != pipeline.WatchConverged {
		t.Fatalf("verdict = %+v, want converged", verdict)
	}
}

// TestWatchStep_FailingCIRequestsFixRound: CI is the machine's own mess, and it
// is the one signal the pipeline may act on unattended - bounded by auto_fix.ci.
func TestWatchStep_FailingCIRequestsFixRound(t *testing.T) {
	outcome, verdict, _, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"FAILURE","bucket":"fail"},{"name":"lint","state":"SUCCESS","bucket":"pass"}]`,
		autoFix: 3,
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if verdict == nil || verdict.Action != pipeline.WatchFix {
		t.Fatalf("verdict = %+v, want a fix round", verdict)
	}
	if outcome.NeedsApproval {
		t.Fatal("a fix round is derived by the daemon, not parked for a human")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(verdict.FindingsJSON), &findings); err != nil {
		t.Fatalf("parse seed findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "build") {
		t.Fatalf("seed findings = %+v, want the failing check named", findings.Items)
	}
	if findings.Items[0].Action != "auto-fix" {
		t.Fatalf("failing check action = %q, want auto-fix", findings.Items[0].Action)
	}
}

// TestWatchStep_FailingCIWithAutoFixDisabledEscalates: honor the same stance the
// rest of the pipeline takes when the operator turns auto-fix off.
func TestWatchStep_FailingCIWithAutoFixDisabledEscalates(t *testing.T) {
	outcome, verdict, _, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"FAILURE","bucket":"fail"}]`,
		autoFix: 0,
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if verdict == nil || verdict.Action != pipeline.WatchEscalate {
		t.Fatalf("verdict = %+v, want escalate", verdict)
	}
	if !outcome.NeedsApproval {
		t.Fatal("an escalation must park the run for the driving agent")
	}
}

// TestWatchStep_PendingChecksKeepPolling: never judge a half-finished CI run.
func TestWatchStep_PendingChecksKeepPolling(t *testing.T) {
	_, verdict, logs, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"FAILURE","bucket":"fail"},{"name":"test","state":"PENDING","bucket":"pending"}]`,
		autoFix: 3,
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want the step to keep polling until cancelled", err)
	}
	if verdict != nil {
		t.Fatalf("verdict = %+v, want none while checks are still running", verdict)
	}
	if !containsLine(logs, "checks still pending") && !containsLine(logs, "still running") {
		t.Fatalf("expected a 'waiting for checks' log, got %v", logs)
	}
}

// TestWatchStep_UnresolvedThreadsEscalateAndNeverAutoFix is the conservative
// posture. An unresolved thread may be a human reviewer's opinion, and the watch
// run cannot tell it apart from a bot's - so it always parks, even with CI
// auto-fix fully enabled.
func TestWatchStep_UnresolvedThreadsEscalateAndNeverAutoFix(t *testing.T) {
	threads := `[
		{"id":"T1","isResolved":false,"isOutdated":false,"comments":{"nodes":[{"author":{"login":"alice"},"body":"this drops the error","path":"main.go","line":12}]}},
		{"id":"T2","isResolved":true,"comments":{"nodes":[{"author":{"login":"bot"},"body":"nit","path":"x.go","line":1}]}}
	]`
	outcome, verdict, _, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		threads: threads,
		autoFix: 3, // auto-fix on: it must still not touch a comment thread
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if verdict == nil || verdict.Action != pipeline.WatchEscalate {
		t.Fatalf("verdict = %+v, want escalate: comment threads are never auto-fixed", verdict)
	}
	if !outcome.NeedsApproval {
		t.Fatal("unresolved threads must park for a decision")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(verdict.FindingsJSON), &findings); err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %d, want only the unresolved thread", len(findings.Items))
	}
	item := findings.Items[0]
	if item.Action != "ask-user" {
		t.Fatalf("thread finding action = %q, want ask-user", item.Action)
	}
	if item.File != "main.go" || item.Line != 12 {
		t.Fatalf("thread finding location = %s:%d, want main.go:12", item.File, item.Line)
	}
	if !strings.Contains(item.Description, "alice") || !strings.Contains(item.Description, "drops the error") {
		t.Fatalf("thread finding = %q, want the author and body carried through", item.Description)
	}
}

// TestWatchStep_GreenButBlockedOnApprovalEscalates is the state that is
// invisible today: every check passes, nothing is unresolved, and the PR still
// cannot merge because a person has not approved it.
func TestWatchStep_GreenButBlockedOnApprovalEscalates(t *testing.T) {
	outcome, verdict, _, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		review:  "REVIEW_REQUIRED",
		autoFix: 3,
	})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if verdict == nil || verdict.Action != pipeline.WatchEscalate {
		t.Fatalf("verdict = %+v, want escalate", verdict)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a PR that only a person can unblock must park for that person")
	}
	if !strings.Contains(verdict.Reason, "approval") {
		t.Fatalf("reason = %q, want it to name the approval block", verdict.Reason)
	}
}

// TestWatchStep_GreenAndApprovedKeepsWatching: nothing to do but wait for the
// merge. It must not converge (the PR is still open) and must not escalate.
func TestWatchStep_GreenAndApprovedKeepsWatching(t *testing.T) {
	_, verdict, _, err := runWatch(t, watchFixture{
		state:   "OPEN",
		checks:  `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`,
		review:  "APPROVED",
		autoFix: 3,
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want the step to keep watching a green open PR", err)
	}
	if verdict != nil {
		t.Fatalf("verdict = %+v, want none: a green approved PR is simply not merged yet", verdict)
	}
}

// TestWatchStep_NoPRURLFails: a watch run exists because a PR exists. Watching
// nothing must be loud, not a silent pass.
func TestWatchStep_NoPRURLFails(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared = &pipeline.RunShared{}
	sctx.Run.PRURL = nil

	if _, err := (&WatchStep{}).Execute(sctx); err == nil {
		t.Fatal("expected a watch run with no PR URL to fail")
	}
}

func containsLine(logs []string, substr string) bool {
	for _, line := range logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}
