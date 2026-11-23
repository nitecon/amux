# Standalone server deployment

The `amux server run` command runs the Go server without the desktop application or an interactive login. It defaults to `127.0.0.1:7913`. Bearer mode requires a nonempty API key in `GATEWAY_API_KEY` and TLS for a non-loopback listener. Choose `--database PATH` for a standalone SQLite database or `--profile DIR` to keep `application.sqlite` and the installation identity in `security.sqlite` under one directory. Profile mode retains an existing Rust installation ID and valid root certificate and key, creates a root pair for a fresh profile, and holds the security owner lock for the lifetime of the process. Incomplete or mismatched root material prevents startup.

The Go server currently provides the server API and its server-owned workers. Native provider workers are a separate `amux worker pool` process; this deployment does not start a provider pool or claim full desktop or gateway deployment parity.

With `--profile`, supply `--profile-tls-name NAME` once per DNS name or IP address to serve HTTPS with a server certificate signed by the profile root. Clients must trust the public `root.pem` and connect using a configured name. Configure profile TLS names or a manual certificate/key pair. Keep all private keys in the profile directory.

For certificate authentication, add `--secure-instance-id UUID` to profile mode and reuse that administrator instance ID across restarts. Profile-managed TLS includes loopback IPs for the local administrator. For remote enrollment, set `--profile-tls-name` to the name remote clients will use and bind both listeners to reachable addresses. The public discovery listener serves the profile CA and secure port. It defaults to the secure listen host on port 24181 and can be set with `--discovery-bind`.

From the server host, `amux server peering-key --profile DIR` prints a short-lived peering key. Add `--copy` to retrieve the current key without replacing it. The command uses the loopback administrator certificate in the profile.

On the client, run `amux client enroll --server https://NAME:PORT --state-dir DIR` and supply the peering key on stdin. The command prints the saved `client-profile.json` path. Pass that path with `--enrolled-profile` to the headless worker, desktop, or console commands. Keep the client state directory private because it contains the client private key.

Run `amux client renew --enrolled-profile PATH` to renew the certificate for that client identity.

For a Rust-to-Go gateway cutover, stop the Rust gateway and back up its full
profile directory. Start the Go gateway with that profile, retaining
`application.sqlite`, `security.sqlite`, the installation root certificate
and key, and the local administrator instance ID. Keep the existing gateway
hostname and ports reachable and include that hostname in the Go TLS names.
Existing enrolled clients use their stored certificates. Generate a new
peering key for each enrollment window. The key expires after ten minutes.


## Docker

Build the standalone server image from the repository root:

```sh
docker build -t amux-server:local .
```

The image uses Go 1.26.11 to build `./cmd/amux` with `CGO_ENABLED=0`, then copies the binary into a `scratch` runtime. It runs as UID/GID 65532 and keeps SQLite state under `/data`. The Go base image is version tagged; no immutable digest or security audit is claimed here.

Create a private environment file on the host (outside the build context) containing `GATEWAY_API_KEY=<strong-secret>`, then start a loopback-only instance on a Linux host:

```sh
docker run -d --name amux-server \
  --restart unless-stopped \
  --network host \
  --env-file /etc/amux/server.env \
  --volume amux-data:/data \
  amux-server:local
```

The container uses the server's loopback default, so host networking makes it available at `http://127.0.0.1:7913` for a local reverse proxy. Docker host networking is platform-specific.

For direct non-loopback access, mount a certificate and private key read-only and override the command with an explicit non-loopback address and both TLS paths. Keep the API key in a host-side environment file and the certificate files outside the image:

```sh
docker run -d --name amux-server \
  --restart unless-stopped \
  --publish 7913:7913 \
  --env-file /etc/amux/server.env \
  --volume amux-data:/data \
  --volume /etc/amux/tls:/tls:ro \
  amux-server:local \
  server run --database /data/amux.db --listen 0.0.0.0:7913 \
  --tls-cert /tls/server.crt --tls-key /tls/server.key
```

The image runs as UID/GID 65532. Grant that UID read access to the mounted certificate and private key with ownership, a restricted group, or an ACL; keep the private key inaccessible to other users. Do not make it world-readable.

The command rejects a non-loopback listener without both TLS options, and startup fails if the configured API-key environment variable is empty or the TLS files cannot be loaded. Back up the `/data` volume as durable SQLite state.

## systemd

These commands install the repository-provided service on a Linux systemd host. Run them as an administrator; they are instructions only and are not run by the repository build.

Build and install the binary and service definition:

