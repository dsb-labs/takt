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

orca apply example.yaml
orca get example
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

restart:
  policy: always

health:
  http: /healthz

container:
  image: nginx:1.27-alpine
```

A workload names exactly one runtime block. `container:` runs an image and `exec:`
runs a command on the host. Everything else applies to either.

## Documentation

- [Manifest reference](docs/manifest.md) — every field a workload can name.
- [Command line](docs/cli.md) — every command and flag.
- [Configuration](docs/configuration.md) — the server's TOML file.
- [Operating orca](docs/operating.md) — exposure, state on disk, and reading logs.
- [Design](docs/design.md) — how reconciliation works and why it is built this way.

[CONTRIBUTING.md](CONTRIBUTING.md) covers building and testing orca.

## Requirements

- Linux. The `exec:` runtime reads `/proc` to identify the processes it started.
- A Docker daemon, for workloads that name `container:`. The `exec:` runtime needs
  nothing beyond the host.

## Licence

MIT. See [LICENSE.md](LICENSE.md).
