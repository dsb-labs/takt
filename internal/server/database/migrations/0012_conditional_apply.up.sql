-- A volume has always been updatable, since an apply replaces its labels, owner
-- and mode, but it carried no record of when that last happened. Existing rows
-- are backfilled from their creation time, which is the last write they had.
ALTER TABLE volume ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';

UPDATE volume SET updated_at = created_at WHERE updated_at = '';

-- The version is the entity tag a conditional apply compares against, counting
-- the writes a row has taken. A workload already carries one. Rows that predate
-- this start at the same place a freshly inserted one does, so a caller cannot
-- tell a backfilled row from a new one, and neither needs to.
ALTER TABLE volume ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE service ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
