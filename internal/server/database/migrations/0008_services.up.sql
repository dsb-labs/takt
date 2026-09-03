CREATE TABLE IF NOT EXISTS service (
    id              TEXT    NOT NULL PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    labels          BLOB    NOT NULL DEFAULT '{}',
    target_labels   BLOB    NOT NULL,
    target_port     INTEGER NOT NULL,
    target_protocol TEXT    NOT NULL,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);
