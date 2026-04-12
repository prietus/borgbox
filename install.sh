#!/usr/bin/env bash
# Borgbox server installer.
#
# Usage:
#   curl -fsSL https://github.com/cprieto/borgbox/releases/latest/download/install.sh | sudo bash
#
# Options:
#   --addr :9999          Listen address (default :9999)
#   --home /var/lib/borg  Home dir for the borg user (default /var/lib/borg,
#                         or keeps existing home if user already exists)
#   --version v0.1.0      Specific version to install (default: latest)
#   --dist /path          Local dist/ directory (for dev; skips download)
#
# Environment variables:
#   BORGBOX_REPO=owner/repo      GitHub repo (default cprieto/borgbox)
#   BORGBOX_VERSION=v0.1.0       Same as --version
set -euo pipefail

REPO="${BORGBOX_REPO:-prietus/borgbox}"
VERSION="${BORGBOX_VERSION:-latest}"
ADDR=":9999"
BORG_HOME=""
LOCAL_DIST=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --addr)    ADDR="$2"; shift 2 ;;
        --home)    BORG_HOME="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --dist)    LOCAL_DIST="$2"; shift 2 ;;
        -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "must run as root (try: sudo $0 ...)" >&2
    exit 1
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
BINARY_NAME="borgbox-${OS}-${ARCH}"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if [[ -n "$LOCAL_DIST" ]]; then
    echo "installing from local dist: $LOCAL_DIST/$BINARY_NAME"
    [[ -f "$LOCAL_DIST/$BINARY_NAME" ]] || { echo "binary not found" >&2; exit 1; }
    cp "$LOCAL_DIST/$BINARY_NAME" "$TMP/borgbox"
else
    if [[ "$VERSION" == "latest" ]]; then
        VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
            | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
        [[ -n "$VERSION" ]] || { echo "could not resolve latest version" >&2; exit 1; }
    fi
    BASE="https://github.com/$REPO/releases/download/$VERSION"
    echo "downloading $BASE/$BINARY_NAME"
    curl -fsSL -o "$TMP/borgbox" "$BASE/$BINARY_NAME"

    if curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" 2>/dev/null; then
        ( cd "$TMP" && grep "$BINARY_NAME" SHA256SUMS | sha256sum -c - ) \
            || { echo "checksum verification failed" >&2; exit 1; }
        echo "checksum ok"
    else
        echo "warning: no SHA256SUMS file in release, skipping verification" >&2
    fi
fi
chmod +x "$TMP/borgbox"

if ! id borg >/dev/null 2>&1; then
    [[ -n "$BORG_HOME" ]] || BORG_HOME=/var/lib/borg
    useradd -r -m -d "$BORG_HOME" -s /bin/bash borg
else
    [[ -n "$BORG_HOME" ]] || BORG_HOME=$(getent passwd borg | cut -d: -f6)
fi

install -d -m 700 -o borg -g borg "$BORG_HOME/repos"
install -d -m 700 -o borg -g borg "$BORG_HOME/.ssh"
if [[ ! -f "$BORG_HOME/.ssh/authorized_keys" ]]; then
    touch "$BORG_HOME/.ssh/authorized_keys"
fi
chown borg:borg "$BORG_HOME/.ssh/authorized_keys"
chmod 600 "$BORG_HOME/.ssh/authorized_keys"

install -m 755 "$TMP/borgbox" /usr/local/bin/borgbox

install -d -m 755 /etc/borgbox
if [[ ! -f /etc/borgbox/token ]]; then
    TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | head -c 40)
    echo -n "$TOKEN" > /etc/borgbox/token
    chown root:borg /etc/borgbox/token
    chmod 640 /etc/borgbox/token
else
    TOKEN=$(cat /etc/borgbox/token)
fi

cat > /etc/borgbox/borgbox.env <<EOF
BORGBOX_ADDR=$ADDR
BORGBOX_REPO_ROOT=$BORG_HOME/repos
BORGBOX_AUTH_KEYS=$BORG_HOME/.ssh/authorized_keys
BORGBOX_TOKEN=@/etc/borgbox/token
EOF
chown root:borg /etc/borgbox/borgbox.env
chmod 640 /etc/borgbox/borgbox.env

cat > /etc/systemd/system/borgbox.service <<EOF
[Unit]
Description=Borgbox API daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=borg
Group=borg
EnvironmentFile=/etc/borgbox/borgbox.env
ExecStart=/usr/local/bin/borgbox
Restart=on-failure
RestartSec=5s

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$BORG_HOME/repos $BORG_HOME/.ssh
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictRealtime=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now borgbox.service

cat <<EOF

borgbox installed.

  Binary   : /usr/local/bin/borgbox
  Service  : systemctl status borgbox
  Config   : /etc/borgbox/borgbox.env
  Token    : /etc/borgbox/token
  Repos    : $BORG_HOME/repos
  Address  : $ADDR

  Bearer token (save this in your client app):

    $TOKEN

Test:
  curl -H "Authorization: Bearer \$(sudo cat /etc/borgbox/token)" http://localhost${ADDR}/api/v1/info

EOF
