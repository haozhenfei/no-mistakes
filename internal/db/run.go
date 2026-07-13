package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Run represents a pipeline run.
type Run struct {
	ID      string
	RepoID  string
	Branch  string
	HeadSHA string
	BaseSHA string
	Status  types.RunStatus
	// Kind is the run's side of the PR boundary: a gate run (intent..pr, owns a
	// worktree) or a watch run (post-PR poller, owns no local state). Rows
	// written before the split read back as gate.
	Kind types.RunKind
	// ParentRunID links a derived run back to the run that spawned it: a watch
	// run points at the gate run whose pr step opened the PR, and a fix gate
	// run points at the watch run that found the problem. It is how a fix run
	// finds its seed findings and how the daemon bounds fix-round depth.
	ParentRunID *string
	PRURL       *string
	Error       *string
	// AwaitingAgentSince is the unix-seconds timestamp at which the run parked
	// at a gate awaiting the driving agent's response (an awaiting_approval or
	// fix_review step). It is nil whenever the run is not parked: the executor
	// sets it on gate entry and clears it the moment the agent responds (or the
	// wait is cancelled). It is observability only and does not affect gate
	// resolution.
	AwaitingAgentSince *int64
	// ParkedMS accumulates the run's total parked-at-gate wall time in
	// milliseconds across every gate wait (local performance telemetry;
	// step duration_ms values exclude this time).
	ParkedMS int64
	// SkipSteps are the steps the caller asked this run to skip (`--skip`, or
	// the `no-mistakes.skip` push option). It is a property of the run, so a
	// later resume of the same run skips the same steps instead of reviving
	// them. Persisted as a JSON array of step names; nil when nothing is
	// skipped.
	SkipSteps []types.StepName
	// OnlySteps is the exclusive selection the run was started with (`--only`):
	// the run executes these and skips everything else. It is persisted
	// SEPARATELY from SkipSteps because a skip list cannot express a positive
	// selection - an on-demand step (qa) is absent from the skip set both when
	// it was selected and when the row predates the step existing. nil means the
	// run selected nothing, which is what every ordinary run and every row
	// written before --only shipped says.
	OnlySteps []types.StepName
	// QAVerdict is the verdict a QA run reached (PASS / PASS_WITH_ISSUES / FAIL /
	// PARTIAL), and it is nil on every other kind of run. Together with HeadSHA it
	// is the durable answer to "what did QA conclude, and about which commit" -
	// the pair a later watch run needs to say a QA verdict has gone stale.
	QAVerdict       *string
	Intent          *string
	IntentSource    *string
	IntentSessionID *string
	IntentScore     *float64
	CreatedAt       int64
	UpdatedAt       int64
}

const runColumns = `id, repo_id, branch, head_sha, base_sha, status, COALESCE(kind, 'gate'), parent_run_id, pr_url, error, awaiting_agent_since, COALESCE(parked_ms, 0), skip_steps, only_steps, qa_verdict, intent, intent_source, intent_session_id, intent_score, created_at, updated_at`

func scanRun(row interface {
	Scan(...any) error
}, r *Run) error {
	var skipSteps, onlySteps *string
	if err := row.Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.Status,
		&r.Kind, &r.ParentRunID,
		&r.PRURL, &r.Error, &r.AwaitingAgentSince, &r.ParkedMS, &skipSteps, &onlySteps, &r.QAVerdict,
		&r.Intent, &r.IntentSource, &r.IntentSessionID, &r.IntentScore,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return err
	}
	steps, err := decodeStepNames(skipSteps)
	if err != nil {
		return fmt.Errorf("skip steps: %w", err)
	}
	r.SkipSteps = steps
	only, err := decodeStepNames(onlySteps)
	if err != nil {
		return fmt.Errorf("only steps: %w", err)
	}
	r.OnlySteps = only
	return nil
}

// encodeStepNames renders a step-name set for storage. An empty set is stored as
// SQL NULL so old rows and "nothing here" are the same thing.
func encodeStepNames(steps []types.StepName) (any, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("encode step names: %w", err)
	}
	return string(data), nil
}

