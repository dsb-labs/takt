CREATE TABLE IF NOT EXISTS workload_reference (
    workload_id   TEXT NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    workload_name TEXT NOT NULL,
    PRIMARY KEY (workload_id, workload_name)
);

CREATE INDEX IF NOT EXISTS idx_workload_reference_workload_id ON workload_reference (workload_id);

CREATE INDEX IF NOT EXISTS idx_workload_reference_workload_name ON workload_reference (workload_name);
