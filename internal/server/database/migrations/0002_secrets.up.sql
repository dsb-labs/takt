CREATE TABLE IF NOT EXISTS secret (
    id         TEXT NOT NULL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    value      BLOB NOT NULL,
    revision   TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workload_secret (
    workload_id TEXT NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    secret_name TEXT NOT NULL,
    PRIMARY KEY (workload_id, secret_name)
);

CREATE INDEX IF NOT EXISTS idx_workload_secret_workload_id ON workload_secret (workload_id);

CREATE INDEX IF NOT EXISTS idx_workload_secret_secret_name ON workload_secret (secret_name);
