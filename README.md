# orca

A single-node workload orchestrator for Docker containers.

You describe a workload in a YAML file and submit it. orca stores that as the
desired state and continuously reconciles the machine against it: starting what
should be running, replacing what is running an outdated specification, restarting
what died, and stopping what nothing asked for.

## A workload

```yaml
version: v1
name: example
labels:
  some-key: some-value
container:
  image: nginx:1.27-alpine
  env:
    EXAMPLE: EXAMPLE
  ports:
    - "8080:80"
```

A workload names exactly one runtime block, and which block it is selects the
driver that runs it.

## Usage

```sh
orca serve                      # run the server, all defaults
orca serve config.toml          # run the server from a config file

orca apply example.yaml         # create or update a workload
orca list                       # list workloads              (alias: ls)
orca get example                # show one workload
orca logs example --tail 20     # read recent output
orca delete example             # remove it and stop its work (alias: rm)
```

Applying the same file twice is a no-op — a workload's version only changes when
its specification does, so re-running `apply` never restarts healthy work. Read
commands print indented JSON, so they pipe into `jq`.

## How it works

- **The database stores desired state only.** What is actually running is observed
  from the driver on demand, so nothing persisted can go stale against reality and
  a restart needs no recovery of orca's own bookkeeping.
- **Ownership lives in container labels** (`orca.workload`, `orca.spec-hash`,
  `orca.version`), which is how the driver rediscovers its work. Restart the server
  and it adopts the containers already running rather than duplicating them.
- **Reconciliation is level-triggered.** Every pass reads the full desired state,
  asks the driver what is running, and acts on the difference. A ticker guarantees
  convergence; Docker's event stream makes it prompt. A missed event costs
  responsiveness, never correctness.
- **A changed specification replaces rather than mutates**, because Docker cannot
  change most of a container's configuration in place. The spec hash recorded on a
  container is what identifies it as outdated.
- **orca owns restarts**, not Docker, so they can be paced by exponential backoff
  and stay visible in the workload's reported state.

## Configuration

Every value has a default, so `orca serve` works with no file at all. A config file
only has to describe what it changes.

```toml
[http]
address = ":7373"

[data]
directory = "~/.local/share/orca"   # the SQLite database lives here

[docker]
host = ""                            # empty uses the environment, then the local socket

[reconcile]
interval = "10s"                     # full pass cadence; events make convergence prompt

[logging]
level = "info"                       # debug, info, warn, error
```

## Development

The OpenAPI document in `api/openapi.yaml` is the source of truth for the wire
format: the server, client and shared models are all generated from it, so an API
change starts there.

```sh
go generate ./...        # regenerate the api and the mocks
go test -race ./...
go tool staticcheck ./...
go run . serve dev.toml  # localhost, debug logging, ./data
```
