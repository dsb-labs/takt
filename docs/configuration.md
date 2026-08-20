# Configuration

The server reads a TOML file, given as the argument to `orca serve`. Every value has a
default, so the server runs with no file at all and a file only has to describe what it
changes.

```toml
[http]
address = "127.0.0.1:7373"

[data]
directory = "~/.local/share/orca"

[docker]
host = ""

[reconcile]
interval = "10s"

[ports]
min = 20000
max = 32000

[logging]
level = "info"
```

## http

| Key | Default | Description |
|---|---|---|
| `address` | `127.0.0.1:7373` | The address the API listens on. |

Loopback by default. Reaching the API is enough to run code on the host, so read
[Operating orca](operating.md) before binding it to a network.

## data

| Key | Default | Description |
|---|---|---|
| `directory` | `~/.local/share/orca` | Where orca keeps its state. |

Holds the SQLite database and the directories the `exec` runtime gives each workload.
orca creates it readable only by the user running the server.

## docker

| Key | Default | Description |
|---|---|---|
| `host` | empty | The Docker daemon to talk to. |

Empty uses the environment, then the local socket. Set it to reach a daemon
elsewhere, such as `tcp://127.0.0.1:2375`.

## reconcile

| Key | Default | Description |
|---|---|---|
| `interval` | `10s` | How often a full reconciliation pass runs. |

The interval is a floor on convergence rather than the usual case. A driver reporting
a change triggers a pass at once, and so does an apply. Shortening this mostly affects
how quickly orca notices something it was never told about.

## ports

| Key | Default | Description |
|---|---|---|
| `min` | `20000` | The lowest host port orca will allocate. |
| `max` | `32000` | The highest host port orca will allocate. |

The range orca allocates from for a container port that names no host port. A port a
manifest pins is used as given, whether or not it falls in this range.

## logging

| Key | Default | Description |
|---|---|---|
| `level` | `info` | One of `debug`, `info`, `warn`, `error`. |

At `info` the server is quiet unless something is wrong. `debug` reports each
reconciliation decision, which is what to turn on when a workload is not behaving as
the manifest says it should.
