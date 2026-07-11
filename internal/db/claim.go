package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/claims"
)

// InsertClaim persists a claim for a run and returns it with an assigned ID and
// timestamp. Evidence and hunk lists are stored as JSON arrays. The
// evidence-less demotion rule is a property of the data itself
// (claims.Claim.SelfAttested), so no special-casing is needed on write: a claim
// with empty Evidence is stored faithfully and read back as self-attested.
func (d *DB) InsertClaim(c claims.Claim) (claims.Claim, error) {
	c.ID = newID()
	c.CreatedAt = now()
	evidenceJSON, err := marshalStringSlice(c.Evidence)
	if err != nil {
		return claims.Claim{}, fmt.Errorf("marshal claim evidence: %w", err)
	}
	hunksJSON, err := marshalStringSlice(c.Hunks)
	if err != nil {
		return claims.Claim{}, fmt.Errorf("marshal claim hunks: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO claims (id, run_id, step, text, kind, evidence_json, hunks_json, verdict, verdict_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.RunID, c.Step, c.Text, c.Kind, evidenceJSON, hunksJSON, c.Verdict, c.VerdictBy, c.CreatedAt,
	)
	if err != nil {
		return claims.Claim{}, fmt.Errorf("insert claim: %w", err)
	}
	return c, nil
}

// GetClaimsByRun returns all claims for a run in insertion order.
func (d *DB) GetClaimsByRun(runID string) ([]claims.Claim, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, step, text, kind, evidence_json, hunks_json, verdict, verdict_by, created_at
		 FROM claims WHERE run_id = ? ORDER BY created_at, id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get claims by run: %w", err)
	}
	defer rows.Close()
	var out []claims.Claim
	for rows.Next() {
		var (
			c                       claims.Claim
			evidenceJSON, hunksJSON sql.NullString
			verdict, verdictBy      sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.RunID, &c.Step, &c.Text, &c.Kind, &evidenceJSON, &hunksJSON, &verdict, &verdictBy, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		if c.Evidence, err = unmarshalStringSlice(evidenceJSON); err != nil {
			return nil, fmt.Errorf("unmarshal claim evidence: %w", err)
		}
		if c.Hunks, err = unmarshalStringSlice(hunksJSON); err != nil {
			return nil, fmt.Errorf("unmarshal claim hunks: %w", err)
		}
		c.Verdict = verdict.String
		c.VerdictBy = verdictBy.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteClaimsByRun removes claims and their verify verdicts for a run. Resume
// uses this when rerunning a step that can regenerate evidence-bound claims, so
// stale conclusions from the previous attempt cannot survive under the same
// run ID.
func (d *DB) DeleteClaimsByRun(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin delete claims by run: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM verify_verdicts WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete verify verdicts by run: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM claims WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete claims by run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete claims by run: %w", err)
	}
	return nil
}

// SetClaimVerdict records the verify step's verdict on a claim.
func (d *DB) SetClaimVerdict(claimID, verdict, verdictBy string) error {
	_, err := d.sql.Exec(`UPDATE claims SET verdict = ?, verdict_by = ? WHERE id = ?`, verdict, verdictBy, claimID)
	if err != nil {
		return fmt.Errorf("set claim verdict: %w", err)
	}
	return nil
}

// DeleteVerifyVerdictsByRun removes verify adjudications and clears claim
// verdict fields for a run. It preserves the claims themselves for cases where
// verify is rerun but the test/QA evidence that produced claims is still valid.
func (d *DB) DeleteVerifyVerdictsByRun(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin delete verify verdicts by run: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM verify_verdicts WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete verify verdicts by run: %w", err)
	}
	if _, err := tx.Exec(`UPDATE claims SET verdict = NULL, verdict_by = NULL WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("clear claim verdicts by run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete verify verdicts by run: %w", err)
	}
	return nil
}

// VerifyVerdict is a persisted record of a single verify-step adjudication:
// which claim was judged, the majority verdict, the rationale, the evidence
// involved, and the per-skeptic votes. It is what the dossier links to when a
// reviewer wants to see why something was refuted (design §4.4a).
type VerifyVerdict struct {
	ID        string
	RunID     string
	ClaimID   string
	Verdict   string
	Rationale string
	Evidence  []string
	Votes     []string
	CreatedAt int64
}

// InsertVerifyVerdict persists a verify adjudication record.
func (d *DB) InsertVerifyVerdict(v VerifyVerdict) (VerifyVerdict, error) {
	v.ID = newID()
	v.CreatedAt = now()
	evidenceJSON, err := marshalStringSlice(v.Evidence)
	if err != nil {
		return VerifyVerdict{}, fmt.Errorf("marshal verdict evidence: %w", err)
	}
	votesJSON, err := marshalStringSlice(v.Votes)
	if err != nil {
		return VerifyVerdict{}, fmt.Errorf("marshal verdict votes: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO verify_verdicts (id, run_id, claim_id, verdict, rationale, evidence_json, votes_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.RunID, v.ClaimID, v.Verdict, v.Rationale, evidenceJSON, votesJSON, v.CreatedAt,
	)
	if err != nil {
		return VerifyVerdict{}, fmt.Errorf("insert verify verdict: %w", err)
	}
	return v, nil
}

// GetVerifyVerdictsByRun returns all verify adjudications for a run.
func (d *DB) GetVerifyVerdictsByRun(runID string) ([]VerifyVerdict, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, claim_id, verdict, rationale, evidence_json, votes_json, created_at
		 FROM verify_verdicts WHERE run_id = ? ORDER BY created_at, id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get verify verdicts by run: %w", err)
	}
	defer rows.Close()
	var out []VerifyVerdict
	for rows.Next() {
		var (
			v                       VerifyVerdict
			rationale               sql.NullString
			evidenceJSON, votesJSON sql.NullString
		)
		if err := rows.Scan(&v.ID, &v.RunID, &v.ClaimID, &v.Verdict, &rationale, &evidenceJSON, &votesJSON, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan verify verdict: %w", err)
		}
		v.Rationale = rationale.String
		if v.Evidence, err = unmarshalStringSlice(evidenceJSON); err != nil {
			return nil, fmt.Errorf("unmarshal verdict evidence: %w", err)
		}
		if v.Votes, err = unmarshalStringSlice(votesJSON); err != nil {
			return nil, fmt.Errorf("unmarshal verdict votes: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func marshalStringSlice(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalStringSlice(v sql.NullString) ([]string, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}
