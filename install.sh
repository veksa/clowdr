#!/bin/sh
# clowdr server install: curl -fsSL https://github.com/veksa/clowdr/releases/latest/download/install.sh | sh
# Options (sh -s -- ...): --domain example.com  --version v0.1.0
#   --binary ./clowdrd   install a local build instead of downloading a release
set -eu

# Everything runs from main, called on the last line: a download cut short by curl | sh runs nothing.
main() {

REPO="${CLOWDR_REPO:-veksa/clowdr}"
# RELEASE_KEY is the base64 ed25519 public key that signs checksums.txt (also in internal/cli/selfupdate.go).
RELEASE_KEY="${CLOWDR_RELEASE_KEY:-trLPx8MgJq3bXtfnnh2STD05iZqqL8msJ8UFlUJBbDU=}"
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

case "$DOMAIN" in
*[!a-z0-9.-]* | .* | *. | *..*) die "invalid domain $DOMAIN (lowercase, like example.com)" ;;
esac
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
		# The tag is part of what is signed, so resolve it: /releases/latest redirects to it.
		VERSION="$(curl -fsSI "https://github.com/$REPO/releases/latest" | tr -d '\r' | sed -n 's|^[Ll]ocation: .*/releases/tag/||p')"
		[ -n "$VERSION" ] || die "cannot find the latest release of $REPO"
	fi
	case "$VERSION" in *[!A-Za-z0-9._-]*) die "invalid version $VERSION" ;; esac
	BASE="https://github.com/$REPO/releases/download/$VERSION"
	say "downloading clowdrd ($ARCH, $VERSION)"
	curl -fsSL "$BASE/clowdrd_linux_$ARCH" -o "$TMP/clowdrd"
	curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
	# checksums.txt must be signed by the release key (the same one clowctl self-update checks),
	# so a replaced release asset cannot get root on the server. The signature covers the tag
	# too ("<tag>\n" + checksums.txt): another release's files do not pass for this one. A fork
	# sets CLOWDR_RELEASE_KEY.
	curl -fsSL "$BASE/checksums.txt.sig" -o "$TMP/checksums.txt.sig" || die "release $VERSION has no checksums.txt.sig"
	command -v openssl >/dev/null 2>&1 || die "openssl is needed to verify the release signature"
	printf -- '-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA%s\n-----END PUBLIC KEY-----\n' "$RELEASE_KEY" >"$TMP/release.pub"
	{ printf '%s\n' "$VERSION"; cat "$TMP/checksums.txt"; } >"$TMP/signed"
	openssl pkeyutl -verify -pubin -inkey "$TMP/release.pub" -rawin -in "$TMP/signed" \
		-sigfile "$TMP/checksums.txt.sig" >/dev/null 2>&1 ||
		die "checksums.txt signature does not match the release key (openssl 3 is required to verify it)"
	EXPECTED="$(grep " clowdrd_linux_$ARCH\$" "$TMP/checksums.txt" | cut -d' ' -f1)"
	ACTUAL="$(sha256sum "$TMP/clowdrd" | cut -d' ' -f1)"
	[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for clowdrd_linux_$ARCH"
fi

install -m 0755 "$TMP/clowdrd" /usr/local/bin/clowdrd
ln -sf /usr/local/bin/clowdrd /usr/local/bin/clowctl

mkdir -p /etc/clowdr
if [ -n "$DOMAIN" ]; then
	if [ ! -f /etc/clowdr/platform.yml ]; then
		# the full default (addons, logs, limits), as clowdrd writes it without a domain
		(umask 077 && /usr/local/bin/clowdrd -default-config >/etc/clowdr/platform.yml)
	fi
	if grep -q '^domain:' /etc/clowdr/platform.yml; then
		sed -i "s/^domain:.*/domain: $DOMAIN/" /etc/clowdr/platform.yml
	elif grep -q '^# domain:' /etc/clowdr/platform.yml; then
		sed -i "s/^# domain:.*/domain: $DOMAIN/" /etc/clowdr/platform.yml
	else
		sed -i "1i domain: $DOMAIN" /etc/clowdr/platform.yml
	fi
fi

cat >/etc/systemd/system/clowdrd.service <<'EOF'
[Unit]
Description=clowdr deploy daemon
Documentation=https://github.com/veksa/clowdr
After=docker.service network-online.target
# Not Requires: a docker stop (package upgrade, colima boot) would stop clowdrd for good, as
# Restart= does not apply to stops systemd itself makes. clowdrd reconnects to docker.
Wants=docker.service network-online.target

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
}

main "$@"
