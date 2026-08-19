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
    - to: 80          # the port the container listens on
    - to: 443
      from: 8443      # optional: pin the host port instead
```

A workload names exactly one runtime block, and which block it is selects the
driver that runs it.

### Ports

`to` is the port your process listens on inside the container. `from` is the host
port that reaches it, and leaving it out is the usual case — orca allocates one and
reports it back, so you never have to invent unique host ports by hand:

```sh
orca get example | jq '.Ports'
[ { "To": 80, "From": 20000, "Dynamic": true } ]
```

An allocated port is sticky: it stays the same across restarts and image bumps, so
anything pointing at it keeps working. Pin `from` only when something outside orca
has to know the address up front; pinning one another workload already holds is
rejected when you apply it, rather than failing quietly later.

### Health

A `health:` block tells orca how to check that a workload is actually working, which
is a different question from whether its runtime reports it started. A process that
is listening and answering errors looks fine to docker:

```yaml
version: v1
name: example
health:
  http: /healthz    # or `tcp: true` to just check the port accepts a connection
  port: 80          # only needed when the workload publishes more than one
  interval: 10s
  timeout: 2s
  retries: 3
  startPeriod: 30s
container:
  image: example/example:latest
  ports:
    - to: 80
```

orca performs the check itself, from the host, against the port it allocated — so an
image carrying no shell can still be checked, and any driver gets the behaviour by
publishing an address rather than implementing checks of its own.

A workload that exhausts its retries is restarted on the same paced schedule a
crashed one takes. `startPeriod` is the grace it gets first: failures inside it don't
count, so a workload slow to become ready isn't killed for failing checks it was
never going to pass yet. Passing one check ends the grace early — it has demonstrably
started.

Only `http` or `tcp` is required; the timings above are the defaults.

## Usage

```sh
orca serve                      # run the server, all defaults
orca serve config.toml          # run the server from a config file

orca apply example.yaml         # create or update a workload
orca list                       # list workloads              (alias: ls)
orca list -q '$.labels.app=web' # ...filtered by a query
orca get example                # show one workload
orca logs example --tail 20     # read recent output
orca delete example             # remove it and stop its work (alias: rm)
orca delete example --wait      # ...and block until it's gone
```

Applying the same file twice is a no-op — a workload's version only changes when
its specification does, so re-running `apply` never restarts healthy work. Read
commands print indented JSON, so they pipe into `jq`.

### Finding workloads

`list` takes repeatable `--query` filters, each a JSON path into the workload's
specification and the value it must hold. A workload has to match all of them, so
adding a query narrows the result:

```sh
orca list -q '$.labels.app=web'
orca list -q '$.labels.app=web' -q '$.labels.env=prod'
orca list -q '$.container.image=nginx:1.27-alpine'
orca list -q '$.container.ports[0].to=80'
```

Labels are just part of the specification, so they need no special syntax. Values
are compared as text, which is why a number is matched by its digits; a boolean is
stored as `1` or `0` and has to be written that way.

Deleting is asynchronous. The workload reads as `terminating` while its containers
are stopped and disappears once nothing is left running for it, so a teardown can
be watched by polling `get` until it 404s. Applying a workload that is still
terminating is rejected rather than resurrecting it half-torn-down.

## How it works

- **Specifications are queryable.** Labels and specs are stored as SQLite JSONB, so
  a filter runs in the database rather than by loading every workload and discarding
  most of them. Storing JSONB also means a malformed specification is rejected as it
  is written rather than when something later tries to read it.
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
- **Host ports are allocated by orca, not the runtime**, so a workload's address is
  known when it is applied rather than discovered afterwards, and a driver whose
  runtime has no allocator of its own inherits the behaviour. A port orca chose is
  revised if it proves unusable; one you pinned never is.
- **Only the reconciler touches the runtime.** Deleting a workload records the
  intent and the reconciler performs the teardown, removing the desired state last.
  Nothing races it for the same containers, and a failure part-way leaves a workload
  that gets torn down again rather than containers nothing records.

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

[ports]
min = 20000                          # range orca allocates host ports from
max = 32000

[logging]
level = "info"                       # debug, info, warn, error
```

## Development

The OpenAPI document in `api/openapi.yaml` is the source of truth for the wire
format: the server, client and shared models are all generated from it, so an API
change starts there.

```sh
go generate ./...          # regenerate the api and the mocks
go test -race ./...        # everything, including the end-to-end suite
go test -short ./...       # unit tests only, no docker needed
go tool staticcheck ./...
go run . serve dev.toml    # localhost, debug logging, ./data
```

The end-to-end tests in `internal/e2e` run a real server against a real Docker
daemon, so they need one running and are skipped by `-short`. They also run on a
daily schedule, which catches breakage that isn't tied to a code change — a runner
upgrading Docker, or the image tag they use moving underneath them.
