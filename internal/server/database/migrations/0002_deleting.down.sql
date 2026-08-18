DROP INDEX IF EXISTS idx_workload_deleting_at;

ALTER TABLE workload DROP COLUMN deleting_at;
