# orca

A single-node workload orchestrator.

You describe a workload in a YAML file and submit it. orca stores that as the desired
state and reconciles the machine against it continuously. It starts what should be
running, replaces what runs an outdated specification, restarts what died, and stops
what nothing asked for.

A workload runs either as a Docker container or as a command on the host.

## Quick start

Download an archive for your platform from the
[releases](https://github.com/dsb-labs/orca/releases) page and put `orca` on your
`PATH`. Then start the server:

```sh
orca serve          # listens on 127.0.0.1:7373
```

Write a manifest and apply it:

```sh
cat > example.yaml <<'EOF'
version: v1
name: example
ports:
  - to: 80
container:
  image: nginx:1.27-alpine
EOF

orca workload apply example.yaml
orca workload get example
```

`get` reports the host port orca allocated, which is how the workload is reached.

Each release also publishes `.deb` and `.rpm` packages that install the server as
a systemd service. See
[Running under systemd](docs/operating.md#running-under-systemd).

## A workload

```yaml
version: v1
name: example

labels:
  some-key: some-value

ports:
  - name: http
    to: 80

env:
  EXAMPLE: EXAMPLE

volumes:
  - name: example-data
    to: /var/lib/example

restart:
  policy: always

health:
  http: /healthz

container:
  image: nginx:1.27-alpine
```

A workload names exactly one runtime block. `container:` runs an image and `exec:`
runs a command on the host. Everything else applies to either. `resources:` applies
to either too, though the exec runtime needs the host to delegate a cgroup subtree
and refuses the limits when it does not.

A `volumes` entry names a volume, a secret or a variable. A volume is storage. The
other two are files holding the value orca stores under that name.

A volume is created before the workload that mounts it and outlives that workload, so
deleting a workload never destroys what it stored. Its manifest is a name and nothing
else:

```yaml
version: v1
name: example-data
```

```sh
orca volume create volume.yaml
orca workload apply example.yaml
```

An `env` value can read a secret rather than holding one. The value is stored
encrypted, and only a workload starting ever sees it:

```sh
printf %s hunter2 | orca secret set db-password
```

```yaml
env:
  DSN: postgres://app:${secret:db-password}@localhost:5432/app
```

Changing a secret's value replaces the workloads reading it, so a rotation does not
have to be followed by an apply.

A secret that is a file — a certificate, a key — can be mounted instead of read from
the environment:

```yaml
volumes:
  - secret: tls-cert
    to: /etc/tls/cert.pem
    signal: SIGHUP
```

`signal` asks orca to rewrite the file and signal the workload when the value changes,
rather than replacing the workload. Leave it out to have the workload replaced.
Mounting a secret writes it to the host filesystem, which
[Secrets](docs/secrets.md) covers.

A variable is the same thing for a value worth reading back — a hostname, a log level,
a feature flag:

```sh
orca variable set db-host db.internal
```

```yaml
env:
  DSN: postgres://app@${var:db-host}:5432/app
```

An `env` value can also read the address of another workload, so one workload can be
pointed at another without either of them naming a port orca chose:

```yaml
version: v1
name: postgres
ports:
  - name: pg
    to: 5432
container:
  image: postgres:17-alpine
```

```yaml
env:
  DSN: postgres://app:${secret:db-password}@${workload:postgres:pg}/app
```

`${workload:postgres:pg}` becomes the address that port is reached at. If orca ever
moves the port, the workloads reading it are redeployed with the new address, so the
dependency keeps working without being re-applied.

## Documentation

- [Manifest reference](docs/manifest.md) — every field a workload can name.
- [Command line](docs/cli.md) — every command and flag.
- [Services](docs/services.md) — reporting the addresses of a labelled set of instances for a load balancer.
- [Volumes](docs/volumes.md) — directories that outlive the workloads mounting them.
- [Secrets](docs/secrets.md) — storing a value a workload can read and you cannot.
- [Variables](docs/variables.md) — storing a value both you and a workload can read.
- [Configuration](docs/configuration.md) — the server's TOML file.
- [Operating orca](docs/operating.md) — exposure, the web UI, state on disk, backups, and reading logs.
- [Upgrading](docs/upgrading.md) — replacing the binary, and what survives it.
- [Design](docs/design.md) — how reconciliation works and why it is built this way.
- [Reconciliation](docs/reconciliation.md) — the reconciler's mechanics, with diagrams.

[CONTRIBUTING.md](CONTRIBUTING.md) covers building and testing orca.

## Requirements

- Linux, on the host rather than in a container. The `exec:` runtime starts
  processes on the machine orca runs on, and reads `/proc` to identify them. See
  [Not in a container](docs/operating.md#not-in-a-container).
- Linux 6.2 or later with Landlock enabled, for workloads that name `exec:`. Every
  `exec` workload is confined by the kernel, and a host that cannot do that refuses to
  run one. See [Confinement](docs/operating.md#confinement).
- A Docker daemon, for workloads that name `container:`. The `exec:` runtime needs
  nothing beyond the host.

## Licence

MIT. See [LICENSE.md](LICENSE.md).
