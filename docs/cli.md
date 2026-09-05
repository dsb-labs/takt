# Command line

```
orca serve [config-file]                Run the orca server

orca workload apply <manifest>          Create or update a workload from a manifest file
orca workload apply --dry-run <file>    Report what applying a manifest would do
orca workload list                      List workloads                       (alias: ls)
orca workload get <name>                Show a single workload
orca workload logs <name>               Read a workload's recent output
orca workload delete <name>             Delete a workload and stop its work  (alias: rm)
orca workload stop <name>               Stop a workload and hold it down
orca workload start <name>              Start a stopped workload
orca workload restart <name>            Replace a workload's running instances

orca volume create <manifest>           Create a volume from a manifest file
orca volume update <manifest>           Update a volume from a manifest file
orca volume list                        List volumes                         (alias: ls)
orca volume get <name>                  Show a single volume
orca volume delete <name>               Delete a volume and the data it holds (alias: rm)

orca service apply <manifest>           Create or update a service from a manifest file
orca service list                       List services                        (alias: ls)
orca service get <name>                 Show a single service
orca service delete <name>              Delete a service                     (alias: rm)

orca secret set <name>                  Set a secret's value
orca secret list                        List secrets                         (alias: ls)
orca secret get <name>                  Show a single secret
orca secret delete <name>               Delete a secret                      (alias: rm)

orca variable set <name> [value]        Set a variable's value
orca variable list                      List variables                       (alias: ls)
orca variable get <name>                Show a single variable
orca variable delete <name>             Delete a variable                    (alias: rm)

orca admin health                       Check that the server is alive
orca admin backup <destination>         Write a backup of the node to a file
orca admin restore <archive> [config]   Restore a node from a backup archive
orca admin rekey                        Re-encrypt every secret under a new key
```

Commands are grouped by what they act on, so a verb reads the same whichever noun
precedes it.

Every command takes `--address` (`-a`), the URL of the server, which defaults to
`http://localhost:7373`. Only `serve` ignores it, since it is the server.

