package db

import "fmt"

// The qa_notices table is the ledger of QA-staleness notices already published
// to a pull request. It exists for one reason: a watch run is a poller that can
// be killed and re-armed from nothing at any moment (see rearmWatchRuns), and a
// re-armed run re-derives its entire verdict from scratch - including "the QA
// verdict on this PR is stale". Without a durable record, every daemon restart
// would post the same staleness comment again on the same PR.
//
// A notice is identified by the three facts that make it unique: the PR it was
// posted to, the QA run whose verdict went stale, and the head SHA it went stale
// against. A later head produces a different notice, which is correct - the
// reviewer needs to know the drift grew.

// QANoticePosted reports whether the staleness notice for this (PR, QA run,
// head) triple has already been published.
func (d *DB) QANoticePosted(prURL, qaRunID, headSHA string) (bool, error) {
	var count int
	err := d.sql.QueryRow(
		`SELECT COUNT(1) FROM qa_notices WHERE pr_url = ? AND qa_run_id = ? AND head_sha = ?`,
		prURL, qaRunID, headSHA,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("qa notice posted: %w", err)
	}
	return count > 0, nil
}

// RecordQANotice records that the staleness notice for this (PR, QA run, head)
// triple has been published. It is idempotent: recording the same triple twice
// is not an error, so a race between two writers cannot fail a run.
func (d *DB) RecordQANotice(prURL, qaRunID, headSHA string) error {
	_, err := d.sql.Exec(
		`INSERT OR IGNORE INTO qa_notices (id, pr_url, qa_run_id, head_sha, created_at) VALUES (?, ?, ?, ?, ?)`,
		newID(), prURL, qaRunID, headSHA, now(),
	)
	if err != nil {
		return fmt.Errorf("record qa notice: %w", err)
	}
	return nil
}
