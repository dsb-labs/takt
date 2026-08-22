CREATE TABLE IF NOT EXISTS variable (
    id         TEXT NOT NULL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    value      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workload_variable (
    workload_id   TEXT NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    variable_name TEXT NOT NULL,
    PRIMARY KEY (workload_id, variable_name)
);

CREATE INDEX IF NOT EXISTS idx_workload_variable_workload_id ON workload_variable (workload_id);

CREATE INDEX IF NOT EXISTS idx_workload_variable_variable_name ON workload_variable (variable_name);
