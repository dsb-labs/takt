# Installing takt

How to get from nothing to a running server. The packaged install answers the
security decisions an installation carries, so prefer it where it fits. What those
decisions are, and how to make them yourself, is the [by hand](#by-hand) section.

Whichever path you take, the host needs what the
[requirements](../README.md#requirements) list: Linux on the host rather than in a
container, Landlock for `exec` workloads, and a Docker daemon for `container`
workloads.

## From the apt repository

Debian and Ubuntu hosts install from the signed repository each release publishes
to:

```sh
curl -fsSL https://apt.dsb.dev/key.asc | sudo tee /usr/share/keyrings/takt.asc >/dev/null
echo "deb [signed-by=/usr/share/keyrings/takt.asc] https://apt.dsb.dev stable main" \
  | sudo tee /etc/apt/sources.list.d/takt.list
sudo apt-get update
sudo apt-get install takt
```

The package installs the binary, the systemd unit, the `takt` system user and
`/etc/takt/config.toml`. From then on `apt-get upgrade` carries takt along with
everything else, and [Upgrading](upgrading.md) says what a new binary means for
what is running.

The service is installed but not started. The server cannot start until it can
reach the Docker socket, and that grant is root-equivalent — anything in the
`docker` group can run a privileged container — so it should happen because you
typed it:

```sh
sudo usermod -aG docker takt
sudo systemctl enable --now takt
```

What the unit sets up, and why, is
[Running under systemd](operating.md#running-under-systemd).

## From the rpm package

Each release attaches an `.rpm` holding the same contents as the `.deb`. There is
no dnf repository yet, so download the package from the
[releases](https://github.com/dsb-labs/takt/releases) page and install it:

```sh
sudo dnf install ./takt_<version>_linux_amd64.rpm
```

The grant-and-enable step above applies unchanged.

## By hand

A host outside those package managers installs from a release archive. The steps
mirror what the package does, and each carries a decision the package made for
you.

1. **Put the binary on the `PATH`.** Download the archive for your platform from
   the [releases](https://github.com/dsb-labs/takt/releases) page and unpack
   `takt` into `/usr/local/bin`.

2. **Create a user for the server.** Everything the server holds — the state
   database, the encryption keys, the files mounted secrets are written to — is
   readable by the user running it, so run it as a dedicated system user rather
   than as yourself:

   ```sh
   sudo useradd --system --home-dir /var/lib/takt --shell /usr/sbin/nologin takt
   ```

3. **Choose the data directory.** The default, `~/.local/share/takt`, suits a
   workstation. A deployment wants `/var/lib/takt`, owned by the `takt` user and
   readable only by it:

   ```sh
   sudo install -d -m 0700 -o takt -g takt /var/lib/takt
   ```

   [State on disk](operating.md#state-on-disk) says what lives there and why the
   mode matters.

4. **Write the configuration.** The server reads a TOML file, and the data
   directory is the one key a deployment must set:

   ```toml
   [data]
   directory = "/var/lib/takt"
   ```

   Put it at `/etc/takt/config.toml`. Every other key has a default —
   [Configuration](configuration.md) lists them.

5. **Grant the Docker socket.** `sudo usermod -aG docker takt`, weighed as the
   apt section describes.

6. **Install the unit.** Use
   [`packaging/takt.service`](../packaging/takt.service) from this repository
   rather than writing one. Its settings carry behaviour — readiness
   notification, workloads surviving a restart, delegated resource limits, the
   capabilities volume ownership needs — that
   [Running under systemd](operating.md#running-under-systemd) documents.

7. **Enable and start.** `sudo systemctl enable --now takt`.

A server can also run without systemd — `takt serve /etc/takt/config.toml` under
any supervisor, or none. What is lost is what the unit provides: `exec` resource
limits need the delegated cgroup subtree, and volume ownership needs the granted
capabilities. The server refuses the operations it cannot honour and names the
missing grant, so a reduced setup fails loudly rather than quietly.

## The first workload

The server listens on `127.0.0.1:7373`, so the CLI works from the same host
without configuration. The [quick start](../README.md#quick-start) applies a
first manifest and reads back the port it was given. For exposing the server
itself beyond the host, read [Exposure](operating.md#exposure) first.
