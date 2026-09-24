# Moving from docker compose

A compose file describes several containers together. takt describes each
workload on its own and reconciles the host against the set of them, so a
compose file becomes one manifest per service, plus a volume manifest per named
volume. This page takes a typical compose file apart and says what each piece
becomes, and what has no equivalent.

## A worked example

```yaml
services:
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: hunter2
    volumes:
      - pgdata:/var/lib/postgresql/data
    restart: unless-stopped

  app:
    image: example/app:1.4.0
    ports:
      - "8080:8080"
    environment:
      DATABASE_URL: postgres://postgres:hunter2@db:5432/app
    depends_on:
      - db
    restart: unless-stopped

volumes:
  pgdata:
```

The same deployment in takt is three files and one secret.

The volume first, since a workload cannot mount one that does not exist:

```yaml
# pgdata.yaml
version: v1
name: pgdata
```

The password is a secret rather than a line in a file:

```sh
printf %s hunter2 | takt secret set db-password
```

The database:

```yaml
# db.yaml
version: v1
name: db
ports:
  - name: pg
    to: 5432
env:
  POSTGRES_PASSWORD: ${secret:db-password}
volumes:
  - name: pgdata
    to: /var/lib/postgresql/data
container:
  image: postgres:17-alpine
```

The application, which reaches the database by asking takt for its address:

```yaml
# app.yaml
version: v1
name: app
ports:
  - to: 8080
    from: 8080
env:
  DATABASE_URL: postgres://postgres:${secret:db-password}@${workload:db:pg}/app
container:
  image: example/app:1.4.0
```

```sh
takt volume apply pgdata.yaml
takt workload apply db.yaml
takt workload apply app.yaml
```

## What each piece becomes

| Compose | takt |
|---|---|
| `services.<name>` | One workload manifest, `name: <name>`. |
| `image` | `container.image`. See [container](manifest.md#container). |
| `command` | `container.command`, as a list. |
| `environment` | `env`. A value may read a secret, a variable, or another workload's address. See [Environment](manifest.md#environment). |
| `ports: "8080:80"` | `ports: [{to: 80, from: 8080}]`. Leave `from` out to let takt choose a host port. See [Ports](manifest.md#ports). |
| `volumes: name:/path` | A volume manifest for `name`, and `volumes: [{name, to: /path}]` in the workload. See [Volumes](volumes.md). |
| `volumes: /host/path:/path` | `volumes: [{path: /host/path, to: /path}]`, which the server has to allow. See [Mounting a host path](manifest.md#mounting-a-host-path). |
| `restart: unless-stopped` or `always` | The default. `restart.policy: on-failure` and `never` are the others. See [Restart](manifest.md#restart). |
| `healthcheck` | `health`, probed by takt from outside the container rather than by a command inside it. See [Health](manifest.md#health). |
| `deploy.resources.limits` | `resources`. See [Resources](manifest.md#resources). |
| `deploy.replicas` | `count`. See [Count](manifest.md#count). |
| `labels` | `labels`. |
| `cap_add`, `cap_drop`, `read_only`, `user`, `pid: host`, `network_mode: host` | The same fields under `container`. |
| `env_file`, `secrets` | `takt secret set` and `${secret:name}`, or a mounted secret. See [Secrets](secrets.md). |

## What has no equivalent

**`depends_on`.** takt has no start order. A workload that reads another's
address through `${workload:db:pg}` does not start until `db` has one, which
covers the case `depends_on` is usually for. A workload that needs another to be
*ready* rather than merely started should retry its connection, which it should do
anyway: takt replaces instances one at a time, and the address it was given stays
the same across a replacement.

**`networks`.** takt does not create Docker networks. A workload reaches another
through the host port takt published for it, which `${workload:name:port}`
resolves to. Two workloads that must share a network namespace can both name
`networkMode: host`.

**`build`.** takt runs images and does not build them. Build and push the image,
then name it. A tag that is rebuilt in place wants `pull: always`, which folds the
image's digest into the specification so a rebuild is applied like any other
change.

**`links` and container DNS.** Nothing resolves `db` inside `app`. Use the address
reference instead, which also survives the port moving.

**`profiles`, `extends`, `x-` anchors.** A manifest is one workload and has no
composition of its own. Generate manifests from whatever templating you already
use, or keep one file per workload.

**`docker compose up` as one operation.** Apply each manifest. The order only
matters for volumes, which must exist before a workload mounts them. Everything
else converges whatever order it arrives in. A [score](score.md) names the
manifests together and applies them in one command, in dependency order, which
is the closer match for a compose file.

## What you gain

- **Secrets that are not in a file.** A compose file with a password in it is a
  password on disk in plain text. takt stores the value encrypted and hands it to
  the workload as it starts. Rotating it replaces the workloads that read it,
  with no file to edit.
- **Addresses that follow the workload.** `${workload:db:pg}` is re-resolved when
  the port moves, and the readers are redeployed.
- **A record of what happened.** `takt workload events` says why an instance was
  replaced or restarted, which `docker compose ps` does not.
- **Processes as well as containers.** An `exec:` workload runs a command on the
  host under the same manifest, confined by the kernel. See
  [exec](manifest.md#exec).
