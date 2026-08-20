# Command line

```
orca serve [config-file]        Run the orca server
orca apply <manifest>           Create or update a workload from a manifest file
orca list                       List workloads                        (alias: ls)
orca get <name>                 Show a single workload
orca logs <name>                Read a workload's recent output
orca delete <name>              Delete a workload and stop its work    (alias: rm)
```

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

## apply

```sh
orca apply example.yaml
```

Parses the manifest, submits it, and prints the resulting workload.

Applying the same file twice is a no-op. A workload's version changes only when its
specification does, so a repeated apply never restarts healthy work.

Applying a workload that is still terminating is rejected rather than resurrecting it
half torn down.

## list

```sh
orca list
orca list -q '$.labels.app=web'
orca list -q '$.labels.app=web' -q '$.labels.env=prod'
orca list -q '$.container.image=nginx:1.27-alpine'
orca list -q '$.ports[0].to=80'
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

## get

```sh
orca get example
```

Prints one workload: the specification that was submitted, the ports orca settled on,
and what the runtime reports about each instance.

## logs

```sh
orca logs example
orca logs example --tail 20
```

| Flag | Default | Description |
|---|---|---|
| `--tail`, `-n` | `100` | Lines to read from the end of the logs. |

Prints a workload's recent output. Both output streams are combined in the order they
were written.

## delete

```sh
orca delete example
orca delete example --wait
```

| Flag | Description |
|---|---|
| `--wait`, `-w` | Block until the workload has finished terminating. |

Deleting is asynchronous. The workload reads as `terminating` while its work is
stopped, and disappears once nothing is left running for it. A teardown can therefore
be watched by polling `get` until the workload is gone.

## Workload states

`get` and `list` report a state derived from the workload's instances.

| State | Meaning |
|---|---|
| `pending` | Nothing is running yet. |
| `running` | The workload's instances are up. |
| `terminating` | The workload is being torn down. |
| `stopped` | Nothing is running, and orca intends to fix that. |
| `completed` | The workload ended, and its restart policy asks for nothing more. |
| `failed` | An instance exited non-zero, or a health check is failing. |
