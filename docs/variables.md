# Variables

A variable is a value both a workload and an operator can read. It is stored as given,
referenced from a manifest by name, and substituted into a workload's environment as
it starts.

```sh
orca variable set db-host db.internal
```

```yaml
env:
  DSN: postgres://app@${var:db-host}:5432/app
```

The reference syntax is documented in the [manifest reference](manifest.md#reading-a-variable).

## Variable or secret

The two work the same way. The difference is whether the value is readable back:

| | Variable | Secret |
|---|---|---|
| Stored | as given | encrypted |
| Returned by the API | yes | never |
| Reported by `list` | with its value | name and revision only |
| Set from an argument | yes | no |
| Changing it redeploys readers | yes | yes |

Use a variable for a hostname, a port, a log level, a feature flag, a region — a value
you would want to read back when working out how something is configured. Use a
[secret](secrets.md) for a password, a token, or a key.

**A variable is readable by anything that can reach the API.** Nothing about it is
protected, so a value that would be damaging to report does not belong in one. See
[Exposure](operating.md#exposure).

## What orca guarantees

- **The value reaches the workload.** Resolution happens as the workload starts, so a
  workload always starts against what orca holds now.
- **The reference is what is stored.** A workload's stored specification holds the
  reference text, not the resolved value, so changing a variable reaches the workloads
  reading it rather than only the ones applied afterwards.
- **Changing a value redeploys the workloads reading it.** A change is not something
  you have to remember to follow with an apply.

## Setting a value

The value is an argument:

```sh
orca variable set log-level debug
```

This is where a variable parts company with a secret, which has no such flag. Arguments
are visible to anything that can list processes on the host and they land in shell
history — exactly what a secret has to avoid, and what a variable has no reason to.

A file or standard input works for a value too long or too awkward to type:

```sh
orca variable set motd --from-file ./motd.txt
printf %s debug | orca variable set log-level
```

A value read either way is taken exactly as given, including a trailing newline.
`printf %s` rather than `echo` is what keeps one out. Giving both an argument and
`--from-file` is refused rather than one silently winning.

An empty value is a value. A workload reading it gets an empty environment variable
rather than none.

## Changing a value

Setting a variable to a new value moves the specification hash of every workload
reading it. The reconciler then replaces their instances, and the new value reaches
each one as it starts.

```sh
orca variable set log-level info
orca workload get example | jq '.Version'
```

Setting a variable to the value it already holds does nothing, so nothing is
redeployed. A configuration management tool that sets every variable on every run
therefore does not restart the fleet each time.

What reaches the hash is the value itself, where a secret contributes a revision
instead. A hash over a secret's value would let a guess at it be tested, and the hash
is reported by the API. A variable's value is reported anyway, so there is nothing for
the indirection to protect. It also means deleting a variable and creating it again
with the same value correctly leaves the workloads reading it alone.

## Deleting

A variable a workload reads is refused, and the message names the workloads:

```sh
orca variable delete log-level
# Error: failed to delete variable: variable is in use: read by example
```

`--force` removes it anyway. Those workloads keep running, because nothing stops a
running process to take something away from it. They fail to start once something
replaces them, and the failure names the variable:

```
failed to resolve environment for workload: LEVEL reads unknown variable: log-level
```

Creating the variable again recovers them without any further action.
