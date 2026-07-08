package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceCoverageCapturesGoProfile(t *testing.T) {
	setupInRunWorktree(t)

	// Pre-write a go coverprofile the "command" leaves behind (the collector
	// parses whatever is at --cover-profile after the run).
	cwd, _ := os.Getwd()
	profile := filepath.Join(cwd, "cover.out")
	if err := os.WriteFile(profile, []byte("mode: set\nexample.com/p/foo.go:1.1,3.2 2 1\n"), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	out, err := executeCmd("evidence", "coverage", "--label", "unit tests", "--format", "go", "--cover-profile", "cover.out", "--", "true")
	if err != nil {
		t.Fatalf("evidence coverage: %v (%s)", err, out)
	}
	if !strings.Contains(out, "coverage") || !strings.Contains(out, "files=1") {
		t.Fatalf("unexpected coverage output: %q", out)
	}

	list, err := executeCmd("evidence", "list")
	if err != nil {
		t.Fatalf("evidence list: %v (%s)", err, list)
	}
	if !strings.Contains(list, "coverage") {
		t.Fatalf("coverage entry not listed: %q", list)
	}
}

func TestEvidenceCoverageRequiresProfile(t *testing.T) {
	setupInRunWorktree(t)
	out, err := executeCmd("evidence", "coverage", "--label", "x", "--", "true")
	if err == nil {
		t.Fatalf("expected error without --cover-profile, got %q", out)
	}
	if !strings.Contains(err.Error(), "cover-profile") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCoverageAddAndList(t *testing.T) {
	setupInRunWorktree(t)

	out, err := executeCmd("coverage", "add", "--file", "calc.go", "--start", "10", "--end", "12", "--state", "runtime-verified", "--evidence", "ev-cov")
	if err != nil {
		t.Fatalf("coverage add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "calc.go:10-12") || !strings.Contains(out, "runtime-verified") {
		t.Fatalf("unexpected add output: %q", out)
	}

	if _, err := executeCmd("coverage", "add", "--file", "conf.go", "--start", "1", "--end", "2", "--state", "unverified", "--reason", "config only"); err != nil {
		t.Fatalf("coverage add unverified: %v", err)
	}

	list, err := executeCmd("coverage", "list")
	if err != nil {
		t.Fatalf("coverage list: %v (%s)", err, list)
	}
	if !strings.Contains(list, "calc.go:10-12") || !strings.Contains(list, "conf.go:1-2") {
		t.Fatalf("coverage list missing entries: %q", list)
	}
	if !strings.Contains(list, "config only") {
		t.Fatalf("unverified reason not shown: %q", list)
	}
}

func TestCoverageAddRejectsInvalidState(t *testing.T) {
	setupInRunWorktree(t)
	if _, err := executeCmd("coverage", "add", "--file", "a.go", "--start", "1", "--end", "2", "--state", "verified"); err == nil {
		t.Fatal("expected error for invalid state")
	}
}

func TestCoverageAddUnverifiedRequiresReason(t *testing.T) {
	setupInRunWorktree(t)
	if _, err := executeCmd("coverage", "add", "--file", "a.go", "--start", "1", "--end", "2", "--state", "unverified"); err == nil {
		t.Fatal("expected error: unverified requires --reason")
	}
}
