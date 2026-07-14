package coverage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/claims"
)

// BehaviorGap is a behavior-class claim (behavior / regression-fixed) that no
// runtime-verified hunk backs. Such a claim asserts what a user observes at
// runtime, so the only thing that can support it is evidence that the changed
// code actually ran: an instrumentation-backed runtime-verified hunk. "attested"
// is the ledger honestly reporting that no such evidence exists, and a behavior
// claim resting on attested hunks is therefore an unsupported runtime pass — the
// exact shape of the coze 6951 failure, where a synthetic render page (the real
// app was never started) backed a "the regression is fixed" claim while all four
// product hunks sat at attested and the run still completed.
//
// The gap is data. The verify step decides the consequence (it caps the verdict
// and parks); the ledger states themselves are not rewritten here.
type BehaviorGap struct {
	ClaimID string
	Kind    string
	Text    string
	// Hunks are the ledger hunks the claim named, none of which is
	// runtime-verified. Empty when the claim named no resolvable hunk and the
	// run-wide fallback decided the gap.
	Hunks []Hunk
	// Detail explains, in one sentence, why the claim has no runtime backing.
	Detail string
}

// BlindSpot is a behavior-class claim whose changed code the coverage engine
// could not account for: no hunk backing it is runtime-verified, but no hunk
// backing it is un-executed either — the engine emitted no records for those
// lines while the statements enclosing them ran.
//
// This is NOT a BehaviorGap. A gap means the machine looked and found the code
// never ran; a blind spot means the machine cannot look. Capping a claim for the
// second is what turned a v8/JSX className change into two "NO RUNTIME EVIDENCE"
// findings on coze MR 6951 against the skeptics' own CONFIRMED verdicts. The
// verify step therefore leaves the verdict alone and reports the blindness as a
// visible finding: unmeasured, not unbacked.
type BlindSpot struct {
	ClaimID string
	Kind    string
	Text    string
	// Hunks are the uninstrumented hunks backing the claim.
	Hunks []Hunk
	// Detail explains, in one sentence, what the engine could not see.
	Detail string
}

// IsBehaviorKind reports whether a claim kind asserts runtime-observable
// behavior. rule-compliance and non-goal claims make no runtime assertion, so
// they are not subject to the runtime-backing requirement.
func IsBehaviorKind(kind string) bool {
	return kind == claims.KindBehavior || kind == claims.KindRegressionFixed
}

// BehaviorBacking splits the behavior-class claims into the two ways a runtime
// pass can lack instrumentation behind it: gaps (the machine looked and the code
// did not run, or nothing was captured at all) and blind spots (the machine
// cannot look, because the engine emits no records for those lines while the
// code around them ran). ledger must be the POST-backfill ledger, so both the
// runtime-verified state and the Runtime class are machine facts, never labels.
//
// Backing is decided per claim when the claim names hunks it covers
// (`claim add --hunks file:start-end`), and run-wide otherwise: a claim that
// names no hunk is unbacked exactly when the run recorded no runtime-verified
// hunk at all, because then no instrumentation observed any changed line and
// nothing in the run can support a runtime pass. That fallback keeps the check
// silent for repos whose tests do produce coverage (a Go repo running
// `evidence coverage` promotes the hunks it executes) while still firing for a
// run like 6951, where nothing ran.
//
// A gap always wins over a blind spot. If ANY hunk backing the claim is
// positively un-executed, the claim is unbacked even when its other hunks are
// only uninstrumented: one line of provably dead changed code is enough to sink
// a runtime pass, and the engine's blindness elsewhere cannot buy it back.
//
// An empty ledger — including the fail-closed case where the coverage audit
// could not run — leaves every behavior claim unbacked. "Could not check" is not
// "checked and clean"; and a hunk with no Runtime class at all (an old row, a
// file no instrumentation reached) is a gap, not a blind spot, because a blind
// spot is a POSITIVE finding about an executed enclosing statement.
func BehaviorBacking(cs []claims.Claim, ledger []LedgerEntry) ([]BehaviorGap, []BlindSpot) {
	runtimeSomewhere, notExecutedSomewhere, blindSomewhere := false, false, false
	for _, e := range ledger {
		switch {
		case backedByRuntime(e):
			runtimeSomewhere = true
		case e.Runtime == RuntimeNotExecuted:
			notExecutedSomewhere = true
		case e.Blind():
			blindSomewhere = true
		}
	}

	var gaps []BehaviorGap
	var blind []BlindSpot
	for _, c := range cs {
		if !IsBehaviorKind(c.Kind) || c.SelfAttested() {
			// Self-attested claims are already excluded from the conclusions by
			// machine rule (claims.SelfAttested) and the verify step never
			// adjudicates them, so there is no pass to prevent.
			continue
		}
		named := matchLedgerEntries(c.Hunks, ledger)
		if len(named) > 0 {
			switch {
			case anyRuntimeVerified(named):
				continue
			case anyNotExecuted(named):
				gaps = append(gaps, BehaviorGap{
					ClaimID: c.ID,
					Kind:    c.Kind,
					Text:    c.Text,
					Hunks:   entryHunks(named),
					Detail: fmt.Sprintf("instrumentation watched the changed code it names and recorded zero hits on %s; none of its hunks is runtime-verified, so the code it claims to have fixed never ran",
						describeHunks(notExecutedEntries(named))),
				})
			case anyBlind(named):
				blind = append(blind, BlindSpot{
					ClaimID: c.ID,
					Kind:    c.Kind,
					Text:    c.Text,
					Hunks:   entryHunks(blindEntries(named)),
					Detail: fmt.Sprintf("the coverage engine emitted no line records for %s, though the statements enclosing them executed — the change is unmeasured, not unexecuted",
						describeHunks(blindEntries(named))),
				})
			default:
				gaps = append(gaps, BehaviorGap{
					ClaimID: c.ID,
					Kind:    c.Kind,
					Text:    c.Text,
					Hunks:   entryHunks(named),
					Detail: fmt.Sprintf("every hunk it names is %s; none is runtime-verified, so no instrumentation ever executed the changed code it claims to have fixed",
						describeStates(named)),
				})
			}
			continue
		}
		switch {
		case runtimeSomewhere:
			continue
		case !notExecutedSomewhere && blindSomewhere:
			blind = append(blind, BlindSpot{
				ClaimID: c.ID,
				Kind:    c.Kind,
				Text:    c.Text,
				Detail:  "no changed hunk in this run is runtime-verified, but the coverage engine emitted no records for the changed lines it could not instrument while the code enclosing them ran — the change is unmeasured, not unexecuted",
			})
		default:
			gaps = append(gaps, BehaviorGap{
				ClaimID: c.ID,
				Kind:    c.Kind,
				Text:    c.Text,
				Detail:  "no changed hunk in this run is runtime-verified — the run captured no instrumentation showing the changed code executing",
			})
		}
	}
	return gaps, blind
}

