# Configuration

The server reads a TOML file, given as the argument to `takt serve`. Every value has a
default, so the server runs with no file at all and a file only has to describe what it
changes.

```toml
[http]
address = "127.0.0.1:7373"
hosts = []
tls-cert = ""
tls-key = ""

[data]
directory = "~/.local/share/takt"

[docker]
host = ""
config-file = ""

[reconcile]
interval = "10s"

[workload]
bind = "127.0.0.1"
min-port = 20000
max-port = 32000

[exec]
allow-paths = []

[secrets]
keys = ""

# Absent by default. Writing the block, even empty, enables authentication.
# [auth]

# [auth.oidc]
# issuer = ""
# client-id = ""
# client-secret = ""
# redirect-url = ""
# scopes = ["openid", "email", "profile"]

[telemetry]
otlp-endpoint = ""

[logging]
level = "info"
```

## http

| Key | Default | Description |
|---|---|---|
| `address` | `127.0.0.1:7373` | The address the API listens on. |
| `hosts` | empty | The host names a request may name. |
| `tls-cert` | empty | The PEM certificate the server presents when it serves TLS. |
| `tls-key` | empty | The PEM private key for `tls-cert`. |

The default is loopback. Reaching the API is enough to run code on the host, so read
[Operating takt](operating.md) before binding it to a network.

`hosts` names the host names takt accepts a request for. An address literal and
`localhost` are always accepted, so an operator reaching takt directly needs nothing
here. Set it to the name a reverse proxy in front of takt serves:

```toml
[http]
address = "127.0.0.1:7373"
hosts = ["takt.example.com"]
```

