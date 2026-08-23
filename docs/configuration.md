# Configuration

The server reads a TOML file, given as the argument to `orca serve`. Every value has a
default, so the server runs with no file at all and a file only has to describe what it
changes.

```toml
[http]
address = "127.0.0.1:7373"
hosts = []

[data]
directory = "~/.local/share/orca"

[docker]
host = ""

[reconcile]
interval = "10s"

[workload]
bind = "127.0.0.1"
min-port = 20000
max-port = 32000

[exec]
allow-paths = []

[secrets]
key-file = ""

[logging]
level = "info"
```

## http

| Key | Default | Description |
|---|---|---|
| `address` | `127.0.0.1:7373` | The address the API listens on. |
| `hosts` | empty | The host names a request may name. |

Loopback by default. Reaching the API is enough to run code on the host, so read
[Operating orca](operating.md) before binding it to a network.

`hosts` names the host names orca accepts a request for. An address literal and
`localhost` are always accepted, so an operator reaching orca directly needs nothing
here. Set it to the name a reverse proxy in front of orca serves:

```toml
[http]
address = "127.0.0.1:7373"
hosts = ["orca.example.com"]
```

A request naming anything else is refused with `421`. That check is what stops a page
in the operator's browser from reaching a loopback-bound API — see
[Operating orca](operating.md#exposure).

A request body is read up to 1 MiB and no further. There is no key for it: a manifest
is a document an operator wrote by hand, and anything past a megabyte is a mistake or
an attempt to see how much the server will hold in memory. A body over the limit is
refused with `400` and `request body too large`.

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

## workload

| Key | Default | Description |
|---|---|---|
| `bind` | `127.0.0.1` | The address a workload's host ports are published on. |
| `min-port` | `20000` | The lowest host port orca will allocate. |
| `max-port` | `32000` | The highest host port orca will allocate. |

`min-port` and `max-port` are the range orca allocates from for a container port that
names no host port. A port a manifest pins is used as given, whether or not it falls in
this range.

`bind` is which interfaces a workload can be reached on. Loopback by default, for the
same reason the API listens there: publishing a port exposes whatever the workload
serves. Set it to `0.0.0.0` to publish on every interface:

```toml
[workload]
bind = "0.0.0.0"
```

It must be an address rather than a name, and it cannot be left empty. A name would
have to be resolved, and what it resolved to could change under a workload already
published on it.

This applies to a port orca publishes for a workload, which means a container. An
`exec` workload binds its own port, so what it listens on is the process's business and
this setting does not reach it.

## exec

| Key | Default | Description |
|---|---|---|
| `allow-paths` | empty | Extra paths every `exec` workload may read. |

Every `exec` workload is confined by the kernel. It reaches its own working directory,
the volumes and values it mounts, and the host's system directories — `/usr`, `/bin`,
`/etc` and the rest. Everything else is refused, including the data directory. See
[Confinement](operating.md#confinement).

That covers a command installed the ordinary way. It does not cover a runtime living
somewhere else: a language under a home directory, or a nix store. Name those here:

```toml
[exec]
allow-paths = ["/opt/jdk", "/nix/store"]
```

Each path is granted read-only, so this widens what a workload may read and never what
it may change. Each path must be absolute. A path that is not on the host is ignored,
so one list can cover several hosts.

There is no manifest equivalent, and that is deliberate. The API has no
authentication, so a workload able to name its own paths could grant itself the data
directory. Which paths are opened is the operator's decision.

## secrets

| Key | Default | Description |
|---|---|---|
| `key-file` | empty | The file holding the key secrets are encrypted with. |

Empty puts the key at `secret.key` inside the data directory. It is generated on first
start, 32 random bytes, readable only by the user running the server.

Set this to keep the key off the same disk as the database:

```toml
[secrets]
key-file = "/etc/orca/secret.key"
```

The file needs a backup, and the backup should not sit beside the database. A value
sealed under a key that is gone cannot be recovered, and anything that can read the
key can read every secret orca holds. See [Secrets](secrets.md#the-encryption-key).

## logging

| Key | Default | Description |
|---|---|---|
| `level` | `info` | One of `debug`, `info`, `warn`, `error`. |

At `info` the server is quiet unless something is wrong. `debug` reports each
reconciliation decision, which is what to turn on when a workload is not behaving as
the manifest says it should.
