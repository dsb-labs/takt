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
go test -race ./...        # everything, including the end-to-end suite
go test -short ./...       # unit tests only, no Docker needed
go tool staticcheck ./...
```

The end-to-end suite in `internal/e2e` runs a real server against a real Docker
daemon, so it needs one running and is skipped by `-short`. It also runs daily. That
catches breakage no code change caused, such as a runner upgrading Docker or an image
tag moving underneath the tests.

## End-to-end tests

```sh
go test -race ./internal/e2e/...
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

## Running a server

```sh
go run . serve dev.toml
```

`dev.toml` binds to localhost, keeps its state in `./data`, and logs at debug level.

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
It grants itself one inside a user namespace, so it runs without any of the above.
It skips only where the test process holds no ambient capability and the host also
refuses unprivileged user namespaces — the `pam_cap` grant covers that case too.

## Layout

```
main.go                   the root command
api/openapi.yaml          the wire format, and the source of the generated code
cmd/                      one directory per subcommand
internal/generated/       the code generated from the wire format
internal/server/          the server and everything it wires together
  api/                    the HTTP surface
  service/                applying, reading and deleting workloads
  reconciler/             the loop that converges the machine
  driver/                 the runtime boundary
    docker/               containers
    exec/                 processes on the host
  health/                 the checks orca performs
  port/                   host port allocation
  database/               SQLite, and desired state
internal/e2e/             the end-to-end suite
pkg/manifest/             parsing and validating a manifest
pkg/client/               the Go client
docs/                     documentation
```

`pkg/` holds the packages something outside orca would import: the manifest parser and
the client. Everything else is `internal/`.

## Commits

Commits are `<scope>: <description>`, where the scope is the directory path being
changed. The body opens with "This commit" and says what changed and why. Sign off with
`git commit -s`.
