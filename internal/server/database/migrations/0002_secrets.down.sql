DROP INDEX IF EXISTS idx_workload_secret_secret_name;

DROP INDEX IF EXISTS idx_workload_secret_workload_id;

DROP TABLE IF EXISTS workload_secret;

DROP INDEX IF EXISTS idx_secret_key_id;

DROP TABLE IF EXISTS secret;

DROP INDEX IF EXISTS idx_encryption_key_is_current;

DROP TABLE IF EXISTS encryption_key;
