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

import "fmt"

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

// FileCoverage is what one instrumentation run recorded about a single file:
// the line ranges it executed (Covered) and the line ranges it instrumented but
// never executed (Uncovered).
//
// Uncovered is what makes the difference between "this code did not run" and
// "the engine has nothing to say about this code" decidable. A line the engine
// emitted no record for at all is in NEITHER list, and that absence is only
// meaningful when the dataset enumerates every record — see Enumerated.
type FileCoverage struct {
	File    string      `json:"file"`
	Covered []LineRange `json:"covered"`
	// Uncovered are instrumented lines with a zero hit count: the engine watched
	// them and they did not run. This is the positive proof of non-execution.
	Uncovered []LineRange `json:"uncovered,omitempty"`
	// Enumerated reports that Covered+Uncovered together list every line the
	// engine emitted a record for in this file, so a line in neither list is a
	// line the engine could not account for. Datasets captured by an older
	// collector recorded only executed lines; they leave this false, and the
	// classifier then falls back to the old covered/not-covered reading rather
	// than inferring anything from an absence it cannot trust.
	Enumerated bool `json:"enumerated,omitempty"`
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

// Runtime classes: what captured instrumentation can actually say about a
// changed hunk. This is the machine's runtime dimension, orthogonal to the four
// evidence StateXxx values above, and it is never agent-writable — Backfill
// derives it from the coverage data on every audit.
//
// The distinction the first three encode is the whole point of the type. Asking
// only "does an executed line intersect this hunk?" collapses two very different
// situations into one wrong answer: a hunk the engine watched and saw never run,
// and a hunk the engine structurally cannot emit a record for. The second is not
// evidence of non-execution — it is the absence of evidence — and treating it as
// the first is what downgraded three executed JSX hunks on coze MR 6951 and then
// capped two independently-confirmed behavior claims as "NO RUNTIME EVIDENCE".
const (
	// RuntimeExecuted: an executed line record intersects the hunk. The hunk ran.
	RuntimeExecuted = "executed"
	// RuntimeNotExecuted: the engine instrumented these lines (or the statement
	// that encloses them) and recorded zero hits. The hunk did not run. This is
	// the state that must never be lost — an unexecuted change is the thing the
	// gate exists to catch.
	RuntimeNotExecuted = "not-executed"
	// RuntimeUninstrumented: the engine emitted no record for these lines at all,
	// yet the nearest enclosing recorded statement DID execute. The engine cannot
	// account for the lines; the code around them ran. v8 folds JSX attribute
	// lines into the enclosing `return`, so a changed className lands here.
	RuntimeUninstrumented = "uninstrumented"
	// RuntimeNoData: no usable instrumentation for this file (no dataset carries
	// it, or the dataset cannot enumerate its records). Nothing is known either
	// way — which, for the gate, is as weak as not-executed.
	RuntimeNoData = "no-data"
)

// RuntimeVerdict is the classification of one hunk against captured
// instrumentation, plus a sentence a reviewer can act on.
type RuntimeVerdict struct {
	Class  string
	Detail string
	// Evidence are the coverage-evidence IDs of the datasets that executed the
	// hunk (only set for RuntimeExecuted).
	Evidence []string
}

// Executed reports whether instrumentation observed the hunk running.
func (v RuntimeVerdict) Executed() bool { return v.Class == RuntimeExecuted }

// ClassifyHunk decides what the captured instrumentation says about a hunk.
//
// The rules, in strict order — each earlier rule is stronger evidence than the
// ones below it, and the order is chosen so the dangerous mistake (calling
// un-executed code executed) cannot happen:
//
//  1. An executed record intersects the hunk anywhere → executed.
//  2. A zero-hit record intersects the hunk → not-executed. Positive proof of
//     non-execution beats any inference about enclosure.
//  3. No record touches the hunk, the dataset enumerates its records, and the
//     nearest recorded line BEFORE the hunk executed → uninstrumented. The
//     engine attributes a construct to the first line of the statement that
//     contains it, so "the record immediately above ran" is the strongest
//     available statement about the enclosing executable unit.
//  4. Anything else → no-data.
//
// Rule 3 uses the enclosing STATEMENT record, not the enclosing function. In
// real v8 lcov for a component with two returns, both `className=` lines carry
// no DA record while the function's FNDA is 1 — but one of them sits under a
// `return (` with 0 hits (a branch that never rendered) and the other under a
// `return (` with 1 hit. A function-level signal calls both executed and loses a
// genuinely dead hunk; the statement-level signal separates them.
func ClassifyHunk(h Hunk, datasets []CoverageData, ids []string) RuntimeVerdict {
	var evidence []string
	executed := false
	notExecuted := false
	uninstrumented := false
	var enclosing int

	hr := h.Range()
	for i, d := range datasets {
		for _, fc := range d.Files {
			if !fileMatch(fc.File, h.File) {
				continue
			}
			if overlapsAny(hr, fc.Covered) {
				executed = true
				if i < len(ids) && ids[i] != "" {
					evidence = append(evidence, ids[i])
				}
				continue
			}
			if overlapsAny(hr, fc.Uncovered) {
				notExecuted = true
				continue
			}
			if !fc.Enumerated {
				// The dataset lists only executed lines, so it cannot distinguish
				// "no record" from "record with zero hits". Infer nothing.
				continue
			}
			line, hit, ok := nearestRecordBefore(fc, h.Start)
			switch {
			case !ok:
				// Nothing recorded above the hunk: no enclosing executable unit to
				// speak for it. Stay silent rather than guess.
			case hit:
				uninstrumented = true
				enclosing = line
			default:
				// The statement that encloses these lines was watched and never
				// ran, so neither did they. This is the JSX attribute inside a
				// branch that never rendered.
				notExecuted = true
			}
		}
	}

	switch {
	case executed:
		return RuntimeVerdict{
			Class:    RuntimeExecuted,
			Detail:   "captured instrumentation executed this hunk",
			Evidence: evidence,
		}
	case notExecuted:
		return RuntimeVerdict{
			Class:  RuntimeNotExecuted,
			Detail: "captured instrumentation watched these lines (or the statement enclosing them) and recorded zero hits: the code did not run",
		}
	case uninstrumented:
		return RuntimeVerdict{
			Class:  RuntimeUninstrumented,
			Detail: fmt.Sprintf("the coverage engine emitted no line record for this hunk, but the enclosing statement it folds these lines into (line %d) executed — the engine cannot account for these lines, which is not evidence that they did not run", enclosing),
		}
	default:
		return RuntimeVerdict{
			Class:  RuntimeNoData,
			Detail: "no captured instrumentation covers this file",
		}
	}
}

func overlapsAny(r LineRange, ranges []LineRange) bool {
	for _, o := range ranges {
		if o.Overlaps(r) {
			return true
		}
	}
	return false
}

// nearestRecordBefore returns the highest recorded line strictly below start and
// whether it executed. That record is the engine's own account of the statement
// the un-recorded lines belong to.
//
// A line carrying both an executed and an unexecuted record (two blocks opening
// on one line) resolves to unexecuted: at a tie, the classifier fails toward
// not-executed, never toward a pass.
func nearestRecordBefore(fc FileCoverage, start int) (line int, hit bool, ok bool) {
	for _, r := range fc.Covered {
		if l := lastLineBelow(r, start); l > line {
			line, hit, ok = l, true, true
		}
	}
	for _, r := range fc.Uncovered {
		if l := lastLineBelow(r, start); l > 0 && l >= line {
			line, hit, ok = l, false, true
		}
	}
	return line, hit, ok
}

// lastLineBelow returns the highest line of r that is strictly below start, or 0
// when r has none.
func lastLineBelow(r LineRange, start int) int {
	if r.Start >= start {
		return 0
	}
	if r.End < start {
		return r.End
	}
	return start - 1
}

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
