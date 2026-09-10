# Access control

Without authentication, anything that reaches takt's listener holds the whole
API, and reaching the API is enough to run code on the host. The auth layer
replaces that network boundary with an answer to who is calling: every request
resolves to a principal, and a policy document binds one of three fixed roles
to each principal.

It is off by default. Writing an `[auth]` block into the
[configuration](configuration.md#auth), even an empty one, turns it on.

```sh
takt acl init
takt acl apply policy.yaml
takt token create prometheus
```

## What takt guarantees

- **Access is auditable in two commands.** `takt token list` names every
  credential and `takt acl get` names every grant. Together they answer "who
  can touch this server" completely, from the server itself.
- **A credential is never stored.** The server keeps only a hash, so a copy of
  the database holds nothing a caller could present. A token is printed once,
  by the create or login that minted it.
- **Revocation is immediate.** Deleting a token refuses the very next request
  presenting it, and a grant removed from the policy is gone on the apply.
- **A bad policy cannot lock you out.** The recovery token bypasses policy,
  and losing the recovery token is recovered at the host with the
  [reset file](#losing-the-recovery-token).

## Enabling

Enabling is three steps: add the block, restart, init.

1. Add `[auth]` to the server's configuration and restart it. Every request
   now requires a credential, except `/api/v1/system/health` and
   `/api/v1/system/ready` — a supervisor must probe those without credentials
   or it cannot manage the process, and they are the whole anonymous surface.
2. Run `takt acl init`. It works exactly once and prints the recovery token,
   which is the root of trust: store it somewhere safe.
3. Apply the first policy with the recovery token, then work with a client
   token of your own from that point on. The recovery token exists for init
   and lockout recovery, not for daily use.

```sh
TAKT_TOKEN="takt_r_..." takt acl apply policy.yaml
TAKT_TOKEN="takt_r_..." takt token create you@example.com
```

## The policy document

The policy is one YAML document, applied whole with `takt acl apply`. A grant
absent from the document is revoked on the apply, with no prune step, so the
file in a git repository is the complete answer to who holds access. It is
designed to live beside the workload manifests and be applied by the same
pipeline, under a principal of its own.

```yaml
# Applied with `takt acl apply policy.yaml`. The whole document replaces
# the current policy, so a grant removed here is revoked on the next apply.
version: v1

# How an OIDC identity becomes a principal and groups. The claims are
# read from the identity token `takt auth login` exchanges.
oidc:
  principalClaim: email
  groupsClaim: groups

groups:
  - name: infra
    members: [david@dsb.dev, ci]

# A grant binds one fixed role to principals or groups.
grants:
  - principals: ["group:infra"]
    role: operator

  - principals: [david@dsb.dev]
    role: admin

  - principals: [prometheus]
    role: viewer
```

Before any apply, the policy is the empty `version: v1` document, which grants
nothing to anyone. `takt acl get` returns the canonical current document, and
its output is valid input to `takt acl apply`.

Two applies cannot silently overwrite each other. The apply is conditional on
the policy not having changed since it was read, so the loser of a race gets
an error to re-run rather than a lost update.

## Roles

Three fixed roles, hierarchical: `admin` covers `operator`, `operator` covers
`viewer`.

| Role | Holds |
|---|---|
| `viewer` | Every read, including logs, metrics and target discovery. |
| `operator` | Workload, volume, service, variable and secret lifecycle. |
| `admin` | Applying the policy and managing tokens. |

Writing a secret is routine operation, not administration, which is why
`operator` holds it — and why `operator` is the grant to be stingy with, not
just `admin`: with secret writes in hand, the ACL surface is the only thing
separating a compromised operator token from full control.

Finer-grained capabilities are deliberately absent. The three roles are where
the human cases land, and a machine wanting less than `viewer` is future work.

## Principals and groups

A principal is a name, never an object: nothing creates one, and a grant to a
name nobody has authenticated as yet is valid. That is the onboarding order —
merge the grant, then the person logs in, or hand over the token `takt token
create` minted for the name.

Principals share one namespace, so the convention is that humans are emails
and machines are bare names. An identity provider's username then cannot
collide with a machine's grants.

A grant names principals directly or through `group:<name>`. A group entry
matches a group defined in the policy's own `groups` list, or a group the
identity provider asserted at login through the claim `groupsClaim` names —
so onboarding a person whose IdP already groups them needs no takt-side edit.

## Tokens

A token is the credential a caller presents, as a bearer header or through
the web UI's session cookie.

- `takt token create <principal>` mints a static token bound to a principal
  and prints it once. Machine consumers — CI, prometheus, the traefik
  provider plugin — keep static tokens. Only humans log in.
- `takt auth login` exchanges an OIDC identity for a token that expires on
  its own, and writes it to the [config file](cli.md#connecting-to-a-server).
- `takt token list` names every credential, including sessions and the
  recovery token, with when each was created and last used.
- `takt token delete <id>` revokes one. `takt auth logout` revokes whatever
  credential made the call.

## Losing the recovery token

Recovery is a host-level act, so holding the machine is what proves the right
to reset:

1. Write a file named `acl.reset` into the server's data directory.
2. Restart the server. It removes the recovery token, deletes the file, and
   logs that it did.
3. Run `takt acl init` again for a fresh recovery token.

Client tokens and the policy survive a reset. The reset exists to recover
from a lost recovery token, not to start over.

## The scrapers

With authentication enabled, `/api/v1/system/metrics` and
`/api/v1/system/prometheus-sd` require the `viewer` role. Prometheus carries
the credential in an `authorization` block — the exact configuration is in
[Operating](operating.md#observability).
