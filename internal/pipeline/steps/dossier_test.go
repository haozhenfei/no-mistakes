package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
)

func capturedEntry(id, label string, verified bool) evidence.LoadedEntry {
	return evidence.LoadedEntry{
		Entry:    evidence.Entry{ID: id, Kind: evidence.KindCommandOutput, Provenance: evidence.ProvenanceCaptured, Label: label, Paths: []string{".no-mistakes/evidence/b/" + id + "/stdout.txt"}},
		Verified: verified,
	}
}

func attestedEntry(id, label string) evidence.LoadedEntry {
	return evidence.LoadedEntry{
		Entry:    evidence.Entry{ID: id, Kind: evidence.KindFile, Provenance: evidence.ProvenanceAttested, Label: label},
		Verified: true,
	}
}

func TestBuildDossier_PassBannerAndCapturedMarker(t *testing.T) {
	data := DossierData{
		RunID:  "run-1",
		Commit: "abcdef123456",
		Claims: []claims.Claim{
			{ID: "c1", Text: "login no longer overflows", Verdict: claims.VerdictConfirmed, Evidence: []string{"ev-1"}},
		},
		Evidence: map[string]evidence.LoadedEntry{
			"ev-1": capturedEntry("ev-1", "login e2e", true),
		},
	}
	out := BuildDossier(data)
	if !strings.Contains(out, "✅ **PASS**") {
		t.Fatalf("expected PASS banner, got:\n%s", out)
	}
	if !strings.Contains(out, markerCaptured) {
		t.Fatal("verified captured evidence must show the captured marker")
	}
	if !strings.Contains(out, "[ev-1](.no-mistakes/evidence/b/ev-1/stdout.txt)") {
		t.Fatalf("expected evidence link, got:\n%s", out)
	}
	if !strings.Contains(out, "attests collection authenticity only") {
		t.Fatal("footnote must state collection-authenticity-only semantics")
	}
}

func TestBuildDossier_FailBannerOnRefuted(t *testing.T) {
	data := DossierData{
		RunID: "run-1",
		Claims: []claims.Claim{
			{ID: "c1", Text: "works", Verdict: claims.VerdictRefuted, Evidence: []string{"ev-1"}},
		},
		Evidence: map[string]evidence.LoadedEntry{"ev-1": capturedEntry("ev-1", "x", true)},
	}
	out := BuildDossier(data)
	if !strings.Contains(out, "❌ **FAIL**") {
		t.Fatalf("expected FAIL banner, got:\n%s", out)
	}
}

func TestBuildDossier_PassWithIssuesOnPlausible(t *testing.T) {
	data := DossierData{
		RunID: "run-1",
		Claims: []claims.Claim{
			{ID: "c1", Text: "works", Verdict: claims.VerdictPlausible, Evidence: []string{"ev-1"}},
		},
		Evidence: map[string]evidence.LoadedEntry{"ev-1": capturedEntry("ev-1", "x", true)},
	}
	out := BuildDossier(data)
	if !strings.Contains(out, "⚠️ **PASS WITH ISSUES**") {
		t.Fatalf("expected PASS WITH ISSUES banner, got:\n%s", out)
	}
}

func TestBuildDossier_SelfAttestedClaimsSeparated(t *testing.T) {
	data := DossierData{
		RunID: "run-1",
		Claims: []claims.Claim{
			{ID: "c1", Text: "confirmed thing", Verdict: claims.VerdictConfirmed, Evidence: []string{"ev-1"}},
			{ID: "c2", Text: "trust me it works"}, // no evidence -> self-attested
		},
		Evidence: map[string]evidence.LoadedEntry{"ev-1": capturedEntry("ev-1", "x", true)},
	}
	out := BuildDossier(data)
	conclusionsIdx := strings.Index(out, "### Claims")
	selfIdx := strings.Index(out, "### Self-attested, unverified")
	if conclusionsIdx < 0 || selfIdx < 0 {
		t.Fatalf("expected both sections, got:\n%s", out)
	}
	if selfIdx < conclusionsIdx {
		t.Fatal("self-attested section must come after conclusions")
	}
	// The self-attested claim must NOT appear in the conclusions table.
	conclusions := out[conclusionsIdx:selfIdx]
	if strings.Contains(conclusions, "trust me it works") {
		t.Fatal("self-attested claim must never appear in conclusions")
	}
	if !strings.Contains(out[selfIdx:], "trust me it works") {
		t.Fatal("self-attested claim must appear in the self-attested section")
	}
}

