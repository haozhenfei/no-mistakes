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
	// Runtime is the machine's instrumentation verdict for this hunk (one of the
	// RuntimeXxx classes), written only by Backfill and never settable by an
	// agent. It answers a different question from State: State is the class of
	// evidence someone recorded, Runtime is whether the coverage engine saw the
	// code run, saw it NOT run, or could not account for it at all. Empty on rows
	// written before an audit (and on legacy rows).
	Runtime string
	// RuntimeDetail explains the Runtime class in one sentence.
	RuntimeDetail string
}

// Blind reports whether the coverage engine could not account for this hunk even
// though the code enclosing it ran. Such a hunk is NOT runtime-verified — it is
// the honest third answer, and callers must keep it visible rather than round it
// to either a pass or a non-execution.
func (e LedgerEntry) Blind() bool { return e.Runtime == RuntimeUninstrumented }

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
	// Runtime is the classification that caused the downgrade — notably
	// uninstrumented, which means "the engine could not account for these lines",
	// not "these lines never ran".
	Runtime string
}

// Backfill reconciles the ledger against captured instrumentation coverage. It
// is the ground-truth step (design §4 principle 4): the runtime-verified state
// is decided by whether an instrumentation run actually executed the hunk, not
// by the agent's label.
//
// For every entry:
//   - Every entry gets the machine's Runtime classification (executed /
//     not-executed / uninstrumented / no-data), whatever its State.
//   - If instrumentation executed the hunk, the state becomes runtime-verified
//     and the covering coverage-evidence IDs are attached (a promotion when the
//     agent had claimed something weaker — the machine found real execution).
//   - If it did not and the entry claimed runtime-verified, it is downgraded (to
//     static-verified when it still carries static evidence, otherwise to
//     attested) and a Downgrade is recorded. The reason is taken from the
//     classification, so a hunk the engine could not instrument is no longer told
//     — falsely — that no coverage intersects it because it never ran.
//   - Otherwise the entry is left as the gate wrote it.
//
// coverageEvidenceIDs maps each CoverageData to the evidence ID it came from,
// parallel to datasets, so attached evidence is traceable. A nil/short slice
// just yields no attached IDs.
//
// staticOK is the SAME executable-static-evidence test the audit applies
// (§4.4c c) and decides where a downgrade lands. It used to be "the entry has
// any evidence at all", which let an attested screenshot masquerade as static
// evidence: the identical hunk landed on static-verified in one run and on
// attested in the next, purely from which evidence IDs the agent happened to
// cite. A nil staticOK keeps the old permissive behavior for callers that have
// no evidence index.
func Backfill(entries []LedgerEntry, datasets []CoverageData, coverageEvidenceIDs []string, staticOK StaticEvidenceChecker) ([]LedgerEntry, []Downgrade) {
	out := make([]LedgerEntry, len(entries))
	copy(out, entries)
	var downgrades []Downgrade
	for i := range out {
		e := &out[i]
		v := ClassifyHunk(e.Hunk(), datasets, coverageEvidenceIDs)
		e.Runtime = v.Class
		e.RuntimeDetail = v.Detail
		switch {
		case v.Executed():
			if e.State != StateRuntimeVerified {
				e.Reason = ""
			}
			e.State = StateRuntimeVerified
			e.Evidence = mergeEvidence(e.Evidence, v.Evidence)
			e.Source = "backfill"
		case e.State == StateRuntimeVerified:
			to := StateAttested
			if staticEvidenceOK(*e, staticOK) {
				to = StateStaticVerified
			}
			reason := "claimed runtime-verified but " + v.Detail
			downgrades = append(downgrades, Downgrade{Hunk: e.Hunk(), From: e.State, To: to, Reason: reason, Runtime: v.Class})
			e.State = to
			e.Reason = reason
			e.Source = "backfill"
		}
	}
	return out, downgrades
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
	// NotExecuted are hunks instrumentation positively watched and saw never run.
	// This is the dangerous set: changed code that provably did not execute.
	NotExecuted []Hunk
	// Blind are hunks the engine emitted no record for even though the statement
	// enclosing them executed. They are NOT runtime-verified and stay in
	// Uncovered — the audit reports the engine's blindness, it does not paper
	// over it — but they are also not evidence of non-execution, and callers
	// (verify) must not treat them as such.
	Blind []Hunk
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
		// The runtime class is a property of the diff and the instrumentation, not
		// of the ledger row, so it is recorded for every changed hunk — including
		// one no gate wrote a row for.
		switch ClassifyHunk(h, datasets, nil).Class {
		case RuntimeNotExecuted:
			report.NotExecuted = append(report.NotExecuted, h)
		case RuntimeUninstrumented:
			report.Blind = append(report.Blind, h)
		}

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
	sortHunks(report.NotExecuted)
	sortHunks(report.Blind)
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

// String renders a compact one-line audit summary for logs. The two runtime
// counts are reported separately on purpose: "3 hunks the engine cannot
// instrument" and "3 hunks that never ran" demand opposite responses, and the
// old single number said the second when it meant the first.
func (r AuditReport) String() string {
	s := fmt.Sprintf("coverage audit: %d/%d hunks runtime-verified (%.0f%%), %d static, %d attested, %d unverified, %d issue(s)",
		r.RuntimeVerified, r.TotalHunks, r.CoverageRate*100, r.StaticVerified, r.Attested, r.Unverified, len(r.Issues))
	if len(r.NotExecuted) > 0 {
		s += fmt.Sprintf(", %d not executed", len(r.NotExecuted))
	}
	if len(r.Blind) > 0 {
		s += fmt.Sprintf(", %d uninstrumented (engine emitted no records)", len(r.Blind))
	}
	return s
}
