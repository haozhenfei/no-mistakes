package evidence

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestName is the append-only signed manifest inside an evidence dir.
const manifestName = "manifest.jsonl"

// Store is a single evidence directory (one per branch) plus the signing key.
type Store struct {
	dir string
	key []byte
}

// Open returns a Store rooted at dir, creating the directory if needed.
func Open(dir string, key []byte) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create evidence dir: %w", err)
	}
	return &Store{dir: dir, key: key}, nil
}

// Dir returns the store's evidence directory.
func (s *Store) Dir() string { return s.dir }

// ManifestPath returns the manifest file path.
func (s *Store) ManifestPath() string { return filepath.Join(s.dir, manifestName) }

// NewID mints a short, collision-resistant evidence ID like "ev-7f3a1c".
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing is catastrophic; fall back to a fixed marker so the
		// caller still gets a usable (if non-unique) id rather than panicking.
		return "ev-000000"
	}
	return "ev-" + hex.EncodeToString(b[:])
}

// ArtifactDir returns the per-entry artifact directory for id.
func (s *Store) ArtifactDir(id string) string { return filepath.Join(s.dir, id) }

// Append signs entry and appends it to the manifest. It returns the signed
// entry. The manifest is JSON-lines so concurrent single-writer appends stay
// append-only and human-diffable when committed.
func (s *Store) Append(entry Entry) (Entry, error) {
	signed, err := Sign(entry, s.key)
	if err != nil {
		return Entry{}, err
	}
	line, err := json.Marshal(signed)
	if err != nil {
		return Entry{}, fmt.Errorf("marshal manifest entry: %w", err)
	}
	f, err := os.OpenFile(s.ManifestPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Entry{}, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Entry{}, fmt.Errorf("write manifest entry: %w", err)
	}
	return signed, nil
}

// Entries returns all manifest entries in this store, in file order.
func (s *Store) Entries() ([]Entry, error) {
	return readManifest(s.ManifestPath())
}

func readManifest(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	scanner := bufio.NewScanner(f)
	// Evidence lines can carry inline output; allow generous line lengths.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			// Skip unparseable lines rather than failing the whole read; a
			// tampered manifest should degrade to "fewer verified entries",
			// not a hard error that hides the rest.
			continue
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

// LoadedEntry is a manifest entry annotated with whether its signature verified
// under the current key. The renderer treats Verified==false entries as
// attested-at-best and flags them, never as captured (design §3.1).
type LoadedEntry struct {
	Entry
	Verified     bool
	ManifestPath string
}

// EffectiveProvenance is the provenance the renderer should trust: an entry that
// claims "captured" but fails verification is downgraded to attested.
func (e LoadedEntry) EffectiveProvenance() string {
	if e.Provenance == ProvenanceCaptured && !e.Verified {
		return ProvenanceAttested
	}
	return e.Provenance
}

// Tampered reports an entry that claims captured provenance but does not verify.
func (e LoadedEntry) Tampered() bool {
	return e.Provenance == ProvenanceCaptured && !e.Verified
}

// LoadAll walks the evidence root under repoRoot, reads every manifest, and
// verifies each entry against key. It is deliberately branch-slug agnostic: it
// finds every manifest.jsonl beneath .no-mistakes/evidence so the renderer does
// not need to recompute whatever slug the collector chose. Entries are returned
// sorted by ID for deterministic rendering.
func LoadAll(repoRoot string, key []byte) ([]LoadedEntry, error) {
	root := Root(repoRoot)
	var loaded []LoadedEntry
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Name() != manifestName {
			return nil
		}
		entries, readErr := readManifest(path)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			loaded = append(loaded, LoadedEntry{
				Entry:        entry,
				Verified:     Verify(entry, key),
				ManifestPath: path,
			})
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].ID < loaded[j].ID })
	return loaded, nil
}
