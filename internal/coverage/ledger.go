package coverage

import (
	"fmt"
	"sort"
)

// LedgerEntry is one row of the coverage ledger: a changed hunk plus its
// verification state, the evidence backing that state, and (when unverified or
// downgraded) a reason. Gates fill the ledger; backfill corrects the runtime
// state from instrumentation truth; the audit reads it (design §5).
type LedgerEntry struct {
	ID        string
	RunID     string
	File      string
	StartLine int
	EndLine   int
	State     string   // one of the four StateXxx values
	Reason    string   // required for unverified; set on downgrade
	Evidence  []string // evidence IDs backing the state
	Source    string   // which gate/agent/backfill last wrote the row
	CreatedAt int64
}

// Hunk returns the entry's changed span.
func (e LedgerEntry) Hunk() Hunk {
	return Hunk{File: e.File, Start: e.StartLine, End: e.EndLine}
}

// Downgrade records a state change backfill made because the ledger's declared
// state was not supported by instrumentation. It is the audit trail of the
// anti-abuse mechanism: an agent that mass-labelled hunks "runtime-verified"
// leaves one Downgrade per unsupported label.
type Downgrade struct {
	Hunk   Hunk
	From   string
	To     string
	Reason string
}

// Backfill reconciles the ledger against captured instrumentation coverage. It
// is the ground-truth step (design §4 principle 4): the runtime-verified state
// is decided by whether an instrumentation run actually executed the hunk, not
// by the agent's label.
//
// For every entry:
//   - If instrumentation covered the hunk, the state becomes runtime-verified
//     and the covering coverage-evidence IDs are attached (a promotion when the
//     agent had claimed something weaker — the machine found real execution).
//   - If instrumentation did NOT cover the hunk and the entry claimed
//     runtime-verified, it is downgraded (to static-verified when it still
//     carries static evidence, otherwise to attested) with an explanatory
//     reason, and a Downgrade is recorded.
//   - Otherwise the entry is left as the gate wrote it.
//
// coverageEvidenceIDs maps each CoverageData to the evidence ID it came from,
// parallel to datasets, so attached evidence is traceable. A nil/short slice
// just yields no attached IDs.
func Backfill(entries []LedgerEntry, datasets []CoverageData, coverageEvidenceIDs []string) ([]LedgerEntry, []Downgrade) {
	out := make([]LedgerEntry, len(entries))
	copy(out, entries)
	var downgrades []Downgrade
	for i := range out {
		e := &out[i]
		covered, evIDs := coveringEvidence(e.Hunk(), datasets, coverageEvidenceIDs)
		switch {
		case covered:
			if e.State != StateRuntimeVerified {
				e.Reason = ""
			}
			e.State = StateRuntimeVerified
			e.Evidence = mergeEvidence(e.Evidence, evIDs)
			e.Source = "backfill"
		case e.State == StateRuntimeVerified:
			to := StateAttested
			if hasStaticEvidence(*e) {
				to = StateStaticVerified
			}
			reason := "claimed runtime-verified but no captured instrumentation coverage intersects this hunk"
			downgrades = append(downgrades, Downgrade{Hunk: e.Hunk(), From: e.State, To: to, Reason: reason})
			e.State = to
			e.Reason = reason
			e.Source = "backfill"
		}
	}
	return out, downgrades
}

// coveringEvidence reports whether any dataset covers the hunk and returns the
// evidence IDs of the datasets that did (datasets with no paired ID still count
// toward coverage, they just contribute no traceable ID).
func coveringEvidence(h Hunk, datasets []CoverageData, ids []string) (bool, []string) {
	covered := false
	var hits []string
	for i, d := range datasets {
		if !HunkCovered(h, []CoverageData{d}) {
			continue
		}
		covered = true
		if i < len(ids) && ids[i] != "" {
			hits = append(hits, ids[i])
		}
	}
	return covered, hits
}

// hasStaticEvidence is a placeholder for "the entry carries evidence that could
// justify a static-verified state". Backfill cannot itself judge static evidence
// quality (that is the audit's job, §4.4c c); it only preserves a static state
// when evidence exists rather than dropping straight to attested.
func hasStaticEvidence(e LedgerEntry) bool {
	return len(e.Evidence) > 0
}

