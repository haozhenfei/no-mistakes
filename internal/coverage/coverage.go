// Package coverage implements the coverage ledger and its instrumentation-backed
// truth backfill (design evidence-review-design.md §4 principle 4, §4.4c, §5).
//
// The ledger records, for every changed diff hunk, one of four verification
// states. The whole point of the subsystem is that the "runtime-verified" state
// is not something an agent can assert into being: it is DERIVED from code
// coverage instrumentation captured by a trusted collector. Backfill overwrites
// agent-declared runtime state with the machine truth (a hunk is runtime
// verified iff an instrumentation run actually executed it), and the audit
// re-checks the ledger against the diff and the instrumentation independently.
//
// This directly counters "agent bulk-labels every hunk verified": labels are not
// trusted, executed lines are (design principle 6 — machine-decision inputs must
// be objective quantities, never agent subjective labels).
package coverage

// Verification states for a ledger hunk (design §4 principle 4, §5). The set is
// closed: every changed hunk resolves to exactly one of these.
const (
	// StateRuntimeVerified: the hunk was executed during a captured
	// instrumentation run (machine fact, backfilled from coverage data).
	StateRuntimeVerified = "runtime-verified"
	// StateStaticVerified: the hunk is backed by executable static evidence
	// (typecheck / AST-equivalence tool output). A natural-language claim does
	// NOT qualify — the audit enforces captured static evidence (§4.4c c).
	StateStaticVerified = "static-verified"
	// StateAttested: the hunk is only self-attested by an agent, with no
	// machine-backed evidence.
	StateAttested = "attested"
	// StateUnverified: the hunk is not verified; Reason must explain why.
	StateUnverified = "unverified"
)

// ValidState reports whether s is one of the four closed ledger states.
func ValidState(s string) bool {
	switch s {
	case StateRuntimeVerified, StateStaticVerified, StateAttested, StateUnverified:
		return true
	default:
		return false
	}
}

// LineRange is an inclusive [Start, End] span of 1-based line numbers.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Overlaps reports whether two inclusive line ranges intersect.
func (r LineRange) Overlaps(o LineRange) bool {
	return r.Start <= o.End && o.Start <= r.End
}

// FileCoverage is the set of executed line ranges for a single file, as parsed
// from an instrumentation profile. Only lines that actually ran (count > 0) are
// recorded; unexecuted instrumented lines are omitted.
type FileCoverage struct {
	File    string      `json:"file"`
	Covered []LineRange `json:"covered"`
}

// CoverageData is structured, parsed coverage for one instrumentation run: the
// ground-truth artifact that backfill and the audit consume. It is stored inline
// in the signed evidence manifest entry, so the coverage numbers a reviewer sees
// are cryptographically bound to the collector run that produced them.
type CoverageData struct {
	Format string         `json:"format"` // "go", "lcov", ...
	Files  []FileCoverage `json:"files"`
}

// Hunk is a changed span of a file: an inclusive new-file line range. Hunks are
// the rows of the coverage ledger (design §5).
type Hunk struct {
	File  string
	Start int
	End   int
}

// Range returns the hunk's line span as a LineRange.
func (h Hunk) Range() LineRange { return LineRange{Start: h.Start, End: h.End} }

// coveredRangesFor returns every executed line range recorded for a hunk's file
// across all supplied coverage datasets, matched by fileMatch so a
// module-prefixed instrumentation path (Go) still lines up with a repo-relative
// hunk path.
func coveredRangesFor(file string, datasets []CoverageData) []LineRange {
	var out []LineRange
	for _, d := range datasets {
		for _, fc := range d.Files {
			if fileMatch(fc.File, file) {
				out = append(out, fc.Covered...)
			}
		}
	}
	return out
}

// HunkCovered reports whether any executed line range in datasets overlaps the
// hunk. This is the atomic instrumentation-truth test used by both backfill and
// the audit.
func HunkCovered(h Hunk, datasets []CoverageData) bool {
	hr := h.Range()
	for _, r := range coveredRangesFor(h.File, datasets) {
		if r.Overlaps(hr) {
			return true
		}
	}
	return false
}
