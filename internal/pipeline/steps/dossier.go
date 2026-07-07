package steps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
)

// DossierData is the fully-resolved input to the dossier renderer. It is
// deliberately plain data (no DB or filesystem access) so rendering is a pure,
// deterministic template — the reviewer-facing summary is never agent prose
// (design §7).
type DossierData struct {
	RunID    string
	Commit   string
	Claims   []claims.Claim
	Evidence map[string]evidence.LoadedEntry // keyed by evidence id
	Verdicts []db.VerifyVerdict
}

// Provenance markers. 🔒 attests COLLECTION AUTHENTICITY ONLY (the output really
// came from that command at that commit) — never semantic correctness. 💬 marks
// agent-attested material, whose content is not machine-backed at all.
const (
	markerCaptured = "🔒"
	markerAttested = "💬"
)

// BuildDossier renders the evidence dossier markdown: a verdict banner, a
// claims-and-evidence conclusions table, a separated self-attested/unverified
// section, and a provenance footnote. Returns "" when there is nothing to show
// (no claims and no verdicts).
func BuildDossier(d DossierData) string {
	conclusions, selfAttested := splitClaims(d.Claims)
	refutedFindings := refutedFindingVerdicts(d.Verdicts)
	if len(conclusions) == 0 && len(selfAttested) == 0 && len(refutedFindings) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Evidence Dossier\n\n")
	b.WriteString(dossierBanner(conclusions, refutedFindings))
	b.WriteString("\n\n")

	if len(conclusions) > 0 {
		b.WriteString(dossierConclusionsTable(d, conclusions))
		b.WriteString("\n")
	}
	if len(refutedFindings) > 0 {
		b.WriteString(dossierRefutedFindings(refutedFindings))
		b.WriteString("\n")
	}
	if selfSection := dossierSelfAttestedSection(d, selfAttested); selfSection != "" {
		b.WriteString(selfSection)
		b.WriteString("\n")
	}
	b.WriteString(dossierFootnote(d))
	return strings.TrimRight(b.String(), "\n")
}

func splitClaims(all []claims.Claim) (conclusions, selfAttested []claims.Claim) {
	for _, c := range all {
		if c.SelfAttested() {
			selfAttested = append(selfAttested, c)
		} else {
			conclusions = append(conclusions, c)
		}
	}
	return conclusions, selfAttested
}

func refutedFindingVerdicts(verdicts []db.VerifyVerdict) []db.VerifyVerdict {
	var out []db.VerifyVerdict
	for _, v := range verdicts {
		if strings.HasPrefix(v.ClaimID, "finding:") && v.Verdict == claims.VerdictRefuted {
			out = append(out, v)
		}
	}
	return out
}

func dossierBanner(conclusions []claims.Claim, refutedFindings []db.VerifyVerdict) string {
	var confirmed, plausible, refuted int
	for _, c := range conclusions {
		switch c.Verdict {
		case claims.VerdictConfirmed:
			confirmed++
		case claims.VerdictPlausible:
			plausible++
		case claims.VerdictRefuted:
			refuted++
		}
	}
	refuted += len(refutedFindings)

	verdict := "PASS"
	emoji := "✅"
	switch {
	case refuted > 0:
		verdict, emoji = "FAIL", "❌"
	case plausible > 0:
		verdict, emoji = "PASS WITH ISSUES", "⚠️"
	}

	// Deliberately non-authoritative wording: state what survived verification,
	// not "verified, no problems" (design §7 anti-overtrust).
	return fmt.Sprintf(
		"%s **%s** — %d claim(s) survived adversarial verification, %d plausible, %d refuted.",
		emoji, verdict, confirmed, plausible, refuted,
	)
}

