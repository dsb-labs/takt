# Command line

```
orca serve [config-file]                Run the orca server

orca workload apply <manifest>          Create or update a workload from a manifest file
orca workload list                      List workloads                       (alias: ls)
orca workload get <name>                Show a single workload
orca workload logs <name>               Read a workload's recent output
orca workload delete <name>             Delete a workload and stop its work  (alias: rm)

orca volume create <manifest>           Create a volume from a manifest file
orca volume list                        List volumes                         (alias: ls)
orca volume get <name>                  Show a single volume
orca volume delete <name>               Delete a volume and the data it holds (alias: rm)
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

## workload logs

```sh
orca workload logs example
orca workload logs example --tail 20
```

| Flag | Default | Description |
|---|---|---|
| `--tail`, `-n` | `100` | Lines to read from the end of the logs. |

Prints a workload's recent output. Both output streams are combined in the order they
were written.

## workload delete

```sh
orca workload delete example
orca workload delete example --wait
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until the workload has finished terminating. |

Deleting is asynchronous. The workload reads as `terminating` while its work is
stopped, and disappears once nothing is left running for it. A teardown can therefore
be watched by polling `workload get` until the workload is gone.

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