```sh
make cli
sudo install -o root -g root -m 0755 bin/amux /usr/local/bin/amux
sudo useradd --system --home-dir /var/lib/amux --shell /usr/sbin/nologin amux
sudo install -d -o root -g root -m 0755 /etc/amux
sudo install -o root -g root -m 0644 deploy/amux-server.service /etc/systemd/system/amux-server.service
sudo install -o root -g root -m 0600 deploy/server.env.example /etc/amux/server.env
sudoedit /etc/amux/server.env
```

Set `GATEWAY_API_KEY` in `/etc/amux/server.env`; keep the file root-readable only. Then load and start the service:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now amux-server.service
sudo systemctl status amux-server.service
```

The service keeps its database in systemd's durable `/var/lib/amux` state directory, runs as the non-login `amux` account, restarts on failure, and allows up to 25 seconds for SIGTERM shutdown. The default listener is loopback for a local TLS-terminating proxy.

To serve a non-loopback address directly, place readable certificate and private-key files on the host and add a systemd drop-in. The override must supply both TLS flags:

```sh
sudo systemctl edit amux-server.service
```

Use this drop-in content, adjusting the address and certificate paths:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/amux server run --database /var/lib/amux/amux.db --listen 0.0.0.0:7913 --tls-cert /etc/amux/tls/server.crt --tls-key /etc/amux/tls/server.key
```

Then apply the override:

```sh
sudo systemctl daemon-reload
sudo systemctl restart amux-server.service
```

Ensure the `amux` account can read both TLS files through ownership, a restricted group, or an ACL, while keeping the private key inaccessible to other users. Do not make it world-readable.

## Headless Linux clients

Use a dedicated service account with access to each existing Git checkout and
the chosen agent provider executable. Enroll that account with an expiring
peering key and place the returned manifest path in `/etc/amux/client.env` as
`AMUX_CLIENT_PROFILE=/absolute/path/to/client-profile.json`. Keep the manifest
and its private key readable by the service account.

Create the state directory before enrollment or the first registration. If
the service has already started, stop it before running these commands.

```sh
sudo install -d -m 0755 /etc/amux
sudo install -d -m 0700 -o amux -g amux /var/lib/amux-client
sudo -u amux /usr/local/bin/amux client enroll --server https://GATEWAY:PORT \
  --state-dir /var/lib/amux-client
```

Supply the key on stdin. Set `client_manifest` to the manifest path printed
by enrollment and use that path in `/etc/amux/client.env`. Register each
existing checkout as the same service account:

```sh
sudo -u amux /usr/local/bin/amux -add-project /srv/repos/example \
  --state /var/lib/amux-client/workers.sqlite \
  --enrolled-profile "$client_manifest" --provider codex --slots 2
```

Registration matches the checkout's Git origin to an existing gateway project.
Use `--project` to choose among matching gateway projects or bind a Git checkout
without an origin to an existing ad-hoc project. The command prints the saved
pool ID and does not clone a repository or start an agent.

Install [amux-client.service](amux-client.service) under
`/etc/systemd/system`, then run `systemctl daemon-reload` and
`systemctl enable --now amux-client`. The client starts the saved pools for
its enrolled gateway. Each worker slot advertises its project and machine
through the existing gateway presence protocol. Check unavailable paths and
worker errors with `journalctl -u amux-client`. The project’s Alerts tab
shows which hosts last reported their checkout unavailable. The client rechecks registered
paths every 30 seconds. It withdraws the worker pool when its checkout becomes
unavailable and resumes it when the registered path is restored. Manual
assignment targets age out on the two-minute registration lease.

Stop the service before adding more checkouts to its journal, then restart it
to pick up the new pools. Use one headless client per machine for checkout
reporting. Manage checkout ownership and cloning outside amux.

### Agent tools on desktop and headless hosts

Both entry points give Codex, Claude, and Antigravity the same project-scoped
`amux mcp` stdio server. Task operations and `submit_work` use the Go gateway.
The current code navigation and knowledge-graph tools are exposed when
`agent-tools` is installed, and the memory tools when the `memory` executable
is installed. Put those executables beside `amux` or set `AMUX_BUNDLED_DIR`.
Install them on the machine running the agent so reads and searches see that
machine's checkout.

Keep MCP as the provider-facing interface. The gateway already has tables and
sync APIs for shared project memory, which makes it the natural home for native
memory search and writes. Keep transient checkout indexes and the worker
recovery journal on the execution host. Before replacing the bundled memory
commands, account for local-only memories, updates, tombstones, and the search
behavior agents rely on.