A request naming anything else is refused with `421`. That check is what stops a page
in the operator's browser from reaching a loopback-bound API — see
[Operating takt](operating.md#exposure).

Set `tls-cert` and `tls-key` together, or not at all, and give both as absolute
paths. With the pair set, the server terminates TLS itself instead of speaking
plain HTTP. The key file must be readable only by the user running the server,
which is the same rule the secret keyring applies. The pair is reread when the
certificate file changes, so a renewal does not need a restart. See
[Operating takt](operating.md#serving-tls-directly) for when to prefer this over
a reverse proxy.

A request body is read up to 1 MiB and no further. There is no key for it: a manifest
is a document an operator wrote by hand, and anything past a megabyte is a mistake or
an attempt to see how much the server will hold in memory. A body over the limit is
refused with `400` and `request body too large`.

## data

| Key | Default | Description |
|---|---|---|
| `directory` | `~/.local/share/takt` | Where takt keeps its state. |

Holds the SQLite database and the directories the `exec` runtime gives each workload.
takt creates it readable only by the user running the server.

## docker

| Key | Default | Description |
|---|---|---|
| `host` | empty | The Docker daemon to talk to. |
| `config-file` | empty | The docker credential file registry credentials come from. |

Empty `host` uses the environment, then the local socket. Set it to reach a daemon
elsewhere, such as `tcp://127.0.0.1:2375`.

`config-file` names the `config.json` that `docker login` writes. When a workload's
image is pulled, or its digest resolved for `pull: always`, the driver reads the
file and sends the credentials it holds for the image's registry. Empty reads
docker's own default location — `~/.docker/config.json`, or wherever
`DOCKER_CONFIG` points — so a `docker login` by the user running the server just
works. Set it when that default holds nothing, notably when takt itself runs in a
container:

```toml
[docker]
config-file = "/etc/takt/docker-config.json"
```

The path must be absolute. The file is read when a pull happens rather than at
startup, so a `docker login` on the host takes effect without restarting takt.
Credential helpers named by the file — a `credsStore` or `credHelpers` entry —
are run, so logins kept in the OS keychain work, provided the helper is on the
server's `PATH`. An absent file means anonymous pulls, which is all a public
image needs.

## reconcile

| Key | Default | Description |
|---|---|---|
| `interval` | `10s` | How often a full reconciliation pass runs. |

The interval is a floor on convergence rather than the usual case. A driver reporting
a change triggers a pass at once, and so does an apply. Shortening this mostly affects
how quickly takt notices something it was never told about.

## workload

| Key | Default | Description |
|---|---|---|
| `bind` | `0.0.0.0` | The address a workload's host ports are published on. |
| `min-port` | `20000` | The lowest host port takt will allocate. |
| `max-port` | `32000` | The highest host port takt will allocate. |
| `allow-host-paths` | empty | The prefixes a path mount may sit beneath. |

`min-port` and `max-port` are the range takt allocates from for a container port that
names no host port. A port a manifest pins is used as given, whether or not it falls in
this range.

`bind` is which interfaces a workload can be reached on. Every interface by default,
unlike the API: a published port exists to be reached, and one of the things reaching
it is another workload on this host. Name an interface's address to restrict that:

```toml
[workload]
bind = "10.0.0.5"
```

That is the way to narrow exposure. A Tailscale or WireGuard address reaches only
what is on that network, which is the usual answer for a workload that should not be
on the LAN.

Loopback is the one value with a catch. A container dialling a port published on
loopback reaches its own loopback rather than the host, so a container cannot reach
another workload at all when `bind` is `127.0.0.1`. Only `exec` workloads reach each
other there. See [Reaching another workload](manifest.md#reaching-another-workload).

It must be an address rather than a name, and it cannot be left empty. A name would
have to be resolved, and what it resolved to could change under a workload already
published on it.

`0.0.0.0` publishes on every interface, but it is not itself an address anything can
dial. A workload
referencing another is given the address of the interface carrying the default route
instead, which is how anything on this host reaches the host.

This applies to a port takt publishes for a workload, which means a container. An
`exec` workload binds its own port, so what it listens on is the process's business and
this setting does not reach it.

`allow-host-paths` is what lets a manifest mount a host path — see
[Mounting a host path](manifest.md#mounting-a-host-path). A path mount is accepted
when its path is one of these prefixes or sits beneath one, and refused otherwise.
The default is empty, which refuses every path mount:

```toml
[workload]
allow-host-paths = ["/mnt/media", "/var/run/docker.sock"]
```

Each prefix must be absolute. Comparison respects path boundaries, so `/mnt/media`
does not cover `/mnt/media-cache`.

There is no manifest equivalent, for the reason the exec `allow-paths` list has none.
A host path reaches outside takt-managed state — the docker socket in particular is
control of the daemon — so which paths are opened is the operator's decision. The
list is checked when a manifest is applied. A workload already stored keeps its
mounts if the list later narrows, the way an exec workload keeps the paths it was
started with.

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
| `keys` | empty | The directory holding the keys secrets are encrypted with. |

Empty puts the keyring at `keys/` inside the data directory. A key is generated on
first start, 32 random bytes, readable only by the user running the server.

It is a directory rather than a single file because `takt admin rekey` writes a new
key before anything points at it. Each key is named by an identifier the database
records, so which one is current is a question the database answers. The keyring keeps the keys
it has replaced, since they still open the backups taken before the rekey.

Set this to keep the keyring off the same disk as the database:

```toml
[secrets]
keys = "/etc/takt/keys"
```

The keyring needs a backup, and the backup should not sit beside the database. A value
sealed under a key that is gone cannot be recovered, and anything that can read a key
can read every secret sealed under it. See
[The encryption key](secrets.md#the-encryption-key).

## auth

The block's presence is the setting: writing `[auth]` into the file, even with
nothing in it, enables authentication, and deleting the block disables it. An
absent block means the network-boundary model — anything that reaches the
listener holds the whole API, and the [exposure guidance](operating.md#exposure)
is the entire defence.

With the block present, every request must carry a credential, except the
health and readiness probes a supervisor needs. [Access control](acl.md) covers
the model and the lifecycle: `takt acl init`, tokens, the policy document and
the recovery path.

## auth.oidc

| Key | Default | Description |
|---|---|---|
| `issuer` | empty | The OIDC issuer logins verify identities against. Its presence enables OIDC. |
| `client-id` | empty | The client identifier registered with the issuer. Required with an issuer. |
| `client-secret` | empty | The client secret, for an issuer that treats takt as a confidential client. Empty means a public client using PKCE alone. |
| `redirect-url` | empty | The URL browsers reach this server by, such as `https://takt.example.com`. Its presence enables the web UI's login redirect. |
| `scopes` | `["openid", "email", "profile"]` | The scopes a login requests from the issuer. |

Without this block, static tokens are the only authentication. With it, `takt
auth login` and the web UI exchange an OIDC identity for a short-lived token.
The issuer must permit two redirect URIs: `http://127.0.0.1:8250/oidc/callback`
for the CLI's loopback flow, and `<redirect-url>/api/v1/auth/oidc/callback` for
the UI's, when `redirect-url` is set.

Add whatever scope the issuer needs before it includes the claim the policy's
`groupsClaim` reads — many issuers put group names behind a `groups` scope.

## telemetry

| Key | Default | Description |
|---|---|---|
| `otlp-endpoint` | empty | The OTLP endpoint traces and logs are exported to. |

Empty is the default, and exports nothing. Metrics need no configuration at all:
the server always collects them and serves them at `/api/v1/system/metrics`. See
[Operating](operating.md#observability).

Set this to a URL such as `http://collector.internal:4318` to export traces and
logs over OTLP/HTTP. The scheme decides whether the connection uses TLS. The value
names where to send the telemetry and nothing else — takt does not know or care
what consumes it.

Everything beyond the endpoint is read from the standard `OTEL_*` environment
variables the OpenTelemetry SDK honours. Use `OTEL_EXPORTER_OTLP_HEADERS` for
credentials, `OTEL_TRACES_SAMPLER` for sampling, and `OTEL_RESOURCE_ATTRIBUTES`
for extra resource attributes, rather than looking for keys here.

## logging

| Key | Default | Description |
|---|---|---|
| `level` | `info` | One of `debug`, `info`, `warn`, `error`. |

At `info` the server is quiet unless something is wrong. `debug` reports each
reconciliation decision, which is what to turn on when a workload is not behaving as
the manifest says it should.

The level applies to what reaches stderr. A log exporter configured under
[telemetry](#telemetry) receives every record regardless, so a quiet terminal does
not mean a thin trail at the collector.
