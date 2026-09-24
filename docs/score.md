# Scores

A deployment is several manifests: a volume, two workloads, a service, and the
variables and secrets they read. Applied one at a time, the operator has to know
the order, and anything that varies between hosts is a hand edit. A score is the
unit that fixes both. It is a file naming the manifests, variables and secrets
that belong together, with Go templating over a values file.

A score is a client-side build step. Rendering it produces ordinary manifests,
and applying it is ordinary API calls in dependency order. The server never
learns what a score is. What ties an install together is a pair of labels on
every resource it applied, and those labels are what delete, prune and list
work from.

## Files

```yaml
# score.yaml
version: v1
name: blog
release: 1.4.0

volumes:
  - pgdata.yaml

workloads:
  - db.yaml
  - web.yaml

services:
  - web.service.yaml

variables:
  - name: web-config
    file: files/web.json
  - name: db-host
    # No file and no value, so the applier must have set it.

secrets:
  - name: db-password
    # Always declare-only.
```

`name` is the package the score describes. `release` is its version, which every
applied resource is labelled with. Both are required. Paths are relative to the
score, and a path that climbs out of its directory is refused: a score is a
self-contained unit that can be handed around.

A `values.yaml` beside `score.yaml` holds the defaults. It is found by
convention rather than named in the score, and it has the same shape as anything
passed to `--values`, so the default file is not special. Values files are the
only input. There is no `--set`, so every input is reviewable in git.

```yaml
# values.yaml
image: example/blog:1.4.0
web:
  replicas: 2
  logLevel: info
```

Directory layout is your business. The manifests can sit beside the score or
under any subdirectory the paths name.

## Templating

Each manifest is rendered as text with Go's `text/template`, then parsed as
YAML and validated the way `takt workload apply` validates a file. Substitution
works in any position, including inside a takt reference, and the renderer has
no schema awareness.

```yaml
# web.yaml
version: v1
name: {{ .Score.Name }}-web

count: {{ .Values.web.replicas }}

labels:
  app: {{ .Score.Name }}

env:
  LOG_LEVEL: {{ .Values.web.logLevel }}
  DATABASE_URL: postgres://postgres:${secret:db-password}@${workload:{{ .Score.Name }}-db:pg}/app

container:
  image: {{ .Values.image }}
```

The context is:

| Field | Holds |
|---|---|
| `.Values` | The merged values. |
| `.Score.Name` | The install name: `--as`, or the score's `name` when not given. |
| `.Score.Release` | The score's `release`. |
| `.Score.Package` | The score's `name`, whatever the install is called. |

A value a template names that the values do not hold fails the render. A hole
in a manifest is more often a typo in a values file than an intent.

