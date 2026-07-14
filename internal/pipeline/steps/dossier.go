package steps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
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
	// Ledger is the per-hunk coverage ledger (design §5), rendered as the
	// coverage-account section. CoverageReport is the machine audit summary
	// (§4.4c): runtime-coverage rate and uncovered list. Both may be zero-valued
	// when no coverage was recorded.
	Ledger         []coverage.LedgerEntry
	CoverageReport *coverage.AuditReport
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
	if len(conclusions) == 0 && len(selfAttested) == 0 && len(refutedFindings) == 0 && len(d.Ledger) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Evidence Dossier\n\n")
	b.WriteString(dossierBanner(conclusions, refutedFindings))
	if line := dossierCoverageBanner(d); line != "" {
		b.WriteString("\n")
		b.WriteString(line)
	}
	b.WriteString("\n\n")

	if len(conclusions) > 0 {
		b.WriteString(dossierConclusionsTable(d, conclusions))
		b.WriteString("\n")
	}
	if cov := dossierCoverageSection(d); cov != "" {
		b.WriteString(cov)
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

// dossierCoverageBanner renders the runtime-coverage line for the conclusion
// banner (design §7 item 1: "改动 hunk 运行时覆盖 41/47"). The number is the
// instrumentation-backfilled count, not an agent tally — a machine fact.
func dossierCoverageBanner(d DossierData) string {
	r := d.CoverageReport
	if r == nil || r.TotalHunks == 0 {
		return ""
	}
	return fmt.Sprintf(
		"📊 Changed-hunk runtime coverage (instrumentation-backfilled): %d/%d (%.0f%%); %d static, %d attested, %d unverified.",
		r.RuntimeVerified, r.TotalHunks, r.CoverageRate*100, r.StaticVerified, r.Attested, r.Unverified,
	)
}

// coverageStateMarker maps a ledger state to a display cell. Runtime-verified
// carries the machine-backfill mark 🔒 because it is instrumentation-backed;
// the others are not runtime-executed and say so plainly.
func coverageStateMarker(state string) string {
	switch state {
	case coverage.StateRuntimeVerified:
		return markerCaptured + " runtime-verified"
	case coverage.StateStaticVerified:
		return "🧩 static-verified"
	case coverage.StateAttested:
		return markerAttested + " attested"
	default:
		return "○ unverified"
	}
}

// coverageRuntimeMarker maps the machine's instrumentation verdict to a display
// cell. It is a separate column from the evidence state on purpose: "nobody
// recorded evidence for this hunk" and "the coverage engine cannot emit records
// for this construct" are different facts, and the reviewer must be able to tell
// the engine's blindness (⊘) from code that provably never ran (✗).
func coverageRuntimeMarker(runtime string) string {
	switch runtime {
	case coverage.RuntimeExecuted:
		return markerCaptured + " executed"
	case coverage.RuntimeNotExecuted:
		return "✗ NOT executed"
	case coverage.RuntimeUninstrumented:
		return "⊘ uninstrumented (engine blind here)"
	case coverage.RuntimeNoData:
		return "— no instrumentation"
	default:
		return "—"
	}
}

// dossierCoverageSection renders the coverage ledger (design §7 item 3): a hunk
// table with the four evidence states, the machine's runtime verdict beside
// each, machine-backfill marks on runtime-verified rows, and a required reason
// on every unverified row. Audit issues are surfaced beneath the table so a
// reviewer sees exactly which changed code nobody ran — and, separately, which
// changed code the coverage engine could not measure at all.
func dossierCoverageSection(d DossierData) string {
	if len(d.Ledger) == 0 {
		return ""
	}
	ledger := append([]coverage.LedgerEntry(nil), d.Ledger...)
	sort.Slice(ledger, func(i, j int) bool {
		if ledger[i].File != ledger[j].File {
			return ledger[i].File < ledger[j].File
		}
		return ledger[i].StartLine < ledger[j].StartLine
	})

	var b strings.Builder
	b.WriteString("### Coverage ledger\n\n")
	b.WriteString("| Hunk | State | Runtime | Reason / evidence |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, e := range ledger {
		detail := e.Reason
		if detail == "" && len(e.Evidence) > 0 {
			detail = "evidence: " + strings.Join(e.Evidence, ", ")
		}
		if detail == "" {
			detail = "—"
		}
		b.WriteString(fmt.Sprintf(
			"| %s:%d-%d | %s | %s | %s |\n",
			mdCell(e.File), e.StartLine, e.EndLine, coverageStateMarker(e.State), coverageRuntimeMarker(e.Runtime), mdCell(detail),
		))
	}
	if issues := dossierCoverageIssues(d); issues != "" {
		b.WriteString("\n")
		b.WriteString(issues)
	}
	return b.String()
}

func dossierCoverageIssues(d DossierData) string {
	if d.CoverageReport == nil {
		return ""
	}
	var b strings.Builder
	if len(d.CoverageReport.Issues) > 0 {
		b.WriteString("**Coverage audit issues:**\n\n")
		for _, is := range d.CoverageReport.Issues {
			b.WriteString(fmt.Sprintf("- ⚠️ [%s] %s:%d-%d — %s\n", is.Kind, mdCell(is.Hunk.File), is.Hunk.Start, is.Hunk.End, mdCell(is.Detail)))
		}
	}
	// Both lists are rendered even when the audit passes: a hunk that never ran
	// and a hunk the engine cannot see are the two things a reviewer of this
	// dossier most needs to know, and neither is an audit "issue".
	if len(d.CoverageReport.NotExecuted) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("**Changed code instrumentation saw NOT execute:**\n\n")
		for _, h := range d.CoverageReport.NotExecuted {
			b.WriteString(fmt.Sprintf("- ✗ %s:%d-%d — zero hits on these lines or the statement enclosing them\n", mdCell(h.File), h.Start, h.End))
		}
	}
	if len(d.CoverageReport.Blind) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("**Changed code the coverage engine could not instrument (unmeasured, NOT unexecuted):**\n\n")
		for _, h := range d.CoverageReport.Blind {
			b.WriteString(fmt.Sprintf("- ⊘ %s:%d-%d — no line records emitted here, though the enclosing statement executed\n", mdCell(h.File), h.Start, h.End))
		}
	}
	return b.String()
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
