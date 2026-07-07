package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/claims"
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
