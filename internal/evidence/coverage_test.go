package evidence

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
)

// writeCoverScript writes a shell script that emits a fixed go coverprofile to
// the given path, so the collector has a real profile to parse without needing
// a Go toolchain in the test.
func writeCoverProfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

func TestCoverage_ParsesAndSignsGoProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell command")
	}
	dir := t.TempDir()
	key := testKey(t)
	store, err := Open(filepath.Join(dir, "evidence"), key)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	profile := filepath.Join(dir, "cover.out")
	// The "command" just writes a canned profile; the collector parses whatever
	// the command leaves at --cover-profile.
	profileContent := "mode: set\ngithub.com/org/repo/foo.go:10.2,12.3 2 1\ngithub.com/org/repo/foo.go:20.2,22.3 2 0\n"
	writeCoverProfile(t, profile, profileContent)

	entry, err := store.Coverage(context.Background(), CoverageOpts{
		Label:        "unit tests",
		Argv:         []string{"true"}, // no-op; profile already on disk
		Format:       "go",
		Dir:          dir,
		RepoRoot:     dir,
		CoverProfile: profile,
		Commit:       "abc123",
		Branch:       "fm/x",
		Now:          fixedClock(),
	})
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if entry.Kind != KindCoverage || entry.Provenance != ProvenanceCaptured {
		t.Fatalf("unexpected kind/provenance: %+v", entry)
	}
	if entry.Coverage == nil {
		t.Fatal("expected parsed coverage attached to entry")
	}
	if len(entry.Coverage.Files) != 1 || entry.Coverage.Files[0].File != "github.com/org/repo/foo.go" {
		t.Fatalf("coverage files wrong: %+v", entry.Coverage.Files)
	}
	// Only the executed block (count 1) is covered: lines 10-12. The count-0
	// block (20-22) must NOT appear.
	covered := entry.Coverage.Files[0].Covered
	if len(covered) != 1 || covered[0].Start != 10 || covered[0].End != 12 {
		t.Fatalf("covered ranges wrong: %+v", covered)
	}

	// The parsed coverage is inside the signed canonical bytes, so tampering with
	// it must break verification.
	if !Verify(entry, key) {
		t.Fatal("coverage entry should verify")
	}
	tampered := entry
	forged := *entry.Coverage
	forged.Files = []coverage.FileCoverage{{File: "github.com/org/repo/foo.go", Covered: []coverage.LineRange{{Start: 20, End: 22}}}}
	tampered.Coverage = &forged
	if Verify(tampered, key) {
		t.Fatal("tampering with coverage data must break the signature")
	}
}

func TestCoverage_UnsupportedFormatDegradesHonestly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell command")
	}
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "evidence"), testKey(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	profile := filepath.Join(dir, "cov.dat")
	writeCoverProfile(t, profile, "whatever")

	entry, err := store.Coverage(context.Background(), CoverageOpts{
		Label:        "python tests",
		Argv:         []string{"true"},
		Format:       "python", // unsupported
		Dir:          dir,
		RepoRoot:     dir,
		CoverProfile: profile,
		Now:          fixedClock(),
	})
	// The run is still recorded (captured command output), but with no coverage
	// data — and the degradation is surfaced, not swallowed.
	if err == nil {
		t.Fatal("expected a degradation error for unsupported format")
	}
	if entry.ID == "" {
		t.Fatal("run should still be recorded even when coverage is unsupported")
	}
	if entry.Coverage != nil {
		t.Fatal("unsupported format must attach NO coverage (never fabricate)")
	}
}

func TestCoverage_RequiresProfilePath(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(filepath.Join(dir, "evidence"), testKey(t))
	if _, err := store.Coverage(context.Background(), CoverageOpts{
		Label: "x", Argv: []string{"true"}, Format: "go", Dir: dir,
	}); err == nil {
		t.Fatal("expected error when --cover-profile is missing")
	}
}
