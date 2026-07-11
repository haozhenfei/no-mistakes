package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeUploadScript writes an executable POSIX upload hook and returns its path.
func writeUploadScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("upload hook script tests are POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "upload.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newUploadFixture(t *testing.T, uploadCmd string) (uploadContext, string) {
	t.Helper()
	workDir := t.TempDir()
	evidenceDir := filepath.Join(workDir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(evidenceDir, "shot.png")
	if err := os.WriteFile(shot, []byte("binary-screenshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	return uploadContext{
		WorkDir:     workDir,
		EvidenceDir: evidenceDir,
		Branch:      "fm/demo",
		RunID:       "run-1",
		Evidence: config.Evidence{
			UploadCmd:     uploadCmd,
			UploadTimeout: 10 * time.Second,
		},
		Log: func(string) {},
	}, shot
}

func TestUploadEvidenceArtifacts_ReplacesLocalPathWithURL(t *testing.T) {
	script := writeUploadScript(t, `echo "https://cdn.example.com/$(basename "$1")"`)
	sctx, shot := newUploadFixture(t, script)

	findings := types.Findings{
		TestingSummary: "Exercised the flow end to end.",
		Artifacts:      []types.TestArtifact{{Kind: "image", Label: "Screenshot", Path: shot}},
	}

	outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	if outcome.Uploaded != 1 || outcome.Failed != 0 {
		t.Fatalf("expected 1 upload and 0 failures, got %+v", outcome)
	}
	got := findings.Artifacts[0]
	if got.URL != "https://cdn.example.com/shot.png" {
		t.Fatalf("expected uploaded URL, got %q", got.URL)
	}
	if got.Path != "" {
		t.Fatalf("expected local path dropped once uploaded, got %q", got.Path)
	}
	if strings.Contains(findings.TestingSummary, "Warning") {
		t.Fatalf("successful upload must not warn, got %q", findings.TestingSummary)
	}
}

// The PR description is the whole point of the hook: prove the URL survives all
// the way into the rendered markdown, and that the daemon-host path does not.
func TestUploadEvidenceArtifacts_URLReachesPRDescription(t *testing.T) {
	script := writeUploadScript(t, `echo "https://cdn.example.com/$(basename "$1")"`)
	sctx, shot := newUploadFixture(t, script)

	findings := types.Findings{
		TestingSummary: "Captured the new dialog.",
		Artifacts:      []types.TestArtifact{{Kind: "image", Label: "Screenshot", Path: shot}},
	}
	uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	findingsJSON := string(raw)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findingsJSON}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findingsJSON, DurationMS: 100}},
	}

	md := BuildTestingSummaryForPR(steps, rounds, "git@github.com:example/widgets.git", "abc123", sctx.WorkDir)
	t.Logf("rendered PR testing markdown:\n%s", md)

	if !strings.Contains(md, "https://cdn.example.com/shot.png") {
		t.Fatalf("expected uploaded URL in PR description, got:\n%s", md)
	}
	if strings.Contains(md, shot) {
		t.Fatalf("expected local evidence path to be absent from PR description, got:\n%s", md)
	}
}

func TestUploadEvidenceArtifacts_FailedUploadDegradesToLocalPathAndWarns(t *testing.T) {
	script := writeUploadScript(t, "echo 'bucket unreachable' >&2\nexit 7\n")
	sctx, shot := newUploadFixture(t, script)

	findings := types.Findings{
		TestingSummary: "Captured the new dialog.",
		Artifacts:      []types.TestArtifact{{Kind: "image", Label: "Screenshot", Path: shot}},
	}

	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }

	outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	if outcome.Uploaded != 0 || outcome.Failed != 1 {
		t.Fatalf("expected 1 failure, got %+v", outcome)
	}
	got := findings.Artifacts[0]
	if got.URL != "" {
		t.Fatalf("failed upload must not set a URL, got %q", got.URL)
	}
	if got.Path != shot {
		t.Fatalf("failed upload must keep the local path, got %q", got.Path)
	}
	if !strings.Contains(findings.TestingSummary, "Captured the new dialog.") ||
		!strings.Contains(findings.TestingSummary, "evidence upload failed for 1 artifact") {
		t.Fatalf("expected the original summary plus an upload warning, got %q", findings.TestingSummary)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "evidence upload failed") {
		t.Fatalf("expected a loud log line, got %v", logs)
	}
}

func TestUploadEvidenceArtifacts_TimeoutDegrades(t *testing.T) {
	script := writeUploadScript(t, "sleep 30\n")
	sctx, shot := newUploadFixture(t, script)
	sctx.Evidence.UploadTimeout = 200 * time.Millisecond

	findings := types.Findings{Artifacts: []types.TestArtifact{{Label: "Screenshot", Path: shot}}}

	start := time.Now()
	outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)
	elapsed := time.Since(start)

	if outcome.Failed != 1 {
		t.Fatalf("expected the timeout to be a failure, got %+v", outcome)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("expected the upload to be bounded by upload_timeout, took %s", elapsed)
	}
	if findings.Artifacts[0].Path != shot {
		t.Fatalf("expected local path retained after timeout, got %q", findings.Artifacts[0].Path)
	}
}

