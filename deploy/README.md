# Standalone server deployment

The `amux server` command runs the Go server without the desktop application or an interactive login. It defaults to `127.0.0.1:7913`. Bearer mode requires a nonempty API key in `GATEWAY_API_KEY` and TLS for a non-loopback listener. Set `database` for a standalone SQLite database or `profile` to keep `application.sqlite` and the installation identity in `security.sqlite` under one directory. Profile mode retains an existing Rust installation ID and valid root certificate and key, creates a root pair for a fresh profile, and holds the security owner lock for the lifetime of the process. Incomplete or mismatched root material prevents startup.

Start with [amux-server.yml](amux-server.yml). Set exactly one of `database` or `profile`. Relative paths resolve from the server process’s working directory.
Keep credentials in the environment variables referenced by the configuration.

Start the server with `amux server --config /absolute/path/to/amux-server.yml`.
For systemd, use that command as `ExecStart` and keep the existing
`EnvironmentFile`. The same command works as the service program and
arguments for Windows or macOS. Set `profile-tls-name` to a YAML list of DNS names or IP addresses.

Run native providers through a configured Amux client.

With `profile`, list each DNS name or IP address in `profile-tls-name` to serve HTTPS with a server certificate signed by the profile root. Clients must trust the public `root.pem` and connect using a configured name. Configure profile TLS names or a manual certificate/key pair. Keep all private keys in the profile directory.

For certificate authentication, add `secure-instance-id` to profile mode and reuse that administrator instance ID across restarts. Profile-managed TLS includes localhost, loopback IPs, and the system hostname (`hostname -f` when available). For remote enrollment, add any additional DNS names or IPs clients use with `profile-tls-name` and bind both listeners to reachable addresses. Restart the server to apply SAN changes. The public discovery listener serves the profile CA and secure port. It defaults to the secure listen host on port 24181 and can be set with `discovery-bind`.

For new installations that need to enroll desktop clients, use **Server** mode
in the desktop app to configure network hosting and generate peering keys.
On each client, choose **Client** and enter the server address and the key.
For an existing secure profile moved to a standalone service, reuse the enrolled
client manifests and credentials. Keep private keys readable only by their owner.

## Upgrade an existing deployment

Older deployments must be upgraded before they can resume work. A per-user
installer replaces the app bundle. A systemd service or container needs its
own binary or image update.

1. Stop the old gateway and connected desktops or headless workers. Closing
   a desktop window can leave its background processes running. For systemd,
   use `systemctl stop amux-client.service` and `systemctl stop amux-server.service`
   on their respective hosts. For Docker, stop the container and keep its state volume.
2. Back up the full server profile or the standalone SQLite database and its
   sidecar files, along with the service configuration and environment file.
   Keep each client's enrollment credentials, YAML files, and worker journal.
3. Install the same release on the server and clients. Translate standalone server
   launch flags into YAML starting from [amux-server.yml](amux-server.yml). Set
   exactly one of `database` or `profile` to the existing state location. Update
   the service command to `amux server --config /absolute/path/to/amux-server.yml`.
   For Docker, rebuild or replace the image and reuse the data volume and YAML
   mount. For systemd, reload the unit definitions with `systemctl daemon-reload`.
4. Start the gateway first and check its logs. Startup can upgrade SQLite and
   recover valid accepted work from the previous scheduler. Start the updated
   clients and confirm that they reconnect and see their projects and tasks.

For a same-host Rust-to-Go upgrade, retain the existing `application.sqlite`,
`security.sqlite`, installation CA certificate and key, and local administrator
instance ID by reusing the complete profile. Keep the original gateway hostname
and ports reachable and include the client-facing name in `profile-tls-name`.
Existing enrollments can reconnect using their saved credentials. To roll back
after the gateway has opened the database, restore the pre-upgrade backup
before starting the old binary.


## Docker

Build the standalone server image from the repository root:

```sh
docker build -t amux-server:local .
```

The image uses Go 1.26.11 to build `./cmd` with `CGO_ENABLED=0`, then copies the binary into a `scratch` runtime. It runs as UID/GID 65532 and keeps SQLite state under `/data`. The Go base image is version tagged; no immutable digest or security audit is claimed here.

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

For direct non-loopback access, save this configuration as `/etc/amux/amux-server.yml` on the host and mount it with the TLS files. Keep the API key in the host-side environment file.

```yaml
database: /data/amux.db
listen: 0.0.0.0:7913
tls-cert: /tls/server.crt
tls-key: /tls/server.key
```

