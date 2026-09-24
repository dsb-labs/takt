# Changelog

Every change an operator or a user of the API would notice, by release. The
newest release is at the top, and changes that have merged but not shipped sit
under Unreleased until a release picks them up. Each release's section becomes
the release notes on GitHub. [Contributing](CONTRIBUTING.md#the-changelog)
describes how an entry is written.

## Unreleased

### Added

- A score bundles the manifests, variables and secrets of one deployment,
  rendered from a values file with Go templates and applied in dependency
  order. `takt score render`, `show`, `apply`, `list` and `delete` are the
  verbs. Everything a score applies carries a `score` and a `score.release`
  label, and `apply --prune` and `delete` work from them. The server is
  unchanged ([#112](https://github.com/dsb-labs/takt/issues/112)).

## v0.9.0 - 2026-09-24

### Added

- Every write can name the version it expects to be replacing, the way an
  `acl apply` already could. `takt workload get` prints the resource's tag as
  `ETag`, `takt workload apply --if-match` carries it back, and takt refuses
  the apply when the resource has moved on since. Volume and service apply,
  secret and variable set, and `acl apply` take the same flag
  ([#89](https://github.com/dsb-labs/takt/issues/89)).
- `takt workload events --since` reads only the events a workload was last
  seen at after the given time, which a poller uses to pick up where it left
  off ([#89](https://github.com/dsb-labs/takt/issues/89),
  [#114](https://github.com/dsb-labs/takt/pull/114)).
- A workload's `logs` block caps what its output may grow to, naming the
  size it is rotated at and how many files are kept. A container's is
  applied by the Docker daemon, and an exec workload's by takt against the
  file the process writes ([#87](https://github.com/dsb-labs/takt/issues/87)).

### Changed

- An `exec` workload no longer inherits the groups the server's user is in.
  The packaged install puts that user in the `docker` group, and a workload
  holding it could run a privileged container. The unit now grants
  `CAP_SETGID` for the drop. A server started without it, as a user in any
  group beyond its primary, refuses `exec` workloads and says so at startup.
  A workload already running when the server is upgraded keeps its groups
  until it is next started
  ([#73](https://github.com/dsb-labs/takt/issues/73)).
- The server warns at startup when authentication is off and a workload
  shares the host's network, since such a workload reaches the API with
  every permission ([#73](https://github.com/dsb-labs/takt/issues/73)).
- **Breaking:** the policy's `ETag` is a count of the applies that changed
  it, `"0"` before any apply, rather than a hash of the document. A tag read
  before upgrading no longer matches. `takt acl get` prints the document under
  `Spec` beside the tag, so capturing it into a file is now
  `takt acl get | jq .Spec` ([#89](https://github.com/dsb-labs/takt/issues/89)).
- Every operation requiring a role documents the 401 and 403 it can answer
  with, where most of them named neither
  ([#89](https://github.com/dsb-labs/takt/issues/89),
  [#114](https://github.com/dsb-labs/takt/pull/114)).
- Every response carries a content security policy, refuses framing and type
  sniffing, and insists on TLS when reached over it. A browser page's origin is
  accepted only from `localhost` or a loopback address, where any address
  literal was accepted before. The session cookie is marked `Secure` behind a
  proxy terminating TLS when the OIDC `redirect-url` is `https`, and the OIDC
  state cookie is cleared as the login completes
  ([#77](https://github.com/dsb-labs/takt/issues/77),
  [#105](https://github.com/dsb-labs/takt/pull/105)).
- The server warns at startup when authentication is enabled and
  `takt acl init` has not run
  ([#78](https://github.com/dsb-labs/takt/issues/78),
  [#104](https://github.com/dsb-labs/takt/pull/104)).
- `allow-host-paths` is checked again each time an instance starts, not only when
  the manifest is applied. A stored workload whose path no longer sits under a
  prefix fails to start and says so in its events
  ([#74](https://github.com/dsb-labs/takt/issues/74),
  [#95](https://github.com/dsb-labs/takt/pull/95)).
- The package makes `/etc/takt/config.toml` readable by root and the `takt` user
  only ([#85](https://github.com/dsb-labs/takt/issues/85),
  [#94](https://github.com/dsb-labs/takt/pull/94)).

### Fixed

- Following the logs of an instance that had not started reported that it
  had ended and was waiting for a replacement
  ([#41](https://github.com/dsb-labs/takt/issues/41),
  [#119](https://github.com/dsb-labs/takt/pull/119)).
- A specification change to a workload running several instances replaced them
  all within a second, since every replacement's own start woke the next pass.
  The next instance now waits until the last replacement has stayed up for ten
  seconds and passed its health check
  ([#80](https://github.com/dsb-labs/takt/issues/80),
  [#107](https://github.com/dsb-labs/takt/pull/107)).
- An `exec` workload could read the server's own configuration, which may hold
  an OIDC client secret, and a TLS key kept under `/etc`, since the confinement
  granted `/etc` whole. The configuration's directory, the TLS key and the
  keyring are now refused ([#75](https://github.com/dsb-labs/takt/issues/75),
  [#106](https://github.com/dsb-labs/takt/pull/106)).
- A backup left on disk by a server that died mid-backup is removed at the next
  startup, and expired tokens are swept at startup rather than an hour later. A
  scheduled run retried after a failure records a run start rather than an
  instance start ([#88](https://github.com/dsb-labs/takt/issues/88),
  [#103](https://github.com/dsb-labs/takt/pull/103)).
- Stopping the server while a client held a followed log or a service stream
  open waited thirty seconds and then exited with an error. The server now ends
  open responses as it shuts down, and a second interrupt ends the process at
  once ([#82](https://github.com/dsb-labs/takt/issues/82),
  [#99](https://github.com/dsb-labs/takt/pull/99)).
- An `exec` workload naming a memory limit smaller than the process takt starts
  it through could be killed before its command ran, and then read as running
  ([#101](https://github.com/dsb-labs/takt/issues/101),
  [#102](https://github.com/dsb-labs/takt/pull/102)).
- A symbolic link beneath an allowed host path carried a path mount wherever it
  pointed. Links are now followed on both sides before the path is compared to
  the prefixes ([#74](https://github.com/dsb-labs/takt/issues/74),
  [#95](https://github.com/dsb-labs/takt/pull/95)).
- An apply that raced a delete of the same workload could report success and
  then have its row removed by the teardown, and a delete that raced an apply
  referencing it could leave that reference dangling. Both checks now run
  inside the transaction that writes
  ([#84](https://github.com/dsb-labs/takt/issues/84),
  [#100](https://github.com/dsb-labs/takt/pull/100)).
- An `env` key holding `=` set a variable other than the one the manifest named,
  and a value holding a NUL byte failed the start rather than the apply. The
  manifest now refuses both
  ([#86](https://github.com/dsb-labs/takt/issues/86),
  [#96](https://github.com/dsb-labs/takt/pull/96)).
- A container `image` docker would refuse was reported when the container was
  created rather than when the manifest was applied
  ([#86](https://github.com/dsb-labs/takt/issues/86),
  [#96](https://github.com/dsb-labs/takt/pull/96)).
- A pinned host port that something outside takt already listened on was
  accepted at apply and failed the start
  ([#86](https://github.com/dsb-labs/takt/issues/86),
  [#96](https://github.com/dsb-labs/takt/pull/96)).
- A driver's event stream ending, as it does when the Docker daemon restarts,
  left the server converging on the interval alone until it was restarted. The
  stream is now watched again
  ([#81](https://github.com/dsb-labs/takt/issues/81),
  [#97](https://github.com/dsb-labs/takt/pull/97)).
- An instance failing its health check under `restart.policy: on-failure` was
  never replaced, and its verdict flapped on every pass. Under `never` the
  verdict flapped the same way. A workload declaring a check under `on-failure`
  also had the check forgotten on every pass while healthy
  ([#79](https://github.com/dsb-labs/takt/issues/79),
  [#98](https://github.com/dsb-labs/takt/pull/98)).
- The packaged unit refused to start on a host without the Docker socket, which
  a server running only `exec` workloads has no need of
  ([#85](https://github.com/dsb-labs/takt/issues/85),
  [#94](https://github.com/dsb-labs/takt/pull/94)).
- A health check's `http` path could name a host of its own, and the server
  probed that host rather than the instance. The manifest now refuses a path
  that does not start with `/`
  ([#76](https://github.com/dsb-labs/takt/issues/76),
  [#93](https://github.com/dsb-labs/takt/pull/93)).
- The first restart of a failed instance waited twice the manifest's `restart.delay`
  ([#83](https://github.com/dsb-labs/takt/issues/83),
  [#92](https://github.com/dsb-labs/takt/pull/92)).

### Removed

- The `idToken` form of `POST /api/v1/auth`, and the Go client's `Login`
  method that sent it. The CLI and the UI log in through the code and token
  exchanges, and a raw identity token carried no nonce binding it to either
  ([#78](https://github.com/dsb-labs/takt/issues/78),
  [#120](https://github.com/dsb-labs/takt/pull/120)).

## v0.8.0 - 2026-09-17

### Added

- `GET /api/v1/node` and `takt node get` report the machine the server runs
  on. The report covers the hostname, kernel, processors, memory, load and the
  disk under the data and volumes directories. It also sums the limits every
  running instance is held to
  ([#66](https://github.com/dsb-labs/takt/pull/66)).
- The web UI lands on a node page showing what the machine has and how much of
  it takt has allocated. The workloads list moves to `/workloads`
  ([#66](https://github.com/dsb-labs/takt/pull/66)).
- `GET /api/v1/services?follow=true` keeps the response open. It writes the
  matching services again, as newline-delimited JSON, each time their backends
  change. The Go client gains `StreamServices`
  ([#63](https://github.com/dsb-labs/takt/pull/63)).
- A changelog, which the release notes are now written from
  ([#70](https://github.com/dsb-labs/takt/pull/70)).

### Changed

- Every `exec` workload runs in a cgroup of its own, so one naming no
  `resources` reports usage too. A host without a delegated subtree still runs
  such a workload, without a reading, and says so at startup. Stopping an
  `exec` workload now reaches a child that made its own session
  ([#49](https://github.com/dsb-labs/takt/issues/49),
  [#68](https://github.com/dsb-labs/takt/pull/68)).
- A health verdict changing wakes the reconciler. It replaces an instance that
  fails its check sooner, and counts a passing one as a backend at once
  ([#63](https://github.com/dsb-labs/takt/pull/63)).
- Listing services observes the host once rather than once per service
  ([#61](https://github.com/dsb-labs/takt/pull/61)).

### Fixed

- A processor figure below a hundredth of a core read `0` in the instance
  table and on every tick of the CPU chart's axis
  ([#67](https://github.com/dsb-labs/takt/pull/67)).
- Stopping an `exec` workload could fail with "no such device" when its
  process ended just as the stop reached its cgroup
  ([#69](https://github.com/dsb-labs/takt/pull/69)).

## v0.7.0 - 2026-09-15

### Added

- A workload records events: what an operator asked for, what the reconciler
  observed, and why an instance was replaced.
  `GET /api/v1/workloads/{name}/events` and `takt workload events` read them,
  the workload page shows them, and `[workload] max-events` caps how many are
  kept, fifty by default ([#55](https://github.com/dsb-labs/takt/pull/55),
  [#56](https://github.com/dsb-labs/takt/pull/56),
  [#57](https://github.com/dsb-labs/takt/pull/57),
  [#58](https://github.com/dsb-labs/takt/pull/58)).
- Every instance reports what it is consuming — memory, processor rate and
  process count — beside the limits its manifest named. `takt workload get`
  prints the figures, and the web UI colours them as they near a limit
  ([#50](https://github.com/dsb-labs/takt/pull/50)).
- An instance has a page of its own in the web UI, at
  `/workloads/{name}/instances/{index}`. It shows memory and CPU charts over
  the last five minutes, the instance's ports and its output
  ([#50](https://github.com/dsb-labs/takt/pull/50)).
- The log viewer draws the colours a program writes, and a carriage return
  redraws the line, so a progress bar animates in place
  ([#51](https://github.com/dsb-labs/takt/pull/51)).

### Changed

- The workload reports the health verdict takt reaches about an instance on
  the instance itself, rather than in a map beside its instances
  ([#50](https://github.com/dsb-labs/takt/pull/50)).

### Fixed

- Deleting a resource from its page in the web UI held the dialog for several
  seconds and flashed a not-found banner before leaving
  ([#42](https://github.com/dsb-labs/takt/issues/42),
  [#46](https://github.com/dsb-labs/takt/pull/46)).
- A session expiring with a page open showed an error banner rather than
  returning to the login page
  ([#43](https://github.com/dsb-labs/takt/issues/43),
  [#45](https://github.com/dsb-labs/takt/pull/45)).
- The events recorded an instance replaced for failing its health check as
  having exited with status zero, which described a process that never
  stopped ([#60](https://github.com/dsb-labs/takt/pull/60)).
- Re-applying a manifest unchanged stamped every workload reading its address
  with a move that did not happen
  ([#60](https://github.com/dsb-labs/takt/pull/60)).
- The events recorded a paced start twice, and a scheduled run both as a run
  and as an instance start
  ([#60](https://github.com/dsb-labs/takt/pull/60)).

### Removed

- `lastError` on a workload. The events say what it said, and why
  ([#56](https://github.com/dsb-labs/takt/pull/56)).

## v0.6.0 - 2026-09-12

### Added

- Workload identity. A manifest names a principal with `${token:principal}` in
  its environment, or mounts one as a file with a `token` volume entry. The
  server mints a credential bound to that principal as the instance starts.
  The credential's life follows the instance. A mounted token rotates under
  its signal, an environment token rotates by replacement, and the server
  revokes both when the workload goes. The token list reports them under the
  `workload` source ([#32](https://github.com/dsb-labs/takt/issues/32),
  [#33](https://github.com/dsb-labs/takt/pull/33)).
- The credentials table in the web UI sorts by any column.

### Changed

- `takt auth login` saves the address and certificate authority it logged in
  against beside the token, so a later command reaches the server the token
  belongs to. The `--address` flag no longer shadows the environment or the
  config file ([#36](https://github.com/dsb-labs/takt/pull/36)).
- Observing the fleet costs less. A pass no longer inspects every
  health-checked container, re-inspects a container that has ended, or
  re-reads an `exec` state file that has not changed
  ([#34](https://github.com/dsb-labs/takt/pull/34)).

### Fixed

- `takt auth login` presented the stored token, and the server refused the
  login when that token had expired. Replacing an expired login meant editing
  the config file ([#36](https://github.com/dsb-labs/takt/pull/36)).
- The docs disagreed with the server in several places. The corrections
  cover the anonymous surface, the reads the viewer role does not hold, the
  by-hand install's unit path, who restarts the service after a package
  upgrade, and what an empty `resources` block means
  ([#38](https://github.com/dsb-labs/takt/pull/38)).

## v0.5.1 - 2026-09-10

### Fixed

- The authentication middleware dropped `http.route` from every span and
  metric, so latency could no longer be read per route
  ([#31](https://github.com/dsb-labs/takt/pull/31)).

## v0.5.0 - 2026-09-10

### Added

- Authentication and access control. Writing an `[auth]` block into the
  configuration makes every request carry a credential. A policy document
  applied with `takt acl apply` grants the `viewer`, `operator` and `admin`
  roles to principals and groups. `takt acl init` mints the recovery token
  once, and a reset file recovers a lost one
  ([#28](https://github.com/dsb-labs/takt/issues/28),
  [#29](https://github.com/dsb-labs/takt/pull/29)).
- `[auth.oidc]` names an issuer. `takt auth login` runs the browser flow and
  stores the token it mints, `takt auth whoami` reports the principal and
  role, and `takt auth logout` revokes the credential. The server exchanges
  the authorization code, so the client secret never reaches the CLI
  ([#29](https://github.com/dsb-labs/takt/pull/29),
  [#30](https://github.com/dsb-labs/takt/pull/30)).
- `takt token create`, `list` and `delete` manage static credentials for
  machines ([#29](https://github.com/dsb-labs/takt/pull/29)).
- Every command resolves how it connects from three places, most specific
  first: flags, then `TAKT_ADDRESS`, `TAKT_TOKEN` and `TAKT_CA_CERT`, then a
  config file. The file is `.takt/config` under the home directory, or
  wherever `--config` or `TAKT_CONFIG` points
  ([#29](https://github.com/dsb-labs/takt/pull/29)).
- The web UI gains a login page, which offers single sign-on when the server
  has it and a token form otherwise. An Access page shows the policy and the
  credentials, and a control appears only for a role that may use it
  ([#29](https://github.com/dsb-labs/takt/pull/29)).
- The Go client gains `WithToken`, the auth, token and policy methods, and the
  `IsUnauthorized` and `IsForbidden` predicates
  ([#29](https://github.com/dsb-labs/takt/pull/29)).

## v0.4.1 - 2026-09-08

### Changed

- takt traces every driver call, every health probe and every image pull, and
  links a pull to the pass that asked for it. The reconcile span carries the
  pass's outcome and counts.
- The database no longer emits a span for every rows iteration and session
  reset.

### Fixed

- Every duration histogram used the OpenTelemetry defaults, whose first bucket
  ends at five seconds. `histogram_quantile` therefore reported 4.75s for
  operations that finish in milliseconds. Each histogram now has boundaries
  fitted to what it measures.

## v0.4.0 - 2026-09-08

### Added

- `networkMode: host` on a container joins the host network, so the port it
  binds is the host port. `from` equals `to`, and `count` must be one.
- Prometheus service discovery at `GET /api/v1/system/prometheus-sd`, in the
  shape `http_sd_configs` reads. A workload labelled `prometheus.scrape` is a
  target group with a target per instance, and `prometheus.port`,
  `prometheus.path` and `prometheus.scheme` select what is scraped.

### Changed

- The system endpoints moved to `/api/v1/system/health`,
  `/api/v1/system/ready` and `/api/v1/system/metrics`. The old paths are gone.

## v0.3.0 - 2026-09-07

### Added

- `propagation: rslave` or `rshared` on a host path mount, so a workload
  observing the whole host sees filesystems mounted after it started.
- The web UI fits a phone: the navigation scrolls, list tables hide their
  secondary columns, and controls take a row of their own.

### Changed

- `takt volume apply` replaces `takt volume create` and `takt volume update`,
  the way workloads and services already work. `PUT /api/v1/volumes/{name}`
  creates or updates, and the client's `ApplyVolume` replaces both methods.

### Fixed

- Updating the mode of a volume another user owns failed on a packaged
  install. The unit now grants `CAP_FOWNER`.
- Leaving a detail page in the web UI flashed it empty before the next page
  arrived.

### Removed

- `POST /api/v1/volumes`, `takt volume create` and `takt volume update`.

## v0.2.0 - 2026-09-06

### Added

- `pidMode: host` on a container shares the host's pid namespace, for a
  metrics exporter observing the host's processes.
- An installation guide, and an apt repository at `apt.dsb.dev` that the quick
  start now leads with.

### Changed

- Image pulls no longer hold the reconcile pass. An instance waiting on a pull
  stays pending, and the pass moves on to every other workload.

### Fixed

- A volume naming both an owner and a mode failed to create on a packaged
  install. The server applied the mode after the directory changed hands.

## v0.1.0 - 2026-09-06

The first release. takt runs container and `exec` workloads on one host,
reconciling them against applied manifests:

- Workloads running as a Docker container or as a confined command on the
  host, with ports, environment, volumes, restart policies, health checks,
  schedules, resource limits and labels.
- Volumes, secrets and variables as resources of their own, read into a
  workload's environment or mounted as files. Rotating one replaces or
  signals the workloads reading it.
- Services selecting backends by label, for a balancer to read.
- The `takt` CLI, a Go client, an OpenAPI description, and a web UI with a
  reference graph.
- Backup, restore and rekey through `takt admin`, traces and metrics through
  OpenTelemetry, and a Debian package with a systemd unit.