func mergeEvidence(existing, add []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range existing {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range add {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// StaticEvidenceChecker reports whether an evidence ID names captured, executable
// static evidence (e.g. a typecheck or AST-equivalence command output) rather
// than an attested natural-language artifact. The audit uses it to enforce
// §4.4c(c): a static-verified hunk must hang off executable static evidence.
type StaticEvidenceChecker func(evidenceID string) bool

// AuditIssue is a single machine-detected ledger defect.
type AuditIssue struct {
	Kind   string // "missing-ledger", "false-runtime", "weak-static", "unverified"
	Hunk   Hunk
	Detail string
}

// AuditReport is the machine verdict of the coverage audit (design §4.4c). It is
// pure data: the verify step turns it into findings and the dossier renders it;
// §6 risk routing (not built here) consumes the coverage rate and uncovered
// list.
type AuditReport struct {
	TotalHunks      int
	RuntimeVerified int
	StaticVerified  int
	Attested        int
	Unverified      int
	Issues          []AuditIssue
	Uncovered       []Hunk // hunks not runtime-verified (data for §6 routing)
	// CoverageRate is runtime-verified hunks / total changed hunks, in [0,1].
	CoverageRate float64
	// Pass is true when there are no completeness, false-runtime, or weak-static
	// issues. It does not itself park the run — routing decides that.
	Pass bool
}

// Audit runs the three §4.4c machine checks over the changed hunks and the
// (already backfilled) ledger:
//
//	(a) completeness — every changed hunk has a ledger entry;
//	(b) truth        — every runtime-verified hunk is covered by captured
//	                   instrumentation;
//	(c) static rigor — every static-verified hunk hangs off captured executable
//	                   static evidence, not a natural-language claim.
//
// It also computes the final coverage rate and the uncovered list. staticOK may
// be nil, in which case any evidence on a static-verified hunk is accepted
// (checks c degrades to "has some evidence").
func Audit(changed []Hunk, ledger []LedgerEntry, datasets []CoverageData, staticOK StaticEvidenceChecker) AuditReport {
	report := AuditReport{TotalHunks: len(changed)}

	// Index ledger entries by hunk for completeness and per-hunk lookups.
	byHunk := map[Hunk]LedgerEntry{}
	for _, e := range ledger {
		byHunk[e.Hunk()] = e
	}

	for _, h := range changed {
		entry, ok := byHunk[h]
		if !ok {
			report.Issues = append(report.Issues, AuditIssue{
				Kind:   "missing-ledger",
				Hunk:   h,
				Detail: "changed hunk has no coverage ledger entry",
			})
			report.Uncovered = append(report.Uncovered, h)
			continue
		}
		switch entry.State {
		case StateRuntimeVerified:
			if !HunkCovered(h, datasets) {
				// (b) A runtime-verified label with no instrumentation behind it
				// is exactly what backfill exists to prevent; flag it if it
				// survived.
				report.Issues = append(report.Issues, AuditIssue{
					Kind:   "false-runtime",
					Hunk:   h,
					Detail: "hunk is labelled runtime-verified but no captured instrumentation covers it",
				})
				report.Uncovered = append(report.Uncovered, h)
				continue
			}
			report.RuntimeVerified++
		case StateStaticVerified:
			if !staticEvidenceOK(entry, staticOK) {
				report.Issues = append(report.Issues, AuditIssue{
					Kind:   "weak-static",
					Hunk:   h,
					Detail: "hunk is labelled static-verified but carries no captured executable static evidence",
				})
			} else {
				report.StaticVerified++
			}
			report.Uncovered = append(report.Uncovered, h)
		case StateAttested:
			report.Attested++
			report.Uncovered = append(report.Uncovered, h)
		default: // unverified or unknown
			report.Unverified++
			report.Uncovered = append(report.Uncovered, h)
		}
	}

	if report.TotalHunks > 0 {
		report.CoverageRate = float64(report.RuntimeVerified) / float64(report.TotalHunks)
	}
	report.Pass = len(report.Issues) == 0
	sortHunks(report.Uncovered)
	return report
}

func staticEvidenceOK(entry LedgerEntry, staticOK StaticEvidenceChecker) bool {
	if len(entry.Evidence) == 0 {
		return false
	}
	if staticOK == nil {
		return true
	}
	for _, id := range entry.Evidence {
		if staticOK(id) {
			return true
		}
	}
	return false
}

func sortHunks(hs []Hunk) {
	sort.Slice(hs, func(i, j int) bool {
		if hs[i].File != hs[j].File {
			return hs[i].File < hs[j].File
		}
		return hs[i].Start < hs[j].Start
	})
}

// String renders a compact one-line audit summary for logs.
func (r AuditReport) String() string {
	return fmt.Sprintf("coverage audit: %d/%d hunks runtime-verified (%.0f%%), %d static, %d attested, %d unverified, %d issue(s)",
		r.RuntimeVerified, r.TotalHunks, r.CoverageRate*100, r.StaticVerified, r.Attested, r.Unverified, len(r.Issues))
}