func TestBuildDossier_TamperedCapturedDowngradedToAttested(t *testing.T) {
	data := DossierData{
		RunID: "run-1",
		Claims: []claims.Claim{
			{ID: "c1", Text: "works", Verdict: claims.VerdictConfirmed, Evidence: []string{"ev-1"}},
		},
		Evidence: map[string]evidence.LoadedEntry{
			// claims captured but signature does NOT verify -> tampered.
			"ev-1": capturedEntry("ev-1", "forged", false),
		},
	}
	out := BuildDossier(data)
	// Provenance marker for the claim must be attested (not captured) since its
	// only evidence failed verification.
	claimsSection := out[strings.Index(out, "### Claims"):]
	if strings.Contains(firstTableRow(claimsSection), markerCaptured) {
		t.Fatalf("tampered evidence must not earn the captured marker:\n%s", out)
	}
	if !strings.Contains(out, "signature invalid") {
		t.Fatal("tampered captured evidence must be visibly flagged")
	}
}

func TestBuildDossier_RefutedFinding(t *testing.T) {
	data := DossierData{
		RunID: "run-1",
		Verdicts: []db.VerifyVerdict{
			{ClaimID: "finding:[review finding] nil deref", Verdict: claims.VerdictRefuted, Rationale: "confirmed real bug"},
		},
	}
	out := BuildDossier(data)
	if !strings.Contains(out, "❌ **FAIL**") {
		t.Fatalf("refuted finding should FAIL the banner:\n%s", out)
	}
	if !strings.Contains(out, "Refuted findings") || !strings.Contains(out, "nil deref") {
		t.Fatalf("refuted finding must be listed:\n%s", out)
	}
}

func TestBuildDossier_EmptyWhenNothing(t *testing.T) {
	if out := BuildDossier(DossierData{RunID: "run-1"}); out != "" {
		t.Fatalf("empty dossier should render nothing, got:\n%s", out)
	}
}

// firstTableRow returns the first markdown table data row (after the header
// separator) from a section, or the whole string if none is found.
func firstTableRow(section string) string {
	lines := strings.Split(section, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "| ---") && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return section
}

func TestBuildDossier_CoverageLedgerSection(t *testing.T) {
	rate := coverage.AuditReport{
		TotalHunks:      3,
		RuntimeVerified: 1,
		Attested:        1,
		Unverified:      1,
		CoverageRate:    1.0 / 3.0,
		Issues: []coverage.AuditIssue{
			{Kind: "weak-static", Hunk: coverage.Hunk{File: "conf.go", Start: 5, End: 6}, Detail: "no captured executable static evidence"},
		},
	}
	data := DossierData{
		RunID:  "run-1",
		Commit: "abcdef123456",
		Ledger: []coverage.LedgerEntry{
			{File: "calc.go", StartLine: 5, EndLine: 7, State: coverage.StateRuntimeVerified, Evidence: []string{"ev-cov"}},
			{File: "calc.go", StartLine: 10, EndLine: 13, State: coverage.StateAttested, Reason: "no coverage intersects"},
			{File: "conf.go", StartLine: 5, EndLine: 6, State: coverage.StateUnverified, Reason: "config only"},
		},
		CoverageReport: &rate,
	}
	out := BuildDossier(data)

	if !strings.Contains(out, "### Coverage ledger") {
		t.Fatalf("expected coverage ledger section, got:\n%s", out)
	}
	// The banner shows the instrumentation-backfilled runtime coverage count.
	if !strings.Contains(out, "runtime coverage") || !strings.Contains(out, "1/3") {
		t.Fatalf("expected runtime-coverage banner 1/3, got:\n%s", out)
	}
	// Runtime-verified rows carry the machine-backfill (captured) marker.
	if !strings.Contains(out, markerCaptured+" runtime-verified") {
		t.Fatalf("expected runtime-verified marker, got:\n%s", out)
	}
	// Unverified rows must show their reason.
	if !strings.Contains(out, "config only") {
		t.Fatalf("expected unverified reason, got:\n%s", out)
	}
	// Audit issues surface beneath the table.
	if !strings.Contains(out, "weak-static") {
		t.Fatalf("expected audit issue, got:\n%s", out)
	}
}

