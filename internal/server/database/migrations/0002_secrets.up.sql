-- The keys a secret's value is sealed under, one row per key the server has ever
-- generated. A key that is no longer current is kept rather than removed: it still
-- opens the backups taken before it was replaced.
CREATE TABLE IF NOT EXISTS encryption_key (
    id         TEXT    NOT NULL PRIMARY KEY,
    is_current INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
);

-- Exactly one key is current, enforced here rather than in the code that rotates
-- them. Two current keys would mean new secrets sealed under one and the rest under
-- another, with nothing recording which.
CREATE UNIQUE INDEX IF NOT EXISTS idx_encryption_key_is_current
    ON encryption_key (is_current) WHERE is_current = 1;

CREATE TABLE IF NOT EXISTS secret (
    id         TEXT NOT NULL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    value      BLOB NOT NULL,
    revision   TEXT NOT NULL,
    -- Which key sealed the value. This is what lets a rekey move every row and the
    -- pointer to the current key in one transaction, so the file on disk never has
    -- to be swapped in step with a commit.
    --
    -- RESTRICT rather than the CASCADE used elsewhere. Removing a key that still
    -- seals a secret would delete the secret, which is the loss this column exists
    -- to prevent.
    key_id     TEXT NOT NULL REFERENCES encryption_key (id) ON DELETE RESTRICT,
    labels     BLOB NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_secret_key_id ON secret (key_id);

CREATE TABLE IF NOT EXISTS workload_secret (
    workload_id TEXT NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    secret_name TEXT NOT NULL,
    PRIMARY KEY (workload_id, secret_name)
);

CREATE INDEX IF NOT EXISTS idx_workload_secret_workload_id ON workload_secret (workload_id);

CREATE INDEX IF NOT EXISTS idx_workload_secret_secret_name ON workload_secret (secret_name);
