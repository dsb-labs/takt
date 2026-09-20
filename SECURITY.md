# Security

## Reporting a vulnerability

Report a vulnerability through
[GitHub's private reporting](https://github.com/dsb-labs/takt/security/advisories/new)
for this repository, or by email to david@dsb.dev. Do not open a public issue for
one.

Say what the vulnerability lets someone do, and how to reproduce it. A fix is
worked on in private, released, and then the advisory is published with credit
to whoever reported it, unless they ask otherwise.

## What takt guarantees, and what it does not

takt runs code on the host it is installed on. That is what it is for, and it
shapes what counts as a vulnerability:

- **Reaching the API is control of the host.** Applying a workload runs code with
  whatever the Docker socket grants. Anyone who can call the API with the
  `operator` role, or who can reach it at all when authentication is off, holds
  the host. That is documented in [Operating takt](docs/operating.md#exposure)
  and [Access control](docs/acl.md#roles), and a report that an operator can do
  something on the host is a report of the design.
- **What takt does guarantee** is written down in the docs: that a secret's
  value is never readable through the API once stored
  ([Secrets](docs/secrets.md#what-takt-guarantees)), that an `exec` workload is
  confined to what its manifest names
  ([Confinement](docs/operating.md#confinement)), that a request from a browser
  page on another origin is refused
  ([Exposure](docs/operating.md#loopback-is-not-a-boundary-against-a-browser)),
  and that a credential a workload was given dies with it
  ([Workload identity](docs/acl.md#workload-identity)). A way past any of those
  is a vulnerability.

## Supported versions

The latest release is supported. A fix ships as a new release rather than as a
patch to an older one.
