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

The server is also published as a container image at `ghcr.io/dsb-labs/orca`.

## A workload

```yaml
version: v1
name: example

labels:
  some-key: some-value

ports:
  - to: 80

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
runs a command on the host. Everything else applies to either.

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

## Documentation

- [Manifest reference](docs/manifest.md) — every field a workload can name.
- [Command line](docs/cli.md) — every command and flag.
- [Secrets](docs/secrets.md) — storing a value a workload reads and you cannot.
- [Configuration](docs/configuration.md) — the server's TOML file.
- [Operating orca](docs/operating.md) — exposure, state on disk, volumes, and reading logs.
- [Design](docs/design.md) — how reconciliation works and why it is built this way.

[CONTRIBUTING.md](CONTRIBUTING.md) covers building and testing orca.

## Requirements

- Linux. The `exec:` runtime reads `/proc` to identify the processes it started.
- A Docker daemon, for workloads that name `container:`. The `exec:` runtime needs
  nothing beyond the host.

## Licence

MIT. See [LICENSE.md](LICENSE.md).