// backedByRuntime reports whether instrumentation executed the entry's hunk. The
// State fallback covers a ledger read back from a row written before the runtime
// class existed, where runtime-verified was still backfill-derived.
func backedByRuntime(e LedgerEntry) bool {
	if e.Runtime != "" {
		return e.Runtime == RuntimeExecuted
	}
	return e.State == StateRuntimeVerified
}

func anyRuntimeVerified(entries []LedgerEntry) bool {
	for _, e := range entries {
		if backedByRuntime(e) {
			return true
		}
	}
	return false
}

func anyNotExecuted(entries []LedgerEntry) bool { return len(notExecutedEntries(entries)) > 0 }

func anyBlind(entries []LedgerEntry) bool { return len(blindEntries(entries)) > 0 }

func notExecutedEntries(entries []LedgerEntry) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range entries {
		if e.Runtime == RuntimeNotExecuted {
			out = append(out, e)
		}
	}
	return out
}

func blindEntries(entries []LedgerEntry) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range entries {
		if e.Blind() {
			out = append(out, e)
		}
	}
	return out
}

func describeHunks(entries []LedgerEntry) string {
	hs := entryHunks(entries)
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		parts = append(parts, fmt.Sprintf("%s:%d-%d", h.File, h.Start, h.End))
	}
	return strings.Join(parts, ", ")
}

func entryHunks(entries []LedgerEntry) []Hunk {
	out := make([]Hunk, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Hunk())
	}
	sortHunks(out)
	return out
}

// describeStates renders the distinct states of the named hunks, e.g.
// "attested" or "attested, unverified".
func describeStates(entries []LedgerEntry) string {
	seen := map[string]bool{}
	var states []string
	for _, e := range entries {
		if !seen[e.State] {
			seen[e.State] = true
			states = append(states, e.State)
		}
	}
	sort.Strings(states)
	return strings.Join(states, ", ")
}

// matchLedgerEntries resolves the claim's --hunks references against the ledger.
// Accepted forms: "file:start-end", "file:line", and a bare "file" (every ledger
// entry in that file). An unresolvable reference simply matches nothing, which
// leaves the claim to the run-wide fallback rather than silently passing it.
func matchLedgerEntries(refs []string, ledger []LedgerEntry) []LedgerEntry {
	var out []LedgerEntry
	seen := map[Hunk]bool{}
	for _, ref := range refs {
		r, ok := parseHunkRef(ref)
		if !ok {
			continue
		}
		for _, e := range ledger {
			if e.File != r.file {
				continue
			}
			if r.hasRange && !r.rng.Overlaps(LineRange{Start: e.StartLine, End: e.EndLine}) {
				continue
			}
			if seen[e.Hunk()] {
				continue
			}
			seen[e.Hunk()] = true
			out = append(out, e)
		}
	}
	return out
}

type hunkRef struct {
	file     string
	rng      LineRange
	hasRange bool
}

func parseHunkRef(s string) (hunkRef, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return hunkRef{}, false
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return hunkRef{file: s}, true
	}
	file, span := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	if file == "" {
		return hunkRef{}, false
	}
	start, end, ok := parseSpan(span)
	if !ok {
		// Not a line span (a Windows drive letter, a stray colon): treat the
		// whole string as a path.
		return hunkRef{file: s}, true
	}
	return hunkRef{file: file, rng: LineRange{Start: start, End: end}, hasRange: true}, true
}

func parseSpan(span string) (int, int, bool) {
	lo, hi, found := strings.Cut(span, "-")
	start, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil || start <= 0 {
		return 0, 0, false
	}
	if !found {
		return start, start, true
	}
	end, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}
