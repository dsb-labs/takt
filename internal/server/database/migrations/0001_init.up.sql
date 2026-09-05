CREATE TABLE IF NOT EXISTS workload (
    id         TEXT    NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    version    INTEGER NOT NULL,
    runtime    TEXT    NOT NULL,
    spec       BLOB    NOT NULL,
    spec_hash  TEXT    NOT NULL,
    labels     BLOB    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    deleted_at TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_workload_runtime ON workload (runtime);

CREATE INDEX IF NOT EXISTS idx_workload_deleted_at ON workload (deleted_at);

CREATE TABLE IF NOT EXISTS workload_port (
    workload_id    TEXT    NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    container_port INTEGER NOT NULL,
    host_port      INTEGER NOT NULL,
    is_dynamic     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workload_id, container_port),
    UNIQUE (host_port)
);

CREATE INDEX IF NOT EXISTS idx_workload_port_workload_id ON workload_port (workload_id);

CREATE TABLE IF NOT EXISTS volume (
    id         TEXT NOT NULL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    labels     BLOB NOT NULL DEFAULT '{}',
    owner      TEXT NOT NULL DEFAULT '',
    mode       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
