#!/bin/sh
# clowdr server install: curl -fsSL https://github.com/veksa/clowdr/releases/latest/download/install.sh | sh
# Options (sh -s -- ...): --domain example.com  --version v0.1.0
#   --binary ./clowdrd   install a local build instead of downloading a release
set -eu

REPO="${CLOWDR_REPO:-veksa/clowdr}"
VERSION="latest"
DOMAIN=""
BINARY=""

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) DOMAIN="$2"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--version) VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--binary) BINARY="$2"; shift 2 ;;
	--binary=*) BINARY="${1#*=}"; shift ;;
	*) echo "unknown option $1" >&2; exit 2 ;;
	esac
done

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root (sudo sh)"
[ "$(uname -s)" = Linux ] || die "the server runs on Linux"

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

command -v docker >/dev/null 2>&1 || die "docker is not installed: curl -fsSL https://get.docker.com | sh"
DOCKER_VERSION="$(docker version --format '{{.Server.Version}}' 2>/dev/null)" || die "docker is not running"
DOCKER_MAJOR="${DOCKER_VERSION%%.*}"
[ "$DOCKER_MAJOR" -ge 29 ] 2>/dev/null || die "Docker $DOCKER_VERSION is too old, clowdr needs 29+: curl -fsSL https://get.docker.com | sh"
if ! docker info --format '{{json .DriverStatus}}' | grep -q 'io.containerd.snapshotter'; then
	die 'docker does not use the containerd image store: add {"features": {"containerd-snapshotter": true}} to /etc/docker/daemon.json, then systemctl restart docker'
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || die "$BINARY not found"
	say "installing $BINARY"
	cp "$BINARY" "$TMP/clowdrd"
else
	if [ "$VERSION" = latest ]; then
		BASE="https://github.com/$REPO/releases/latest/download"
	else
		BASE="https://github.com/$REPO/releases/download/$VERSION"
	fi
	say "downloading clowdrd ($ARCH, $VERSION)"
	curl -fsSL "$BASE/clowdrd_linux_$ARCH" -o "$TMP/clowdrd"
	curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
	EXPECTED="$(grep " clowdrd_linux_$ARCH\$" "$TMP/checksums.txt" | cut -d' ' -f1)"
	ACTUAL="$(sha256sum "$TMP/clowdrd" | cut -d' ' -f1)"
	[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for clowdrd_linux_$ARCH"
fi

install -m 0755 "$TMP/clowdrd" /usr/local/bin/clowdrd
ln -sf /usr/local/bin/clowdrd /usr/local/bin/clowctl

mkdir -p /etc/clowdr
if [ -n "$DOMAIN" ]; then
	if [ -f /etc/clowdr/platform.yml ]; then
		if grep -q '^domain:' /etc/clowdr/platform.yml; then
			sed -i "s/^domain:.*/domain: $DOMAIN/" /etc/clowdr/platform.yml
		else
			sed -i "1i domain: $DOMAIN" /etc/clowdr/platform.yml
		fi
	else
		printf 'domain: %s\n' "$DOMAIN" >/etc/clowdr/platform.yml
	fi
fi

cat >/etc/systemd/system/clowdrd.service <<'EOF'
[Unit]
Description=clowdr deploy daemon
Documentation=https://github.com/veksa/clowdr
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/clowdrd
Restart=always
RestartSec=2
# The daemon is not in the traffic path: nginx keeps serving while it restarts.
KillMode=process
TimeoutStopSec=60
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable clowdrd >/dev/null 2>&1
systemctl restart clowdrd

say "waiting for clowdrd"
i=0
while [ ! -s /var/lib/clowdr/admin-token ]; do
	i=$((i + 1))
	if [ $i -gt 60 ]; then
		journalctl -u clowdrd -n 30 --no-pager >&2 || true
		die "clowdrd did not start"
	fi
	sleep 1
done
TOKEN="$(cat /var/lib/clowdr/admin-token)"

if [ -n "$DOMAIN" ]; then
	SERVER="https://deploy.$DOMAIN"
	EXTRA=""
else
	IP="$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | cut -d' ' -f1)"
	SERVER="https://$IP:7443"
	j=0
	while [ ! -s /var/lib/clowdr/tls/fingerprint ] && [ $j -lt 20 ]; do j=$((j + 1)); sleep 1; done
	EXTRA=" --fingerprint $(cat /var/lib/clowdr/tls/fingerprint)"
fi

say "clowdr is running."
echo
echo "On your machine:"
echo "  clowctl context add $(hostname) $SERVER --token $TOKEN$EXTRA"
echo
echo "The admin token is stored in /var/lib/clowdr/admin-token (root only)."
