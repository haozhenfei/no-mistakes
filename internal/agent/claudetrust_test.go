package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// readConfig decodes a .claude.json file into a generic map for assertions.
func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return m
}

func projectTrust(t *testing.T, cfg map[string]any, root string) any {
	t.Helper()
	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		t.Fatalf("projects missing or wrong type: %T", cfg["projects"])
	}
	entry, ok := projects[root].(map[string]any)
	if !ok {
		t.Fatalf("project %q missing or wrong type: %T", root, projects[root])
	}
	return entry["hasTrustDialogAccepted"]
}

func TestClaudeConfigPath_HonorsConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/some-config-dir")
	got, err := claudeConfigPath()
	if err != nil {
		t.Fatalf("claudeConfigPath: %v", err)
	}
	if want := filepath.Join("/tmp/some-config-dir", ".claude.json"); got != want {
		t.Errorf("with CLAUDE_CONFIG_DIR: got %q, want %q", got, want)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err = claudeConfigPath()
	if err != nil {
		t.Fatalf("claudeConfigPath: %v", err)
	}
	if want := filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("without CLAUDE_CONFIG_DIR: got %q, want %q", got, want)
	}
}

func TestMarkProjectTrusted_SetsTrueAndPreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	root := "/Users/x/.no-mistakes/repos/deadbeef.git"

	// A realistic config: unrelated top-level keys, an unrelated project, and
	// the target project present but untrusted with a sibling field.
	original := `{
  "numStartups": 2426,
  "theme": "light",
  "tipsHistory": {"new-user-warmup": 1},
  "projects": {
    "/Users/x/other/repo": {"hasTrustDialogAccepted": true, "allowedTools": ["Bash"]},
    "` + root + `": {"hasTrustDialogAccepted": false, "lastCost": 0.5}
  }
}`
	if err := os.WriteFile(cfg, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := markProjectTrusted(cfg, root); err != nil {
		t.Fatalf("markProjectTrusted: %v", err)
	}

	m := readConfig(t, cfg)
	// Target flipped to true.
	if got := projectTrust(t, m, root); got != true {
		t.Errorf("target trust = %v, want true", got)
	}
	// Sibling field on the target entry preserved.
	proj := m["projects"].(map[string]any)
	targetEntry := proj[root].(map[string]any)
	if targetEntry["lastCost"] != 0.5 {
		t.Errorf("target lastCost preserved = %v, want 0.5", targetEntry["lastCost"])
	}
	// Unrelated project untouched.
	if got := projectTrust(t, m, "/Users/x/other/repo"); got != true {
		t.Errorf("other project trust = %v, want true (untouched)", got)
	}
	otherEntry := proj["/Users/x/other/repo"].(map[string]any)
	if tools, ok := otherEntry["allowedTools"].([]any); !ok || len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("other project allowedTools not preserved: %v", otherEntry["allowedTools"])
	}
	// Unrelated top-level fields preserved.
	if m["numStartups"] != float64(2426) {
		t.Errorf("numStartups preserved = %v, want 2426", m["numStartups"])
	}
	if m["theme"] != "light" {
		t.Errorf("theme preserved = %v, want light", m["theme"])
	}

	// Permission bits of the original file are preserved by the atomic rewrite.
	fi, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestMarkProjectTrusted_AlreadyTrustedDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	root := "/Users/x/.no-mistakes/repos/deadbeef.git"
	original := `{"projects":{"` + root + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(cfg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := markProjectTrusted(cfg, root); err != nil {
		t.Fatalf("markProjectTrusted: %v", err)
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("already-trusted config was rewritten:\nbefore=%s\nafter =%s", before, after)
	}
}

func TestMarkProjectTrusted_CreatesEntryWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	root := "/Users/x/.no-mistakes/repos/deadbeef.git"
	// Config exists with other projects but not the target one.
	if err := os.WriteFile(cfg, []byte(`{"projects":{"/other":{"hasTrustDialogAccepted":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := markProjectTrusted(cfg, root); err != nil {
		t.Fatalf("markProjectTrusted: %v", err)
	}
	m := readConfig(t, cfg)
	if got := projectTrust(t, m, root); got != true {
		t.Errorf("new entry trust = %v, want true", got)
	}
	if got := projectTrust(t, m, "/other"); got != true {
		t.Errorf("existing entry = %v, want true (untouched)", got)
	}
}

func TestMarkProjectTrusted_CreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "nested", ".claude.json") // parent dir missing too
	root := "/Users/x/.no-mistakes/repos/deadbeef.git"
	if err := markProjectTrusted(cfg, root); err != nil {
		t.Fatalf("markProjectTrusted: %v", err)
	}
	m := readConfig(t, cfg)
	if got := projectTrust(t, m, root); got != true {
		t.Errorf("trust = %v, want true", got)
	}
}

func TestMarkProjectTrusted_RefusesToClobberMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude.json")
	garbage := []byte(`{ this is not valid json `)
	if err := os.WriteFile(cfg, garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	err := markProjectTrusted(cfg, "/Users/x/.no-mistakes/repos/deadbeef.git")
	if err == nil {
		t.Fatal("expected error on malformed config, got nil")
	}
	after, err2 := os.ReadFile(cfg)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(after) != string(garbage) {
		t.Errorf("malformed config was modified: %s", after)
	}
}

// TestEnsureClaudeWorkspaceTrusted_KeysOnMirrorCommonDir is the end-to-end
// regression: a worktree cut from a bare mirror (the gate's shape) is trusted
// under the mirror's git-common-dir, which is the key the claude CLI reports in
// its untrusted-workspace error - not the worktree path.
func TestEnsureClaudeWorkspaceTrusted_KeysOnMirrorCommonDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	mirror := filepath.Join(base, "mirror.git")
	worktree := filepath.Join(base, "wt")

	runGit(t, "", "init", "--bare", mirror)
	// A bare repo needs a commit before a worktree can be added; seed one via a
	// throwaway normal clone.
	seed := filepath.Join(base, "seed")
	runGit(t, "", "clone", mirror, seed)
	runGit(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	runGit(t, seed, "push", "origin", "HEAD:refs/heads/main")
	runGit(t, mirror, "worktree", "add", worktree, "main")

	// Point the CLI config at a temp dir and pre-seed it untrusted, mirroring
	// the observed bug where hasTrustDialogAccepted sat at false.
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	cfgPath := filepath.Join(cfgDir, ".claude.json")

	// Resolve the exact key the CLI would use so we assert against it.
	commonDir, err := claudeTrustRoot(context.Background(), worktree)
	if err != nil {
		t.Fatalf("claudeTrustRoot: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"projects":{"`+commonDir+`":{"hasTrustDialogAccepted":false}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureClaudeWorkspaceTrusted(context.Background(), worktree); err != nil {
		t.Fatalf("ensureClaudeWorkspaceTrusted: %v", err)
	}

	m := readConfig(t, cfgPath)
	if got := projectTrust(t, m, commonDir); got != true {
		t.Errorf("mirror common-dir trust = %v, want true (key=%q)", got, commonDir)
	}
	// The worktree path itself must NOT be the trust key (would be the wrong fix).
	if projects, ok := m["projects"].(map[string]any); ok {
		if _, present := projects[worktree]; present {
			t.Errorf("worktree path %q should not be a trust key", worktree)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Keep ambient GIT_CONFIG_* injection (agent harnesses) from leaking in.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
