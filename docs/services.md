# Services

A service is a named selection of workload instances to balance requests across. It
selects workloads by their labels, and reports the address of every selected
instance that is fit to serve. An external load balancer reads those addresses and
spreads requests across them — takt stays off the data path and runs no proxy of
its own.

```yaml
version: v1
name: web
target:
  labels:
    app: web
  port: 8080
```

```sh
takt service apply service.yaml
takt service get web
```

```json
{
  "Name": "web",
  "Target": {
    "labels": { "app": "web" },
    "port": 8080,
    "protocol": "tcp"
  },
  "Backends": [
    { "Workload": "web", "Instance": 0, "Address": "10.0.0.5:20000" },
    { "Workload": "web", "Instance": 1, "Address": "10.0.0.5:20001" }
  ]
}
```

This is what a [count](manifest.md#count) is for. A workload running three instances
publishes its port at three addresses, and a reference from a single reader resolves
to one of them. A service reports all three, so something that balances per request
can reach every one.

## The target

The target says what the service selects:

```yaml
target:
  labels:
    app: web
    tier: backend
  port: 8080
  protocol: tcp
```

A workload is selected when it carries every label the target names. At least one
label is required, because a target selecting everything is more likely a mistake
than an intent. The labels are the workload's own — the same ones
`takt workload list --query` filters by — so one selector can span several
workloads.

The port is always a number: the port inside the workload, as a manifest's `ports`
entry writes its `to`. A port's name cannot be used here, because the selected
workloads need not agree on their names. The protocol defaults to `tcp`.

The workloads the target selects do not have to exist. A service is a question asked
of whatever is running, so one applied ahead of its workloads reports no backends
until they arrive.

## Backends

A selected instance is reported as a backend when all of these hold:

- It is observed running.
- It passes its health check, when the workload declares one. A workload with no
  check contributes its running instances as they are.
- Its workload is not being deleted. A workload being torn down keeps serving until
  the teardown reaches it, but a balancer told about its addresses would keep
  sending requests to addresses about to vanish.

A selected workload that does not publish the target port contributes nothing rather
than failing the service, since one selector may span workloads where only some
publish it.

Each backend names its workload, its instance index, and the address that reaches
it. The address joins the server's [workload address](configuration.md#workload)
with the host port allocated to that instance, so it is the same address a
`${workload:name:port}` reference resolves to.

Backends are computed when they are read. Nothing is stored, so a reallocated port,
a health change or a scale is reflected the next time the service is read.

## Feeding a balancer

`takt service get` reports every backend, so an external balancer is configured
from one read:

```sh
takt service get web | jq -r '.Backends[].Address'
```

The backends move when the fleet does — a scale, a failed check, a reallocated
port — so a balancer's configuration goes stale the same way a written-down host
port does. Re-read the service after changing the workloads it selects. A streamed
feed that pushes changes to a balancer plugin is planned but not built yet.

## Labels on the service

A service carries labels of its own, separate from the target's, held to the
[same rules](manifest.md#labels) as every other resource's. `takt service list
--query` filters by them:

```sh
takt service list --query '$.labels.env=prod'
```

A query reaches only the service's own labels. The target's labels say what the
service selects rather than what it is.

## Deleting a service

```sh
takt service delete web
```

The workloads the service selected keep running. What stops is the service
reporting their addresses. Deletion is synchronous, unlike a workload's: there is
nothing running to wind down, only a record to remove.