[Sprig](https://masterminds.github.io/sprig/) supplies the helper functions,
minus anything impure: no `env`, `now` or the date helpers, `uuidv4`, the
`rand*` family, `shuffle`, or key and certificate generation. A render has to be
a pure function of the directory and the values. Otherwise it stops working as a
review artefact, and prune's comparison stops meaning anything.

There is no conditional or loop syntax in `score.yaml`. A file that renders to
nothing but whitespace and comments is skipped, and a file that renders to a
multi-document stream yields several resources. Together those cover the cases
a `when` or a `for` would:

```yaml
# worker.yaml
{{- if .Values.worker.enabled }}
version: v1
name: {{ .Score.Name }}-worker
container:
  image: {{ .Values.image }}
{{- end }}
```

Two escapes are worth knowing. `{{ "{{" }}` renders a literal `{{`, and takt's
own `$$` renders a literal `${` once the manifest is applied. The two layers
do not interact: the template runs first and never sees a takt reference as
anything but text.

A render that fails inside a manifest prints that document with line numbers
on standard error, so the line the error names can be found.

## Variables and secrets

A variable entry with `file:` or `value:` is owned by the score. The applier
sets it, and it is overwritten on every apply. An entry with neither is
required: it must already exist when the score is applied, and the score never
touches it. A `file:` is rendered with the same context as a manifest, and so is
a `value:`.

A score-owned variable should be an export: something read from outside the
score, or retuned live by an operator. A value only this score's own workloads
read belongs templated straight into `env:`. Minting a variable for it implies a
tunability the score does not honour, since the next apply writes the templated
value back. `file:` is the case that earns one. A templated configuration file
mounted as a variable, ideally with `signal: SIGHUP`, means an apply rewrites it
in place and reloads the program rather than replacing the workload.

Secrets are declare-only. No value ever enters a score or a values file. The
apply finds every declared secret that is not set and prompts for each without
echo, before anything is applied. `--secret name=@path` reads one from a file
instead, which is the form for CI. `--secret name=value` is refused, for the
reason `takt secret set` takes no value argument. Without a terminal, or with
`--no-input`, a missing secret fails the apply naming every one.

Only a missing secret is set. Supplying one is a per-invocation act, never
recorded in the score. Setting a secret again bumps its revision and redeploys
every reader, so an apply that set every secret every time would restart the
fleet each run. A `--secret` for a name the score does not declare is refused
as a typo.

Every variable and secret a workload reads has to appear in the score, as an
owned or required variable or as a declared secret. A reference to one the
score does not list fails the render. The score is meant to be an honest
inventory of what its workloads need, and the requirement check works from it.

## Tokens and access control

Neither is managed by a score. A token needs nothing to exist first:
`${token:principal}` asserts an identity and the server mints a token as the
instance starts, so there is nothing to create or check. What the author
documents is which principals the workloads assert and what the policy has to
grant them. `takt score show` lists those principals by parsing the rendered
manifests, so an operator knows what to grant before applying.

A principal that should differ per install wants templating off `.Score.Name`,
the same way a workload name does.

The access policy is out of scope. `takt acl apply` replaces it whole, so a
score touching it could silently revoke access. See [Access control](acl.md).

## Ordering

The rendered manifests give one dependency graph. A workload depends on the
volumes it mounts by name, the variables and secrets it reads, and any workload
it reaches through `${workload:name:port}`, since the server refuses a reference
to a workload that does not exist yet. Services depend on nothing: a target's
workloads need not exist.

Resources are applied in that order, with ties broken by kind and then name, so
an apply is volumes, then variables, then workloads with each after the ones it
references, then services. A cycle among the workloads fails the render.

The requirement check falls out of the same graph. Anything a workload depends
on that the score does not apply is a requirement: every required variable,
every declared secret, and any volume or workload a manifest names that the
score does not. Every requirement that does not exist on the server is reported
at once, before anything is applied.

## Labels and ownership

Every resource a score applies carries two labels:

| Label | Holds |
|---|---|
| `score` | The install name. |
| `score.release` | The score's `release`. |

A manifest that sets either itself is refused. They sit outside the `takt.`
prefix the docker driver reserves, and the server does not write them, so they
are only as trustworthy as whoever last applied the resource. For a
single-operator host, prune confirming interactively is the protection.

A resource that already exists without this score's label belongs to something
else, and applying over it is refused. That covers a variable set by hand, a
workload from another score, and a second install of the same score that did
not template its names. Pass `--adopt` to take such resources over, which is how
a deployment applied by hand becomes a score without deleting it first.

The variable and secret entries in `score.yaml` are not templated, so their
names are fixed for every install. Two installs of one score on one host can
share a required variable or a declared secret, but an owned variable would
collide. Give an owned variable a name specific to the package rather than to
the install, or keep to one install per host where the score owns variables.

## Applying

```sh
takt score apply ./blog
takt score apply ./blog --values prod.yaml --as blog-prod
```

An apply renders every manifest and validates it client-side, checks every
requirement and every ownership, sets any missing secrets, then applies the plan.
Nothing reaches the server while a requirement is missing or a resource is
unowned. A failure partway stops at once. There is no rollback: the output says
what landed, and the next apply carries on from there. Since every manifest was
validated before the first call, almost nothing reaches that state.

An apply over an install that exists overwrites what the score owns. Owned
variables are set again, which is a no-op when the value did not change.
Workloads whose rendered specification did not change are left alone, the way
`takt workload apply` leaves them.

Multiple installs of one score on one host are supported. `--as` sets the
install name, manifests template their names and principals off `.Score.Name`,
and the render fails on two resources of one kind sharing a name. The install
name is held to the workload name grammar, since it flows into workload names
and principals.

## Pruning

```sh
takt score apply ./blog --prune
```

Once everything is applied, `--prune` deletes whatever carries this install's
label that the score no longer names. That is how a manifest removed from the
score, or one that now renders to nothing, leaves the server. Prune compares
the rendered set against the label, so it is only as safe as the render is
repeatable.

Pruning destroys in reverse order, waiting for each workload, and asks first
because a volume goes with its data. `--yes` skips the question. Without a
terminal and without `--yes`, nothing is pruned.

## Deleting

```sh
takt score delete blog
takt score delete blog-prod --yes --timeout 10m
```

Delete works from the label, so an install can be removed without the directory
or the values that produced it. Services go first, then workloads with each
before the workloads it references, then variables, then volumes. Each workload
is waited for before the next step. Waiting is what makes the order legal: a
volume a workload mounts and a variable a workload reads are both refused while
the workload holds them, and teardown is asynchronous.

Volumes are deleted with the data they hold. Backups are your business, and
`volume delete` is the one thing in takt that destroys stored data, so the
command prints what it will delete and asks first. `--yes` is the
non-interactive form. What the score required is not the score's to remove: a
required variable and a declared secret stay.

## Listing

```sh
takt score list
```

Groups every resource carrying the `score` label by install and reports the
resources under each, with every release found in the group. More than one
release under an install means an apply failed partway and the next one has not
finished the job.

## Reviewing a change

There is no `diff` verb. `takt score render` prints the rendered stream, and
`git diff` covers the rest:

```sh
takt score render ./blog --values prod.yaml > rendered/prod.yaml
git diff rendered/
```

The stream is the rendered text as written, before the score labels are
attached, so it reads like the source files rather than like a re-serialisation
of them. A file that rendered to nothing is left out.

## What a score is not

**No includes or nesting.** takt already composes through required variables
and secrets: a postgres score exports `db-host`, a blog score requires it. On a
single node you install one postgres and share it rather than embedding one per
application. Nesting would be a second composition mechanism, and every piece of
machinery it needs exists only to serve it. A private sidecar is answered by
vendoring the manifest into your own score.

**No remote distribution, yet.** The command argument is a location rather than
a path, so a registry scheme can land later without changing the shape of
anything here.

**Not a compose file.** A compose file maps onto a score more closely than onto
loose manifests, and [Moving from docker compose](compose.md) says what each
piece becomes.
