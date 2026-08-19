-- Initial schema. Logs never live in SQLite: they are files on disk (SPEC §10).

CREATE TABLE processes (
    id             TEXT PRIMARY KEY,
    worker         TEXT NOT NULL,
    type           TEXT NOT NULL,
    state          TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    attempt        INTEGER NOT NULL DEFAULT 0,
    max_attempts   INTEGER NOT NULL DEFAULT 1,
    lock_key       TEXT NOT NULL DEFAULT '',

    command        TEXT NOT NULL,
    args           TEXT NOT NULL,          -- JSON array
    env            TEXT NOT NULL,          -- JSON object
    cwd            TEXT NOT NULL,
    run_user       TEXT NOT NULL DEFAULT '',
    run_group      TEXT NOT NULL DEFAULT '',
    timeout_ns     INTEGER NOT NULL DEFAULT 0,
    metadata       TEXT NOT NULL DEFAULT '{}',

    pid            INTEGER NOT NULL DEFAULT 0,
    pid_start_time INTEGER NOT NULL DEFAULT 0,
    exit_code      INTEGER,
    signal         TEXT NOT NULL DEFAULT '',
    log_truncated  INTEGER NOT NULL DEFAULT 0,

    created_at     TEXT NOT NULL,
    queued_at      TEXT,
    started_at     TEXT,
    finished_at    TEXT,
    retry_at       TEXT
);

CREATE INDEX idx_processes_state ON processes (state);
CREATE INDEX idx_processes_worker_created ON processes (worker, created_at);
CREATE INDEX idx_processes_created ON processes (created_at);

-- One row per attempt, so that logs and exit codes stay addressable per try.
CREATE TABLE attempts (
    process_id     TEXT NOT NULL REFERENCES processes (id) ON DELETE CASCADE,
    attempt        INTEGER NOT NULL,
    pid            INTEGER NOT NULL DEFAULT 0,
    pid_start_time INTEGER NOT NULL DEFAULT 0,
    exit_code      INTEGER,
    signal         TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    log_truncated  INTEGER NOT NULL DEFAULT 0,
    started_at     TEXT,
    finished_at    TEXT,
    PRIMARY KEY (process_id, attempt)
);

-- A held lock is a row. The unique key makes concurrent acquisition impossible
-- and survives a restart, unlike an in-memory map.
CREATE TABLE locks (
    key        TEXT PRIMARY KEY,
    process_id TEXT NOT NULL REFERENCES processes (id) ON DELETE CASCADE,
    acquired_at TEXT NOT NULL
);

CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    process_id   TEXT NOT NULL,
    created_at   TEXT NOT NULL
);

CREATE INDEX idx_idempotency_created ON idempotency_keys (created_at);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT NOT NULL,
    token_name TEXT NOT NULL,
    action     TEXT NOT NULL,
    process_id TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_audit_at ON audit_log (at);
