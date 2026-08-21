# Manifest reference

A manifest describes one workload. The client parses it and submits the result, so
YAML is an authoring convenience rather than part of the wire format.

An unknown key is an error rather than being ignored, so a mistyped field is reported
when you apply the file.

## Shape

```yaml
version: v1
name: example

labels:
  some-key: some-value

ports:
  - to: 80
    from: 8080

env:
  EXAMPLE: EXAMPLE

volumes:
  - name: example-data
    to: /var/lib/example

restart:
  policy: always

health:
  http: /healthz
  port: 80
  interval: 10s
  timeout: 2s
  retries: 3
  startPeriod: 30s

container:
  image: nginx:1.27-alpine
  command: ["nginx", "-g", "daemon off;"]
```

| Field | Required | Description |
|---|---|---|
| `version` | yes | The schema version. Must be `v1`. |
| `name` | yes | Identifies the workload. Lowercase alphanumeric and dashes, up to 63 characters. |
| `labels` | no | Arbitrary key-value pairs. |
| `ports` | no | The ports the workload publishes. |
| `env` | no | Environment variables set for the workload. |
| `volumes` | no | The volumes the workload mounts, and where it finds each one. |
| `restart` | no | What happens when the workload ends. |
| `schedule` | no | When the workload runs, rather than running continuously. Not shown above, since a scheduled workload cannot declare a health check. |
| `health` | no | How orca decides the workload is working. |
| `container` | one of | Run the workload as a Docker container. |
| `exec` | one of | Run the workload as a command on the host. |

The name is the workload's identity. Applying the same name again updates that
workload rather than creating a second one.

## Runtimes

A workload names exactly one runtime block. Which block it names selects the driver
that runs it, so there is no separate field saying which to use.

### container

```yaml
container:
  image: nginx:1.27-alpine
  command: ["nginx", "-g", "daemon off;"]
```

| Field | Required | Description |
|---|---|---|
| `image` | yes | The image reference to run. Pulled when it is not present locally. |
| `command` | no | Replaces the command the image declares. |

`command` is the command and its arguments rather than a string. Nothing has to decide
where to split it, and no shell is involved unless the command names one. Leaving it
out runs what the image declares.

### exec

```yaml
exec:
  command: ["/usr/local/bin/backup", "--target", "/data"]
```

| Field | Required | Description |
|---|---|---|
| `command` | yes | The command to run, and its arguments. |

The command runs on the host rather than in a container. orca creates a directory for
each version of the workload and runs the command inside it, capturing its output
there. See [Operating orca](operating.md) for where those files live.

An exec workload starts with only the environment `env` names, plus a `PATH`. It does
not inherit the server's environment.

## Ports

```yaml
ports:
  - to: 80          # the port the workload listens on
  - to: 443
    from: 8443      # the host port that reaches it
```

`to` is the port the workload listens on. `from` is the host port that reaches it.

For a container, leaving `from` out is the usual case. orca allocates a host port and
reports it back, so you never have to invent unique numbers by hand.

```sh
orca workload get example | jq '.Ports'
[ { "To": 80, "From": 20000, "Dynamic": true } ]
```

An allocated port is sticky. It stays the same across restarts and image changes, so
anything pointing at it keeps working. Pin `from` when something outside orca has to
know the address up front. Pinning a port another workload holds is rejected when you
apply the manifest.

An exec workload must name `from`. The process binds a port on the host directly, so
there is no mapping to make. orca records the port to stop another workload taking it,
and allocates nothing.

Ports sit beside the runtime blocks rather than inside one, because reaching a
workload is a question about the workload. A runtime that publishes nothing rejects
them rather than ignoring them.

## Environment

```yaml
env:
  DATABASE_URL: postgres://localhost/example
```

A workload starts with only what `env` names. It does not inherit the server's
environment, which may hold credentials the workload has no business reading.

## Volumes

```yaml
volumes:
  - name: example-data
    to: /var/lib/example
```

`name` is the volume to mount. `to` is where the workload finds it.

A volume has to exist before a workload can mount it. Applying a manifest naming one
that does not is rejected, so a mistyped name is reported rather than quietly becoming
a second empty volume:

```sh
orca volume create volume.yaml
orca workload apply example.yaml
```

The volume manifest is a name and nothing else, since a volume holds data and has
nothing to configure:

```yaml
version: v1
name: example-data
```

A volume outlives the workloads that mount it. Deleting a workload leaves its volumes
alone, and `orca volume delete` is the only thing in orca that removes stored data. See
[Command line](cli.md) for those commands.

`to` is written the same way whichever runtime runs the workload, so a workload moved
between them keeps its manifest. It must be an absolute path, and not `/`.

Where it resolves to differs, because a container has a filesystem of its own and a
process on the host does not.