`--ca-cert` names a PEM file holding the certificate authority the client checks
the server's certificate against, instead of the system roots. This is how you
talk to a server that terminates TLS with a self-signed certificate. See
[Operating orca](operating.md#serving-tls-directly).

Read commands print indented JSON, so they pipe into `jq`.

## serve

```sh
orca serve                  # every default
orca serve config.toml      # from a file
```

Runs the server in the foreground. The configuration file is optional and only has to
describe what it changes. See [Configuration](configuration.md).

## workload apply

```sh
orca workload apply example.yaml
```

| Flag | Description |
|---|---|
| `--dry-run` | Report what applying the manifest would do, and apply nothing. |

Parses the manifest, submits it, and prints the resulting workload.

Applying the same file twice is a no-op. A workload's version changes only when its
specification does, so a repeated apply never restarts healthy work.

Applying a workload that is still terminating is rejected rather than resurrecting it
half torn down.

### workload apply --dry-run

```sh
orca workload apply --dry-run example.yaml
```

Resolves the manifest exactly as an apply resolves it, prints what applying it would
do, and writes nothing.

```json
{
  "Spec": {
    "version": "v1",
    "name": "example",
    "ports": [
      { "to": 80, "from": 31729, "protocol": "tcp" }
    ],
    "restart": { "policy": "always", "delay": 1000000000 },
    "container": { "image": "nginx:1.28-alpine" }
  },
  "SpecHash": "d491e8d9d8b9859be95227e8c1484a9b457eccec59949a91b6f0d8ed293c4ce0",
  "Created": false,
  "Replaced": true,
  "Unknown": null,
  "Changed": ["$.container.image"]
}
```

Everything an apply refuses this refuses too, with the same error: a volume, secret,
variable or workload the manifest names and nothing holds, a pinned host port another
workload has, a workload being torn down, and a manifest that is not runnable. A dry
run that passes is therefore a statement about the apply rather than about the file.

`Replaced` is the field worth reading. A workload is replaced when its specification
hash moves, and the hash covers the resolved specification along with the revision of
every secret and the value of every variable the workload reads. An apply can
therefore replace a running instance because something outside the file moved, with
nothing in the manifest to say so.

`Changed` says what moved. The paths are into the specification the report carries,
which is the resolved one rather than the file, so a field orca defaulted is compared
as orca stored it. A field is named once, at the level the difference starts: a
container block that was added reads as `$.container` rather than as every field
inside it.

An empty `Changed` alongside `Replaced` is not a contradiction. The hash covers what
a workload reads as well as what it says, so an image rebuilt under the same tag
replaces an instance with the manifest untouched.

`Created` is true when nothing holds the name yet. Such a workload has nothing
running and nothing to differ from, so `Replaced` is false for it and `Changed` is
empty.

Nothing is allocated. A port mapping with no `from` needs a host port, and allocating
one would consume a port or move a workload's address while reporting that nothing
had changed. Such a port is printed without a `from`, and its path is listed in
`Unknown`:

```json
{
  "Spec": {
    "version": "v1",
    "name": "example",
    "labels": { "app": "web" },
    "ports": [
      { "to": 80, "from": 31729, "protocol": "tcp" },
      { "to": 443, "protocol": "tcp" }
    ],
    "restart": { "policy": "always", "delay": 1000000000 },
    "container": { "image": "nginx:1.28-alpine" }
  },
  "SpecHash": "",
  "Created": false,
  "Replaced": true,
  "Unknown": ["$.ports[1].from"],
  "Changed": ["$.container.image", "$.labels", "$.ports[1]"]
}
```

The paths in both lists are written in the syntax `orca workload list --query` uses.
A port the workload already holds is reported with the host port it holds, because
that is settled and needs no allocation, and it is never listed as changed: the
difference between a stored host port and one not yet allocated is orca's to settle
rather than something the operator wrote.

`SpecHash` is empty whenever `Unknown` is not. The host ports reach the hash, so one
computed before they are settled would be a hash the apply never stores. Such an
apply still replaces what is running, and says so.

## workload list

```sh
orca workload list
orca workload list -q '$.labels.app=web'
orca workload list -q '$.labels.app=web' -q '$.labels.env=prod'
orca workload list -q '$.container.image=nginx:1.27-alpine'
orca workload list -q '$.ports[0].to=80'
```

| Flag | Description |
|---|---|
| `--query`, `-q` | A `path=value` filter into the specification. Repeatable. |

Each query is a JSON path into the workload's specification and the value it must
hold. A workload has to match every query given, so adding one narrows the result.

Labels are part of the specification, so they need no special syntax. Values are
compared as text, which is why a number is matched by its digits. A boolean is stored
as `1` or `0` and has to be written that way.

Filtering happens in the database rather than in the client, so a query that matches
little does not cost a read of everything it discards.

## workload get

```sh
orca workload get example
```

Prints one workload: the specification that was submitted, the ports orca settled on,
and what the runtime reports about each instance.

A workload that names a schedule also reports when it next runs.

A workload that is failing to converge reports why and when, in `lastError` and
`lastErrorAt`. The error clears once an attempt succeeds, so a workload sitting
`pending` with an error is one the server has tried and failed to start — where one
without is merely slow.

## workload logs

```sh
orca workload logs example
orca workload logs example --tail 20
orca workload logs example --previous
orca workload logs example --follow
orca workload logs example --since 10m
```

| Flag | Default | Description |
|---|---|---|
| `--tail`, `-n` | `100` | Lines to read from the end of the logs. |
| `--previous`, `-p` | `false` | Read the instance that was replaced rather than the one running now. |
| `--follow`, `-f` | `false` | Keep reading output until the instance ends. |
| `--instance`, `-i` | every instance | The index of the instance to read, for a workload running more than one. |
| `--since` | empty | Read only the output written since a duration ago or an RFC 3339 time. |

Prints a workload's recent output. Both output streams are combined in the order they
were written.

`--previous` reads the attempt before the one running now, which is what a workload
that keeps restarting needs: the current attempt has not failed yet, so its output does
not say why the workload is failing. A workload that has only ever run once has no
earlier attempt, and the output is empty.

`--follow` keeps the command running and prints output as the workload produces it.
It ends when the instance ends, or when you press Ctrl-C. A replacement is a new
instance, so a workload that restarts while you watch needs the command again. This
cannot be combined with `--previous`, which reads an instance that has already ended.

`--instance` selects one instance's output by its index for a workload running more
than one. Without it every instance's output is returned. A follow of such a workload
requires it, because a follow reads one stream and interleaving several would return
output nothing could attribute.

`--since` takes either a duration such as `10m` or an RFC 3339 time such as
`2026-08-25T12:00:00Z`. It applies to container workloads, whose runtime holds a
timestamp for every line it keeps. It is ignored for exec workloads, whose output is a
plain file with no timestamps in it.

## workload delete

```sh
orca workload delete example
orca workload delete example --wait
orca workload delete postgres --force
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until the workload has finished terminating. |
| `--force`, `-f` | Delete the workload even though another references its address. |

Deleting is asynchronous. The workload reads as `terminating` while its work is
stopped, and disappears once nothing is left running for it. A teardown can therefore
be watched by polling `workload get` until the workload is gone.

A workload another one references is refused, and the error names the workloads
reading its address. `--force` deletes it anyway: those workloads are redeployed and
then report the reference they can no longer resolve, retrying until something holds
the name again. See [Reaching another workload](manifest.md#reaching-another-workload).

## workload stop

```sh
orca workload stop example
orca workload stop example --wait
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until nothing is running for the workload. |

Stops the workload and holds it down. The workload reads as `suspended`, its
instances are stopped, and nothing runs for it until `workload start` clears the
mark. Suspension survives a server restart, so a workload stopped before an upgrade
is still stopped afterwards.

The specification and its version are untouched, so a stop and a start resume the
workload rather than replacing it. Applying a new manifest while it is stopped is
allowed, and the change takes effect when it is started. Any values the workload
mounted are removed from the disk once nothing is running, exactly as a deletion
removes them.

## workload start

```sh
orca workload start example
orca workload start example --wait
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until the workload has left `pending`. |

Clears the suspension. The server starts the workload's instances on its next pass,
from whatever specification is stored. A scheduled workload waits for its next
occurrence rather than running the ones it missed while stopped. Starting a workload
that is not stopped changes nothing.

## workload restart

```sh
orca workload restart example
orca workload restart example --wait
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until a replacement instance has appeared. |

Replaces the workload's running instances with new ones started from the unchanged
specification, so the version does not move. A stopped workload is refused, since
nothing would start until it is started again.

The request is held in memory rather than stored. One the server has not acted on
yet is lost with the server, and can simply be sent again.

## volume create

```sh
orca volume create volume.yaml
```

Creates a volume and the directory backing it. The manifest is a name, labels if you
want them, and optionally who owns the directory and what permission bits it carries
— see [Volumes](manifest.md#volumes) for what `owner` and `mode` mean:

```yaml
version: v1
name: example-data
labels:
  app: web
  team: platform
owner: "470:470"
mode: "0755"
```

A volume has to exist before a workload can mount it, so that a mistyped name is
reported rather than becoming a second empty volume. Creating one that already exists
is refused, because a volume holds data and the caller may well have meant a name they
have not used yet.

## volume update

```sh
orca volume update volume.yaml
```

Replaces the labels, the owner and the mode on the volume the manifest names.

Those are the whole of what this changes, and the whole of what a volume has to
change. Its name identifies it, the directory holding its data is named for the
identifier it was assigned, and its contents are the workloads' to write.

The fields in the manifest replace the ones stored, the way applying a workload
manifest replaces a workload's. A manifest carrying no labels removes them all. The
owner and mode are applied to the directory again, which is how a live volume is
handed to another user. A manifest clearing either leaves the directory as it
stands.

Nothing mounting the volume is redeployed. None of these fields say anything about
the volume's place in a workload's specification, so no specification hash moves.

## volume list

```sh
orca volume list
orca volume list -q '$.labels.app=web'
orca volume list -q '$.labels.app=web' -q '$.labels.env=prod'
```

| Flag | Description |
|---|---|
| `--query`, `-q` | A `path=value` filter into the labels. Repeatable. |

Prints every volume: where its data is on the host, and which workloads mount it. A
volume nothing mounts is one that can be deleted without forcing.

Each query is a JSON path into the volume's labels and the value it must hold, with
the syntax `workload list` accepts. A volume has to match every query given. A query
can reach only the labels.

## volume get

```sh
orca volume get example-data
```

Prints one volume. `Path` is where its data is on the host running the server, which is
what something taking a backup needs.

## volume delete

```sh
orca volume delete example-data
orca volume delete example-data --force
```

| Flag | Description |
|---|---|
| `--force`, `-f` | Remove the volume even though a workload mounts it. |

This is the only thing in orca that removes stored data. Deleting a workload leaves its
volumes alone.

A volume a workload mounts is refused, and the workloads holding it are named:

```
$ orca volume delete example-data
Error: failed to delete volume: volume is in use: mounted by writer
```

`--force` removes it anyway. The workloads keep running with a mount that no longer
resolves, so this is for a volume whose workloads are known not to need it.

A workload being torn down still counts as holding a volume. Its work is still running
until the reconciler has stopped it, so the data it mounts is still in use.

## service apply

```sh
orca service apply service.yaml
```

Creates or updates a service from a manifest file. The stored selection becomes what
the manifest says, however many times it is applied. See [Services](services.md) for
the manifest and what a service selects.

The workloads the target selects do not have to exist. A service applied ahead of its
workloads reports no backends until they arrive.

## service list

```sh
orca service list
orca service list --query '$.labels.env=prod'
```

| Flag | Description |
|---|---|
| `--query`, `-q` | A `path=value` query into the service's labels. Repeatable. |

Each service reports its target and the backends it currently selects. A query
reaches only the service's own labels, not its target's.

## service get

```sh
orca service get web
```

Prints one service: its target, and the address of every selected instance that is
running, passing its check when the workload declares one, and not being torn down.

## service delete

```sh
orca service delete web
```

The workloads the service selected keep running. What stops is the service reporting
their addresses.

## secret set

```sh
orca secret set db-password --from-file ./password
printf %s hunter2 | orca secret set db-password
```

| Flag | Description |
|---|---|
| `--from-file`, `-f` | Read the value from this file rather than standard input. |
| `--label`, `-l` | A `key=value` label to attach. Repeatable. |

Stores a value, encrypted. There is deliberately no flag that takes the value:
arguments are visible to anything that can list processes on the host, and they land
in shell history.

The value is taken exactly as given, including a trailing newline. `printf %s` rather
than `echo` is what keeps one out of it.

**Labels replace rather than merge.** Setting a value without `--label` removes the
labels the secret had, the way applying a workload manifest without them does. There
is one desired state, and the request carries all of it.

Labelling a secret is not rotating it. The revision stays put, so nothing reading the
secret is replaced — the same reasoning that keeps `orca admin rekey` from redeploying
the fleet.

**A label is as readable as the secret's name.** The value is not, and a label is no
place to put one.

Setting a secret to the value it already holds does nothing, so a script that sets
every secret on every run does not restart the workloads reading them. A value that
did change replaces those workloads, and reaches them as they start.

## secret list

```sh
orca secret list
orca secret list -q '$.labels.app=web'
```

| Flag | Description |
|---|---|
| `--query`, `-q` | A `path=value` filter into the labels. Repeatable. |

Prints every secret: its revision, and which workloads read it. No value, here or
anywhere else. This is how you find out what exists in order to reference it from a
manifest.

Each query is a JSON path into the secret's labels and the value it must hold, with
the syntax `workload list` accepts. A secret has to match every query given. A query
can reach only the labels, so it cannot probe what a secret holds.

## secret get

```sh
orca secret get db-password
```

Prints one secret. `Revision` changes whenever the value changes, which is how a
rotation is confirmed without the value being shown.

## secret delete

```sh
orca secret delete db-password
orca secret delete db-password --force
```

| Flag | Description |
|---|---|
| `--force`, `-f` | Remove the secret even though a workload reads it. |

A secret a workload reads is refused, and the workloads reading it are named.

`--force` removes it anyway. Those workloads keep running, and fail to start once
something replaces them. Creating the secret again recovers them. See
[Secrets](secrets.md).

## variable set

```sh
orca variable set log-level debug
orca variable set motd --from-file ./motd.txt
printf %s debug | orca variable set log-level
```

| Flag | Description |
|---|---|
| `--from-file`, `-f` | Read the value from this file rather than the argument or standard input. |
| `--label`, `-l` | A `key=value` label to attach. Repeatable. |

Stores a value as given. The value may be an argument here, where a secret's may not:
arguments are visible to anything that can list processes and they land in shell
history, which a variable has no reason to avoid.

A value read from a file or from standard input is taken exactly as given, including
a trailing newline. Giving both an argument and `--from-file` is refused.

**Labels replace rather than merge.** Setting a value without `--label` removes the
labels the variable had. Labelling one replaces no workload: what redeploys a reader
is the value it reads.

Setting a variable to the value it already holds does nothing, so a script that sets
every variable on every run does not restart the workloads reading them. A value that
did change replaces those workloads, and reaches them as they start.

## variable list

```sh
orca variable list
orca variable list -q '$.labels.app=web'
```

| Flag | Description |
|---|---|
| `--query`, `-q` | A `path=value` filter into the labels. Repeatable. |

Prints every variable: its value, and which workloads read it. The values are shown,
unlike `secret list`, since reviewing what a fleet is configured with is the reason to
choose a variable.

Each query is a JSON path into the variable's labels and the value it must hold, with
the syntax `workload list` accepts. A variable has to match every query given. A
query can reach only the labels, not the values the list reports.

## variable get

```sh
orca variable get log-level
```

Prints one variable, including its value.

## variable delete

```sh
orca variable delete log-level
orca variable delete log-level --force
```

| Flag | Description |
|---|---|
| `--force`, `-f` | Remove the variable even though a workload reads it. |

A variable a workload reads is refused, and the workloads reading it are named.

`--force` removes it anyway. Those workloads keep running, and fail to start once
something replaces them. Creating the variable again recovers them. See
[Variables](variables.md).

## admin health

```sh
orca admin health
orca admin health --wait 30s
```

| Flag | Description |
|---|---|
| `--wait` | How long to wait for the server to become healthy, rather than asking once. |

Asks the server's health endpoint and exits zero when it answers. Nothing is
printed on success: the exit code is the signal, which is what a script
conditions on.

`--wait` asks once a second until the server answers or the duration runs out,
for the moment after starting a server when the next step needs it listening.

## admin backup

```sh
orca admin backup /backups/orca.zip
orca admin backup /backups/orca.zip --include-keys
```

| Flag | Description |
|---|---|
| `--include-keys` | Put the keyring in the archive. |

Asks the server for a consistent snapshot of its database and writes it to the
destination as a zip archive. The server keeps running throughout.

Copying `state.db` by hand does not do the same thing. The database runs in
write-ahead logging mode, so what is committed at any moment is spread across
`state.db`, `state.db-wal` and `state.db-shm`. A copy of the first alone is stale or
torn, and it fails quietly: SQLite opens the result and the missing transactions are
noticed later, if at all.

A destination that already exists is refused rather than replaced. The file is
written readable only by its owner, because it holds every workload's specification,
environment included.

The path and the size are printed as JSON. What the backup does not cover is printed
to standard error, so the two do not mix when the output is piped.

**Volume data is not in the archive.** Run `orca volume list` for each volume's path
on the host and back those up separately. orca has no business copying arbitrary user
data.

**The keyring is not in the archive** unless `--include-keys` is passed. A database
without its keys decrypts nothing, and that is what makes a copy of it safe to keep
somewhere a key would not be. An archive holding both is key material: it opens every
secret the node holds, and it keeps opening them long after it was taken. Setting
`secrets.keys` to a path you already back up is the better answer. See
[The encryption key](secrets.md#the-encryption-key).

Every key goes in, not only the one sealing secrets now. A key that `orca admin rekey`
replaced still opens the archives taken before it was replaced.

See [`admin restore`](#admin-restore) for putting one back, and
[Restoring a node](operating.md#restoring-a-node) for the whole procedure.

## admin restore

```sh
orca admin restore /backups/orca.zip /etc/orca/config.toml
```

Reads a backup archive back into the data directory a configuration file names.

**Run this with the server stopped.** Unlike everything else under `admin`, this
command talks to no server. It reads the same configuration file `orca serve` does,
works over the data directory directly, and refuses to run while anything is
listening on the configured address — a restore under a running server writes a
database out from under the connections reading it. The configuration file may be
left out, in which case the defaults apply, exactly as for `orca serve`.

It writes `state.db` into the data directory and the keyring into wherever
`secrets.keys` puts it, both readable only by their owner, and removes any stale
`state.db-wal` and `state.db-shm` first. SQLite replays a stale log against a
restored database perfectly happily, and the node then comes up holding state that is
quietly not what was backed up.

Nothing else in the data directory is touched. An archive holding an entry this
command has nowhere to put is refused in full, before anything is written.

A database already in the data directory is refused rather than replaced, as is a key
the keyring already answers to under the same identifier. Move a database aside
first, deliberately.

What was written is printed as JSON, along with what the restored node still needs:

| Field | Meaning |
|---|---|
| `Restored` | The files written. |
| `MissingKeys` | Keys the secrets are sealed under that the keyring does not hold. |
| `MissingVolumes` | Volumes whose data is not on this host, with the path it belongs at. |

**Volume data is not in a backup and is not restored here.** Copy each volume's
contents to the path `MissingVolumes` names. That path ends in the identifier the
volume was assigned, which is what it is found by: creating a volume of the same name
on the restored node gives it a fresh identifier, and the row and the data end up in
different directories. Nothing reads as an error — the workload starts and its
storage is empty.

A restore that reports something missing still succeeded, and exits zero. Copying
volume data after the database is a reasonable order to work in. See
[Restoring a node](operating.md#restoring-a-node).

## admin rekey

```sh
orca admin rekey
```

Generates a new encryption key, re-seals every secret under it, and starts using it.
The server keeps running throughout.

This is the way off the key a node was started with, which matters when that key leaks
and as ordinary hygiene. The alternative is setting every secret again, which needs
you to still hold every plaintext — the thing a secret store exists to avoid.

**No workload is redeployed.** A rekey changes how a value is stored, not what it is,
so no revision moves and no specification hash with it.

The key that was replaced is kept in the keyring, because it still opens the backups
taken before now. Take a fresh backup of the keyring afterwards: the copy you had
opens nothing the node holds. See
[Rotating the key](secrets.md#rotating-the-key).

## Workload states

`workload get` and `workload list` report a state derived from the workload's instances.

| State | Meaning |
|---|---|
| `pending` | Nothing is running yet. |
| `running` | The workload's instances are up. |
| `degraded` | At least one instance is up and at least one has failed. |
| `terminating` | The workload is being torn down. |
| `stopped` | An instance ended cleanly, and the restart policy will run it again. |
| `completed` | The workload ended, and its restart policy asks for nothing more. |
| `failed` | An instance exited non-zero, or a health check is failing. |
| `suspended` | The workload was stopped by an operator, and stays down until it is started again. |
