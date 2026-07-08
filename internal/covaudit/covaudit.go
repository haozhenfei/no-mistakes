// Package covaudit orchestrates the coverage-ledger backfill and audit
// (design §4.4c). It sits above the pure coverage logic (internal/coverage),
// the ledger store (internal/db), and the evidence vault (internal/evidence),
// gluing them together so both the verify step and the `no-mistakes coverage`
// CLI run the SAME machine judgment — there is one truth, not two.
//
// The flow, in order:
//
//  1. Parse the run's base..head diff into changed hunks (the audit domain).
//  2. Load captured, signature-verified coverage evidence — the instrumentation
//     ground truth. Only verified-captured entries count; a tampered or attested
//     entry contributes no coverage.
//  3. Backfill the ledger against that truth: promote executed hunks to
//     runtime-verified, downgrade unsupported runtime-verified labels. Persist
//     the corrections.
//  4. Insert placeholder "unverified" rows for changed hunks no gate recorded,
//     so the ledger is a complete record of the diff (§4.4c a) and every changed
//     hunk surfaces in the dossier.
//  5. Run the §4.4c audit and return the report plus the corrected ledger.
package covaudit

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// Result is the outcome of a coverage backfill + audit pass.
type Result struct {
	Report     coverage.AuditReport
	Ledger     []coverage.LedgerEntry // post-backfill, complete over the diff
	Downgrades []coverage.Downgrade
	// ChangedHunks is the diff-derived audit domain.
	ChangedHunks []coverage.Hunk
	// CoverageEvidenceIDs are the captured coverage evidence entries that
	// supplied instrumentation truth.
	CoverageEvidenceIDs []string
}

// Run executes the full backfill + audit for a run and persists the corrected
// ledger. workDir is the run's worktree, keyPath the evidence signing key, and
// base/head the diff endpoints.
func Run(ctx context.Context, database *db.DB, runID, workDir, keyPath, base, head string) (Result, error) {
	diff, err := git.Diff(ctx, workDir, base, head)
	if err != nil {
		return Result{}, fmt.Errorf("coverage audit: diff %s..%s: %w", base, head, err)
	}
	changed := coverage.ParseDiffHunks(diff)

	key, err := evidence.LoadOrCreateKey(keyPath)
	if err != nil {
		return Result{}, fmt.Errorf("coverage audit: load evidence key: %w", err)
	}
	loaded, err := evidence.LoadAll(workDir, key)
	if err != nil {
		return Result{}, fmt.Errorf("coverage audit: load evidence: %w", err)
	}

	datasets, coverageIDs, staticOK := classifyEvidence(loaded)

	ledger, err := database.GetCoverageEntriesByRun(runID)
	if err != nil {
		return Result{}, fmt.Errorf("coverage audit: load ledger: %w", err)
	}

	// Backfill against instrumentation truth and persist the corrections.
	backfilled, downgrades := coverage.Backfill(ledger, datasets, coverageIDs)
	for i := range backfilled {
		if !ledgerEntryEqual(ledger[i], backfilled[i]) {
			if err := database.UpdateCoverageEntry(backfilled[i]); err != nil {
				return Result{}, fmt.Errorf("coverage audit: persist backfill: %w", err)
			}
		}
	}

	// Complete the ledger: insert placeholder unverified rows for changed hunks
	// no gate recorded, so the ledger faithfully covers the whole diff.
	backfilled, err = fillMissingHunks(database, runID, changed, backfilled)
	if err != nil {
		return Result{}, err
	}

	report := coverage.Audit(changed, backfilled, datasets, staticOK)
	return Result{
		Report:              report,
		Ledger:              backfilled,
		Downgrades:          downgrades,
		ChangedHunks:        changed,
		CoverageEvidenceIDs: coverageIDs,
	}, nil
}

// classifyEvidence splits loaded evidence into: coverage datasets (with parallel
// IDs) and a static-evidence checker. Only verified-captured entries count.
//
// The static checker enforces §4.4c(c): a static-verified hunk must hang off
// captured executable static evidence (a typecheck / AST tool run through
// `evidence exec` or `evidence coverage`), never an attested natural-language
// note. So an evidence ID passes only when its entry is captured command-output
// or coverage.
func classifyEvidence(loaded []evidence.LoadedEntry) ([]coverage.CoverageData, []string, coverage.StaticEvidenceChecker) {
	var datasets []coverage.CoverageData
	var coverageIDs []string
	captured := map[string]evidence.LoadedEntry{}
	for _, e := range loaded {
		if e.EffectiveProvenance() != evidence.ProvenanceCaptured {
			continue
		}
		captured[e.ID] = e
		if e.Kind == evidence.KindCoverage && e.Coverage != nil {
			datasets = append(datasets, *e.Coverage)
			coverageIDs = append(coverageIDs, e.ID)
		}
	}
	staticOK := func(id string) bool {
		e, ok := captured[id]
		if !ok {
			return false
		}
		return e.Kind == evidence.KindCommandOutput || e.Kind == evidence.KindCoverage
	}
	return datasets, coverageIDs, staticOK
}

// fillMissingHunks inserts an unverified ledger row for each changed hunk with
// no existing entry, then returns the ledger reloaded so callers see the
// placeholders.
func fillMissingHunks(database *db.DB, runID string, changed []coverage.Hunk, ledger []coverage.LedgerEntry) ([]coverage.LedgerEntry, error) {
	have := map[coverage.Hunk]bool{}
	for _, e := range ledger {
		have[e.Hunk()] = true
	}
	inserted := false
	for _, h := range changed {
		if have[h] {
			continue
		}
		_, err := database.InsertCoverageEntry(coverage.LedgerEntry{
			RunID:     runID,
			File:      h.File,
			StartLine: h.Start,
			EndLine:   h.End,
			State:     coverage.StateUnverified,
			Reason:    "no gate recorded verification for this hunk",
			Source:    "coverage-audit",
		})
		if err != nil {
			return nil, fmt.Errorf("coverage audit: insert placeholder entry: %w", err)
		}
		inserted = true
	}
	if !inserted {
		return ledger, nil
	}
	return database.GetCoverageEntriesByRun(runID)
}

func ledgerEntryEqual(a, b coverage.LedgerEntry) bool {
	if a.State != b.State || a.Reason != b.Reason || a.Source != b.Source {
		return false
	}
	if len(a.Evidence) != len(b.Evidence) {
		return false
	}
	for i := range a.Evidence {
		if a.Evidence[i] != b.Evidence[i] {
			return false
		}
	}
	return true
}
