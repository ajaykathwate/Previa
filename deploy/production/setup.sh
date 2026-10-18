#!/usr/bin/env bash
# Prepare a fresh Debian 12 / Ubuntu 24.04 host for Previa
# (docs/25-production-deployment.md#fresh-host-setup). Run as root from a
# checkout of the release:
#
#   sudo deploy/production/setup.sh
#
# It is idempotent and never overwrites an existing /etc/previa/previa.env.
# It installs Docker Engine, age, rclone, Tailscale (for SSH; joining the
# tailnet is the operator's `tailscale up`) and lazydocker (a terminal view of
# containers and logs); allows the user namespaces that rootless image builds
# need (Ubuntu 24.04); opens only SSH, HTTP and HTTPS, keeping SSH on
# Tailscale only once the operator has moved it there; creates the
# directories and file modes Previa expects; generates the secrets it can
# (session, database, webhook, repository-secrets key); and installs the
# nightly backup timer. Values that come from outside (GitHub App, domains,
# DNS token, backup recipient, Sentry DSN) are left for the operator.
set -Eeuo pipefail
umask 022

[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
# shellcheck source=/dev/null
. /etc/os-release
case "${ID:-}" in
debian | ubuntu) ;;
*) echo "supported hosts: Debian 12 or Ubuntu 24.04 (found ${PRETTY_NAME:-unknown})" >&2; exit 1 ;;
esac

REPO=$(cd "$(dirname "$(readlink -f "$0")")/../.." && pwd)
CONFIG=/etc/previa
STATE=/var/lib/previa
APP_UID=65532 # the images' non-root user
# lazydocker release and the SHA-256 of its Linux archives (its checksums.txt).
LAZYDOCKER_VERSION=0.25.2
declare -A LAZYDOCKER_SHA256=(
	[x86_64]=0d9dbfc26068b218e7ed84b104748cadc6e3cf733c0afd35465306fb39b9523c
	[arm64]=005c38b685aaa557e7d646d83a3dadb5024340eeed8c6a2e1949eee6f530de23
)

echo "== packages"
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg ufw age rclone openssl >/dev/null
if ! command -v docker >/dev/null; then
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
		>/etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin >/dev/null
fi
systemctl enable --now docker >/dev/null

# Tailscale carries SSH so port 22 can close to the internet
# (docs/25-production-deployment.md, Step 3). Installing it changes nothing
# until the operator runs `tailscale up`.
if ! command -v tailscale >/dev/null; then
	echo "== Tailscale"
	install -d -m 0755 /usr/share/keyrings
	curl -fsSL "https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}.noarmor.gpg" \
		-o /usr/share/keyrings/tailscale-archive-keyring.gpg
	curl -fsSL "https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}.tailscale-keyring.list" \
		-o /etc/apt/sources.list.d/tailscale.list
	apt-get update -qq
	apt-get install -y -qq tailscale >/dev/null
fi

if [[ $(lazydocker --version 2>/dev/null || true) != *"Version: ${LAZYDOCKER_VERSION}"* ]]; then
	echo "== lazydocker ${LAZYDOCKER_VERSION}"
	case $(uname -m) in
	x86_64) arch=x86_64 ;;
	aarch64) arch=arm64 ;;
	*) arch= ;;
	esac
	if [[ -n $arch ]]; then
		download=$(mktemp -d)
		curl -fsSL -o "$download/lazydocker.tar.gz" \
			"https://github.com/jesseduffield/lazydocker/releases/download/v${LAZYDOCKER_VERSION}/lazydocker_${LAZYDOCKER_VERSION}_Linux_${arch}.tar.gz"
		echo "${LAZYDOCKER_SHA256[$arch]}  $download/lazydocker.tar.gz" | sha256sum -c --quiet
		tar -xzf "$download/lazydocker.tar.gz" -C "$download" lazydocker
		install -m 0755 "$download/lazydocker" /usr/local/bin/lazydocker
		rm -rf "$download"
	else
		echo "no lazydocker build for $(uname -m); skipped"
	fi
fi

# Ubuntu 24.04 restricts unprivileged user namespaces, which every rootless
# BuildKit builder needs; without this, every preview build fails. Kernels
# without the switch (Debian 12) need nothing. The file reapplies it at boot.
if [[ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]]; then
	echo "== user namespaces for rootless builds"
	printf '%s\n' 'kernel.apparmor_restrict_unprivileged_userns=0' >/etc/sysctl.d/90-previa-rootless.conf
	sysctl -q -p /etc/sysctl.d/90-previa-rootless.conf
