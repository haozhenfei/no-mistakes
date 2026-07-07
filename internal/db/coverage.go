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
		`INSERT INTO coverage_entries (id, run_id, file, start_line, end_line, state, reason, evidence_json, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.RunID, e.File, e.StartLine, e.EndLine, e.State, e.Reason, evidenceJSON, e.Source, e.CreatedAt,
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
		`SELECT id, run_id, file, start_line, end_line, state, reason, evidence_json, source, created_at
		 FROM coverage_entries WHERE run_id = ? ORDER BY created_at, id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get coverage entries by run: %w", err)
	}
	defer rows.Close()
	var out []coverage.LedgerEntry
	for rows.Next() {
		var (
			e              coverage.LedgerEntry
			reason, source sql.NullString
			evidenceJSON   sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.RunID, &e.File, &e.StartLine, &e.EndLine, &e.State, &reason, &evidenceJSON, &source, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan coverage entry: %w", err)
		}
		e.Reason = reason.String
		e.Source = source.String
		if e.Evidence, err = unmarshalStringSlice(evidenceJSON); err != nil {
			return nil, fmt.Errorf("unmarshal coverage evidence: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateCoverageEntry writes back a row's state, reason, evidence, and source by
// ID. The verify step uses it to persist backfill corrections (a false
// runtime-verified label downgraded to attested, or an uncovered hunk promoted
// to runtime-verified) so the ledger on disk reflects instrumentation truth.
func (d *DB) UpdateCoverageEntry(e coverage.LedgerEntry) error {
	evidenceJSON, err := marshalStringSlice(e.Evidence)
	if err != nil {
		return fmt.Errorf("marshal coverage evidence: %w", err)
	}
	_, err = d.sql.Exec(
		`UPDATE coverage_entries SET state = ?, reason = ?, evidence_json = ?, source = ? WHERE id = ?`,
		e.State, e.Reason, evidenceJSON, e.Source, e.ID,
	)
	if err != nil {
		return fmt.Errorf("update coverage entry: %w", err)
	}
	return nil
}
