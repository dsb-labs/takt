# Volumes

A volume is a directory that outlives the workloads mounting it. It is created
before anything mounts it, holds whatever those workloads write, and keeps the
data through every replacement, restart and deletion until the volume itself is
deleted.

```sh
takt volume apply volume.yaml
takt workload apply example.yaml
```

```yaml
volumes:
  - name: example-data
    to: /var/lib/example
```

The mount entry's fields — where `to` resolves for each runtime, mounting
read-only, and the rest — are documented in the
[manifest reference](manifest.md#volumes). For a host directory takt does not
manage, such as a media library on its own mount point, mount a
[host path](manifest.md#mounting-a-host-path) instead.

## The volume manifest

The manifest is a name, labels if you want them, and optionally who owns the
directory backing the volume and what permission bits it carries:

```yaml
version: v1
name: example-data
labels:
  app: web
  team: platform
owner: "470:470"
mode: "0755"
```

A volume has to exist before a workload can mount it. Applying a workload
manifest naming one that does not is rejected, so a mistyped name is reported
rather than quietly becoming a second empty volume.

Applying the manifest again updates the volume rather than creating a second
one, and the stored fields become what the manifest says. Labels follow the
rules in [Labels](manifest.md#labels), unchanged. Nothing mounting the volume
is redeployed by a change to them, because a label says nothing about the
storage.

## Ownership

`owner` and `mode` exist for an image that runs as a fixed non-root user.
Without them the directory is owned by the user running the server and readable
only by it, so such an image cannot write to the volume it mounts.

`owner` is a numeric `uid` or `uid:gid` — a name would resolve against the
host's user database, so the same manifest would mean different users on
different hosts. `mode` is an octal string such as `"0755"`, up to four digits
so a shared volume can carry the setgid bit.

Both are applied to the directory when the volume is created, and again on
every apply, which is how a live volume is handed to another user.
Removing either from the manifest leaves the directory as it stands.

Assigning another user needs the server to carry `CAP_CHOWN`. The packaged unit
grants it. Grant it yourself when you run the server another way — under
systemd:

```ini
[Service]
User=takt
AmbientCapabilities=CAP_DAC_OVERRIDE CAP_CHOWN CAP_FOWNER
```

The grant does not reach the workloads. takt drops its ambient capabilities
before an exec workload's command runs, and a container's capabilities come
from the Docker daemon rather than from takt. A server without the grant
refuses only a volume manifest that names another user, and the error names the
capability. `CAP_FOWNER` is what lets an apply change the mode of a directory
the server assigned away, and its refusal names it the same way.

## Where the data lives

Each volume gets a directory under the server's
[data directory](operating.md#state-on-disk), named for the identifier takt
assigned it:

```
volumes/<id>/
```

Everything a workload writes to a mounted volume is in there. The directory is
created when the volume is created and removed only when the volume is deleted,
so it survives the workloads that mount it — including a workload being
replaced, restarted or deleted.

`takt volume list` reports where each volume's data is, which is what something
taking a backup needs:

```sh
takt volume list | jq -r '.[] | "\(.Name)\t\(.Path)"'
```

Nothing tells a workload where its volume is on the host. An exec workload told
that would know it sits inside takt's data directory, and could walk out of it.

A volume is bind-mounted into a container, so the Docker daemon has to share
this filesystem. Volumes do not work against a daemon reached over the network.

A [backup](operating.md#backups) covers the volume's record and not its
contents, which are yours to copy from the paths reported above. Restoring one
has a rule worth knowing — the data goes back under the identifier, not under a
new volume of the same name. See [Restoring a node](operating.md#restoring-a-node).

## Deleting

A volume outlives the workloads that mount it. Deleting a workload leaves its
volumes alone, and `takt volume delete` is the only thing in takt that removes
stored data.

A volume a workload mounts is refused, and the workloads holding it are named.
Pass `--force` to remove it anyway — see [volume delete](cli.md#volume-delete)
for the command's details.

### Deleting a volume a container wrote

A container runs as whatever user its image names, and the files it writes to a
volume belong to that user. The postgres image is the familiar case: it re-owns
its data directory and makes it readable only by its own user. The server's
user then cannot remove those files, and `takt volume delete` fails with a
permission error.

The `CAP_DAC_OVERRIDE` capability lets the server remove files whatever their
owner. The packaged unit grants it. Grant it yourself when you run the server
another way — under systemd:

```ini
[Service]
User=takt
AmbientCapabilities=CAP_DAC_OVERRIDE
```

The grant does not reach the workloads. takt drops its ambient capabilities
before an exec workload's command runs, so the command holds none of them. A
container's capabilities come from the Docker daemon rather than from takt.

A server without the grant runs everything, and only deleting a volume holding
another user's files needs it. The same ownership stops anything else running
as the server's user — a backup, for one — from reading those files, and the
capability changes nothing for them.
