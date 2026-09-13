CREATE TABLE IF NOT EXISTS workload_event (
    id          TEXT NOT NULL PRIMARY KEY,
    workload_id TEXT NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    reason      TEXT NOT NULL,
    data        BLOB NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workload_event_coalesce ON workload_event (workload_id, reason, data);

CREATE INDEX IF NOT EXISTS idx_workload_event_recent ON workload_event (workload_id, last_seen DESC);
