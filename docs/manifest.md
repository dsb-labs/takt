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

resources:
  memory: 512m
  cpu: 0.5
  pids: 100

container:
  image: nginx:1.27-alpine
  command: ["nginx", "-g", "daemon off;"]
```

| Field | Required | Description |
|---|---|---|
| `version` | yes | The schema version. Must be `v1`. |
| `name` | yes | Identifies the workload. Lowercase alphanumeric and dashes, up to 63 characters. |
| `count` | no | How many instances to run. One when omitted. See [Count](#count). |
| `labels` | no | Key-value pairs attached to the workload. See [Labels](#labels). |
| `ports` | no | The ports the workload publishes. |
| `env` | no | Environment variables set for the workload. A value may reference a secret or a variable. |
| `volumes` | no | What the workload mounts — a volume, a secret or a variable — and where it finds each one. |
| `restart` | no | What happens when the workload ends. |
| `schedule` | no | When the workload runs, rather than running continuously. Not shown above, since a scheduled workload cannot declare a health check. |
| `health` | no | How orca decides the workload is working. |
| `resources` | no | The resource limits the workload runs under. |
| `container` | one of | Run the workload as a Docker container. |
| `exec` | one of | Run the workload as a command on the host. |

The name is the workload's identity. Applying the same name again updates that
workload rather than creating a second one.

To find out what applying a manifest would do without doing it, use
`orca workload apply --dry-run`. It reports whether the workload would be created,
whether its running instances would be replaced, and everything it names that does
not exist. See [Command line](cli.md#workload-apply---dry-run).

## Labels

Labels are how a workload is found: `orca workload list -q '$.labels.app=web'`
matches against them.

A key is lowercase alphanumeric, optionally separated by dots, dashes, underscores
or slashes, up to 63 characters — so a key like `app.example.com/name` works as
written.
Keys starting with `orca.` are refused. The server writes its own labels under that
prefix, and refusing yours is better than silently overwriting it.

A value is free text without control characters, up to 256 bytes. An empty value
is allowed. Up to 32 labels may be carried.

Volumes, secrets and variables carry labels under the same rules, so there is one
answer to what a label may be. A volume carries its labels in its manifest. A secret
and a variable take theirs from `--label`, since neither is described by a manifest.

**A label is as readable as the thing that carries it.** That matters most for a
secret, whose value is deliberately unreadable: a label on a secret is as public as
its name, and is no place to put a credential.

## Count

```yaml
count: 3
```

How many instances of the workload to run. One when omitted. Each instance is
converged on its own: it is started, health-checked, restarted and replaced
independently, so one instance crashing does not touch the others. While one is
down and a sibling still runs, the workload reports the `degraded` state. A
specification change rolls across the instances one reconcile pass at a time.

Each instance publishes the workload's ports on host ports of its own, which is why
a count above one cannot be combined with a pinned `from`: one host port cannot
reach more than one listener. An exec workload must pin every port it publishes, so
an exec workload with ports always runs one instance. A schedule cannot be combined
with a count either, because N copies of a cron job firing at once is almost never
what a schedule means.

**On one node, a count buys throughput rather than availability.** The host is the
failure domain, and a second instance on the same host does not survive it losing
power. Several copies of a single-threaded service across several cores is the case
this serves.

orca does not cap the count. The machine does: each instance costs a container or a
process, memory, and — for a workload publishing ports — a host port per port from
the configured range. A count the machine cannot serve fails at those limits rather
than at validation.

Instances share what the workload mounts. A volume is one directory on the host,
and every instance reads and writes the same one. That is correct for data that is
safe for concurrent writers and corrupting for anything that is not — a database
file, for one — and orca cannot tell which is which. Mount a volume into a workload
with a count only when its contents tolerate concurrent writers.

## Runtimes

A workload names exactly one runtime block. Which block it names selects the driver
that runs it, so there is no separate field saying which to use.

### container

```yaml
container:
  image: nginx:1.27-alpine
  pull: missing
  command: ["nginx", "-g", "daemon off;"]
  user: "65532:65532"
  readOnly: true
  capAdd: [NET_ADMIN]
  capDrop: [ALL]
