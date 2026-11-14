# Pebble, Let's Encrypt's test CA, as the ACME server of a test host, so the deploy host gets a
# real certificate through clowdrd's own ACME flow and clients check it like any other.
# Shared by test/e2e/npm.sh and the local stand (test/stand). Needs root and docker.
# shellcheck shell=bash

PEBBLE_IMAGE="${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}"
# PEBBLE_ROOT is the root that signs the certificates Pebble issues (new on every Pebble start).
PEBBLE_ROOT=/usr/local/share/ca-certificates/clowdr-test-pebble-root.crt

# pebble_up DEPLOY_HOST: starts Pebble (every challenge valid), trusts its API certificate and
# its issuing root system-wide (curl, clowdrd, clowctl) and for docker pushes to DEPLOY_HOST.
# Node clients get the root through NODE_EXTRA_CA_CERTS.
pebble_up() {
	local host=$1 i
	docker rm -f clowdr-test-pebble >/dev/null 2>&1 || true
	docker run -d --name clowdr-test-pebble -e PEBBLE_VA_ALWAYS_VALID=1 -e PEBBLE_WFE_NONCEREJECT=0 \
		-p 127.0.0.1:14000:14000 -p 127.0.0.1:15000:15000 "$PEBBLE_IMAGE" >/dev/null
	docker cp clowdr-test-pebble:/test/certs/pebble.minica.pem /usr/local/share/ca-certificates/clowdr-test-pebble-minica.crt
	for i in $(seq 60); do
		if curl -fsS --cacert /usr/local/share/ca-certificates/clowdr-test-pebble-minica.crt \
			https://localhost:15000/roots/0 -o "$PEBBLE_ROOT" 2>/dev/null; then
			break
		fi
		[ "$i" -lt 60 ] || { echo "pebble did not start" >&2; return 1; }
		sleep 1
	done
	update-ca-certificates >/dev/null 2>&1
	# dockerd read the system roots when it started: certs.d is read per request.
	mkdir -p "/etc/docker/certs.d/$host"
	cp "$PEBBLE_ROOT" "/etc/docker/certs.d/$host/ca.crt"
	export NODE_EXTRA_CA_CERTS="$PEBBLE_ROOT"
}

# pebble_platform DOMAIN: a platform.yml for clowdrd that takes certificates from Pebble.
pebble_platform() {
	mkdir -p /etc/clowdr
	(umask 077 && printf 'domain: %s\nacme_directory: https://localhost:14000/dir\n' "$1" >/etc/clowdr/platform.yml)
}
