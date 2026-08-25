CREATE TABLE workload_port_old (
    workload_id    TEXT    NOT NULL REFERENCES workload (id) ON DELETE CASCADE,
    container_port INTEGER NOT NULL,
    host_port      INTEGER NOT NULL,
    is_dynamic     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workload_id, container_port),
    UNIQUE (host_port)
);

INSERT INTO workload_port_old (workload_id, container_port, host_port, is_dynamic)
SELECT workload_id, container_port, host_port, is_dynamic
FROM workload_port
WHERE protocol = 'tcp';

DROP TABLE workload_port;

ALTER TABLE workload_port_old RENAME TO workload_port;

CREATE INDEX IF NOT EXISTS idx_workload_port_workload_id ON workload_port (workload_id);
