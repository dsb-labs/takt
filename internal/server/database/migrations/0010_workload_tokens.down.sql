DROP INDEX IF EXISTS idx_token_workload_id;

ALTER TABLE token DROP COLUMN workload_version;

ALTER TABLE token DROP COLUMN workload_instance;

ALTER TABLE token DROP COLUMN workload_id;