For a container, the volume is mounted at `to` and the workload uses that path as
written.

For an exec workload, the volume is placed at `to` inside the directory the process
runs in. **Such a workload reaches it by the relative path**, so `to: /var/lib/example`
is read and written as `var/lib/example`:

```yaml
volumes:
  - name: example-data
    to: /var/lib/example

exec:
  command: ["/usr/local/bin/backup", "--target", "var/lib/example"]
```

Resolving the absolute path there would mean confining the process to its own
directory, which needs privileges orca does not have. An absolute path in a command
therefore reaches the host's own root, wherever the volume was mounted.

Two mounts cannot name the same volume, or the same path. A trailing slash makes no
difference, so `/data` and `/data/` are the same mount.

Volumes sit beside the runtime blocks rather than inside one, for the same reason ports
do: where a workload keeps its data is a question about the workload.

## Restart

```yaml
restart:
  policy: on-failure
  attempts: 5
  delay: 10s
```

| Field | Required | Default | Description |
|---|---|---|---|
| `policy` | no | `always` | Whether to run the workload again. |
| `attempts` | no | unlimited | Consecutive restarts before orca gives up. |
| `delay` | no | `1s` | How long to wait before the first restart. |

| Policy | Behaviour |
|---|---|
| `always` | Restart whatever the exit code. |
| `on-failure` | Restart only after a non-zero exit. |
| `never` | Never restart. |

`always` is what a long-running service wants. `on-failure` is what a one-off job
wants: a job that exits cleanly has finished its work.

A workload orca will not restart reads as `completed` when it exited cleanly and
`failed` when it did not. The policy decides whether to run it again. The exit code
decides whether it worked, so a workload retired under `never` still reports that it
failed.

Changing the specification runs a completed workload again, because what already ran
is then out of date. Applying an unchanged manifest does nothing, so a repeated apply
does not run a job twice. To run an unchanged job again, delete it and apply it.

`delay` is the first wait, and each consecutive failure doubles it up to a ceiling
orca sets. A workload that reaches `attempts` is left exactly as it ended, so its
outcome stays readable. Changing its specification starts it again.

## Health

```yaml
health:
  http: /healthz
  port: 80
  interval: 10s
  timeout: 2s
  retries: 3
  startPeriod: 30s
```

| Field | Required | Default | Description |
|---|---|---|---|
| `http` | one of | | The path to request. Any 2xx response passes. |
| `tcp` | one of | | Check that the port accepts a connection. |
| `port` | no | | Which published port to check. Needed when more than one is published. |
| `interval` | no | `10s` | How often to check. |
| `timeout` | no | `2s` | How long one check may take. |
| `retries` | no | `3` | Consecutive failures that mark the workload failed. |
| `startPeriod` | no | `30s` | Grace before failures are counted. |

Whether a workload is working is a different question from whether its runtime says
it started. A process that is listening and answering errors looks healthy to Docker.

orca performs the check itself, against the port it published, so an image carrying no
shell can still be checked. A workload that exhausts its retries is restarted on the
same paced schedule a crashed one takes.

`startPeriod` is the grace a workload gets first. Failures inside it do not count. A
workload slow to become ready is therefore not replaced for failing checks it was never
going to pass yet, and passing one check ends the grace early.

`timeout` must not exceed `interval`. A check that could outlast the gap between
checks would overlap itself, and the failure count would stop meaning consecutive
failures.

A health check needs a published port, whatever the runtime.

## Schedule

```yaml
schedule:
  cron: "*/5 * * * *"
  overlap: replace
```

| Field | Required | Default | Description |
|---|---|---|---|
| `cron` | yes | | A cron expression, in the standard five-field form. |
| `overlap` | no | `replace` | What to do when an occurrence is due and the previous run has not finished. |

A scheduled workload runs at the times its expression names and waits in between. It
is not started when it is applied: a schedule says when to run, and the moment of
applying is not one of those times.

The expression is read in the server's local time.

| Overlap | Behaviour |
|---|---|
| `replace` | Stop the running instance and start the occurrence. |
| `skip` | Leave the running instance alone and miss the occurrence. |

`replace` keeps the schedule honest, so a run that outlasts its interval never
finishes. `skip` is for a job that must not be interrupted. Either way the workload
runs one instance at a time.

The schedule outranks the restart policy. An occurrence coming due starts the workload
whatever the last run did, so the policy applies only between occurrences. There it
retries a run that failed.

A run that ended cleanly is not restarted. Starting it again would run the workload at
a time its schedule does not name.

Occurrences missed while the server was down are missed. The occurrence orca runs is
the first after the last run, so a workload down for several does not run once for each.

A scheduled workload cannot declare a health check. A check restarts a workload that
stops answering, and a scheduled workload is expected to end.

`orca workload get` reports when a scheduled workload next runs, once it has run at least once.