func decodeStepNames(raw *string) ([]types.StepName, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var steps []types.StepName
	if err := json.Unmarshal([]byte(*raw), &steps); err != nil {
		return nil, fmt.Errorf("decode skip steps: %w", err)
	}
	if len(steps) == 0 {
		return nil, nil
	}
	return steps, nil
}

// RunOptions carries the properties fixed at a run's birth: what kind of run it
// is, which run derived it, which PR it is about, and which steps it was told to
// skip or selected. All of them are written WITH the row rather than by a later
// UPDATE, so a crash - or a reader - between insert and update can never see a
// run whose identity is only half there. A watch run inserted without its PR URL
// and updated a moment later is a watch run that briefly looks like it is
// watching nothing.
type RunOptions struct {
	Kind        types.RunKind
	ParentRunID string
	// PRURL is the pull request a derived run is about. Empty for a gate run,
	// which opens its PR later (the pr step records it).
	PRURL     string
	SkipSteps []types.StepName
	// OnlySteps is the run's selection; see Run.OnlySteps for why it is stored
	// alongside SkipSteps rather than derived from it.
	OnlySteps []types.StepName
}

// InsertRun creates a new gate run record with no skipped steps.
func (d *DB) InsertRun(repoID, branch, headSHA, baseSHA string) (*Run, error) {
	return d.InsertRunWithOptions(repoID, branch, headSHA, baseSHA, RunOptions{})
}

// InsertRunWithSkipSteps creates a new gate run record and records the steps the
// caller asked to skip. The skip set is a property of the run, so a later resume
// skips the same steps instead of reviving them.
func (d *DB) InsertRunWithSkipSteps(repoID, branch, headSHA, baseSHA string, skipSteps []types.StepName) (*Run, error) {
	return d.InsertRunWithOptions(repoID, branch, headSHA, baseSHA, RunOptions{SkipSteps: skipSteps})
}

