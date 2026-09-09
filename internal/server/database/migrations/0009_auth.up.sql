-- The credentials that authenticate API callers. The token itself is never
-- stored: the hash is what a presented credential is looked up by, so a copy
-- of the database holds nothing a caller could present.
CREATE TABLE IF NOT EXISTS token (
    id              TEXT NOT NULL PRIMARY KEY,
    hash            TEXT NOT NULL UNIQUE,
    type            TEXT NOT NULL,
    -- What minted the token: init, an operator's create, an OIDC login, or a
    -- browser session. Reported by the token list so an operator can tell a
    -- standing credential from a login that will expire on its own.
    source          TEXT NOT NULL,
    -- Empty for the recovery token, which authenticates as no principal and
    -- sits above policy.
    principal       TEXT NOT NULL DEFAULT '',
    -- The groups the identity provider asserted when the token was minted,
    -- as a JSON array. Captured at login because the policy is evaluated on
    -- every request and the assertion is only available at login.
    asserted_groups BLOB NOT NULL DEFAULT '[]',
    -- Empty when the token does not expire, which is every token an operator
    -- creates by hand.
    expires_at      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    last_used_at    TEXT NOT NULL DEFAULT ''
);

-- At most one recovery token exists, enforced here rather than in the code
-- that mints it. `takt acl init` works exactly once because this index makes
-- the second insert a constraint violation.
CREATE UNIQUE INDEX IF NOT EXISTS idx_token_recovery
    ON token (type) WHERE type = 'recovery';

-- The access-control policy, stored whole as one row. The document replaces
-- rather than accumulates, so a grant absent from it does not exist.
CREATE TABLE IF NOT EXISTS policy (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    document   BLOB    NOT NULL,
    etag       TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);
