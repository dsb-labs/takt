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
container's host port is published on the address `workload.bind` names, which starts
as loopback:

```toml
[workload]
bind = "0.0.0.0"
```

Set that when something on the network has to reach a workload. Anything a workload
serves is then reachable wherever the host is, so it is worth knowing which workloads
that covers.

An `exec` workload is not covered either way. The process binds its own port, so what
it listens on is decided by the command rather than by orca.

## State on disk

Everything orca keeps lives under the data directory, `~/.local/share/orca` by
default:

```
state.db          the workloads that have been applied
state.db-wal      SQLite's write-ahead log
state.db-shm      SQLite's shared-memory index
secret.key        the key secrets are encrypted with
exec/state/       what orca started, one directory per exec workload
exec/workloads/   where exec workloads run, one directory each
volumes/          one directory per volume
mounts/files/     the secrets and variables workloads mount, one directory each
mounts/state/     what orca wrote for each of them
```

The database holds each workload's stored specification, environment included, so orca
creates the directory and its files readable only by the user running the server.

A secret's value is encrypted in the database rather than held in a specification, so
a backup of `state.db` alone does not disclose one. `secret.key` is what decrypts
them, and it needs a backup of its own: a value sealed under a key that is gone cannot
be recovered. Keeping the two apart is what makes the database safe to copy. See
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

## Exec workload directories

Each version of an `exec` workload gets a directory of its own, under two separate
trees:

```
exec/workloads/<id>/<version>/
  cwd/            the process's working directory
  output.log      the process's output, both streams combined
  previous.log    the output of the attempt this one replaced

exec/state/<id>/<version>/
  state.json      the process orca started, and how it ended
  retained        present when the record is kept only for its output
```

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

An `exec` workload runs as the same user as the server. File permissions therefore
stop it reaching nothing that user can reach, which includes `secret.key`, the
database, and every other workload's mounted plaintext. Running workloads as a
separate user would need privileges orca deliberately does not ask for.

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

What it does not cover is anything not reached through a filesystem path. Signal
scoping arrives in a later Landlock version than orca requires, so a confined workload
can still send a signal to the server. The network is not restricted either.

There is deliberately no grant for `/tmp`. It is shared by every process running as
the same user, so granting it would let one workload read what another wrote there. A
workload needing scratch space has its own working directory, and `TMPDIR` will point
a command at it.

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

### The attempt before this one

orca replaces a workload by stopping it and starting it again, so the output an
operator wants is often the attempt that has just gone. It keeps that attempt:

```sh
orca workload logs example --previous
```

This matters most for a workload that keeps restarting. The attempt running now has not
failed yet, so its output does not say why the workload is failing. The attempt before
it does.

One attempt is kept per workload, so a workload crashing in a loop does not fill the
disk. Reading the previous output of a workload that has only ever run once gives
nothing, because there is no earlier attempt.

What is kept is not counted as running. It has no bearing on the workload's state, and a
container being kept for its output is not a second instance:

```sh
orca workload get example
```

It goes when the workload does. Deleting a workload removes what was kept along with
everything else, so nothing is left on the host afterwards.

For a container this is the stopped container itself, which `docker ps --all` shows with
orca's labels on it. For an exec workload it is a `previous.log` beside the
`output.log` the current attempt is writing.

## Watching what orca is doing

At `info` the server is quiet unless something is wrong. Setting the level to `debug`
reports each decision a reconciliation pass makes. Turn it on when a workload is not
behaving the way its manifest says it should:

```toml
[logging]
level = "debug"
```

Every log line carries the workload it concerns, so filtering by name shows one
workload's history.

## Host ports

orca allocates a host port for a container port that names none, from the range in the
configuration. An allocation is sticky: it survives restarts and specification changes,
so anything pointing at it keeps working.

A port orca chose is revised if the workload fails to start on it, since something
outside orca may hold it. A port a manifest pinned is never moved, because it was asked
for.
