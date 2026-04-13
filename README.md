# borgbox

Minimal HTTP API daemon for managing [Borg](https://www.borgbackup.org/) backup
repositories on a self-hosted server. Designed to be driven by a GUI frontend
so the end user never has to SSH into the server to create repos, register
keys, or see stats.

## Install

On your Linux server (the one that will host the repos):

```bash
curl -fsSL https://github.com/cprieto/borgbox/releases/latest/download/install.sh | sudo bash
```

The installer:
1. Detects OS/arch and downloads the correct binary from the latest release.
2. Verifies the SHA256 against `SHA256SUMS`.
3. Creates a system `borg` user (or reuses the existing one).
4. Creates `~borg/repos/` and `~borg/.ssh/authorized_keys` with the right
   permissions.
5. Writes `/etc/borgbox/{token,borgbox.env}` (token is random, 40 chars).
6. Installs and enables `borgbox.service` under systemd.
7. Prints the bearer token on stdout — save it for your client app.

Options:

```bash
sudo bash install.sh \
  --addr :9999 \
  --home /var/lib/borg \
  --version v0.1.0
```

Pin a specific version:

```bash
curl -fsSL https://github.com/cprieto/borgbox/releases/download/v0.1.0/install.sh | sudo bash
```

Install from a local `dist/` during development:

```bash
sudo ./install.sh --dist ./dist
```

## API

Base URL: `http://<host>:9999/api/v1`
Auth: `Authorization: Bearer <token>` (all endpoints except `/health`).

| Method   | Path                                  | Auth | Description                                       |
|----------|---------------------------------------|------|---------------------------------------------------|
| `GET`    | `/api/v1/health`                      | no   | Liveness check.                                   |
| `GET`    | `/api/v1/info`                        | yes  | Version, free disk, uptime, borg version.         |
| `GET`    | `/api/v1/system/stats`                | yes  | Hostname, host uptime, load avg, storage.         |
| `GET`    | `/api/v1/sessions`                    | yes  | Active `borg serve` processes (from `/proc`).     |
| `GET`    | `/api/v1/repos`                       | yes  | List all repos with `ssh_url` and `registered`.   |
| `POST`   | `/api/v1/repos`                       | yes  | Create repo directory and register pubkey.        |
| `POST`   | `/api/v1/repos/import`                | yes  | Register an existing on-disk repo (no mkdir).     |
| `GET`    | `/api/v1/repos/{name}`                | yes  | Detailed stats for one repo.                      |
| `DELETE` | `/api/v1/repos/{name}`                | yes  | Remove directory and authorized_keys entry.       |
| `GET`    | `/api/v1/repos/{name}/archives`       | yes  | List archives via `borg list --json`.             |
| `POST`   | `/api/v1/repos/{name}/break-lock`     | yes  | Run `borg break-lock` on a stuck repo (sync).     |
| `POST`   | `/api/v1/repos/{name}/check`          | yes  | Async `borg check`. Returns `job_id`.             |
| `POST`   | `/api/v1/repos/{name}/prune`          | yes  | Async `borg prune` with retention policy.         |
| `POST`   | `/api/v1/repos/{name}/compact`        | yes  | Async `borg compact`.                             |
| `GET`    | `/api/v1/jobs`                        | yes  | List recent jobs (in-memory, max 200).            |
| `GET`    | `/api/v1/jobs/{id}`                   | yes  | Status, exit code and last 200 log lines.         |
| `GET`    | `/api/v1/jobs/{id}/stream`            | yes  | Server-Sent Events stream of log + status.        |

### Create a repo

```http
POST /api/v1/repos
Content-Type: application/json
Authorization: Bearer <token>

{
  "name": "mac",
  "pubkey": "ssh-ed25519 AAAAC3... user@host"
}
```

`201 Created` returns the full `repoInfo` including `ssh_url` and
`registered: true`:

```json
{
  "name": "mac",
  "path": "/var/lib/borg/repos/mac",
  "ssh_url": "ssh://borg@hive.local/var/lib/borg/repos/mac",
  "registered": true,
  "size_bytes": 0,
  "initialized": false,
  "modified_at": "2026-04-13T14:32:08Z"
}
```

`409 Conflict` if a repo with that name already has a key registered. `400`
for invalid inputs.

After a successful create, the client app can run:

```bash
borg init --encryption=repokey-blake2 <ssh_url>
```

The daemon never calls `borg init` itself — it only prepares the server side
so the client's first `borg init` over SSH succeeds.

### List and import existing repos

`GET /api/v1/repos` walks the repo root and returns every directory it finds,
whether or not it was created by borgbox. The `registered` field tells the
client which ones already have an `authorized_keys` entry:

```json
[
  {
    "name": "mac",
    "ssh_url": "ssh://borg@hive.local/var/lib/borg/repos/mac",
    "registered": true,
    "initialized": true,
    "size_bytes": 12345678,
    "path": "/var/lib/borg/repos/mac",
    "modified_at": "2026-04-13T14:32:08Z"
  },
  {
    "name": "nas-archive",
    "ssh_url": "ssh://borg@hive.local/var/lib/borg/repos/nas-archive",
    "registered": false,
    "initialized": true,
    "size_bytes": 987654321,
    "path": "/var/lib/borg/repos/nas-archive",
    "modified_at": "2026-02-10T08:11:44Z"
  }
]
```

A client can import one of the unregistered repos (attach a new pubkey to the
existing directory, no mkdir) with:

```http
POST /api/v1/repos/import
Content-Type: application/json
Authorization: Bearer <token>

{
  "name": "nas-archive",
  "pubkey": "ssh-ed25519 AAAAC3... user@host"
}
```

Responses:
- `201 Created` with the `repoInfo` (now `registered: true`).
- `404 Not Found` if the directory does not exist under the repo root.
- `400 Bad Request` if the directory exists but is not a borg repo (missing
  `config` file).
- `409 Conflict` if an `authorized_keys` entry tagged `# borgbox:<name>`
  already exists.

### List archives

```http
GET /api/v1/repos/mac/archives
Authorization: Bearer <token>
X-Borg-Passphrase: <passphrase>      # only for encrypted repos
```

Returns the array of archives reported by `borg list --json`. The header is
discarded after the call — borgbox never persists passphrases.

### Maintenance jobs (async)

`check`, `prune` and `compact` return `202 Accepted` with a `job_id`. Poll
`/api/v1/jobs/{id}` to follow progress.

```http
POST /api/v1/repos/mac/check
Content-Type: application/json
Authorization: Bearer <token>

{ "repair": false, "verify_data": false, "passphrase": "..." }
```

```http
POST /api/v1/repos/mac/prune
Content-Type: application/json
Authorization: Bearer <token>

{
  "keep_daily":   7,
  "keep_weekly":  4,
  "keep_monthly": 12,
  "keep_yearly":  2,
  "dry_run":      false,
  "passphrase":   "..."
}
```

```http
POST /api/v1/repos/mac/compact
Content-Type: application/json
Authorization: Bearer <token>

{ "passphrase": "..." }
```

Job record:

```json
{
  "id": "j_8f2a4c",
  "repo": "mac",
  "kind": "check",
  "status": "running",
  "started_at": "2026-04-13T10:02:17Z",
  "finished_at": "",
  "exit_code": -1,
  "dry_run": false,
  "log_tail": ["Starting repository check", "..."]
}
```

`status` is one of `queued`, `running`, `done`, `error`. `exit_code` is `-1`
while the job is still queued or running. The job registry is in-memory only,
capped at 200 jobs (oldest finished are evicted first), and is wiped on
borgbox restart.

### Streaming job output (SSE)

For long-running `check`/`prune`/`compact` jobs, the admin UI can subscribe to
a live feed instead of polling `/jobs/{id}`:

```http
GET /api/v1/jobs/j_8f2a4c/stream
Accept: text/event-stream
Authorization: Bearer <token>
```

The response is a `text/event-stream`. On connect, borgbox replays the current
`log_tail` and emits the current `status`, then keeps the connection open and
pushes new events as the underlying `borg` process writes to stdout/stderr.
When the job finishes, a final `status` event is emitted followed by `end`:

```
event: log
data: {"type":"log","line":"Starting repository check","status":"","exit_code":0}

event: log
data: {"type":"log","line":"finished segment check","status":"","exit_code":0}

event: status
data: {"type":"status","line":"","status":"done","exit_code":0}

event: end
data: {}
```

Comments (`: ping`) are sent every 15s as keep-alive. `404` if the job ID is
unknown (jobs older than the 200-entry cap may have been evicted).

## What borgbox is *not*

- Not a borg server. `borg serve` is invoked by sshd via a forced command in
  `authorized_keys`, using the system's `/usr/bin/borg` binary. borgbox only
  manages the filesystem layout and the key file.
- No TLS out of the box. Put it behind Tailscale, Caddy, or nginx if it needs
  to leave the LAN.
- No multi-user, quotas, or multi-key-per-repo yet.

## Security model

- Runs as the unprivileged `borg` user, not root.
- systemd unit enforces `ProtectSystem=strict`, `NoNewPrivileges`, `ReadWritePaths`
  limited to the repo root and SSH dir.
- Each key line in `authorized_keys` has a `command="borg serve --restrict-to-repository ..."`
  prefix plus `restrict` (no PTY, no forwarding, no X11).
- Repo names are validated against `^[a-z0-9][a-z0-9-]{0,62}$`.
- SSH public keys are validated by type prefix before being written.
- Delete removes *only* the line tagged `# borgbox:<name>`, so manually-added
  keys are safe.

## Building locally

```bash
make build    # native binary
make dist     # cross-compile for linux/darwin × amd64/arm64
```

Cross-compile outputs land in `dist/` with a `SHA256SUMS` file, mirroring what
goreleaser produces in CI.

## Releasing

Tag the commit and push:

```bash
git tag v0.3.0
git push --tags
```

GitHub Actions runs goreleaser, which compiles all 4 targets, creates the
GitHub release, uploads the binaries, writes `SHA256SUMS`, and attaches
`install.sh` so the `curl | sudo bash` URL keeps working for the new version.

## Configuration

The daemon reads these env vars (also accepted as CLI flags):

| Variable                  | Default                  | Description                                   |
|---------------------------|--------------------------|-----------------------------------------------|
| `BORGBOX_ADDR`            | `:9999`                  | Listen address.                               |
| `BORGBOX_REPO_ROOT`       | `~borg/repos`            | Where repo directories live.                  |
| `BORGBOX_AUTH_KEYS`       | `~borg/.ssh/authorized_keys` | File to append SSH keys to.               |
| `BORGBOX_TOKEN`           | *(required)*             | Bearer token. Prefix with `@` to read a file. |
| `BORGBOX_SSH_HOST`        | `hostname`               | Hostname used to build `ssh_url` in repo responses. |
| `BORGBOX_SSH_USER`        | current user             | User used in `ssh_url` (usually `borg`).      |
| `BORGBOX_SSH_PORT`        | `22`                     | Port included in `ssh_url` when non-default.  |

The installer writes these to `/etc/borgbox/borgbox.env`.

## License

MIT. See [LICENSE](LICENSE).