// InsertRunWithOptions creates a new run record of the requested kind.
func (d *DB) InsertRunWithOptions(repoID, branch, headSHA, baseSHA string, opts RunOptions) (*Run, error) {
	kind := opts.Kind
	if kind == "" {
		kind = types.RunKindGate
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("insert run: unknown run kind %q", kind)
	}
	ts := now()
	r := &Run{
		ID:        newID(),
		RepoID:    repoID,
		Branch:    branch,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		Status:    types.RunPending,
		Kind:      kind,
		SkipSteps: opts.SkipSteps,
		OnlySteps: opts.OnlySteps,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	if len(opts.SkipSteps) == 0 {
		r.SkipSteps = nil
	}
	if len(opts.OnlySteps) == 0 {
		r.OnlySteps = nil
	}
	if opts.ParentRunID != "" {
		parent := opts.ParentRunID
		r.ParentRunID = &parent
	}
	if opts.PRURL != "" {
		prURL := opts.PRURL
		r.PRURL = &prURL
	}
	encodedSkip, err := encodeStepNames(r.SkipSteps)
	if err != nil {
		return nil, err
	}
	encodedOnly, err := encodeStepNames(r.OnlySteps)
	if err != nil {
		return nil, err
	}
	_, err = d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, kind, parent_run_id, pr_url, skip_steps, only_steps, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA, r.Status, r.Kind, r.ParentRunID, r.PRURL, encodedSkip, encodedOnly, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	return r, nil
}

// GetRun returns a run by ID.
func (d *DB) GetRun(id string) (*Run, error) {
	r := &Run{}
	err := scanRun(d.sql.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, id), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// GetRunsByRepo returns all runs for a repo, newest first.
func (d *DB) GetRunsByRepo(repoID string) ([]*Run, error) {
	rows, err := d.sql.Query(`SELECT `+runColumns+` FROM runs WHERE repo_id = ? ORDER BY created_at DESC, id DESC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetRunsByRepoHead returns the runs for a repo matching an exact branch and
// head SHA, newest first. It lets a caller detect the run created by a specific
// push without scanning (and rebuilding step data for) the repo's entire run
// history, so the cost stays bounded to the handful of runs for one head.
func (d *DB) GetRunsByRepoHead(repoID, branch, headSHA string) ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND head_sha = ? ORDER BY created_at DESC, id DESC`,
		repoID, branch, headSHA,
	)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo head: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetRunsByRepoBranch returns the runs for a repo branch, newest first.
func (d *DB) GetRunsByRepoBranch(repoID, branch string) ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? ORDER BY created_at DESC, id DESC`,
		repoID, branch,
	)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo branch: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetActiveRun returns the currently active run (pending or running) for a repo,
// if any. When branch is non-empty, only a run on that exact branch is returned
// - the setup wizard relies on this to decide whether a new run is needed for
// the current branch. When branch is empty, returns the most recently created
// active run across any branch.
func (d *DB) GetActiveRun(repoID, branch string) (*Run, error) {
	r := &Run{}
	var err error
	if branch == "" {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID,
		), r)
	} else {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch,
		), r)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	return r, nil
}

// GetActiveRuns returns all pending or running runs across all repos, newest first.
func (d *DB) GetActiveRuns() ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?) ORDER BY created_at DESC, id DESC`,
		types.RunPending, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("get active runs: %w", err)
	}
	defer rows.Close()

	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// UpdateRunStatus updates a run's status and updated_at timestamp.
func (d *DB) UpdateRunStatus(id string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	return nil
}

// UpdateRunPRURL sets the PR URL on a run.
func (d *DB) UpdateRunPRURL(id, prURL string) error {
	_, err := d.sql.Exec(`UPDATE runs SET pr_url = ?, updated_at = ? WHERE id = ?`, prURL, now(), id)
	if err != nil {
		return fmt.Errorf("update run pr url: %w", err)
	}
	return nil
}

// UpdateRunQAVerdict records the verdict a QA run reached. The run's head SHA
// is already on the row, so the pair "this verdict is about this commit" becomes
// durable the moment the QA step finishes - which is what lets a later watch run
// notice that the PR has moved past the commit QA actually exercised.
func (d *DB) UpdateRunQAVerdict(id, verdict string) error {
	_, err := d.sql.Exec(`UPDATE runs SET qa_verdict = ?, updated_at = ? WHERE id = ?`, verdict, now(), id)
	if err != nil {
		return fmt.Errorf("update run qa verdict: %w", err)
	}
	return nil
}

// LatestQAVerdict returns the newest run on a branch whose QA node reached a
// verdict, or nil when QA has never completed for it. The run's head SHA is the
// commit QA actually exercised.
//
// A recorded verdict is the bar, not a completed run: the QA node finishes while
// the watch run it belongs to is still polling CI, so requiring the RUN to be
// terminal would hide the verdict for as long as the PR is open - which is
// exactly when it is needed. Conversely, a run whose QA node never reported has
// no qa_verdict at all, so a crashed or cancelled QA pass can never be mistaken
// for one that verified something.
func (d *DB) LatestQAVerdict(repoID, branch string) (*Run, error) {
	r := &Run{}
	err := scanRun(d.sql.QueryRow(
		`SELECT `+runColumns+` FROM runs
		 WHERE repo_id = ? AND branch = ? AND qa_verdict IS NOT NULL
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		repoID, branch,
	), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest qa verdict: %w", err)
	}
	return r, nil
}

// UpdateRunHeadSHA updates the run head SHA and timestamp.
func (d *DB) UpdateRunHeadSHA(id, headSHA string) error {
	_, err := d.sql.Exec(`UPDATE runs SET head_sha = ?, updated_at = ? WHERE id = ?`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run head sha: %w", err)
	}
	return nil
}

// StartRunResume marks a previously terminal run active again for an explicit
// resume attempt, clearing stale terminal error/parked state and setting the
// head the resumed worktree will validate.
func (d *DB) StartRunResume(id, headSHA string) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET status = ?, head_sha = ?, error = NULL, awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`,
		types.RunRunning, headSHA, now(), id,
	)
	if err != nil {
		return fmt.Errorf("start run resume: %w", err)
	}
	return nil
}

// UpdateRunError sets the error message on a run.
func (d *DB) UpdateRunError(id, errMsg string) error {
	return d.UpdateRunErrorStatus(id, errMsg, types.RunFailed)
}

// UpdateRunErrorStatus sets the error message and terminal status on a run.
func (d *DB) UpdateRunErrorStatus(id, errMsg string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, updated_at = ? WHERE id = ?`, errMsg, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run error: %w", err)
	}
	return nil
}

// RunIntent carries the four intent-related columns persisted on a run.
type RunIntent struct {
	Summary   string
	Source    string
	SessionID string
	Score     float64
}

// UpdateRunIntent persists the inferred user intent for a run.
func (d *DB) UpdateRunIntent(id string, intent RunIntent) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET intent = ?, intent_source = ?, intent_session_id = ?, intent_score = ?, updated_at = ? WHERE id = ?`,
		intent.Summary, intent.Source, intent.SessionID, intent.Score, now(), id,
	)
	if err != nil {
		return fmt.Errorf("update run intent: %w", err)
	}
	return nil
}

// SetRunAwaitingAgent marks a run as parked awaiting the driving agent,
// stamping awaiting_agent_since with the current time. Called by the executor
// when a step enters a gate (awaiting_approval / fix_review). This is a pollable
// observability signal only; it does not change gate resolution.
func (d *DB) SetRunAwaitingAgent(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run awaiting agent: %w", err)
	}
	return nil
}

// ClearRunAwaitingAgent clears the awaiting-agent marker on a run. Called by the
// executor the moment the agent responds (or the approval wait is cancelled) and
// the run resumes, so awaiting_agent_since is non-nil exactly while a gate is
// actually parked.
func (d *DB) ClearRunAwaitingAgent(id string) error {
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("clear run awaiting agent: %w", err)
	}
	return nil
}

