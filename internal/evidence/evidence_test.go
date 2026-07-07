package evidence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	t := time.Unix(1_700_000_000, 0)
	return func() time.Time { return t }
}

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "evidence.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	return key
}

func TestSignVerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	entry := Entry{
		ID:         "ev-1",
		Kind:       KindCommandOutput,
		Provenance: ProvenanceCaptured,
		Argv:       []string{"go", "test"},
		SHA256:     HashBytes([]byte("hello")),
		EnvFingerprint: map[string]string{
			"os":     "linux",
			"branch": "fm/x",
		},
	}
	signed, err := Sign(entry, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if signed.Signature == "" {
		t.Fatal("expected non-empty signature")
	}
	if !Verify(signed, key) {
		t.Fatal("freshly signed entry should verify")
	}
}

func TestVerifyDetectsMetadataTamper(t *testing.T) {
	key := testKey(t)
	signed, err := Sign(Entry{ID: "ev-1", Argv: []string{"echo", "ok"}, SHA256: "abc"}, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	tampered := signed
	tampered.Argv = []string{"echo", "malicious"}
	if Verify(tampered, key) {
		t.Fatal("tampered argv should fail verification")
	}
}

func TestVerifyDetectsArtifactHashTamper(t *testing.T) {
	key := testKey(t)
	signed, _ := Sign(Entry{ID: "ev-1", SHA256: HashBytes([]byte("real"))}, key)
	tampered := signed
	tampered.SHA256 = HashBytes([]byte("forged"))
	if Verify(tampered, key) {
		t.Fatal("tampered artifact hash should fail verification")
	}
}

func TestVerifyFailsWithWrongKey(t *testing.T) {
	key1 := testKey(t)
	key2 := testKey(t)
	signed, _ := Sign(Entry{ID: "ev-1", SHA256: "abc"}, key1)
	if Verify(signed, key2) {
		t.Fatal("entry must not verify under a different key")
	}
}

func TestVerifyRejectsUnsignedEntry(t *testing.T) {
	key := testKey(t)
	if Verify(Entry{ID: "ev-1"}, key) {
		t.Fatal("entry without a signature must not verify")
	}
}

func TestLoadOrCreateKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perms = %o, want 600", perm)
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(k1) != string(k2) {
		t.Fatal("key should be stable across loads")
	}
}

func TestExecCapturesAndSigns(t *testing.T) {
	repoRoot := t.TempDir()
	key := testKey(t)
	store, err := Open(DirForBranch(repoRoot, "fm/login-fix"), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entry, err := store.Exec(context.Background(), ExecOpts{
		Label:    "login e2e",
		Argv:     []string{"printf", "PASS"},
		Dir:      repoRoot,
		RepoRoot: repoRoot,
		Commit:   "abc1234",
		RunID:    "run-1",
		Branch:   "fm/login-fix",
		Now:      fixedClock(),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if entry.Provenance != ProvenanceCaptured {
		t.Fatalf("provenance = %q, want captured", entry.Provenance)
	}
	if entry.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", entry.ExitCode)
	}
	if entry.SHA256 != HashBytes([]byte("PASS")) {
		t.Fatalf("sha256 mismatch: %q", entry.SHA256)
	}
	if !Verify(entry, key) {
		t.Fatal("captured entry should verify")
	}
	// stdout artifact written under the evidence dir.
	stdout := filepath.Join(store.ArtifactDir(entry.ID), "stdout.txt")
	data, err := os.ReadFile(stdout)
	if err != nil {
		t.Fatalf("read stdout artifact: %v", err)
	}
	if string(data) != "PASS" {
		t.Fatalf("stdout artifact = %q, want PASS", data)
	}
}

func TestExecRecordsNonZeroExit(t *testing.T) {
	repoRoot := t.TempDir()
	key := testKey(t)
	store, _ := Open(DirForBranch(repoRoot, "b"), key)
	entry, err := store.Exec(context.Background(), ExecOpts{
		Label:    "failing",
		Argv:     []string{"sh", "-c", "exit 7"},
		Dir:      repoRoot,
		RepoRoot: repoRoot,
		Now:      fixedClock(),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if entry.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", entry.ExitCode)
	}
	if !Verify(entry, key) {
		t.Fatal("entry should still verify on non-zero exit")
	}
}

func TestAttachIsAlwaysAttested(t *testing.T) {
	repoRoot := t.TempDir()
	key := testKey(t)
	file := filepath.Join(repoRoot, "screenshot.png")
	if err := os.WriteFile(file, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	store, _ := Open(DirForBranch(repoRoot, "b"), key)
	entry, err := store.Attach(AttachOpts{
		Label:    "login screenshot",
		File:     file,
		RepoRoot: repoRoot,
		Branch:   "b",
		Now:      fixedClock(),
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if entry.Provenance != ProvenanceAttested {
		t.Fatalf("provenance = %q, want attested", entry.Provenance)
	}
	if entry.SHA256 != HashBytes([]byte("PNGDATA")) {
		t.Fatal("sha256 should hash the attached bytes")
	}
	if !Verify(entry, key) {
		t.Fatal("attested entry should verify (registration integrity)")
	}
}

func TestLoadAllVerifiesAndFlagsTamper(t *testing.T) {
	repoRoot := t.TempDir()
	key := testKey(t)
	store, _ := Open(DirForBranch(repoRoot, "fm/x"), key)
	if _, err := store.Exec(context.Background(), ExecOpts{
		Label: "ok", Argv: []string{"printf", "ok"}, Dir: repoRoot, RepoRoot: repoRoot, Now: fixedClock(),
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	loaded, err := LoadAll(repoRoot, key)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d entries, want 1", len(loaded))
	}
	if !loaded[0].Verified {
		t.Fatal("entry should verify")
	}
	if loaded[0].EffectiveProvenance() != ProvenanceCaptured {
		t.Fatal("verified captured entry keeps captured provenance")
	}

	// A reader using the WRONG key sees the same entry as unverified and thus
	// downgraded to attested — the render-time enforcement of design §3.1.
	otherKey := testKey(t)
	loadedWrong, err := LoadAll(repoRoot, otherKey)
	if err != nil {
		t.Fatalf("LoadAll wrong key: %v", err)
	}
	if loadedWrong[0].Verified {
		t.Fatal("entry must not verify under the wrong key")
	}
	if !loadedWrong[0].Tampered() {
		t.Fatal("captured entry that fails verification must be flagged tampered")
	}
	if loadedWrong[0].EffectiveProvenance() != ProvenanceAttested {
		t.Fatal("unverified captured entry must render as attested at best")
	}
}

func TestBranchSlug(t *testing.T) {
	cases := map[string]string{
		"fm/login-fix":     "fm-login-fix",
		"feature/AB_123.4": "feature-AB_123.4",
		"///":              "",
		"main":             "main",
	}
	for in, want := range cases {
		if got := BranchSlug(in); got != want {
			t.Errorf("BranchSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
