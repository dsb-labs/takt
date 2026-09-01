# Contributing

## Building

```sh
go build -o orca .
```

Go 1.26 or later. The `exec` runtime reads `/proc`, so orca runs on Linux.

## The API is generated

`api/openapi.yaml` is the source of truth for the wire format. The server interface,
the client, and their shared models are generated from it. An API change starts
there:

```sh
go generate ./...
```

That regenerates the API and the test mocks. CI checks that running it produces no
diff, so a change to the specification and a change to the generated code land
together.

## Testing

```sh
make test                  # unit tests, no Docker needed
make e2e                   # the end-to-end suite, needs a Docker daemon
go tool staticcheck ./...
```

The make targets wrap the run in `scripts/delegated.sh`, which starts it inside a
delegated cgroup subtree — the thing that lets the server enforce resource limits on
exec workloads. See the [delegation](docs/operating.md#delegation) section of the
operating guide. Plain `go test` commands still work for narrowing to one package or
one test, but run them through the wrapper when the tests touch resource limits:

```sh
./scripts/delegated.sh go test -run TestDriver_ResourceLimits ./internal/server/driver/exec
```

Without a delegated subtree those tests fail naming the fix, rather than skip: a
skipped test is coverage nobody notices losing.

The end-to-end suite in `internal/e2e` runs a real server against a real Docker
daemon, so it needs one running and is skipped by `-short`. It also runs daily. That
catches breakage no code change caused, such as a runner upgrading Docker or an image
tag moving underneath the tests.

### Golden files

Expected values that are too long to read inline live in `testdata/*.golden`, asserted
with `gotest.tools/v3/golden`. Regenerate them with `-update`:

```sh
go test ./pkg/manifest -update
```

The golden files under `internal/server/spechash` hold specification hashes, and one
of those moving replaces every running instance on every node when operators upgrade.
Read the diff and decide the change is one you meant to make before you regenerate
them. Refreshing a golden file to make the suite green is how a fleet-wide redeploy
ships unnoticed.

## End-to-end tests

```sh
make e2e
```

Each test starts a real server inside the test process and drives it through the
public client. `-run` narrows the suite to one test in the usual way, and `-v` raises
the server's log level to debug.

Every test writes a debug bundle to `internal/e2e/artifacts/<test name>/`: the
server's spans in `trace.json`, its logs in `logs.json`, and a final metrics scrape
in `metrics.prom`. A failure can be diagnosed from what the server actually did
rather than reconstructed from assertion messages. A test that restarts its server
accumulates both runs' output in one bundle. The directory is not tracked, and the
nightly workflow uploads it when a run fails.

## Load testing

```sh
go run . dev loadtest scenarios/smoke.toml
```

`orca dev loadtest` drives a running server through a scenario: it creates the
secrets, variables and volumes the scenario names, applies the fleet at once, waits
for it to converge, churns against it, tears it down and reports. The command is
hidden, because it is for working on orca rather than for operating it.

Run `smoke.toml` before a commit. It is a few of everything and takes seconds.

### What a scenario says

A scenario describes the **shape of a fleet** rather than the workloads in it. What a
measurement depends on is that eighty workloads exist and that a third of them publish
a port, not what the eighty are running — so the workload body belongs to the tool and
does as little as possible. A scenario that could name an image would eventually name
a large one, and the run would measure the daemon's network rather than orca.

```toml
[fleet]
containers = 80
exec       = 80

# Proportions, so a scenario scales by changing the counts alone.
dynamic-ports = 0.33
health-checks = 0.25
failing       = 0.05
mounts-secret = 0.25
```

Every proportion names a subsystem it stresses, so a scenario can be read to see what
it covers. Unknown keys are refused: a mistyped proportion that silently did nothing
would leave a scenario claiming coverage it does not have.

The scenarios live in `scenarios/`:

| Scenario | What it stresses |
|---|---|
| `smoke.toml` | a few of everything, in seconds |
| `churn.toml` | the mixed fleet of 160 the published numbers were measured against |
| `ports.toml` | the host port allocator, pinned and allocated together |
| `secrets.toml` | rotation, which redeploys every workload reading the value |
| `failures.toml` | restart backoff and giving up |
| `references.toml` | workloads reading each other's addresses, applied in two waves |
| `stampede.toml` | a thousand workloads with every feature at once, churned hard |

Add one by adding a file. The package's tests parse every scenario in the directory
and build each into specifications, checked against the same validation an operator's
manifest gets — so a scenario describing a fleet the server would refuse fails the
build rather than a run.

### Reading a run

The report is JSON on stdout, and the command says nothing else. It carries the
scenario's name and description, so a report read on its own says which run produced
it and what that run was for.

```sh
go run . dev loadtest scenarios/churn.toml > report.json
```

The command exits non-zero when a request failed, a workload never ran, or something
was left behind. Latency is reported rather than judged: how fast a machine is says
nothing about whether the code is right, so there are no thresholds to go stale.

`--data-dir` points at the server's data directory and adds a check of what was left
on disk after teardown. That only works when the load test runs on the server's own
host, and it is the only way to see a leak the API does not expose — a directory
holding a secret's plaintext is not something any endpoint reports.

```sh
go run . dev loadtest scenarios/secrets.toml --data-dir ./data
```

Everything a run creates is named after `--prefix`, which defaults to something unique
to the run, so two load tests against one server neither collide nor tear down each
other's work. `--keep` leaves the fleet in place to inspect.

**Do not run a load test while the end-to-end suite is running.** Both drive the same
Docker daemon, and a teardown removes containers by orca's label.

## Running a server

```sh
make dev
```

`dev.toml` binds to localhost, keeps its state in `./data`, and logs at debug level.
The target runs the server through `scripts/delegated.sh`, so it can enforce
resource limits on exec workloads. A plain `go run . serve dev.toml` works too, and
refuses manifests naming limits on exec workloads when the shell is not delegated.

### The web UI

The UI sources live under `internal/ui/app`, a Vue and TypeScript project built by
Vite. `make ui` builds the bundle into `internal/ui/dist`, which the binary embeds
on the next build. A binary built without the bundle still serves the API, and
answers page requests with a message saying the UI is not in the build.

For working on the UI itself, run `make dev-ui` beside `make dev`. It starts the
Vite dev server with hot reload and proxies API requests to the server on
localhost:7373.

The request and response types are generated from `api/openapi.yaml` into
`src/api/schema.d.ts` and committed. After changing the spec, run `yarn generate`
in `internal/ui/app` — CI fails on drift the same way it does for the Go code.

## The capability the dev loop wants

Deleting a volume a container wrote as another user needs `CAP_DAC_OVERRIDE` — see
[Deleting a volume a container wrote](docs/operating.md#deleting-a-volume-a-container-wrote).
A production server gets it from its systemd unit. A dev server started with `go run`
has no unit, so give your own sessions the capability once through `pam_cap`:

```sh
# /etc/security/capability.conf, above the "none *" line. The ^ raises the
# capability as ambient, so every process in the session holds it.
^cap_dac_override <your-user>
```

`pam_cap.so` is already in the PAM stack on Debian and Ubuntu. Log in again and
check with `grep CapAmb /proc/self/status`, which reports `0000000000000002`.
After that, volume deletion works under `go run` and in the tests, with nothing
to redo when the binary is rebuilt.

One test watches the confinement trampoline strip this capability from a workload.
The test grants itself a capability inside a user namespace, so it runs without any
of the above.
It skips only where the test process holds no ambient capability and the host also
refuses unprivileged user namespaces — the `pam_cap` grant covers that case too.

## Layout

```
main.go                   the root command
api/openapi.yaml          the wire format, and the source of the generated code
cmd/                      one directory per subcommand
scenarios/                load test scenarios
internal/generated/       the code generated from the wire format
internal/wire/            mapping between the wire and canonical specifications
internal/server/          the server and everything it wires together
  api/                    the HTTP surface
  service/                applying, reading and deleting workloads
  reconciler/             the loop that converges the machine
  driver/                 the runtime boundary
    docker/               containers
    exec/                 processes on the host
  health/                 the checks orca performs
  port/                   host port allocation and claiming
  secret/                 the encryption a secret is stored under
  spechash/               the hash a workload is replaced on
  telemetry/              traces and metrics
  database/               SQLite, and desired state
internal/e2e/             the end-to-end suite
internal/loadtest/        the load test scenarios are run from here
pkg/manifest/             the canonical specification, and parsing one
pkg/client/               the Go client
docs/                     documentation
```

`pkg/` holds the packages something outside orca would import: the manifest parser and
the client. Everything else is `internal/`.

## The wire format stops at the API

`manifest.Spec` is the specification orca reasons about. It is what a manifest file
parses into, what the server stores, and what the hash covers. The generated wire
types stay in the three packages with business in them: `internal/server/api`,
`pkg/client`, and `internal/wire`, which maps between the two.

A change below the HTTP API works on `manifest.Spec`. Importing
`internal/generated/api` anywhere else means the conversion is happening too late.

## Commits

Commits are `<scope>: <description>`, where the scope is the directory path being
changed. The body opens with "This commit" and says what changed and why. Sign off with
`git commit -s`.
