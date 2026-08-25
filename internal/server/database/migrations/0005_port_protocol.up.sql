CREATE TABLE workload_port_new (
    workload_id    TEXT    NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    container_port INTEGER NOT NULL,
    host_port      INTEGER NOT NULL,
    protocol       TEXT    NOT NULL DEFAULT 'tcp' CHECK (protocol IN ('tcp', 'udp')),
    is_dynamic     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workload_id, container_port, protocol),
    UNIQUE (host_port, protocol)
);

INSERT INTO workload_port_new (workload_id, container_port, host_port, protocol, is_dynamic)
SELECT workload_id, container_port, host_port, 'tcp', is_dynamic
FROM workload_port;

DROP TABLE workload_port;

ALTER TABLE workload_port_new RENAME TO workload_port;

CREATE INDEX IF NOT EXISTS idx_workload_port_workload_id ON workload_port (workload_id);
