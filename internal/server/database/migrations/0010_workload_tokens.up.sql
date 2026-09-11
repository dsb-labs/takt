-- A token minted for a workload is bound to it. The reconciler revokes the
-- token when what it was minted for goes away, and the cascade removes any
-- the reconciler did not reach if the workload row is deleted first. NULL for
-- every token that was not minted for a workload.
ALTER TABLE token ADD COLUMN workload_id TEXT REFERENCES workload (id) ON DELETE CASCADE;

-- The instance the token was minted for. NULL for a token every instance of
-- the workload shares, which is what a token mounted as a file is.
ALTER TABLE token ADD COLUMN workload_instance INTEGER;

-- The workload version the token was minted for. Set for a shared token,
-- because the mounted file is kept per version: during a rolling replacement
-- the old version's instances keep reading the old file, so its token stays
-- valid until the reconciler reclaims what the replacement superseded. NULL
-- for an instance-bound token, whose life is the instance's own.
ALTER TABLE token ADD COLUMN workload_version INTEGER;

CREATE INDEX IF NOT EXISTS idx_token_workload_id ON token (workload_id);
