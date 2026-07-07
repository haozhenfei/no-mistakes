// Package evidence implements the Evidence Vault: a signed manifest of
// artifacts that a pipeline (or an in-run agent) produces while validating a
// change. Its whole reason for existing is the captured/attested trust
// boundary described in the design (evidence-review-design.md §3):
//
//   - "captured" evidence is produced by a trusted collector (evidence exec)
//     that records the exact command, cwd, environment fingerprint, output, and
//     exit code, then signs the manifest entry with a key kept OUTSIDE the
//     worktree. A reviewer (and the dossier renderer) can trust that the output
//     really came from that command at that commit.
//   - "attested" evidence is an agent-supplied artifact (evidence attach). The
//     signature only says "this file was registered at time T"; it makes no
//     claim about how the file was produced.
//
// The signature attests COLLECTION AUTHENTICITY ONLY, never semantic validity —
// whether the evidence actually supports a claim is a judgement made by the
// verify step, not something the signature can back (design principle 2).
//
// MVP scope: this package writes evidence in "branch-commit" mode
// (design §3.4 option 1) under <repo>/.no-mistakes/evidence/<branch-slug>/ so
// the artifacts ride the branch and render on the PR. Reproducibility fields
// (normalized_sha256, replay, record/replay) and the daemon-side collection
// service are intentionally deferred.
package evidence

import (
	"path/filepath"
	"strings"
)

// Provenance levels. See package doc for the trust semantics.
const (
	// ProvenanceCaptured marks evidence produced and signed by a trusted
	// collector (evidence exec). Only entries whose signature verifies are
	// treated as captured at render time.
	ProvenanceCaptured = "captured"
	// ProvenanceAttested marks agent-supplied artifacts (evidence attach). The
	// signature covers only registration, not how the file was produced.
	ProvenanceAttested = "attested"
)

// Evidence kinds (MVP subset of design §3.3).
const (
	KindCommandOutput = "command-output"
	KindFile          = "file"
)

// Entry is a single manifest record. It mirrors design §3.3 minus the
// reproducibility fields (normalized_sha256, normalizer_version, replay), which
// are deferred for MVP. The Signature covers every other field plus the
// artifact SHA-256; see sign.go.
type Entry struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Provenance     string            `json:"provenance"`
	Collector      string            `json:"collector"`
	Label          string            `json:"label"`
	Argv           []string          `json:"argv,omitempty"`
	CWD            string            `json:"cwd,omitempty"`
	Commit         string            `json:"commit,omitempty"`
	RunID          string            `json:"run_id,omitempty"`
	ExitCode       int               `json:"exit_code"`
	DurationMS     int64             `json:"duration_ms"`
	SHA256         string            `json:"sha256"`
	EnvFingerprint map[string]string `json:"env_fingerprint,omitempty"`
	Paths          []string          `json:"paths,omitempty"`
	Claims         []string          `json:"claims,omitempty"`
	CreatedAt      int64             `json:"created_at"`
	Signature      string            `json:"signature,omitempty"`
}

// evidenceRootRel is the in-repo directory (relative to the worktree root) that
// holds all evidence for branch-commit mode.
const evidenceRootRel = ".no-mistakes/evidence"

// Root returns the evidence root directory inside a worktree.
func Root(repoRoot string) string {
	return filepath.Join(repoRoot, evidenceRootRel)
}

// DirForBranch returns the per-branch evidence directory. Evidence for a single
// run lives under a branch-slug subdirectory so committed evidence from
// different branches never collides in the tree.
func DirForBranch(repoRoot, branch string) string {
	slug := BranchSlug(branch)
	if slug == "" {
		slug = "detached"
	}
	return filepath.Join(Root(repoRoot), slug)
}

// BranchSlug turns a branch name into a filesystem-safe slug. Path separators
// become dashes so the whole branch maps to a single directory segment.
func BranchSlug(branch string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