```

| Field | Required | Description |
|---|---|---|
| `image` | yes | The image reference to run. |
| `pull` | no | When the image is pulled: `always`, `missing` or `never`. Defaults to `missing`. |
| `command` | no | Replaces the command the image declares. |
| `user` | no | The user to run as, replacing the one the image declares. |
| `readOnly` | no | Make the root filesystem read-only. |
| `capAdd` | no | Kernel capabilities to grant beyond the default set. |
| `capDrop` | no | Kernel capabilities to remove from the default set. |

`pull: missing` pulls the image only when it is not present on the host, which pins a
tag that is already there until something removes it. This rewards pinning a tag or a
digest in `image`, which is the deterministic way to run a container.

`pull: always` is for a tag that moves, such as `:latest`. It pulls on every start,
and the tag's digest is resolved from the registry and folded into the specification
hash — so a rebuilt tag reads as an ordinary specification change and the instance is
replaced. The digest is resolved when the server computes the hash: an apply, a
changed secret or variable, or a port reallocation. It is not watched continuously,
so re-applying the manifest is how a rebuilt tag is picked up on demand. One cost
follows from this. Each of those operations is a registry round-trip, and fails when
the registry is unreachable — including changing a secret that a `pull: always`
workload reads.

Pulls and digest lookups carry the credentials the host's docker credential file
holds for the image's registry, so a private image works wherever a `docker pull` on
the host would. See [Configuration](configuration.md#docker).

`pull: never` never pulls, and starting fails when the image is absent. It is for a
host whose images arrive some other way — built locally, or loaded from an archive.

`command` is the command and its arguments rather than a string. Nothing has to decide
where to split it, and no shell is involved unless the command names one. Leaving it
out runs what the image declares.

Every container is created with the `no-new-privileges` option set, so a process
inside cannot gain privileges through a setuid binary. There is no field to turn it
off, because it breaks essentially nothing that is not already doing something
suspect.

The other hardening fields are opt-in. `user` takes any form Docker accepts — a name,
a numeric identifier, or a `user:group` pair. Many stock images run as root unless
this says otherwise.

`capDrop: [ALL]` with `capAdd` naming what the workload actually needs is the hardened
configuration. It is not the default because it breaks too many stock images.

`readOnly` applies to the image's own filesystem. Mounted volumes and mounted values
are separate mounts with rules of their own, so a volume stays writable and a mounted
value stays readable whatever this says. An image that writes temporary files needs
them pointed at a volume before it can run read-only.

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
  - name: http      # what the rest of the manifest calls it
    to: 80          # the port the workload listens on
  - to: 443
    from: 8443      # the host port that reaches it
  - to: 51820
    protocol: udp   # tcp when left out
```

`to` is the port the workload listens on. `from` is the host port that reaches it.
`protocol` is `tcp` or `udp`, and defaults to `tcp`. `name` is optional and is what
the rest of the manifest refers to the port by.

For a container, leaving `from` out is the usual case. orca allocates a host port and
reports it back, so you never have to invent unique numbers by hand.

```sh
orca workload get example | jq '.Ports'
[ { "Name": "http", "To": 80, "From": 20000, "Protocol": "tcp", "Dynamic": true } ]
```

An allocated port is sticky. It stays the same across restarts and image changes, so
anything pointing at it keeps working. Pin `from` when something outside orca has to
know the address up front. Pinning a port another workload holds is rejected when you
apply the manifest.

A workload running more than one instance publishes each port once per instance, at
a host port of its own. `orca workload get` reports every mapping, with `Instance`
saying which instance a mapping reaches, and each instance's allocation is sticky on
its own.

