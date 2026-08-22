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

This costs nothing for an operator using the CLI or `curl`. It matters for whoever
runs orca on a workstation they also browse from.

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
exec/state/       what orca started, one directory per exec workload
exec/workloads/   where exec workloads run, one directory each
volumes/          one directory per volume
```

The database holds each workload's stored specification, environment included, so orca
creates the directory and its files readable only by the user running the server.

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

exec/state/<id>/<version>/
  state.json      the process orca started, and how it ended
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

`state.json` records the process identifier and the kernel's start time for that
process. Both have to match for orca to claim the workload is still running. A process
identifier alone is reused, so a record naming one that is alive may be describing
something else.

A workload's directories in both trees are removed when the workload is deleted, so its
output survives for as long as the workload does.

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
