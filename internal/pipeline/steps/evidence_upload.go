package steps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Evidence upload hook (test.evidence.upload_cmd).
//
// Evidence is only interesting for the few days a change is under review, but
// committing screenshots and recordings into the branch leaves them in git
// history forever. The upload hook is the third option: hand each evidence file
// to a user-supplied command, take back a URL, and put only the URL in the PR
// description. no-mistakes deliberately knows nothing about object storage —
// which bucket, which credentials, which CDN all live in the user's script.
//
// The contract, in full:
//
//   - Invocation: once per evidence file, from the worktree, with the ABSOLUTE
//     file path appended as the command's last argument (POSIX: the command line
//     runs under `sh -c '<upload_cmd> "$@"' ...`, so it may carry its own flags).
//     The same path is also exported as NM_EVIDENCE_FILE, alongside
//     NM_EVIDENCE_LABEL, NM_EVIDENCE_RUN_ID, and NM_EVIDENCE_BRANCH.
//   - Success: exit code 0 and an absolute http/https URL as the last non-empty
//     line of STDOUT. Only stdout is read, so the script is free to log progress
//     to stderr. The last line (not the first) wins so a chatty uploader that
//     prints progress to stdout still works.
//   - Failure means any of: non-zero exit, timeout, empty stdout, or stdout that
//     is not a well-formed absolute http/https URL.
//   - On failure the run DEGRADES, it does not fail: the artifact keeps its local
//     path (exactly today's no-upload behavior), a warning is logged, and a
//     warning line is appended to testing_summary so the PR reader knows the link
//     is missing and why. A storage outage must not fail a change whose tests
//     actually passed — the upload is a publication channel, not a gate. The
//     failure is loud rather than silent precisely because the alternative
//     (evidence quietly vanishing) is what the hook exists to prevent.
//   - Timeout: config.Evidence.UploadTimeout per invocation (default 2m).
//
// The hook only ever uploads files that already exist under the evidence
// directory or the worktree; an artifact path pointing anywhere else on the
// daemon host is left alone rather than shipped to the user's storage.

// evidenceUploadOutcome reports what happened across one test step's uploads.
type evidenceUploadOutcome struct {
	Uploaded int
	Failed   int
}

// uploadEvidenceArtifacts runs the configured upload hook over every local
// evidence artifact in findings and rewrites each successful one to carry only
// its URL. It mutates findings in place and never returns an error: every
// failure path degrades to the artifact's existing local path.
func uploadEvidenceArtifacts(ctx context.Context, sctx uploadContext, findings *types.Findings) evidenceUploadOutcome {
	var outcome evidenceUploadOutcome
	ev := sctx.Evidence
	if strings.TrimSpace(ev.UploadCmd) == "" || findings == nil {
		return outcome
	}
	timeout := ev.UploadTimeout
	if timeout <= 0 {
		timeout = config.DefaultEvidenceUploadTimeout
	}

	for i := range findings.Artifacts {
		artifact := &findings.Artifacts[i]
		if strings.TrimSpace(artifact.URL) != "" {
			continue // the agent already published this one
		}
		abs, ok := uploadableArtifactPath(sctx.WorkDir, sctx.EvidenceDir, artifact.Path)
		if !ok {
			continue
		}
		sctx.Log(fmt.Sprintf("uploading evidence: %s", artifact.Path))
		uploaded, err := runEvidenceUpload(ctx, sctx, ev.UploadCmd, timeout, abs, artifact.Label)
		if err != nil {
			outcome.Failed++
			sctx.Log(fmt.Sprintf("evidence upload failed for %s: %v (keeping local path)", artifact.Path, err))
			continue
		}
		outcome.Uploaded++
		sctx.Log(fmt.Sprintf("evidence uploaded: %s -> %s", artifact.Path, uploaded))
		artifact.URL = uploaded
		// Drop the local path: it is a daemon-host path that means nothing to a
		// PR reader once the URL exists, and keeping it would tempt the push step
		// into committing the file the upload just made unnecessary.
		artifact.Path = ""
	}

	if outcome.Failed > 0 {
		findings.TestingSummary = appendUploadWarning(findings.TestingSummary, outcome.Failed)
	}
	return outcome
}

