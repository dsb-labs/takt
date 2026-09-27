# Troubleshooting

What the common failures look like, where to read why, and what to do about
each. The first place to look is always the same:

```sh
takt workload events <name>
takt workload get <name>
```

The events say what the reconciler observed and why it acted, and the workload's
state says where that left it. The server's own log, `journalctl -u takt` on a
packaged install, says what the server could not do at all.

## A workload never leaves `pending`

The reconciler has not managed to start it. The events say why. The usual
reasons:

- **`Pulling image ...` repeating.** The image is still being fetched, or the
  registry is refusing it. A private image needs a `docker login` by the user
  running the server, or `docker.config-file` pointing at a credential file. See
  [Configuration](configuration.md#docker).
- **`Waiting ... before restart`** with `failed to start workload: no such
  image`. The image reference is wrong, or `pull: never` was set and the image is
  not on the host.
- **`Waiting for ... to resolve`.** The workload reads another's address, or a
  secret or variable, that does not exist yet. It starts once the thing it names
  exists. See [Reaching another workload](manifest.md#reaching-another-workload).
- **`Holding instance ... until ... passes its health check`.** The workload
  reads another's address, and the instance it resolves to has not passed its
  health check yet. It starts on the pass after the check passes, or after
  `reconcile.readiness-wait`, whichever comes first. Look at the named workload:
  `takt workload get` reports its check. See
  [Reaching another workload](manifest.md#reaching-another-workload).

## `no host port available`

The range under `[workload]` is exhausted, or every port in it is held by
something outside takt: takt probes a port before it allocates it. Widen
`min-port` and `max-port`, or find what else is listening in the range. See
[Configuration](configuration.md#workload).

A pinned port fails differently. A port another workload holds is refused when
the manifest is applied, naming the holder, and so is a port a daemon on the host
already listens on.

## `host path allowed read-only`

The mount sits beneath a prefix `allow-host-paths` grants for reading only, and
does not say `readOnly: true`. Either add the field, or have the operator list the
tree without the `:ro` suffix. The message names the prefix that decided. An `exec`
workload cannot mount anything read-only, so none of its path mounts may sit beneath
a read-only prefix. At apply the manifest is refused and nothing is stored. At start
the refusal is in the workload's events, since the tree can change in between. See
[Configuration](configuration.md#workload).

## `kernel does not support confining exec workloads`

The server logs this at startup, and refuses every `exec` workload with it. The
host's kernel is older than 6.2, or Landlock is compiled out or disabled. There is
no reduced mode: an `exec` workload runs confined or not at all. Container
workloads are unaffected. See [Confinement](operating.md#confinement).

## `host does not delegate a cgroup subtree`

The server logs this at startup. An `exec` workload naming `resources` is refused
with it, and one naming none runs but reports no usage. The server is not inside
a cgroup it may create children in. Under systemd the unit needs `Delegate=yes`,
which the packaged unit sets. A server started from a shell needs one arranged for
it, which is what `scripts/delegated.sh` does for development. See
[Delegation](operating.md#delegation).

## `authentication required` from the CLI

The server has an `[auth]` block and the CLI is not sending a credential it
accepts. Run `takt auth login` for an OIDC issuer, or export a static token as
`TAKT_TOKEN`. A token that has expired reads the same way. See
[Access control](acl.md) and [Command line](cli.md).

## The web UI refuses to load, or a request is refused with `421` or `403`

A request naming a host that is not an address, `localhost`, or a name in
`http.hosts` is refused with `421`. Behind a reverse proxy, set `hosts` to the name
the proxy serves. A browser page on another origin is refused with `403`, which is
what stops a page elsewhere driving the API. See
[Exposure](operating.md#exposure).

## An instance keeps being replaced

`takt workload events` says which of two things is happening:

- **`Replacing instance ..., which failed its health check`.** Check the path
  and port the manifest names, and whether the workload answers on the address
  the server probes, which is the host port on `workload.bind`.
- **`Instance ... exited`** with a status. The process is ending on its own. Read
  its output with `takt workload logs <name> --previous`, which is the attempt
  before the one running now.

Restarts are paced: each consecutive failure doubles the wait, up to two minutes.
An instance that stays up for ten seconds clears it. See
[Reconciliation](reconciliation.md#pacing-and-giving-up).

## A deleted workload is still listed as `terminating`

The reconciler is still stopping its work. A container that ignores `SIGTERM`
takes the runtime's grace period to die. A workload that stays `terminating` past
that is one whose runtime cannot be reached: check the server's log for the
driver's error, and that the Docker daemon is running.

## An image is gone that was expected to stay

takt removes images that no workload names and no container uses, an hour after
they became unreferenced. An image built or pulled ahead of the manifest that
names it is such an image. The server's log names each removal at `info`. Apply
the manifest within the hour, run a container from the image so it is held, or
set `prune = false` under `[docker]`. See [Images](operating.md#images).

## The server will not start

The log names what stopped it. The ones that come up:

- **`failed to connect to docker`.** The socket is not reachable. On a packaged
  install the `takt` user needs to be in the `docker` group, which the install
  leaves to you. See [Installing](installing.md).
- **`failed to open database`** naming a newer schema version. The database was
  written by a newer takt than this one. Reinstall the newer version, or restore
  a backup taken by this one. See [Upgrading](upgrading.md).
- **A key file that is readable by others.** The secret keyring and a TLS key
  must be readable by the server's user alone, and the server refuses to start
  otherwise rather than narrow the mode itself.

## Something else

Read the events, then the log at `debug`:

```toml
[logging]
level = "debug"
```

The debug log says what every pass observed and decided. If it does not explain
what you are seeing, open an issue with the events and the relevant log lines.
