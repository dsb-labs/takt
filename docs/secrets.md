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
- An exec workload's environment is readable at `/proc/<pid>/environ`, by the user
  running it and by root.

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

Values are encrypted with AES-256-GCM. The key lives beside the database:

```
~/.local/share/orca/secret.key
```

It is generated on first start, 32 random bytes, readable only by the user running
the server. Point `secrets.key-file` somewhere else to keep it off the same disk as
the database. See [Configuration](configuration.md).

**Back the key up, and keep the backup separate from the database.** A value sealed
under a key that is gone cannot be recovered. A backup of the data directory holds
both, which makes it a complete copy and also a single thing worth protecting.

If the key is lost, the secrets are not recoverable. Set each one again, which moves
its revision and redeploys the workloads reading it.

The secret's name is part of what the encryption authenticates, so a value copied to
another row in the database fails to open rather than decrypting as whichever secret
now holds it.
