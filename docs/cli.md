# Command line

```
orca serve [config-file]                Run the orca server

orca workload apply <manifest>          Create or update a workload from a manifest file
orca workload list                      List workloads                       (alias: ls)
orca workload get <name>                Show a single workload
orca workload logs <name>               Read a workload's recent output
orca workload delete <name>             Delete a workload and stop its work  (alias: rm)
orca workload stop <name>               Stop a workload and hold it down
orca workload start <name>              Start a stopped workload
orca workload restart <name>            Replace a workload's running instances

orca volume create <manifest>           Create a volume from a manifest file
orca volume list                        List volumes                         (alias: ls)
orca volume get <name>                  Show a single volume
orca volume delete <name>               Delete a volume and the data it holds (alias: rm)

orca secret set <name>                  Set a secret's value
orca secret list                        List secrets                         (alias: ls)
orca secret get <name>                  Show a single secret
orca secret delete <name>               Delete a secret                      (alias: rm)

orca variable set <name> [value]        Set a variable's value
orca variable list                      List variables                       (alias: ls)
orca variable get <name>                Show a single variable
orca variable delete <name>             Delete a variable                    (alias: rm)

orca admin backup <destination>         Write a backup of the node to a file
orca admin restore <archive> [config]   Put a node back from a backup archive
orca admin rekey                        Re-encrypt every secret under a new key
```

Commands are grouped by what they act on, so a verb reads the same whichever noun
precedes it.

Every command except `serve` takes `--address` (`-a`), the URL of the server, which
defaults to `http://localhost:7373`.

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

Parses the manifest, submits it, and prints the resulting workload.

Applying the same file twice is a no-op. A workload's version changes only when its
specification does, so a repeated apply never restarts healthy work.

Applying a workload that is still terminating is rejected rather than resurrecting it
half torn down.

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

Creates a volume and the directory backing it. The manifest is a name and nothing else:

```yaml
version: v1
name: example-data
```

A volume has to exist before a workload can mount it, so that a mistyped name is
reported rather than becoming a second empty volume. Creating one that already exists
is refused, because a volume holds data and the caller may well have meant a name they
have not used yet.

## volume list

```sh
orca volume list
```

Prints every volume: where its data is on the host, and which workloads mount it. A
volume nothing mounts is one that can be deleted without forcing.

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

## secret set

```sh
orca secret set db-password --from-file ./password
printf %s hunter2 | orca secret set db-password
```

| Flag | Description |
|---|---|
| `--from-file`, `-f` | Read the value from this file rather than standard input. |

Stores a value, encrypted. There is deliberately no flag that takes the value:
arguments are visible to anything that can list processes on the host, and they land
in shell history.

The value is taken exactly as given, including a trailing newline. `printf %s` rather
than `echo` is what keeps one out of it.

Setting a secret to the value it already holds does nothing, so a script that sets
every secret on every run does not restart the workloads reading them. A value that
did change replaces those workloads, and reaches them as they start.

## secret list

```sh
orca secret list
```

Prints every secret: its revision, and which workloads read it. No value, here or
anywhere else. This is how you find out what exists in order to reference it from a
manifest.

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

Stores a value as given. The value may be an argument here, where a secret's may not:
arguments are visible to anything that can list processes and they land in shell
history, which a variable has no reason to avoid.

A value read from a file or from standard input is taken exactly as given, including
a trailing newline. Giving both an argument and `--from-file` is refused.

Setting a variable to the value it already holds does nothing, so a script that sets
every variable on every run does not restart the workloads reading them. A value that
did change replaces those workloads, and reaches them as they start.

## variable list

```sh
orca variable list
```

Prints every variable: its value, and which workloads read it. The values are shown,
unlike `secret list`, since reviewing what a fleet is configured with is the reason to
choose a variable.

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
| `terminating` | The workload is being torn down. |
| `stopped` | Nothing is running, and orca intends to fix that. |
| `completed` | The workload ended, and its restart policy asks for nothing more. |
| `failed` | An instance exited non-zero, or a health check is failing. |
| `suspended` | The workload was stopped by an operator, and stays down until it is started again. |