func dossierConclusionsTable(d DossierData, conclusions []claims.Claim) string {
	var b strings.Builder
	b.WriteString("### Claims\n\n")
	b.WriteString("| Claim | Verdict | Provenance | Evidence |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, c := range conclusions {
		marker, _ := claimProvenance(d, c)
		b.WriteString(fmt.Sprintf(
			"| %s | %s | %s | %s |\n",
			mdCell(c.Text),
			verdictCell(c.Verdict),
			marker,
			evidenceLinks(d, c.Evidence),
		))
	}
	return b.String()
}

func dossierRefutedFindings(refuted []db.VerifyVerdict) string {
	var b strings.Builder
	b.WriteString("### Refuted findings\n\n")
	for _, v := range refuted {
		subject := strings.TrimPrefix(v.ClaimID, "finding:")
		b.WriteString(fmt.Sprintf("- ❌ %s — %s\n", mdCell(subject), mdCell(v.Rationale)))
	}
	return b.String()
}

// dossierSelfAttestedSection lists evidence-less claims and attested-only
// evidence together, separated from the conclusions so a reviewer never mistakes
// self-attested material for verified conclusions (design §5, §7).
func dossierSelfAttestedSection(d DossierData, selfAttested []claims.Claim) string {
	attestedEvidence := attestedEntries(d)
	if len(selfAttested) == 0 && len(attestedEvidence) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Self-attested, unverified\n\n")
	b.WriteString("_The material below is not machine-backed as verified conclusions._\n\n")
	for _, c := range selfAttested {
		b.WriteString(fmt.Sprintf("- %s %s\n", markerAttested, mdCell(c.Text)))
	}
	for _, e := range attestedEvidence {
		flag := ""
		if e.Tampered() {
			flag = " ⚠️ signature invalid — treat as unverified"
		}
		b.WriteString(fmt.Sprintf("- %s %s (%s)%s\n", markerAttested, mdCell(e.Label), e.ID, flag))
	}
	return b.String()
}

func dossierFootnote(d DossierData) string {
	commit := d.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf(
		"---\n_Dossier for run `%s` at commit `%s`. %s attests collection authenticity only — that the output came from the recorded command at this commit — not semantic correctness, which is a reviewer judgement. Signatures are verified at render time; entries that fail verification are shown as attested and flagged._",
		d.RunID, commit, markerCaptured,
	)
}

// claimProvenance returns the provenance marker for a claim: 🔒 only when every
// bound evidence entry is present and verified-captured; 💬 otherwise (any
// attested, missing, or signature-invalid evidence).
func claimProvenance(d DossierData, c claims.Claim) (string, bool) {
	allCaptured := len(c.Evidence) > 0
	for _, id := range c.Evidence {
		e, ok := d.Evidence[id]
		if !ok || e.EffectiveProvenance() != evidence.ProvenanceCaptured {
			allCaptured = false
			break
		}
	}
	if allCaptured {
		return markerCaptured, true
	}
	return markerAttested, false
}

func evidenceLinks(d DossierData, ids []string) string {
	if len(ids) == 0 {
		return "—"
	}
	var parts []string
	for _, id := range ids {
		e, ok := d.Evidence[id]
		if !ok {
			parts = append(parts, fmt.Sprintf("%s (missing)", id))
			continue
		}
		label := id
		if e.Tampered() {
			label = id + " ⚠️"
		}
		if len(e.Paths) > 0 {
			parts = append(parts, fmt.Sprintf("[%s](%s)", label, e.Paths[0]))
		} else {
			parts = append(parts, label)
		}
	}
	return strings.Join(parts, ", ")
}

func attestedEntries(d DossierData) []evidence.LoadedEntry {
	var out []evidence.LoadedEntry
	for _, e := range d.Evidence {
		if e.EffectiveProvenance() == evidence.ProvenanceAttested {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func verdictCell(verdict string) string {
	switch verdict {
	case claims.VerdictConfirmed:
		return "CONFIRMED"
	case claims.VerdictPlausible:
		return "PLAUSIBLE"
	case claims.VerdictRefuted:
		return "REFUTED"
	default:
		return "unverified"
	}
}

// mdCell escapes a value for use inside a markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}