fi

echo "== firewall: SSH, HTTP and HTTPS only"
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
# Once SSH has moved to Tailscale (a rule on tailscale0, docs/25 Step 3), a
# rerun must not open it to the internet again.
if ufw status | grep -q 'on tailscale0'; then
	ufw allow in on tailscale0 to any port 22 proto tcp >/dev/null
else
	ufw allow OpenSSH >/dev/null
fi
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
# Docker publishes ports through its own iptables chains; Compose publishes
# only 80 and 443 (and Grafana on 127.0.0.1 when its optional profile runs).

echo "== directories"
install -d -m 0700 -o root -g root "$CONFIG"
install -d -m 0755 -o root -g root "$STATE"
# The worker (UID 65532) writes the route file; the router reads it.
install -d -m 0755 -o "$APP_UID" -g "$APP_UID" "$STATE/router"
# backup.sh writes the status file; the server reads it.
install -d -m 0755 -o root -g root "$STATE/backup"

echo "== settings"
if [[ ! -f $CONFIG/previa.env ]]; then
	postgres_password=$(openssl rand -hex 24)
	sed -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${postgres_password}|" \
		-e "s|REPLACE_WITH_POSTGRES_PASSWORD|${postgres_password}|" \
		-e "s|^SESSION_SECRET=.*|SESSION_SECRET=$(openssl rand -hex 32)|" \
		-e "s|^GITHUB_WEBHOOK_SECRET=.*|GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)|" \
		-e "s|^SECRETS_ENCRYPTION_KEYS=.*|SECRETS_ENCRYPTION_KEYS=1:$(openssl rand -base64 32)|" \
		-e "s|^GRAFANA_ADMIN_PASSWORD=.*|GRAFANA_ADMIN_PASSWORD=$(openssl rand -hex 16)|" \
		"$REPO/deploy/production/previa.env.example" >"$CONFIG/previa.env"
	echo "created $CONFIG/previa.env with generated secrets; fill in the rest"
else
	echo "$CONFIG/previa.env exists; left unchanged"
fi
docker_gid=$(stat -c %g /var/run/docker.sock)
sed -i "s|^DOCKER_GID=.*|DOCKER_GID=${docker_gid}|" "$CONFIG/previa.env"
chown root:root "$CONFIG/previa.env"
chmod 600 "$CONFIG/previa.env"
# Credential files: the App key is read by UID 65532 inside the containers;
# the DNS token by the router (root inside its container).
for file in github-app.pem dns-credential; do
	if [[ -f $CONFIG/$file ]]; then
		chmod 600 "$CONFIG/$file"
	fi
done
if [[ -f $CONFIG/github-app.pem ]]; then
	chown "$APP_UID:$APP_UID" "$CONFIG/github-app.pem"
fi

echo "== nightly backup timer"
cat >/etc/systemd/system/previa-backup.service <<UNIT
[Unit]
Description=Previa database and state backup
Wants=docker.service
After=docker.service

[Service]
Type=oneshot
ExecStart=$REPO/deploy/production/backup.sh
Nice=10
UNIT
cat >/etc/systemd/system/previa-backup.timer <<'UNIT'
[Unit]
Description=Nightly Previa backup

[Timer]
OnCalendar=*-*-* 02:30:00 UTC
RandomizedDelaySec=15min
Persistent=true

[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now previa-backup.timer >/dev/null

cat <<NEXT

Host prepared. Next (docs/25-production-deployment.md):
  1. Move SSH to Tailscale: tailscale up, then follow Step 3's checks before
     closing port 22 to the internet.
  2. Put the GitHub App key in $CONFIG/github-app.pem and the DNS token in
     $CONFIG/dns-credential, then rerun this script to fix their modes.
  3. Fill in the remaining values in $CONFIG/previa.env (domains, GitHub App,
     OAuth, ACME, BACKUP_AGE_RECIPIENT, BACKUP_REMOTE, SENTRY_DSN). Keep a copy
     of SECRETS_ENCRYPTION_KEYS and the age private key offline.
  4. Build the images from this checkout (Step 7):
     docker compose -f $REPO/deploy/production/compose.yml --env-file $CONFIG/previa.env --profile images build server worker egress
  5. docker compose -f $REPO/deploy/production/compose.yml --env-file $CONFIG/previa.env up -d --wait
NEXT