// AddRunParkedDuration accumulates parked-at-gate wall time onto a run's
// total. Called by the executor when a gate wait ends.
func (d *DB) AddRunParkedDuration(id string, ms int64) error {
	if ms <= 0 {
		return nil
	}
	_, err := d.sql.Exec(`UPDATE runs SET parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ? WHERE id = ?`, ms, now(), id)
	if err != nil {
		return fmt.Errorf("add run parked duration: %w", err)
	}
	return nil
}

func (d *DB) CompleteRunAwaitingAgent(id string, ms int64) error {
	if ms < 0 {
		ms = 0
	}
	_, err := d.sql.Exec(
		`UPDATE runs SET awaiting_agent_since = NULL, parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ? WHERE id = ?`,
		ms, now(), id,
	)
	if err != nil {
		return fmt.Errorf("complete run awaiting agent: %w", err)
	}
	return nil
}

// RecoverStaleRuns marks any runs stuck in pending/running status as interrupted
// and fails any in-progress steps. This is called at daemon startup to clean
// up after a previous crash. Returns the number of recovered runs.
func (d *DB) RecoverStaleRuns(errMsg string) (int, error) {
	return d.RecoverStaleRunsExcept(errMsg, nil)
}

// RecoverStaleRunsExcept marks active runs as interrupted unless their IDs appear
// in preserved. Callers use preserved only after independently proving a run
// can be reconstructed safely.
func (d *DB) RecoverStaleRunsExcept(errMsg string, preserved map[string]struct{}) (int, error) {
	ts := now()

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	placeholders, args := recoveryExclusionClause(preserved)
	stepArgs := []any{
		types.StepStatusFailed, errMsg, ts,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		types.RunPending, types.RunRunning,
	}
	stepArgs = append(stepArgs, args...)
	_, err = tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?
		 WHERE status IN (?, ?, ?, ?) AND run_id IN (
			SELECT id FROM runs WHERE status IN (?, ?)`+placeholders+`
		 )`,
		stepArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale steps: %w", err)
	}

	// Interrupt stale runs. Clear any awaiting-agent marker so a recovered run
	// is never reported as still parked awaiting the agent,
	// accumulating the marker's elapsed time into the run's parked total so
	// the parked evidence survives the crash.
	runArgs := []any{types.RunInterrupted, errMsg, ts, ts, ts, types.RunPending, types.RunRunning}
	runArgs = append(runArgs, args...)
	result, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
			awaiting_agent_since = NULL, updated_at = ? WHERE status IN (?, ?)`+placeholders,
		runArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale runs: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return int(count), nil
}

func recoveryExclusionClause(preserved map[string]struct{}) (string, []any) {
	if len(preserved) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(preserved))
	placeholders := make([]string, 0, len(preserved))
	for id := range preserved {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return " AND id NOT IN (" + strings.Join(placeholders, ", ") + ")", args
}
