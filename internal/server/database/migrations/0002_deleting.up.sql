ALTER TABLE workload ADD COLUMN deleting_at TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_workload_deleting_at ON workload (deleting_at);
