# Upgrading takt

An upgrade replaces the server with a newer one. This page says what that does to
the database and to the workloads, and what going back takes. The short version: the
database is handled for you, the workloads keep running, and the way back is a
backup rather than a downgrade.

## The procedure

1. Stop the server.
2. Replace the binary with the new one. For a package install, install the new
   package — your `/etc/takt/config.toml` is kept.
3. Start the server.

Workloads keep running through all three steps. A container keeps running when the
server stops, and so does an exec process. The restarted server rediscovers both
rather than duplicating them. See
[Restarting the server](operating.md#restarting-the-server).

One caveat from that page applies here. An exec workload that ends while the server
is down leaves no exit code, and takt reports it as a failure. Under
`restart: on-failure` such a job runs again after the upgrade.

## The database

The server migrates its database at startup, before it serves a request. There is
nothing to run by hand. A newer binary opening an older database brings the schema
up to what it expects, and a binary opening a database it already matches changes
nothing.

## Workloads do not restart

**An upgrade does not restart your workloads unless the release notes say it does.**

The reason is in how takt decides to replace anything. A running instance is
replaced when the hash of its workload's resolved specification changes, and only
then. See [Replacing rather than mutating](design.md#replacing-rather-than-mutating).
An upgrade therefore redeploys the node only if the new binary computes different
hashes than the old one for the same specifications.

The design works to keep those hashes still. A feature that adds to what a hash
covers contributes nothing for the workloads not using it, so existing hashes do not
move when a release adds one. Golden tests over the hash encoding pin every known
shape of specification to a known hash, so an accidental change fails the build
before it ships.

A release that must move the hashes will say so in its release notes, and the first
reconciliation pass after the upgrade replaces every running instance once.

## Going back

Downgrading is not supported. Replacing the binary with an older one is not the
reverse of an upgrade:

- An upgrade that added a migration leaves the schema at a version the older binary
  does not know. That binary refuses to start, with
  `failed to run migrations: no migration found for version N`.
- An upgrade that added no migration lets the older binary start, against state a
  newer version wrote. Nothing checks that reading it backwards is safe.

The migrations have down counterparts, but the server never runs them and no command
does. They exist for development.

The rollback mechanism is a backup. Take one before the upgrade:

```sh
takt admin backup /backups/pre-upgrade.zip
```

To go back: stop the server, restore the backup, and start the older binary. The
restore procedure, including moving the current database aside first, is in
[Restoring a node](operating.md#restoring-a-node). Volume data is not in the archive
and does not need to be — the rollback happens on the same host, so the volumes are
where they were.

What a rollback cannot return is time. Workloads applied or changed after the backup
was taken are not in it, and the first reconciliation pass converges the node to the
restored state.