// A hook that exits 0 but prints something that is not a URL is a failure, not a
// URL: the value is interpolated straight into the PR description.
func TestUploadEvidenceArtifacts_RejectsNonURLStdout(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty stdout", "exit 0\n"},
		{"prose", "echo 'upload complete'\n"},
		{"relative path", "echo 'evidence/shot.png'\n"},
		{"file scheme", "echo 'file:///etc/passwd'\n"},
		{"markdown breaking", "echo 'https://x.example.com/a b)c'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := writeUploadScript(t, tc.body)
			sctx, shot := newUploadFixture(t, script)
			findings := types.Findings{Artifacts: []types.TestArtifact{{Label: "Screenshot", Path: shot}}}

			outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)

			if outcome.Failed != 1 || outcome.Uploaded != 0 {
				t.Fatalf("expected %s to be rejected, got %+v", tc.name, outcome)
			}
			if findings.Artifacts[0].URL != "" {
				t.Fatalf("expected no URL for %s, got %q", tc.name, findings.Artifacts[0].URL)
			}
			if findings.Artifacts[0].Path != shot {
				t.Fatalf("expected local path retained for %s", tc.name)
			}
		})
	}
}

// stderr is the script's own log channel and must never be mistaken for a URL;
// the last non-empty stdout line wins so a chatty uploader still works.
func TestUploadEvidenceArtifacts_ReadsLastStdoutLineAndIgnoresStderr(t *testing.T) {
	script := writeUploadScript(t, "echo 'https://wrong.example.com/from-stderr' >&2\necho 'uploading...'\necho 'https://cdn.example.com/final'\n")
	sctx, shot := newUploadFixture(t, script)
	findings := types.Findings{Artifacts: []types.TestArtifact{{Label: "Screenshot", Path: shot}}}

	uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	if got := findings.Artifacts[0].URL; got != "https://cdn.example.com/final" {
		t.Fatalf("expected the last stdout line, got %q", got)
	}
}

// The contract: a bare hook reads the evidence path as $1; a hook that carries
// its own flags gets the path appended as the trailing argument (like any CLI
// that takes a file); and NM_EVIDENCE_FILE always carries it regardless.
func TestUploadEvidenceArtifacts_PassesPathAsTrailingArgumentAndEnv(t *testing.T) {
	for _, tc := range []struct {
		name     string
		extra    string
		wantArgs func(shot string) string
	}{
		{
			name:     "bare hook reads the path as $1",
			extra:    "",
			wantArgs: func(shot string) string { return shot },
		},
		{
			name:     "hook with flags gets the path appended last",
			extra:    " --bucket evidence",
			wantArgs: func(shot string) string { return "--bucket evidence " + shot },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "seen.txt")
			script := writeUploadScript(t, "printf 'args=[%s] first=%s env=%s label=%s branch=%s run=%s\\n' \"$*\" \"$1\" \"$NM_EVIDENCE_FILE\" \"$NM_EVIDENCE_LABEL\" \"$NM_EVIDENCE_BRANCH\" \"$NM_EVIDENCE_RUN_ID\" > "+out+"\necho https://cdn.example.com/ok\n")
			sctx, shot := newUploadFixture(t, script+tc.extra)
			findings := types.Findings{Artifacts: []types.TestArtifact{{Label: "Screenshot", Path: shot}}}

			uploadEvidenceArtifacts(context.Background(), sctx, &findings)

			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("upload script did not run: %v", err)
			}
			got := strings.TrimSpace(string(data))
			if !strings.Contains(got, "args=["+tc.wantArgs(shot)+"]") {
				t.Fatalf("expected the evidence path as the trailing argument, got %q", got)
			}
			if !strings.Contains(got, "env="+shot) {
				t.Fatalf("expected NM_EVIDENCE_FILE to carry the path, got %q", got)
			}
			if tc.extra == "" && !strings.Contains(got, "first="+shot) {
				t.Fatalf("expected a bare hook to read the path as $1, got %q", got)
			}
			if !strings.Contains(got, "label=Screenshot") || !strings.Contains(got, "branch=fm/demo") || !strings.Contains(got, "run=run-1") {
				t.Fatalf("expected label/branch/run env, got %q", got)
			}
		})
	}
}