func TestBuildDossier_CoverageOnlyStillRenders(t *testing.T) {
	// A run with no claims but a coverage ledger still produces a dossier.
	data := DossierData{
		RunID:  "run-1",
		Commit: "abc",
		Ledger: []coverage.LedgerEntry{
			{File: "x.go", StartLine: 1, EndLine: 2, State: coverage.StateUnverified, Reason: "no gate recorded verification"},
		},
		CoverageReport: &coverage.AuditReport{TotalHunks: 1, Unverified: 1},
	}
	out := BuildDossier(data)
	if out == "" {
		t.Fatal("expected a dossier for a coverage-only run")
	}
	if !strings.Contains(out, "### Coverage ledger") {
		t.Fatalf("expected coverage ledger, got:\n%s", out)
	}
}

// The third state must be visible in the dossier, not silently upgraded to a
// pass and not silently reported as dead code. A reviewer reading the dossier
// has to be able to tell "the engine watched this and it never ran" from "the
// engine cannot emit records here".
func TestBuildDossier_ShowsBlindHunksApartFromUnexecutedOnes(t *testing.T) {
	blind := coverage.Hunk{File: "src/Row.tsx", Start: 14, End: 14}
	dead := coverage.Hunk{File: "src/Row.tsx", Start: 7, End: 7}
	data := DossierData{
		RunID:  "run-1",
		Commit: "abc",
		Ledger: []coverage.LedgerEntry{
			{
				File: blind.File, StartLine: blind.Start, EndLine: blind.End,
				State: coverage.StateAttested, Runtime: coverage.RuntimeUninstrumented,
				Reason: "the coverage engine emitted no line record for this hunk",
			},
			{
				File: dead.File, StartLine: dead.Start, EndLine: dead.End,
				State: coverage.StateAttested, Runtime: coverage.RuntimeNotExecuted,
			},
		},
		CoverageReport: &coverage.AuditReport{
			TotalHunks: 2, Attested: 2,
			Blind:       []coverage.Hunk{blind},
			NotExecuted: []coverage.Hunk{dead},
		},
	}
	out := BuildDossier(data)
	if !strings.Contains(out, "uninstrumented (engine blind here)") {
		t.Fatalf("the blind hunk's runtime class must show in the ledger table, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT executed") {
		t.Fatalf("the unexecuted hunk must stay loud, got:\n%s", out)
	}
	if !strings.Contains(out, "could not instrument (unmeasured, NOT unexecuted)") {
		t.Fatalf("the dossier must name the engine's blindness as such, got:\n%s", out)
	}
	if !strings.Contains(out, "Changed code instrumentation saw NOT execute") {
		t.Fatalf("the dossier must list provably unexecuted changed code separately, got:\n%s", out)
	}
	if strings.Contains(out, "🔒 runtime-verified") {
		t.Fatalf("a blind hunk must never render as a machine-backed pass, got:\n%s", out)
	}
}