A container's port is published on every interface unless the server is configured
otherwise, so anything the host is reachable at reaches the workload. See
[Operating orca](operating.md#workload-ports-are-published-separately).

### Naming a port

A workload publishing one port needs no name. One publishing several does, or the
things that select a port — a health check, and another workload reaching this one —
have to restate the number:

```yaml
ports:
  - name: http
    to: 8080
  - name: metrics
    to: 9090

health:
  http: /healthz
  port: http
```

A name follows the rules a workload name does: lowercase letters, digits and dashes.
It may not read as a number, because a port may also be selected by its number, and
`8080` would otherwise be two different ports written the same way.

Two ports may share a name only when they publish the same `to` on different
protocols. That is one service published over TCP and UDP, named once:

```yaml
ports:
  - name: dns
    to: 53
  - name: dns
    to: 53
    protocol: udp
```

Renaming a port does not move it. The host port a workload holds is kept, so a rename
is a change to what the manifest calls the port rather than a reason to redeploy the
workload at a new address.

An exec workload must name `from`. The process binds a port on the host directly, so
there is no mapping to make. orca records the port to stop another workload taking it,
and allocates nothing.

Ports sit beside the runtime blocks rather than inside one, because reaching a
workload is a question about the workload. A runtime that publishes nothing rejects
them rather than ignoring them.

### Publishing on both protocols

TCP and UDP are separate address spaces. 20000/tcp and 20000/udp are unrelated ports,
so a workload may publish the same number on both. DNS is the usual case.

```yaml
ports:
  - to: 53
    protocol: tcp
  - to: 53
    protocol: udp
```

When both host ports are allocated, orca gives them the same number, so the workload is
reached at one address whichever protocol a caller uses.

```sh
orca workload get dns | jq '.Ports'
[
  { "To": 53, "From": 20000, "Protocol": "tcp", "Dynamic": true },
  { "To": 53, "From": 20000, "Protocol": "udp", "Dynamic": true }
]
```

Pin the two separately if you need to. A pinned port is only checked against the
protocol it names, so one workload holding 5353/tcp does not stop another taking
5353/udp.

## Environment

```yaml
env:
  DATABASE_URL: postgres://localhost/example
```

A workload starts with only what `env` names. It does not inherit the server's
environment, which may hold credentials the workload has no business reading.

### Reading a secret

An `env` value can reference a secret rather than holding it:

```yaml
env:
  EXAMPLE: ${secret:secret-name}
  DSN: postgres://app:${secret:db-password}@localhost:5432/app
  LITERAL: $$notasecret
```

`${secret:name}` is replaced by the value of that secret as the workload starts.
`$$` is a literal dollar sign, which is how a value that has to contain one says so.
That is the whole syntax. It is not a template language, so there is nothing to
evaluate and no conditionals to write.

A reference may sit inside a longer string, as `DSN` shows, and the same secret may
be referenced from several variables.

Anything else after an unescaped `$` is an error rather than literal text:

| Value | Result |
| --- | --- |
| `${secret:db-password}` | The value of `db-password` |
| `$$notasecret` | The literal `$notasecret` |
| `$${secret:name}` | The literal `${secret:name}` |
| `${secret:db-password` | Rejected: not closed by `}` |
| `${workload:postgres:pg}` | The address `postgres` publishes `pg` at |
| `${env:HOME}` | Rejected: `env` is not a reference type |
| `${secret:db-password:pg}` | Rejected: only a workload reference names a port |
| `${secret:DB_PASSWORD}` | Rejected: not a name a secret may have |
| `$HOME` | Rejected: a bare `$` |

References work in `env` values only. A reference in an `env` *key* is not a
reference, and nowhere else in a manifest is scanned for one. Keeping the surface
this narrow means a secret cannot reach a workload's name, its labels, or its image.

The secret has to exist before a workload can read it. Applying a manifest naming one
that does not is rejected, and the message names the secret:

```sh
printf %s hunter2 | orca secret set db-password
orca workload apply example.yaml
```

What is stored is the reference, never the value. A workload's specification is
readable through the API, so a resolved value there would be readable too. See
[Secrets](secrets.md).

### Reading a variable

An `env` value can reference a variable the same way, with `${var:name}`:

```yaml
env:
  EXAMPLE: ${var:variable-name}
  DSN: postgres://app@${var:db-host}:5432/app
  LITERAL: $$notavariable
```

Everything above applies unchanged: the same escaping, the same rejections, the same
`env`-values-only scope, and the same requirement that the variable exists before a
workload can read it.

One value may hold both kinds:

```yaml
env:
  DSN: postgres://app:${secret:db-password}@${var:db-host}:5432/app
```

The kind is part of what is being asked for, so `${var:token}` and `${secret:token}`
name two different things and one is never substituted for the other.

The difference between the two is whether the value is readable back. A variable's is
returned by the API. A secret's is not. See [Variables](variables.md) and
[Secrets](secrets.md).

### Reaching another workload

An `env` value can reference the address of another workload, with
`${workload:name}` or `${workload:name:port}`:

```yaml
env:
  DSN: postgres://app:${secret:db-password}@${workload:postgres:pg}/app
  HOST: ${workload:postgres}
```

`${workload:name:port}` resolves to a host and a port, such as `10.0.0.5:20432`. The
port is selected the way a health check selects one: by the name it was given, or by
the port inside the workload. `${workload:name}` resolves to the host alone, for a
value whose port you already know and would otherwise write twice.

A workload running more than one instance publishes the port at several addresses,
and a reference still resolves to one of them. Which one is derived from the reading
workload's own name and instance, so a reader running several instances spreads them
evenly across the target's. Changing the target's count moves the arithmetic and the
readers are redeployed onto the new spread, rolling one instance per pass. A single
reader keeps sending everything to one instance — orca does not balance requests.

The address is not a URL. orca does not know what the workload speaks, so a bare
address composes into whatever you are writing.

This exists because a host port orca allocated is not something to write down. It is
reported rather than chosen, and it is revised if the workload fails to start on it.
A reference records the dependency instead, and every consumer follows the port
wherever it goes:

```yaml
version: v1
name: postgres
ports:
  - name: pg
    to: 5432
container:
  image: postgres:17-alpine
```

Apply that first. The workload has to exist, and has to publish at least one port,
before another can reference it — a manifest naming one that does not is rejected the
way a manifest naming an unknown secret is.

When the referenced port moves, the workloads reading it are redeployed and pick up
the new address as they start. That is the whole point of writing the reference rather
than the number: nothing has to be re-applied by hand.

A workload another one references cannot be deleted without `--force`. Forcing it
leaves those workloads unable to resolve the reference, which they report and retry
until something holds the name again. This is also what makes ordering unnecessary in
the other direction: a workload whose dependency has not started yet keeps retrying
rather than failing for good.

Two workloads may reference each other. Ports are allocated without consulting a
reference, so there is nothing circular to resolve.

A container reaching another workload depends on `workload.bind` naming an address a
container can dial. The default publishes on every interface, which one can. Loopback
is the exception, and only `exec` workloads reach each other there. See
[Configuration](configuration.md#workload).

## Volumes

```yaml
volumes:
  - name: example-data
    to: /var/lib/example
  - secret: secret-name
    to: /var/secret.json
  - var: variable-name
    to: /var/example.json
```

An entry names exactly one source, and which one it names decides what appears at the
path:

| Source | What appears at `to` |
|---|---|
| `name` | A volume, which is a directory that outlives the workload. |
| `secret` | A file holding the secret's value. |
| `var` | A file holding the variable's value. |

Naming none, or naming two, is an error. It is one list rather than two, because what
a workload finds in its filesystem is one question however the contents are produced.

`to` is where the workload finds it, and works the same way for all three. The rest of
this section is about mounting a volume. [Mounting a value](#mounting-a-value) covers
the other two.

`name` is the volume to mount. `to` is where the workload finds it.

A volume has to exist before a workload can mount it. Applying a manifest naming one
that does not is rejected, so a mistyped name is reported rather than quietly becoming
a second empty volume:

```sh
orca volume create volume.yaml
orca workload apply example.yaml
```

The volume manifest is a name, and labels if you want them. A volume holds data and
has nothing else to configure:

```yaml
version: v1
name: example-data
labels:
  app: web
  team: platform
```

Labels follow the rules in [Labels](#labels), unchanged. `orca volume update` replaces
them. Nothing mounting the volume is redeployed, because a label says nothing about
the storage.

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

## Mounting a value

A mount can name a secret or a variable instead of a volume. The workload then finds a
file holding that value:

```yaml
volumes:
  - secret: tls-cert
    to: /etc/tls/cert.pem
  - var: app-config
    to: /etc/app/config.json
```

This is the option for a value that is a file — a certificate, a key, a credentials
document, a configuration fragment. An `env` reference covers a value that fits in an
environment variable. Use whichever the program wants.

The file holds the value and nothing else. No trailing newline is added, so what a
workload reads is what `orca secret set` was given.

The value has to exist before a workload can mount it, exactly as a volume does.
Applying a manifest naming one that does not is rejected, and the message names it.

`to` resolves the same way it does for a volume, so an exec workload reaches the file by
the relative path. Two mounts cannot share a path. A secret and a variable may share a
*name*, and mounting both is two mounts rather than a duplicate.

**A mounted secret is written to the host filesystem.** There is no way to put a value
inside a container without writing it somewhere first. An `env` reference is the option
that writes nothing. See [Secrets](secrets.md#mounting-a-secret-as-a-file) for what that
exposes and for how long.

### When a mounted value changes

By default, changing a mounted value replaces the workload's instances, exactly as
changing a referenced secret does. That is what a program which reads its file once at
startup needs.

`signal` asks for the other behaviour:

```yaml
volumes:
  - secret: tls-cert
    to: /etc/tls/cert.pem
    signal: SIGHUP
```

orca then rewrites the file in place and sends the signal. The workload keeps running,
so a program that rereads its configuration keeps its connections and its uptime
through a rotation.

| Signal | |
|---|---|
| `SIGHUP` | What most programs reload on. |
| `SIGUSR1` | For a program that reloads on a user-defined signal. |
| `SIGUSR2` | The other user-defined signal. |

Anything else is rejected, including `SIGTERM` and `SIGKILL`. Whether a workload runs
is orca's decision to make through the [restart policy](#restart), so a manifest that
could stop one would be taking it.

Only a mounted secret or variable may name a signal. A volume holds whatever the
workload puts there, so there is no change orca could report.

The file is rewritten rather than replaced. A mount follows the file it was given, so a
replacement would leave the workload reading the old contents.

A value read from `env` as well as from a signalling mount still replaces the workload.
An environment variable is fixed once a process has started, so there is no way to
change one without a restart.

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

A workload that orca will not restart reads as `completed` when it exited cleanly and
as `failed` when it did not. The policy decides whether to run it again. The exit code
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
  port: http
  interval: 10s
  timeout: 2s
  retries: 3
  startPeriod: 30s
```

| Field | Required | Default | Description |
|---|---|---|---|
| `http` | one of | | The path to request. Any 2xx response passes. |
| `tcp` | one of | | Check that the port accepts a connection. |
| `port` | no | | Which published port to check, by name or by number. Needed when more than one is published. |
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

`port` takes either the name a port was given or its number, so `port: http` and
`port: 8080` select the same port of a workload publishing `http` on 8080. Naming it
is worth preferring: the number is restated in two places otherwise, and a manifest
that changes one and not the other still applies.

A health check needs a published TCP port, whatever the runtime. Both probes connect,
and a connection to a UDP port succeeds whatever is behind it, so a check against one
would report the workload as healthy however broken it is. A workload publishing only
UDP is rejected when you apply the manifest. One publishing both is checked on its TCP
side.

## Resources

```yaml
resources:
  memory: 512m
  cpu: 0.5
  pids: 100
```

| Field | Required | Description |
|---|---|---|
| `memory` | no | The most memory the workload may use, as a size such as `512m` or `1g`. |
| `cpu` | no | The most CPU the workload may use, in cores. Fractions are allowed. |
| `pids` | no | The most processes and threads the workload may create. |

A limit that is not named is not applied. There is no default ceiling: a limit orca
invented would be wrong for most workloads, and a workload killed by a limit nobody
set is worse than one that was never limited. Unset means unlimited.

`memory` is a hard cap, and it covers swap. A workload that reaches it is killed
rather than allowed to swap past it, so the limit means what it says. The restart
policy then treats the kill as any other failure.

Resources sit beside the runtime blocks because how much a workload may consume is a
question about the workload, and the limits mean the same thing on either runtime. A
container's are enforced by its own runtime. An exec workload's are enforced with a
cgroup of its own, which needs the host to delegate a cgroup subtree to orca —
running the server under systemd with `Delegate=yes` grants one, and
[operating](operating.md#delegation) describes it. A host without one refuses the
apply rather than accepting limits that would silently never apply.

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

`replace` keeps the schedule honest: a run that outlasts its interval is stopped so
that the next occurrence starts on time, which means a run that always outlasts it
never finishes. `skip` is for a job that must not be interrupted. Either way the workload
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

A scheduled workload cannot run more than one instance, because N copies of a cron
job firing at once is almost never what a schedule means.

`orca workload get` reports when a scheduled workload next runs, once it has run at least once.