```sh
docker run -d --name amux-server \
  --restart unless-stopped \
  --publish 7913:7913 \
  --env-file /etc/amux/server.env \
  --volume amux-data:/data \
  --volume /etc/amux/tls:/tls:ro \
  --volume /etc/amux/amux-server.yml:/etc/amux/amux-server.yml:ro \
  amux-server:local
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
sudo install -o root -g root -m 0644 deploy/amux-server.yml /etc/amux/amux-server.yml
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

To serve a non-loopback address directly, edit `/etc/amux/amux-server.yml` and set the listener and TLS files. For example:

```yaml
database: /var/lib/amux/amux.db
listen: 0.0.0.0:7913
tls-cert: /etc/amux/tls/server.crt
tls-key: /etc/amux/tls/server.key
```

Apply the settings with `sudo systemctl restart amux-server.service`.

Ensure the `amux` account can read both TLS files through ownership, a restricted group, or an ACL, while keeping the private key inaccessible to other users. Do not make it world-readable.

## Headless Linux clients

Use a dedicated service account with access to the Git checkouts and provider
executables. Enroll that account with an expiring peering key. Keep the
enrollment manifest and its private key readable only by the service account.

```sh
sudo install -d -m 0700 -o amux -g amux /var/lib/amux-client /var/lib/amux-client/.amux
```

Save the following configuration as `/var/lib/amux-client/.amux/config.yaml`,
owned by the `amux` account. Set the manifest path to the account's existing
enrollment.


```yaml
enrolled_profile: '/var/lib/amux-client/client-profile.json'
```

Save device settings alongside it as `/var/lib/amux-client/.amux/device.yaml`.

```yaml
worker_journal: '/var/lib/amux-client/workers.sqlite'
projects_directory: '/srv/repos'
projects:
  example: '/srv/repos/example'
providers: [codex]
```

If upgrading an existing service, keep its journal path and enter the existing
gateway project IDs and checkout paths. The client reuses matching saved pools
and their worker identities.

Install [amux-client.service](amux-client.service) under
`/etc/systemd/system`, run `systemctl daemon-reload`, then run
`systemctl enable --now amux-client`. Edit the YAML and use
`systemctl reload amux-client` to reload without restarting. Use
`journalctl -u amux-client` for the reload result.
The project’s Alerts tab shows which hosts last reported an unavailable checkout.
Keep one client per service account and machine for checkout reporting.

### Agent tools on desktop and headless hosts

Both entry points give Codex, Claude, and Antigravity the same project-scoped
stdio MCP server registered by the local Amux client. Task operations and `submit_work` use the Go gateway.
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

## YAML-configured headless client

Desktop, headless, and native MCP use `~/.amux/config.yaml` for gateway connectivity. After enrolling, create it with native,
absolute paths. Use single quotes around Windows paths so YAML preserves
backslashes.

```yaml
enrolled_profile: '/home/user/.amux/cluster/client-profile.json'
```

Keep device-specific settings in `~/.amux/device.yaml`.

```yaml
projects_directory: '/home/user/projects'
projects:
  amux: '/home/user/projects/amux'
providers: [codex, antigravity]
```

Point `enrolled_profile` at the manifest from your actual enrollment.
The client reads its gateway endpoint from the manifest unless you set
`gateway_endpoint`. For bearer authentication, set `gateway_endpoint` and
`api_key_env` in YAML.

Desktop and headless read the gateway's provider switches at startup. The
optional device `providers` list further limits eligible providers. Use an
empty list to disable all providers for that device.

On first startup, an existing `~/.amux/config.yml` is split into the two
new files if `config.yaml` is absent. The original file remains available
as a backup.

The client matches Git origins directly under `projects_directory` to
active gateway projects. If multiple checkouts share an origin, it uses the
first path in alphabetical order. Use `projects` to select a specific path
for a gateway project ID. Explicit entries in `project_urls` or `projects`
can register missing gateway projects. Discovery alone does not create them.
A project needs a local checkout before this client can run its tasks.

To stop a configured client from executing a project, list its gateway project
ID under `excluded_projects` in that client's `device.yaml`:

```yaml
excluded_projects:
  sre: true
```

The checkout stays bound and reported, but this client runs no workers or local
chats for that project. Restart the client or send it `SIGHUP` on Unix. Existing workers drain after the reload.
The desktop menu action writes the same key. Switching gateways preserves the
exclusion entries when the desktop rewrites the same profile.

Start the client with `amux headless`. It keeps its worker journal alongside
the YAML as `workers.db`. Set `worker_journal` to the absolute path of an
existing journal to reuse it across desktop and service starts.
After editing projects or providers, restart the client or send it `SIGHUP` on Unix. Invalid configurations leave the last accepted bindings
in effect. Removed or moved workers drain their current runs before a replacement
can take work. Changing the enrollment manifest path requires a client restart.

When the desktop hosts a gateway, it writes a `hosted_gateway` section for its
local enrollment and starts the client. The client starts the gateway as a
separate `amux server` process. Closing the desktop leaves the client and
gateway running. Reopening the desktop reuses the services.

Run each independent client with its own home directory and worker journal.
