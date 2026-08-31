# Operating orca

## Exposure

Applying a workload runs code, with whatever access the Docker socket grants. Treat
orca's port as equivalent to that socket, because in practice it is.

The default address is loopback for that reason. Restrict who can reach it before
binding it to a network:

- A reverse proxy that requires a credential.
- A WireGuard or Tailscale interface, so the port is only reachable inside it.
- An SSH tunnel, for occasional access from one machine.

Set `hosts` to the name the proxy serves when you put one in front of orca. See
[Configuration](configuration.md#http).

Encrypting secrets does not change this. An attacker who can reach the API can apply
a workload that reads any secret, because that is what a workload is for. What
encryption protects is the database file and a backup of it. See
[Secrets](secrets.md#what-it-does-not).

Variables are not protected at all. Anything that can reach the API can read every
variable and its value, which is what they are for. Put anything that would be
damaging to report in a secret instead. See [Variables](variables.md).

The `/metrics` endpoint reports workload names as label values. Anything that can
reach the port can already run arbitrary workloads, so the names disclose nothing
new — but they are disclosed, and a scraper is one more thing with reach to
account for.

### Serving TLS directly

The server terminates TLS itself when the configuration names a certificate pair:

```toml
[http]
address = "127.0.0.1:7373"
tls-cert = "/etc/orca/tls/cert.pem"
tls-key = "/etc/orca/tls/key.pem"
```

A reverse proxy is still the recommended front door, because it can also require
a credential — TLS encrypts the connection and authenticates nobody. Serving TLS
directly is for the deployment that only wanted the proxy to terminate TLS, such
as a certbot-managed certificate on a home server. Over WireGuard or Tailscale,
which are themselves encrypted, it adds little.

The pair is reread when the certificate file changes, so a renewal tool that
rewrites the files needs no restart and no hook. A rewrite the server cannot
load keeps the previous pair in use and is retried, so a renewal caught between
its two writes heals itself.

The key file must be readable only by the user running the server, or the server
refuses to start. This is the same rule the secret keyring applies, and for the
same reason.

A client trusts a self-signed pair by naming its certificate:

```sh
orca workload list --address https://orca.example.com:7373 --ca-cert /etc/orca/tls/cert.pem
```

There is no flag to skip verification. The host name check described below still
applies to a TLS listener.

### Loopback is not a boundary against a browser

A loopback bind stops another machine reaching orca. It does not stop a web page.

A browser sends a request to `127.0.0.1` on behalf of whatever page it has open. An
attacker serves a page from a name they control, points that name at `127.0.0.1`, and
the browser treats what follows as same-origin. The request arrives from loopback, so
nothing about where it came from says the operator asked for it.

orca checks the name each request asks for, which is what closes this. A request
naming an address or `localhost` is accepted, since an attacker cannot point either at
a victim's own machine. A request naming anything else has to name something in
`hosts`, or it is refused. A request from a browser page on another origin is refused
whatever it names.

A request carrying a body must also declare `Content-Type: application/json`. That is
already the only body the API reads, and requiring it turns away the form-encoded and
plain-text requests a browser will send across origins without asking permission
first.

The CLI does all of this correctly. A hand-written `curl` that sends a body needs the
header:

```sh
curl -X PUT -H 'Content-Type: application/json' \
  --data-binary @workload.json \
  http://127.0.0.1:7373/api/v1/workloads/example
```

This matters most for whoever runs orca on a workstation they also browse from.

### Workload ports are published separately

Restricting reach to orca's own port does not restrict reach to its workloads. A
container's host port is published on the address `workload.bind` names, which is
every interface by default. Anything a workload serves is reachable wherever the host
is, so it is worth knowing which workloads that covers.

Name an interface's address to narrow that:

```toml
[workload]
bind = "10.0.0.5"
```

A Tailscale or WireGuard address is the usual answer, and reaches only what is on that
network.

Loopback is not the answer it looks like. A container dialling a port published on
loopback reaches its own loopback rather than the host, so `bind = "127.0.0.1"` stops
containers reaching each other as well as stopping the network reaching them. See
[Configuration](configuration.md#workload).

An `exec` workload is not covered either way. The process binds its own port, so what
it listens on is decided by the command rather than by orca.

## Running under systemd

Each release publishes a `.deb` and an `.rpm` package. A package installs:

- the `orca` binary, at `/usr/bin/orca`.
- a systemd unit, `orca.service`.
- a sysusers entry that creates the `orca` system user.
- `/etc/orca/config.toml`, which an upgrade never overwrites.

The unit runs `orca serve /etc/orca/config.toml` as the `orca` user, with the data
directory at `/var/lib/orca`. systemd creates that directory, owned by the `orca`
user and readable only by it, which is the mode [State on disk](#state-on-disk)
requires.

Installing the package does not enable or start the service. The server cannot start
until it can reach the Docker socket, and granting that access is yours to decide
rather than a default:

```sh
usermod -aG docker orca
systemctl enable --now orca
```

Membership of the `docker` group is root-equivalent. Anything in it can run a
privileged container, so the grant above hands the `orca` user the host. That is
what running orca means — see [Exposure](#exposure) — but it should happen because
you typed it, not because a package script did.

Three of the unit's settings carry behaviour documented elsewhere:

- `Type=notify`. The server tells systemd it is ready when its listener is bound,
  so `Restart=on-failure` and unit ordering track the server actually serving.
- `KillMode=process`. Stopping the unit signals the server alone, so workloads
  survive a restart. The default mode kills every process in the unit's cgroup.
  See [Restarting the server](#restarting-the-server).
- `Delegate=yes` with `DelegateSubgroup=main`, which is what makes `exec` resource
  limits enforceable. See [Delegation](#delegation).

The unit also confines the server. `ProtectSystem=strict` makes the filesystem
read-only outside `/var/lib/orca`, `ProtectHome=yes` hides home directories, and
`NoNewPrivileges=yes` stops privilege escalation. An `exec` workload inherits these
restrictions, and they compose with [Confinement](#confinement): the system
directories a workload may read stay readable, and everything it may write sits
under `/var/lib/orca`. One consequence is worth knowing — under
`NoNewPrivileges=yes` a workload cannot run a setuid binary.

The unit grants the server `CAP_DAC_OVERRIDE`, so `orca volume delete` can remove
files a container wrote as another user. The grant does not reach the workloads.
See [Deleting a volume a container wrote](#deleting-a-volume-a-container-wrote).

### Not in a container

orca runs on the host, and the release deliberately publishes no container image.
The `exec` runtime starts processes on the machine orca runs on, so inside a
container those workloads would run inside it too. Volumes and mounted values break
more quietly: the Docker daemon resolves a bind mount against the host filesystem,
and orca would write them somewhere the daemon cannot see.

## State on disk

Everything orca keeps lives under the data directory, `~/.local/share/orca` by
default:

```
state.db          the workloads that have been applied
state.db-wal      SQLite's write-ahead log
state.db-shm      SQLite's shared-memory index
keys/             the keys secrets are encrypted with, one file each
exec/state/       what orca started, one directory per exec workload
exec/workloads/   where exec workloads run, one directory each
volumes/          one directory per volume
mounts/files/     the secrets and variables workloads mount, one directory each
mounts/state/     what orca wrote for each of them
```

The database holds each workload's stored specification, environment included, so orca
creates the directory and its files readable only by the user running the server.

A secret's value is encrypted in the database rather than held in a specification, so
a backup of `state.db` alone does not disclose one. The keyring is what decrypts them,
and it needs a backup of its own: a value sealed under a key that is gone cannot be
recovered. Keeping the two apart is what makes the database safe to copy. See
[Secrets](secrets.md#the-encryption-key).

`mounts/files/` is the exception. A workload that mounts a secret gets a file holding
the plaintext, because there is no way to put a value inside a container without writing
it somewhere first. Those files are written as a workload starts and removed once
nothing is running for it, and their directories are readable only by the user running
the server. **A backup of the data directory includes them in the clear.** See
[Mounting a secret as a file](secrets.md#mounting-a-secret-as-a-file).

The database holds desired state only. What is actually running is observed from the
runtime when asked, so nothing persisted can go stale against reality. A restarted
server needs no recovery of orca's own bookkeeping.

## Backups

```sh
orca admin backup /backups/orca.zip
```

The server takes a consistent snapshot of its database while it keeps running and
sends it back as a zip archive.

**Do not copy `state.db` with `cp`.** The database runs in write-ahead logging mode,
so what is committed at any moment is spread across `state.db`, `state.db-wal` and
`state.db-shm`. Copying the first alone produces a file that is stale or torn, and it
fails quietly: SQLite opens the result happily, and the transactions that are missing
are noticed later, if at all. On a busy node most of the committed state can be in
the log rather than in the database file.

A backup covers what is in the database: workloads, volumes' records, secrets,
variables and port allocations. Three things it does not cover, each for its own
reason:

| Not in the archive | Why | What to do |
|---|---|---|
| The keyring | The database and the keys are separable on purpose, so a copy of the database is safe to keep where a key would not be. | Back it up separately. It changes only when you rekey. |
| Volume data | Copying arbitrary user data is not orca's job. | `orca volume list` reports each path. Back those up yourself. |
| `mounts/` | Transient. Rewritten as a workload starts. | Nothing. |

`--include-keys` puts the keyring in the archive. It makes a restore one step instead
of two, and it makes the archive key material: anything that can read it can read
every secret the node holds. Setting `secrets.keys` to somewhere already covered by a
backup is the better answer, and then the default needs nothing.

Every key goes in, not only the one sealing secrets now. A key that `orca admin rekey`
replaced still opens the archives taken before it was replaced.

## Restoring a node

This section is written for the case that matters: the host is gone, and a
replacement has to come back holding what the old one held. A corrupted file on a host that still exists is
the same procedure with more of it already in place.

```sh
orca admin restore /backups/orca.zip /etc/orca/config.toml
```

**Run it with the server stopped.** This is the one command that talks to no server: it
reads the configuration file `orca serve` reads, works over the data directory
directly, and refuses to run while anything is listening on the configured address.
A restore under a running server writes a database out from under the connections
reading it.

It writes `state.db` into the data directory and the keyring into wherever
`secrets.keys` puts it, and removes any stale `state.db-wal` and `state.db-shm`
first. That last part is what goes wrong by hand: SQLite replays a stale log against
a restored database perfectly happily, and the node comes up holding state that is
quietly not what was backed up.

A database already in the data directory is refused rather than replaced. Move it
aside first, deliberately.

### The part that is yours

Volume data is not in a backup, so the restore cannot put it back. What it does
instead is tell you exactly what is missing:

```json
{
  "Restored": ["/var/lib/orca/state.db", "/var/lib/orca/keys/da8lt98hpe2jmgmjl9fg.key"],
  "MissingKeys": null,
  "MissingVolumes": [
    {
      "ID": "da8ltc8hpe2jmgmjl9g0",
      "Name": "example-data",
      "Path": "/var/lib/orca/volumes/da8ltc8hpe2jmgmjl9g0"
    }
  ]
}
```

Copy each volume's backed-up contents to the path named there, and start the server.

**Restore the data under the identifier, not under a new volume of the same name.** A
volume is found by the identifier it was assigned. Creating one called
`example-data` on the restored node gives it a fresh identifier, so the row and the
data end up in different directories — and nothing reads as an error. The workload
starts, mounts its volume, and finds it empty.

`MissingKeys` is the other half. It names every key the secrets are sealed under that
the keyring does not hold, which is what an archive taken without `--include-keys`
leaves behind. Restore the keyring's own backup before starting the server. Without
it every workload reading a secret fails to start, and nothing at the workload says
why.

### What is deliberately not restored

`exec/state/` and `mounts/` are not in the archive and must not be copied across from
an old host. The first records process identifiers, which on a new host name whatever
happens to hold those numbers now — the liveness check exists to prevent exactly that
mistaken identity. The second is rewritten as a workload starts. The first
reconciliation pass re-derives both.

## Exec workload directories

Each version of each instance of an `exec` workload gets a directory of its own,
under two separate trees:

```
exec/workloads/<id>/<instance>/<version>/
  cwd/            the process's working directory
  output.log      the process's output, both streams combined
  previous.log    the output of the attempt this one replaced

exec/state/<id>/<instance>/<version>/
  state.json      the process orca started, and how it ended
  retained        present when the record is kept only for its output
```

`<instance>` is the instance's index, counted from zero. A workload running one
instance keeps everything under `0`.

The process runs in `cwd`. What orca records about it lives in the other tree, which
holds no working directory at all, so a workload writing above its own has nothing of
orca's to reach. A workload that could rewrite its own record could name any process on
the host as its own.

`<id>` is the identifier orca assigned the workload rather than its name. A name is
what an operator types and reaches orca from places no manifest validated, so it is
never a path component. It is read from inside a record when orca needs it.

`output.log` grows for as long as the workload runs. orca does not rotate or truncate
it, so a workload that writes continuously needs watching.

`previous.log` is the output of the attempt a replacement took the place of, which is
what `orca workload logs --previous` reads. Stopping a workload moves `output.log` to it,
so the attempt starting next writes to a file of its own rather than appending to the one
before it. Only the most recent replaced attempt is kept, so this is one file rather than
one per restart.

`state.json` records the process identifier and the kernel's start time for that
process. Both have to match for orca to claim the workload is still running. A process
identifier alone is reused, so a record naming one that is alive may be describing
something else.

A workload's directories in both trees are removed when the workload is deleted, so its
output survives for as long as the workload does.

## Confinement

An `exec` workload runs as the same user as the server, so file permissions draw no
boundary around it: everything that user can reach, it can reach, including the
keyring, the database, and every other workload's mounted plaintext. Running
workloads as a separate user would need privileges orca deliberately does not ask
for.

The kernel is what draws the boundary instead. orca confines every `exec` workload with
[Landlock](https://landlock.io), which lets an unprivileged process restrict itself
before it runs the command. A confined workload reaches:

- its own working directory, for reading and writing,
- each volume it mounts, for reading and writing,
- each secret or variable it mounts, for reading only,
- its own command, and the host's system directories,
- anything `exec.allow-paths` names, for reading only.

Everything else is refused, the rest of the data directory included. There is no
opt-out, and no reduced mode on a host that offers less: confinement that did nothing
on some hosts would be a guarantee nobody could rely on.

**This requires Linux 6.2 or later, with Landlock enabled.** A host below that refuses
to start `exec` workloads and says so at startup. Container workloads are unaffected,
so such a host still runs everything else. The version is set by the third Landlock
interface, which is the first where a read-only grant also prevents truncation. Below
it a workload could empty a value it cannot rewrite.

Two consequences are worth knowing:

- **A workload cannot read another process's environment.** An `exec` workload's
  environment is readable at `/proc/<pid>/environ` by the user running it, so without
  confinement one workload could read another's secrets from it. Confinement closes
  that.
- **A workload cannot attach a debugger to the server.** Landlock scopes `ptrace`
  between domains. Without that, restricting the filesystem alone would be defeatable.

A workload also starts with no ambient capabilities. orca drops the ambient set
before the command runs, so a capability granted to the server — see
[Deleting a volume a container wrote](#deleting-a-volume-a-container-wrote) — never
reaches a workload.

What it does not cover is anything not reached through a filesystem path. Signal
scoping arrives in a later Landlock version than orca requires, so a confined workload
can still send a signal to the server. The network is not restricted either.

There is deliberately no grant for `/tmp`. It is shared by every process running as
the same user, so granting it would let one workload read what another wrote there. A
workload needing scratch space has its own working directory, and `TMPDIR` will point
a command at it.

## Delegation

An `exec` workload naming `resources:` runs in a cgroup of its own, which is what
enforces the limits. Writing below `/sys/fs/cgroup` needs either root or a subtree
delegated to orca's user, so enforcement needs the host's help where confinement does
not. orca derives the subtree from the cgroup it was started in — there is nothing to
configure.

Running the server under systemd with `Delegate=yes` on its unit grants a subtree.
Setting `DelegateSubgroup=main` as well is recommended: it places the server in a
leaf of the subtree, which orca otherwise has to arrange for itself, and it keeps a
restart working when processes survive the old server. A bare `orca serve` from a
shell may or may not sit in a delegated cgroup, and the startup log says which.

A host without a delegated subtree refuses an apply that names limits on an `exec`
workload, and says so at startup. There is no reduced mode, for the reason
confinement has none: a limit that did nothing on some hosts would be a guarantee
nobody could rely on. In particular orca does not fall back to rlimits — they cap
address space rather than memory used and count the user's processes rather than the
workload's, so the same manifest field would mean something different per runtime.
Container workloads are unaffected, and so is every `exec` workload naming no limits.

Inside the subtree, orca keeps a `main` cgroup holding the server and every unlimited
workload, and one `orca-<id>-<version>` cgroup per limited workload. The limits are
written before the command starts, so it never runs outside them, and the cgroup is
removed when the workload stops.

One interaction with the service manager is worth knowing. A limited workload's
processes necessarily live inside the unit's subtree, and systemd's default
`KillMode=control-group` kills everything in it when the unit stops. A limited
workload outlives a server restart only under `KillMode=process`, which signals the
server alone. An unlimited workload is unaffected by the limits work either way.

## Volumes

Each volume gets a directory named for the identifier orca assigned it:

```
volumes/<id>/
```

Everything a workload writes to a mounted volume is in there. The directory is created
when the volume is created and removed only when the volume is deleted, so it survives
the workloads that mount it — including a workload being replaced, restarted or
deleted.

`orca volume list` reports where each volume's data is, which is what something taking
a backup needs:

```sh
orca volume list | jq -r '.[] | "\(.Name)\t\(.Path)"'
```

Nothing tells a workload where its volume is on the host. An exec workload told that
would know it sits inside orca's data directory, and could walk out of it.

A volume is bind-mounted into a container, so the Docker daemon has to share this
filesystem. Volumes do not work against a daemon reached over the network.

### Deleting a volume a container wrote

A container runs as whatever user its image names, and the files it writes to a
volume belong to that user. The postgres image is the familiar case: it re-owns its
data directory and makes it readable only by its own user. The server's user then
cannot remove those files, and `orca volume delete` fails with a permission error.

The `CAP_DAC_OVERRIDE` capability lets the server remove files whatever their
owner. The packaged unit grants it. Grant it yourself when you run the server
another way — under systemd:

```ini
[Service]
User=orca
AmbientCapabilities=CAP_DAC_OVERRIDE
```

The grant does not reach the workloads. orca drops its ambient capabilities before an
exec workload's command runs, so the command holds none of them. A container's
capabilities come from the Docker daemon rather than from orca.

A server without the grant runs everything, and only deleting a volume holding
another user's files needs it. The same ownership stops anything else
running as the server's user — a backup, for one — from reading those files, and the
capability changes nothing for them.

## Restarting the server

A container keeps running when the server stops, and so does an exec process. orca
rediscovers both on its next start rather than duplicating them, so restarting the
server is a different thing from restarting the workloads it runs.

Containers are found by the labels orca sets on them. Exec processes are found from the
records under `exec/state`, which is why those have to outlive the server that wrote
them.

One thing does not survive. Only a process's parent can collect its exit code, so an
exec workload that ends while the server is down leaves none.

orca reports that as a failure rather than a clean exit. A job which may not have
finished is better run again than assumed complete. Under `restart: always` it changes
nothing, and under `on-failure` it means such a job runs again.

## Reading logs

```sh
orca workload logs example --tail 20
```

For a container, orca reads the logs from the Docker daemon. For an exec workload it
reads `output.log`. Either way both output streams come back combined in the order
they were written.

### Watching output as it arrives

```sh
orca workload logs example --follow
```

This keeps the connection open and prints each line as the workload writes it, which is
what watching a workload start needs. It ends when the instance ends, or when you press
Ctrl-C.

A replacement is a new instance. A workload that restarts while you are watching ends
the command, and reading the next attempt means running it again.

A workload running more than one instance is followed one instance at a time, named
by `--instance` with the index to read. A follow without one is refused, because a
follow reads one stream and interleaving several would return output nothing could
attribute. An ordinary read without `--instance` returns every instance's output.

### Output since a moment

```sh
orca workload logs example --since 10m
orca workload logs example --since 2026-08-25T12:00:00Z
```

A duration says how long ago, and an RFC 3339 time names the moment itself. Use the
second when correlating a workload's output with another record.

This applies to container workloads. The Docker daemon holds a timestamp for every line
it keeps, so it does the filtering itself. An exec workload's output is a plain file
with no timestamps in it, so orca ignores `--since` for one rather than filtering on
times it would have to invent.

### The attempt before this one

orca replaces a workload by stopping it and starting it again, so the output an
operator wants is often the attempt that has just gone. It keeps that attempt:

```sh
orca workload logs example --previous
```

This matters most for a workload that keeps restarting. The attempt running now has not
failed yet, so its output does not say why the workload is failing. The attempt before
it does.

One attempt is kept per instance, so a workload crashing in a loop does not fill the
disk. Reading the previous output of a workload that has only ever run once gives
nothing, because there is no earlier attempt.

What is kept is not counted as running. It has no bearing on the workload's state,
and a container being kept for its output is a corpse rather than another instance:

```sh
orca workload get example
```

It goes when the workload does. Deleting a workload removes what was kept along with
everything else, so nothing is left on the host afterwards.

For a container this is the stopped container itself, which `docker ps --all` shows with
orca's labels on it. For an exec workload it is a `previous.log` beside the
`output.log` the current attempt is writing.

## Watching what orca is doing

Start with the workload itself. A workload the server has tried and failed to start
reports why in `orca workload get`, as `lastError` with the time it was recorded. The
error stands until an attempt succeeds, so a workload failing on every pass carries a
recent timestamp where one that failed once an hour ago does not. It is held in
memory: a server restart clears it, and the next pass either fails again and restores
it or succeeds. One error is reported per workload, which for a workload running
several instances is the most recent failure among them.

At `info` the server is quiet unless something is wrong. Setting the level to `debug`
reports each decision a reconciliation pass makes. Turn it on when a workload is not
behaving the way its manifest says it should:

```toml
[logging]
level = "debug"
```

Every log line carries the workload it concerns, so filtering by name shows one
workload's history.

## Observability

The server describes itself on three endpoints, served from the same listener as
everything else and declared in the same OpenAPI document:

- `/health` answers as long as the process serves requests. It suits a supervisor
  deciding whether to restart the process.
- `/ready` reports whether the server can do its job: the database answers, and
  every configured driver answered the most recent attempt to observe it. A server
  whose Docker daemon has gone away is alive but not ready, and the two need
  different answers. A not-ready response is a 503 carrying the reasons.
- `/metrics` serves everything the server measures in the Prometheus text format,
  ready to scrape with no collector in between.

Driver answers on `/ready` are cached from the reconciler's own passes rather than
fetched per request, so polling costs nothing. The answer is at most one reconcile
interval plus the driver timeout old. Before the first pass completes, the server
reports not ready. Note that the exec driver reads local state and so almost
always answers — in practice the driver half of readiness is about the Docker
daemon.

### Scraping

Point a Prometheus at `/metrics`. A scrape target that names the server by address
always passes the host check. One that names it by hostname must have that
hostname in `hosts`, or every scrape fails with a 421. See
[Configuration](configuration.md#http).

The metrics to alert on first:

- `orca_reconcile_passes_total` stops increasing when the reconciler has stopped
  converging, which is exactly the failure a log does not surface.
- `orca_workloads{state="failed"}` counts workloads in the failed state, derived
  by the same rules `orca workload get` reports.
- `orca_ports_used` against `orca_ports_capacity` warns before an apply fails
  with no free port. Usage carries a `protocol` label, because the range holds as
  many UDP ports as TCP ones.

Alongside orca's own instruments, the scrape carries the standard OpenTelemetry
HTTP server metrics, with request counts and durations per route and status, and
the Go runtime's own metrics — goroutine count, memory use and garbage collection
timings — which is where a leak in orca itself shows.

### Traces and logs

Set `otlp-endpoint` under [telemetry](configuration.md#telemetry) to export traces
and logs over OTLP. Without it, both are inert and `/metrics` still works.

Each reconciliation pass is a trace: a root span for the pass, a span per workload
converged, a span per driver observation, a span per image pull, and a span per
database query. The Docker client's own requests parent underneath, so "why did
this pass take ninety seconds" reads down to the daemon call or the contended
write that cost the time. The server's log records travel the same pipeline with
trace correlation attached.

The scrape also carries the standard `db.client.*` connection pool metrics. The
wait time series is the one to watch: it is the time writes spend queueing on
SQLite's write lock.

## Host ports

orca allocates a host port for a container port that names none, from the range in the
configuration. A workload running several instances holds one allocation per instance.
An allocation is sticky to its instance: it survives restarts and specification
changes, so anything pointing at it keeps working.

The range covers each protocol separately, since TCP and UDP are unrelated address
spaces. A workload holding 20000/tcp leaves 20000/udp free for another.

A port orca chose is revised if the workload fails to start on it, since something
outside orca may hold it. A port a manifest pinned is never moved, because it was asked
for.
