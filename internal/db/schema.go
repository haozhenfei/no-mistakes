package db

const schemaSQL = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch               TEXT NOT NULL,
    head_sha             TEXT NOT NULL,
    base_sha             TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending',
    kind                 TEXT NOT NULL DEFAULT 'gate',
    parent_run_id        TEXT,
    pr_url               TEXT,
    error                TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
    skip_steps           TEXT,
    only_steps           TEXT,
    allow_gate_config    INTEGER NOT NULL DEFAULT 0,
    qa_verdict           TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS qa_notices (
    id         TEXT PRIMARY KEY,
    pr_url     TEXT NOT NULL,
    qa_run_id  TEXT NOT NULL,
    head_sha   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(pr_url, qa_run_id, head_sha)
);

CREATE TABLE IF NOT EXISTS step_results (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name        TEXT NOT NULL,
    step_order       INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    exit_code        INTEGER,
    duration_ms      INTEGER,
    log_path         TEXT,
    findings_json    TEXT,
    error            TEXT,
    started_at       INTEGER,
    completed_at     INTEGER,
    last_activity_at INTEGER,
    last_activity    TEXT,
    agent_pid        INTEGER,
    auto_fix_limit   INTEGER,
    validated_head_sha TEXT,
    config_hash      TEXT
);

CREATE TABLE IF NOT EXISTS step_rounds (
    id                   TEXT PRIMARY KEY,
    step_result_id       TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round                INTEGER NOT NULL,
    trigger_type         TEXT NOT NULL,
    findings_json        TEXT,
    user_findings_json   TEXT,
    selected_finding_ids TEXT,
    selection_source     TEXT,
    fix_summary          TEXT,
    duration_ms          INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_invocations (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name             TEXT NOT NULL,
    round                 INTEGER NOT NULL,
    purpose               TEXT NOT NULL,
    agent                 TEXT NOT NULL,
    model                 TEXT,
    session_mode          TEXT NOT NULL,
    session_key           TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    exit_status           TEXT NOT NULL,
    failure_category      TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER
);

CREATE INDEX IF NOT EXISTS idx_agent_invocations_run_started_id
    ON agent_invocations (run_id, started_at, id);

CREATE TABLE IF NOT EXISTS run_agent_sessions (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    agent      TEXT NOT NULL,
    session_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, role)
);

CREATE TABLE IF NOT EXISTS intent_cache (
    cache_key   TEXT PRIMARY KEY,
    summary     TEXT NOT NULL,
    agent_name  TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS claims (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step          TEXT NOT NULL,
    text          TEXT NOT NULL,
    kind          TEXT NOT NULL,
    evidence_json TEXT,
    hunks_json    TEXT,
    verdict       TEXT,
    verdict_by    TEXT,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS verify_verdicts (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_id      TEXT NOT NULL,
    verdict       TEXT NOT NULL,
    rationale     TEXT,
    evidence_json TEXT,
    votes_json    TEXT,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS coverage_entries (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    file          TEXT NOT NULL,
    start_line    INTEGER NOT NULL,
    end_line      INTEGER NOT NULL,
    state         TEXT NOT NULL,
    reason        TEXT,
    evidence_json TEXT,
    source        TEXT,
    created_at    INTEGER NOT NULL,
    runtime       TEXT,
    runtime_detail TEXT
);
`

// migrationStatements hold additive schema changes applied to databases that
// were created before the referenced columns existed. Each statement must be
// idempotent via its error being tolerated when the column already exists.
var migrationStatements = []string{
	`ALTER TABLE repos ADD COLUMN fork_url TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selected_finding_ids TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN selection_source TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN fix_summary TEXT`,
	`ALTER TABLE step_rounds ADD COLUMN user_findings_json TEXT`,
	`ALTER TABLE runs ADD COLUMN intent TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_source TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_session_id TEXT`,
	`ALTER TABLE runs ADD COLUMN intent_score REAL`,
	`ALTER TABLE runs ADD COLUMN awaiting_agent_since INTEGER`,
	`ALTER TABLE runs ADD COLUMN parked_ms INTEGER`,
	`ALTER TABLE runs ADD COLUMN skip_steps TEXT`,
	// only_steps records the exclusive selection a run was started with
	// (`--only`). It has to be its own column: skip_steps cannot express it.
	// An on-demand step (qa) is "selected" exactly when it is ABSENT from the
	// skip set - but it is also absent from every row written before the step
	// existed, so inferring selection from the skip set alone would append an
	// unrequested QA pass to those rows on resume or crash recovery. A NULL
	// only_steps says "this run selected nothing", which is the truth for every
	// legacy row and every ordinary run.
	`ALTER TABLE runs ADD COLUMN only_steps TEXT`,
	// allow_gate_config is the run's explicit opt-in to letting its agents write
	// the gate's own config (.no-mistakes.yaml). The DEFAULT 0 backfills every
	// existing row with the default-deny, which is the answer we want for them:
	// no run that predates the change boundary ever asked for the permission.
	`ALTER TABLE runs ADD COLUMN allow_gate_config INTEGER NOT NULL DEFAULT 0`,
	// qa_verdict is the QA run's four-value verdict (PASS / PASS_WITH_ISSUES /
	// FAIL / PARTIAL). It lives on the run row rather than only inside the step's
	// findings blob because the watch run has to read it later, from a different
	// run, to say "QA verified <sha> and said PASS, but the head has moved since".
	// It is NULL on every run that is not a QA run.
	`ALTER TABLE runs ADD COLUMN qa_verdict TEXT`,
	// Every row written before the gate/watch split is a gate run: the
	// pre-split pipeline had no other kind. The NOT NULL DEFAULT backfills
	// them in place, so an existing database keeps loading unchanged.
	`ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'gate'`,
	`ALTER TABLE runs ADD COLUMN parent_run_id TEXT`,
	`ALTER TABLE step_results ADD COLUMN last_activity_at INTEGER`,
	`ALTER TABLE step_results ADD COLUMN last_activity TEXT`,
	`ALTER TABLE step_results ADD COLUMN agent_pid INTEGER`,
	`ALTER TABLE step_results ADD COLUMN auto_fix_limit INTEGER`,
	// The coverage ledger's runtime dimension: what instrumentation actually saw
	// (executed / not-executed / uninstrumented / no-data), as distinct from the
	// evidence class in `state`. NULL on rows written before it existed.
	`ALTER TABLE coverage_entries ADD COLUMN runtime TEXT`,
	`ALTER TABLE coverage_entries ADD COLUMN runtime_detail TEXT`,
	`ALTER TABLE step_results ADD COLUMN validated_head_sha TEXT`,
	`ALTER TABLE step_results ADD COLUMN config_hash TEXT`,
}
