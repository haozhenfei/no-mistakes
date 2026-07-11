package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// overrideFixture builds a manager with a real DB holding one repo, plus a
// global config file the test can write per case.
func overrideFixture(t *testing.T, workingPath, recordedBranch, globalConfig string) (*RunManager, *db.DB, *db.Repo) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	repo, err := d.InsertRepo(workingPath, "git@example.com:acme/monorepo.git", recordedBranch)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(globalConfig), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	return NewRunManager(d, p, nil), d, repo
}

// TestLoadRepo_AppliesGlobalDefaultBranchOverride proves the second escape
// hatch: a repo whose server HEAD is a frozen master can be rebased and diffed
// against its real integration baseline by naming it in the global config. The
// DB row keeps recording what the server answered.
func TestLoadRepo_AppliesGlobalDefaultBranchOverride(t *testing.T) {
	workingPath := t.TempDir()
	mgr, d, repo := overrideFixture(t, workingPath, "master",
		"repos:\n  "+workingPath+":\n    default_branch: integration/2026-07\n")

	loaded, err := mgr.loadRepo(repo.ID)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}
	if loaded.DefaultBranch != "integration/2026-07" {
		t.Fatalf("default branch = %q, want integration/2026-07 (global override ignored)", loaded.DefaultBranch)
	}

	row, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if row.DefaultBranch != "master" {
		t.Fatalf("DB row default_branch = %q, want master: the override is applied on read, never written back", row.DefaultBranch)
	}
}

func TestLoadRepo_WithoutOverrideKeepsRecordedBranch(t *testing.T) {
	workingPath := t.TempDir()
	mgr, _, repo := overrideFixture(t, workingPath, "master", "agent: claude\n")

	loaded, err := mgr.loadRepo(repo.ID)
	if err != nil {
		t.Fatalf("loadRepo: %v", err)
	}
	if loaded.DefaultBranch != "master" {
		t.Fatalf("default branch = %q, want master", loaded.DefaultBranch)
	}
}

// TestLoadRepo_UnreadableGlobalConfigStillReturnsRepo pins the failure
// ordering: a broken global config must not stop the repo record from loading,
// because the callers still have to create the run row that carries the "parse
// global config" error to the user (e2e assertInvalidConfigPushCleansWorktree).
func TestLoadRepo_UnreadableGlobalConfigStillReturnsRepo(t *testing.T) {
	workingPath := t.TempDir()
	mgr, _, repo := overrideFixture(t, workingPath, "master", "invalid: yaml: [[[")

	loaded, err := mgr.loadRepo(repo.ID)
	if err != nil {
		t.Fatalf("loadRepo must tolerate a broken global config, got: %v", err)
	}
	if loaded == nil || loaded.DefaultBranch != "master" {
		t.Fatalf("loadRepo = %+v, want the recorded repo with default branch master", loaded)
	}
}

// TestResolveAllowRepoCommands_GlobalOverrideBeatsTrustedCopy pins the
// precedence rule: the global per-repo override wins over the trusted
// default-branch copy in both directions, and the trusted copy still decides
// when no override is configured.
func TestResolveAllowRepoCommands_GlobalOverrideBeatsTrustedCopy(t *testing.T) {
	repo := &db.Repo{WorkingPath: "/repo/a"}
	trustedOn := &config.RepoConfig{AllowRepoCommands: true}
	trustedOff := &config.RepoConfig{AllowRepoCommands: false}

	on := writeGlobalConfig(t, "repos:\n  /repo/a:\n    allow_repo_commands: true\n")
	off := writeGlobalConfig(t, "repos:\n  /repo/a:\n    allow_repo_commands: false\n")
	silent := writeGlobalConfig(t, "agent: claude\n")

	// The deadlock case: the default branch is frozen and carries no
	// .no-mistakes.yaml at all (trusted == nil), so the switch could never be
	// flipped there. The global override flips it.
	if !resolveAllowRepoCommands(on, repo, nil) {
		t.Fatal("global override must enable the opt-in with no trusted copy at all")
	}
	if !resolveAllowRepoCommands(on, repo, trustedOff) {
		t.Fatal("global override true must beat a trusted copy saying false")
	}
	if resolveAllowRepoCommands(off, repo, trustedOn) {
		t.Fatal("global override false must beat a trusted copy saying true")
	}
	if !resolveAllowRepoCommands(silent, repo, trustedOn) {
		t.Fatal("without an override the trusted copy must decide (true)")
	}
	if resolveAllowRepoCommands(silent, repo, trustedOff) {
		t.Fatal("without an override the trusted copy must decide (false)")
	}
	if resolveAllowRepoCommands(on, &db.Repo{WorkingPath: "/repo/other"}, trustedOff) {
		t.Fatal("an override keyed at another repo path must not leak across repos")
	}
}

// TestLoadRecoveredConfig_GlobalOverrideEnablesPushedCommands is the deadlock
// end to end at the config layer: the default branch has no .no-mistakes.yaml
// (so the trusted copy is nil and commands would normally be forced empty), yet
// the maintainer's global override makes the pushed branch's commands.test run.
func TestLoadRecoveredConfig_GlobalOverrideEnablesPushedCommands(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"),
		[]byte("commands:\n  test: rush test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workingPath := t.TempDir()
	mgr, _, repo := overrideFixture(t, workingPath, "master",
		"repos:\n  "+workingPath+":\n    allow_repo_commands: true\n")

	// No default branch fetched → trusted copy nil (fail-closed path).
	repo.DefaultBranch = ""
	cfg, err := mgr.loadRecoveredConfig(context.Background(), &db.Run{ID: "run"}, repo, workDir)
	if err != nil {
		t.Fatalf("loadRecoveredConfig: %v", err)
	}
	if cfg.Commands.Test != "rush test" {
		t.Fatalf("commands.test = %q, want %q: the global opt-in must honor the pushed branch's commands", cfg.Commands.Test, "rush test")
	}

	// Control: without the override, the same pushed commands are dropped.
	bare, _, bareRepo := overrideFixture(t, t.TempDir(), "master", "agent: claude\n")
	bareRepo.DefaultBranch = ""
	cfg, err = bare.loadRecoveredConfig(context.Background(), &db.Run{ID: "run"}, bareRepo, workDir)
	if err != nil {
		t.Fatalf("loadRecoveredConfig: %v", err)
	}
	if cfg.Commands.Test != "" {
		t.Fatalf("commands.test = %q, want empty without the opt-in", cfg.Commands.Test)
	}
}

func writeGlobalConfig(t *testing.T, body string) *config.GlobalConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadGlobal(path)
	if err != nil {
		t.Fatalf("load global config: %v", err)
	}
	return cfg
}
