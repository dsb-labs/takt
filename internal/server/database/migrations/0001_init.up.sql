CREATE TABLE IF NOT EXISTS workload (
    name       TEXT    NOT NULL PRIMARY KEY,
    version    INTEGER NOT NULL,
    runtime    TEXT    NOT NULL,
    schedule   TEXT    NOT NULL DEFAULT '',
    spec       TEXT    NOT NULL,
    spec_hash  TEXT    NOT NULL,
    labels     TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_workload_runtime ON workload (runtime);
