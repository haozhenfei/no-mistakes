package db

import (
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/coverage"
)

// InsertCoverageEntry persists one coverage-ledger row for a run and returns it
// with an assigned ID and timestamp. Each row is a changed hunk plus its
// verification state; gates fill the ledger and the verify step backfills it
// against instrumentation truth (design §5).
func (d *DB) InsertCoverageEntry(e coverage.LedgerEntry) (coverage.LedgerEntry, error) {
	e.ID = newID()
	e.CreatedAt = now()
	evidenceJSON, err := marshalStringSlice(e.Evidence)
	if err != nil {
		return coverage.LedgerEntry{}, fmt.Errorf("marshal coverage evidence: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO coverage_entries (id, run_id, file, start_line, end_line, state, reason, evidence_json, source, created_at, runtime, runtime_detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.RunID, e.File, e.StartLine, e.EndLine, e.State, e.Reason, evidenceJSON, e.Source, e.CreatedAt, e.Runtime, e.RuntimeDetail,
	)
	if err != nil {
		return coverage.LedgerEntry{}, fmt.Errorf("insert coverage entry: %w", err)
	}
	return e, nil
}

// GetCoverageEntriesByRun returns all coverage-ledger rows for a run in
// insertion order.
func (d *DB) GetCoverageEntriesByRun(runID string) ([]coverage.LedgerEntry, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, file, start_line, end_line, state, reason, evidence_json, source, created_at, runtime, runtime_detail
		 FROM coverage_entries WHERE run_id = ? ORDER BY created_at, id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get coverage entries by run: %w", err)
	}
	defer rows.Close()
	var out []coverage.LedgerEntry
	for rows.Next() {
		var (
			e                      coverage.LedgerEntry
			reason, source         sql.NullString
			evidenceJSON           sql.NullString
			runtime, runtimeDetail sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.File, &e.StartLine, &e.EndLine, &e.State, &reason, &evidenceJSON, &source, &e.CreatedAt, &runtime, &runtimeDetail); err != nil {
			return nil, fmt.Errorf("scan coverage entry: %w", err)
		}
		e.Reason = reason.String
		e.Source = source.String
		e.Runtime = runtime.String
		e.RuntimeDetail = runtimeDetail.String
		if e.Evidence, err = unmarshalStringSlice(evidenceJSON); err != nil {
			return nil, fmt.Errorf("unmarshal coverage evidence: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteCoverageEntriesByRun removes the coverage ledger for a run. Explicit
// resume calls this before rerunning steps that can regenerate or audit the
// ledger, avoiding stale per-hunk coverage conclusions under the same run ID.
func (d *DB) DeleteCoverageEntriesByRun(runID string) error {
	_, err := d.sql.Exec(`DELETE FROM coverage_entries WHERE run_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("delete coverage entries by run: %w", err)
	}
	return nil
}

// UpdateCoverageEntry writes back a row's state, reason, evidence, source, and
// machine runtime class by ID. The verify step uses it to persist backfill corrections (a false
// runtime-verified label downgraded to attested, or an uncovered hunk promoted
// to runtime-verified) so the ledger on disk reflects instrumentation truth.
func (d *DB) UpdateCoverageEntry(e coverage.LedgerEntry) error {
	evidenceJSON, err := marshalStringSlice(e.Evidence)
	if err != nil {
		return fmt.Errorf("marshal coverage evidence: %w", err)
	}
	_, err = d.sql.Exec(
		`UPDATE coverage_entries SET state = ?, reason = ?, evidence_json = ?, source = ?, runtime = ?, runtime_detail = ? WHERE id = ?`,
		e.State, e.Reason, evidenceJSON, e.Source, e.Runtime, e.RuntimeDetail, e.ID,
	)
	if err != nil {
		return fmt.Errorf("update coverage entry: %w", err)
	}
	return nil
}
