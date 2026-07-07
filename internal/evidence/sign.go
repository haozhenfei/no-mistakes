package evidence

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// keySize is the length in bytes of the HMAC signing key.
const keySize = 32

// LoadOrCreateKey reads the HMAC signing key from keyPath, creating a fresh
// random key (0600) if the file does not yet exist. The whole security model
// depends on keyPath living OUTSIDE the worktree (the daemon's private state
// dir under NM_HOME) so an agent that can write into the worktree cannot forge
// a signature by rewriting the key. See design §3.1.
//
// MVP trust-boundary note: with local (in-worktree) collector execution, the
// collector process runs as the same OS user as the daemon and could in
// principle read this key. Moving collection into the daemon so the key never
// enters an agent-reachable process is the deferred hardening (design §11.6).
func LoadOrCreateKey(keyPath string) ([]byte, error) {
	data, err := os.ReadFile(keyPath)
	if err == nil {
		key, decodeErr := decodeKey(data)
		if decodeErr != nil {
			return nil, fmt.Errorf("evidence key %s is corrupt: %w", keyPath, decodeErr)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read evidence key: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate evidence key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, fmt.Errorf("create evidence key dir: %w", err)
	}
	encoded := []byte(hex.EncodeToString(key) + "\n")
	if err := os.WriteFile(keyPath, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("write evidence key: %w", err)
	}
	return key, nil
}

func decodeKey(data []byte) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, err
	}
	if len(key) < keySize {
		return nil, fmt.Errorf("key too short: %d bytes", len(key))
	}
	return key, nil
}

// SignatureFor computes the hex HMAC-SHA256 signature over the canonical form
// of an entry (every field except Signature itself). Because the canonical form
// includes SHA256 (the artifact hash), tampering with either the metadata or
// the artifact bytes invalidates the signature.
func SignatureFor(entry Entry, key []byte) (string, error) {
	canonical, err := canonicalBytes(entry)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Sign returns a copy of entry with its Signature field populated.
func Sign(entry Entry, key []byte) (Entry, error) {
	sig, err := SignatureFor(entry, key)
	if err != nil {
		return Entry{}, err
	}
	entry.Signature = sig
	return entry, nil
}

// Verify reports whether entry's Signature matches its content under key. It is
// constant-time in the comparison to avoid leaking via timing.
func Verify(entry Entry, key []byte) bool {
	if entry.Signature == "" {
		return false
	}
	want, err := SignatureFor(entry, key)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(entry.Signature), []byte(want)) == 1
}

// canonicalBytes marshals the entry with an empty Signature so signing and
// verification hash exactly the same bytes. Go's encoding/json emits struct
// fields in declaration order and sorts map keys, so the output is
// deterministic without a bespoke canonicalizer.
func canonicalBytes(entry Entry) ([]byte, error) {
	entry.Signature = ""
	return json.Marshal(entry)
}

// HashBytes returns the hex SHA-256 of b, the artifact-hash convention used in
// manifest entries.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashFile returns the hex SHA-256 of the file at path.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}