func TestUploadEvidenceArtifacts_SkipsArtifactsOutsideWorkdirAndAlreadyPublished(t *testing.T) {
	script := writeUploadScript(t, "echo https://cdn.example.com/uploaded\n")
	sctx, _ := newUploadFixture(t, script)

	outside := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(outside, []byte("token"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings := types.Findings{Artifacts: []types.TestArtifact{
		{Label: "Outside the worktree", Path: outside},
		{Label: "Missing file", Path: filepath.Join(sctx.EvidenceDir, "nope.png")},
		{Label: "Already published", URL: "https://existing.example.com/x", Path: ""},
		{Label: "Inline log", Content: "some output"},
	}}

	outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	if outcome.Uploaded != 0 || outcome.Failed != 0 {
		t.Fatalf("expected nothing to be uploaded, got %+v", outcome)
	}
	if findings.Artifacts[0].URL != "" {
		t.Fatalf("a path outside the worktree must never be shipped to external storage")
	}
	if findings.Artifacts[2].URL != "https://existing.example.com/x" {
		t.Fatalf("an agent-published URL must be left alone, got %q", findings.Artifacts[2].URL)
	}
}

func TestUploadEvidenceArtifacts_NoHookConfiguredIsNoop(t *testing.T) {
	sctx, shot := newUploadFixture(t, "")
	findings := types.Findings{Artifacts: []types.TestArtifact{{Label: "Screenshot", Path: shot}}}

	outcome := uploadEvidenceArtifacts(context.Background(), sctx, &findings)

	if outcome.Uploaded != 0 || outcome.Failed != 0 {
		t.Fatalf("expected a no-op without upload_cmd, got %+v", outcome)
	}
	if findings.Artifacts[0].Path != shot || findings.Artifacts[0].URL != "" {
		t.Fatalf("expected today's behavior preserved when no hook is configured")
	}
}

// The hook exists to keep binaries out of git history, so it wins over
// store_in_repo: evidence stays in the temp dir and the push step's
// stageInRepoEvidence (which resolves through the same function) stages nothing.
func TestResolveTestEvidenceLocation_UploadCmdOverridesStoreInRepo(t *testing.T) {
	workDir := t.TempDir()

	inRepo := resolveTestEvidenceLocation(workDir, "fm/demo", "run-1", config.Evidence{
		StoreInRepo: true,
		Dir:         ".no-mistakes/evidence",
	})
	if !inRepo.StoreInRepo || !strings.HasPrefix(inRepo.Dir, workDir) {
		t.Fatalf("store_in_repo alone must still store in the repo, got %+v", inRepo)
	}

	uploaded := resolveTestEvidenceLocation(workDir, "fm/demo", "run-1", config.Evidence{
		StoreInRepo: true,
		Dir:         ".no-mistakes/evidence",
		UploadCmd:   "/path/to/upload.sh",
	})
	if uploaded.StoreInRepo {
		t.Fatalf("upload_cmd must keep evidence out of the repo, got %+v", uploaded)
	}
	if strings.HasPrefix(uploaded.Dir, workDir) {
		t.Fatalf("uploaded evidence must live outside the worktree, got %q", uploaded.Dir)
	}
	if uploaded.Dir != testEvidenceDir("run-1") {
		t.Fatalf("expected the temporary evidence dir, got %q", uploaded.Dir)
	}
}

// The compatibility + isolation guarantee, exercised through the real push-step
// staging code on a real git repo: store_in_repo alone still commits evidence
// with the branch (unchanged behavior), and adding upload_cmd stops it from
// entering the repo at all.
func TestStageInRepoEvidence_UploadCmdKeepsEvidenceOutOfTheRepo(t *testing.T) {
	for _, tc := range []struct {
		name       string
		uploadCmd  string
		wantStaged bool
	}{
		{name: "store_in_repo only stages evidence", uploadCmd: "", wantStaged: true},
		{name: "upload_cmd keeps evidence out of the repo", uploadCmd: "/opt/upload.sh", wantStaged: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContext(t, nil, workDir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Test.Evidence = config.Evidence{
				StoreInRepo: true,
				Dir:         ".no-mistakes/evidence",
				UploadCmd:   tc.uploadCmd,
			}

			// Write a screenshot where the test step would have written it for
			// this configuration.
			location := resolveTestEvidenceLocation(workDir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
			if err := os.MkdirAll(location.Dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(location.Dir, "shot.png"), []byte("binary-screenshot"), 0o644); err != nil {
				t.Fatal(err)
			}

			step := &PushStep{}
			if err := step.stageInRepoEvidence(sctx); err != nil {
				t.Fatalf("stageInRepoEvidence: %v", err)
			}

			staged, err := git.Run(context.Background(), workDir, "diff", "--cached", "--name-only")
			if err != nil {
				t.Fatal(err)
			}
			gotStaged := strings.Contains(staged, "shot.png")
			if gotStaged != tc.wantStaged {
				t.Fatalf("staged evidence = %v, want %v (git diff --cached: %q)", gotStaged, tc.wantStaged, staged)
			}
		})
	}
}