// uploadContext is the slice of StepContext the uploader needs, kept narrow so
// it can be exercised without a full pipeline.
type uploadContext struct {
	WorkDir     string
	EvidenceDir string
	Branch      string
	RunID       string
	Env         []string
	Evidence    config.Evidence
	Log         func(string)
}

// runEvidenceUpload invokes the hook for one file and returns the URL it
// printed. Only stdout is captured: stderr belongs to the script's own logging
// and must never be mistaken for a URL.
func runEvidenceUpload(ctx context.Context, sctx uploadContext, uploadCmd string, timeout time.Duration, absPath, label string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd.exe", "/c", uploadCmd+" "+strconv.Quote(absPath))
	} else {
		// "$@" appends the path as the final argument while letting upload_cmd
		// carry its own flags; the extra "sh" is argv[0] for the inner shell.
		cmd = exec.CommandContext(runCtx, "sh", "-c", uploadCmd+` "$@"`, "sh", absPath)
	}
	shellenv.ConfigureShellCommand(cmd)
	cmd.Dir = sctx.WorkDir
	cmd.Env = mergeEnv(withPWD(sctx.WorkDir, append(append([]string{}, sctx.Env...),
		"NM_EVIDENCE_FILE="+absPath,
		"NM_EVIDENCE_LABEL="+label,
		"NM_EVIDENCE_RUN_ID="+sctx.RunID,
		"NM_EVIDENCE_BRANCH="+sctx.Branch,
	)))

	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("upload_cmd timed out after %s", timeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("upload_cmd exited %d: %s", ee.ExitCode(), firstLine(string(ee.Stderr)))
		}
		return "", fmt.Errorf("upload_cmd: %w", err)
	}
	link, ok := parseUploadedURL(string(out))
	if !ok {
		return "", fmt.Errorf("upload_cmd printed no absolute http(s) URL on stdout (got %q)", truncateForLog(string(out)))
	}
	return link, nil
}

// parseUploadedURL takes the last non-empty line of stdout and accepts it only
// as an absolute http/https URL with no whitespace or markdown-breaking
// characters — the value is interpolated straight into the PR description.
func parseUploadedURL(stdout string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if candidate == "" {
			continue
		}
		if !isSafeEvidenceURL(candidate) {
			return "", false
		}
		return candidate, true
	}
	return "", false
}

func isSafeEvidenceURL(candidate string) bool {
	if strings.ContainsAny(candidate, " \t<>[]()\"'") {
		return false
	}
	for _, r := range candidate {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

// uploadableArtifactPath resolves an artifact path to an existing regular file
// inside the evidence directory or the worktree. Anything else — a missing file,
// a directory, or a path elsewhere on the daemon host — is not ours to upload.
func uploadableArtifactPath(workDir, evidenceDir, artifactPath string) (string, bool) {
	clean := strings.TrimSpace(artifactPath)
	if clean == "" {
		return "", false
	}
	abs := clean
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workDir, abs)
	}
	abs = filepath.Clean(abs)

	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	for _, root := range []string{evidenceDir, workDir} {
		if root != "" && pathWithinRoot(root, abs) {
			return abs, true
		}
	}
	return "", false
}

func pathWithinRoot(root, target string) bool {
	rootAbs := resolveArtifactPathSymlinks(filepath.Clean(root))
	targetAbs := resolveArtifactPathSymlinks(filepath.Clean(target))
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func appendUploadWarning(summary string, failed int) string {
	noun := "artifact"
	if failed > 1 {
		noun = "artifacts"
	}
	warning := fmt.Sprintf("Warning: evidence upload failed for %d %s; the local evidence path is shown instead of a URL.", failed, noun)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return warning
	}
	return summary + "\n\n" + warning
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no stderr output"
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return truncateForLog(s)
}

func truncateForLog(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
