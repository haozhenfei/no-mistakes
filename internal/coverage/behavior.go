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

// IsBehaviorKind reports whether a claim kind asserts runtime-observable
// behavior. rule-compliance and non-goal claims make no runtime assertion, so
// they are not subject to the runtime-backing requirement.
func IsBehaviorKind(kind string) bool {
	return kind == claims.KindBehavior || kind == claims.KindRegressionFixed
}

// UnbackedBehaviorClaims returns one BehaviorGap per behavior-class claim that
// no runtime-verified hunk supports. ledger must be the POST-backfill ledger, so
// runtime-verified means "instrumentation actually executed this hunk", never an
// agent label.
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
// An empty ledger — including the fail-closed case where the coverage audit
// could not run — leaves every behavior claim unbacked. "Could not check" is not
// "checked and clean".
func UnbackedBehaviorClaims(cs []claims.Claim, ledger []LedgerEntry) []BehaviorGap {
	runtimeSomewhere := false
	for _, e := range ledger {
		if e.State == StateRuntimeVerified {
			runtimeSomewhere = true
			break
		}
	}

	var gaps []BehaviorGap
	for _, c := range cs {
		if !IsBehaviorKind(c.Kind) || c.SelfAttested() {
			// Self-attested claims are already excluded from the conclusions by
			// machine rule (claims.SelfAttested) and the verify step never
			// adjudicates them, so there is no pass to prevent.
			continue
		}
		named := matchLedgerEntries(c.Hunks, ledger)
		if len(named) > 0 {
			if anyRuntimeVerified(named) {
				continue
			}
			gaps = append(gaps, BehaviorGap{
				ClaimID: c.ID,
				Kind:    c.Kind,
				Text:    c.Text,
				Hunks:   entryHunks(named),
				Detail: fmt.Sprintf("every hunk it names is %s; none is runtime-verified, so no instrumentation ever executed the changed code it claims to have fixed",
					describeStates(named)),
			})
			continue
		}
		if runtimeSomewhere {
			continue
		}
		gaps = append(gaps, BehaviorGap{
			ClaimID: c.ID,
			Kind:    c.Kind,
			Text:    c.Text,
			Detail:  "no changed hunk in this run is runtime-verified — the run captured no instrumentation showing the changed code executing",
		})
	}
	return gaps
}

func anyRuntimeVerified(entries []LedgerEntry) bool {
	for _, e := range entries {
		if e.State == StateRuntimeVerified {
			return true
		}
	}
	return false
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
