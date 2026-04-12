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

| Method   | Path                      | Auth | Description                                 |
|----------|---------------------------|------|---------------------------------------------|
| `GET`    | `/api/v1/health`          | no   | Liveness check.                             |
| `GET`    | `/api/v1/info`            | yes  | Version, free disk, uptime, borg version.   |
| `GET`    | `/api/v1/repos`           | yes  | List all repos with size and init state.    |
| `POST`   | `/api/v1/repos`           | yes  | Create repo directory and register pubkey.  |
| `GET`    | `/api/v1/repos/{name}`    | yes  | Detailed stats for one repo.                |
| `DELETE` | `/api/v1/repos/{name}`    | yes  | Remove directory and authorized_keys entry. |

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

`201 Created` on success. `409 Conflict` if a repo with that name already has
a key registered. `400` for invalid inputs.

After a successful create, the client app can run:

```bash
borg init --encryption=repokey-blake2 ssh://borg@<host>/./repos/mac
```

The daemon never calls `borg init` itself — it only prepares the server side
so the client's first `borg init` over SSH succeeds.

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
git tag v0.2.0
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

The installer writes these to `/etc/borgbox/borgbox.env`.
