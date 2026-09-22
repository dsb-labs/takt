-- The remaining writes get the same entity tag a workload, volume or service
-- carries: a count of the writes a row has taken, which a conditional write
-- compares against. Rows that predate this start where a fresh row does.
ALTER TABLE secret ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE variable ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

-- The policy's tag was a hash of the document. It becomes the same counter
-- as everything else, so one tag format reaches every conditional write. The
-- hash column goes with it: the document is what it was derived from, and it
-- can be derived again if anything ever needs it.
ALTER TABLE policy ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

ALTER TABLE policy DROP COLUMN etag;
