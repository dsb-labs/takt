# Secrets

A secret is a value a workload can read and an operator cannot. It is stored encrypted,
referenced from a manifest by name, and decrypted only to be handed to a workload as
it starts.

```sh
printf %s hunter2 | orca secret set db-password
```

```yaml
env:
  DSN: postgres://app:${secret:db-password}@localhost:5432/app
```

The reference syntax is documented in the [manifest reference](manifest.md#reading-a-secret).

For a value that is not worth hiding — a hostname, a log level — use a
[variable](variables.md) instead. It works the same way, and its value is readable
back.

A workload that wants a file rather than an environment variable can mount the secret
instead. That writes the value to disk, which is covered in
[Mounting a secret as a file](#mounting-a-secret-as-a-file).

## What orca guarantees

- **The value is not readable back.** No endpoint returns one, so no command prints
  one. `orca secret get` reports the name, the revision and the workloads reading it.
- **The value is not in the database.** A workload's stored specification holds the
  reference text. The secret's own row holds the encrypted bytes.
- **Changing a value redeploys the workloads reading it.** A rotation is not
  something you have to remember to follow with an apply.

## What it does not

Once a workload is running, its environment is visible to anything that can inspect
the process:

- A container's environment is readable with `docker inspect`, and to anything else
  that can reach the Docker socket.
- An exec workload's environment is readable at `/proc/<pid>/environ`, by root and by
  the user running it. Another exec workload is not one of those readers: every exec
  workload is confined by the kernel, which refuses it that file. See
  [Confinement](operating.md#confinement).

Both are inherent to giving a process an environment. Any orchestrator that sets
environment variables has the same property. What orca guarantees is narrower and
still worth having: the value is not in the database, not in a backup of it, not in
the API, and not in a log.

Reaching orca's API is already enough to run code on the host, so an attacker who can
reach it can start a workload that reads any secret. Encryption at rest protects the
database file, not the API. See [Exposure](operating.md#exposure).

## Setting a value

The value is read from a file or from standard input:

```sh
orca secret set db-password --from-file ./password
printf %s hunter2 | orca secret set db-password
```

There is deliberately no `--value` flag. Arguments are visible to anything that can
list processes on the host, and they land in shell history, so a flag would undo the
feature for whoever used it.

The value is taken exactly as given. `printf %s` rather than `echo` is what keeps a
trailing newline out of it, and orca does not trim one: a credential that ends in
whitespace is not orca's to correct.

An empty value is a value. A workload reading it gets an empty variable rather than
none.

## Mounting a secret as a file

Some secrets are files. A certificate, a private key, a service-account document: a
program wants a path, not an environment variable. A mount gives it one:

```yaml
volumes:
  - secret: tls-cert
    to: /etc/tls/cert.pem
```

The syntax is documented in the
[manifest reference](manifest.md#mounting-a-value).

**This writes the plaintext to the host filesystem.** There is no way to put a value
inside a container without writing it somewhere first, so mounting a secret trades the
"not on disk" guarantee above for a file the workload can open. It is worth knowing
exactly what that costs:

- The file lives under the data directory, in `mounts/files/`, in directories readable
  only by the user running the server.
- The file itself is read-only and readable by any user that can reach it. A container
  runs as a user of its own, so a file only the server's user could read would be
  unreadable by the workload that mounted it. The directory above is what keeps
  everything else out.
- An exec workload is granted the files it mounts and no others, so one exec workload
  cannot read what another mounts even though both run as the same user. See
  [Confinement](operating.md#confinement).
- The file is written as the workload starts and removed once nothing is running for it.
  A `orca workload delete` takes it off the disk.
- A backup of the data directory includes it, in the clear. This is the one place a
  secret's value is not encrypted at rest.

An `env` reference remains the option that writes nothing to disk. Prefer it when the
program will take a value that way.

## Rotation

Setting a secret to a new value moves its revision, which moves the specification hash
of every workload reading it. The reconciler then replaces their instances, and the new
value reaches each one as it starts.

```sh
printf %s hunter3 | orca secret set db-password
orca workload get example | jq '.Version'
```

Setting a secret to the value it already holds does nothing. The revision stays put,
so nothing is redeployed. A configuration management tool that sets every secret on
every run therefore does not restart the fleet each time.

The revision is random rather than a counter, and says nothing about the value. It is
reported so that a rotation can be confirmed without the value being shown.

A mounted secret can ask to be signalled instead of replaced:

```yaml
volumes:
  - secret: tls-cert
    to: /etc/tls/cert.pem
    signal: SIGHUP
```

orca then rewrites the file and signals the workload, which keeps running. This is what
a server holding open connections wants from a certificate rotation. See
[When a mounted value changes](manifest.md#when-a-mounted-value-changes).

## Deleting

A secret a workload reads is refused, and the message names the workloads:

```sh
orca secret delete db-password
# Error: failed to delete secret: secret is in use: read by example
```

`--force` removes it anyway. Those workloads keep running, because nothing stops a
running process to take something away from it. They fail to start once something
replaces them, and the failure names the secret:

```
failed to resolve environment for workload: DSN reads unknown secret: db-password
```

Creating the secret again recovers them without any further action.

## The encryption key

Values are encrypted with AES-256-GCM. The keys live in a directory beside the
database:

```
~/.local/share/orca/keys/da879s0hpe2ten8re4u0.key
```

A key is generated on first start, 32 random bytes, readable only by the user running
the server. Point `secrets.keys` somewhere else to keep the keyring off the same disk
as the database. See [Configuration](configuration.md).

Each key is named by an identifier, and the database records which key sealed each
secret. That is what lets `orca admin rekey` write a new key before anything points
at it, so a rotation is never a moment where the database and the keyring disagree.

**Back the keyring up, and keep the backup separate from the database.** A value
sealed under a key that is gone cannot be recovered. A backup of the data directory
holds both, which makes it a complete copy and also a single thing worth protecting.

`orca admin backup` leaves the keyring out for this reason, so the archive it writes
is safe to keep where a key would not be. Point `secrets.keys` at somewhere your
existing backups already cover and there is nothing else to remember. See
[Backups](operating.md#backups).

If the keys are lost, the secrets are not recoverable. Set each one again, which
moves its revision and redeploys the workloads reading it.

### Rotating the key

```sh
orca admin rekey
```

The server generates a key, re-seals every secret under it, and starts using it. This
is the way off the key a node was started with, which matters when that key leaks —
in a backup, on an imaged disk, committed by accident — and as ordinary hygiene.

**No workload is redeployed.** A rekey changes how a value is stored, not what it is,
so no revision moves and no specification hash with it. A node that was running before
the rekey is running the same instances after it.

The rewrite is one transaction, and the new key reaches the keyring before anything
points at it. A rekey that is interrupted therefore leaves every secret under the old
key or every secret under the new one, with nothing to repair by hand.

The key that was replaced is kept. It still opens the backups taken before the rekey,
which is why `orca admin backup --include-keys` carries the whole keyring rather than
the current key alone.

**Back the keyring up afterwards.** The copy you had opens nothing the node now
holds.

The secret's name is part of what the encryption authenticates, so a value copied to
another row in the database fails to open rather than decrypting as whichever secret
now holds it.
