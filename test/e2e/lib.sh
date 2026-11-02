	for h in "${ON_EXIT[@]}"; do eval "$h" || true; done
	[ -z "${LOAD_PID:-}" ] || kill "$LOAD_PID" 2>/dev/null || true
# Shared by the end-to-end and chaos tests: helpers, and `setup`, which installs clowdrd on
# this host (Docker 29 + containerd image store, install.sh, an admin context, project shop
# with a token) and leaves a copy of examples/shop in $WORK/shop as the current directory.
# shellcheck shell=bash

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
DOMAIN=shop.example.com
export CLOWDR_CONFIG="$WORK/clowctl-config"
export CLOWDR_TOKEN=""

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() {
	printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2
	exit 1
}
dump() {
	step "diagnostics"
	journalctl -u clowdrd -n 300 --no-pager || true
	docker ps -a || true
	ls -R /etc/clowdr/nginx || true
	cat /etc/clowdr/nginx/conf.d/*.conf || true
}
# on_exit: hooks run before the diagnostics/cleanup trap (e.g. freeing a filled disk).
ON_EXIT=()
cleanup() {
	local code=$? h
	for h in "${ON_EXIT[@]}"; do eval "$h" || true; done
	[ -z "${LOAD_PID:-}" ] || kill "$LOAD_PID" 2>/dev/null || true
	[ $code -eq 0 ] || dump
	rm -f "$WORK/stop"
	exit $code
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || fail "run as root"
command -v jq >/dev/null || fail "jq is required"

# get PATH: a request through nginx with the project's Host header.
get() { curl -fsS --max-time 5 -H "Host: $DOMAIN" "http://127.0.0.1/$1"; }
# within SECONDS CMD...: retry CMD once a second until it succeeds.
within() {
	local n=$1 i
	shift
	for i in $(seq "$n"); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	"$@"
}
# eventually CMD...: retry for up to 30s (logs are flushed asynchronously).
eventually() { within 30 "$@"; }
has() { # has PATTERN CMD...: CMD's output matches PATTERN
	local pattern=$1
	shift
	local out
	out="$("$@" 2>&1)" || return 1
	grep -q -- "$pattern" <<<"$out"
}

not_served() { [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Host: $DOMAIN" http://127.0.0.1/release.txt)" != 200 ]; }
no_containers() { [ -z "$(docker ps -aq --filter label=clowdr.project=shop --filter label=clowdr.role=service)" ]; }
no_store_containers() { [ -z "$(docker ps -aq --filter label=clowdr.project=store --filter label=clowdr.role=service)" ]; }

# Background load: one request at a time against the site, counting failures, until
# $WORK/stop appears. The result lands in $WORK/load as "<requests> <failures>".
load_start() {
	rm -f "$WORK/stop" "$WORK/load"
	(
		total=0 failed=0
		while [ ! -f "$WORK/stop" ]; do
			total=$((total + 1))
			curl -fsS -o /dev/null --max-time 5 -H "Host: $DOMAIN" http://127.0.0.1/release.txt || failed=$((failed + 1))
		done
		echo "$total $failed" >"$WORK/load"
	) &
	LOAD_PID=$!
	sleep 1
}
load_result() { # load_result WHAT: prints the counts, sets LOAD_TOTAL and LOAD_FAILED
	touch "$WORK/stop"
	wait "$LOAD_PID"
	read -r LOAD_TOTAL LOAD_FAILED <"$WORK/load"
	echo "$1: $LOAD_TOTAL requests, $LOAD_FAILED failed"
	[ "$LOAD_TOTAL" -gt 10 ] || fail "$1: load generator made only $LOAD_TOTAL requests"
}
load_stop() { # load_stop WHAT: fails if any request failed
	load_result "$1"
	[ "$LOAD_FAILED" -eq 0 ] || fail "$1: $LOAD_FAILED of $LOAD_TOTAL requests failed"
}

cc() { clowctl --context e2e -e prod "$@"; }
release() { # release TAG: deploy web with release.txt = TAG
	echo "$1" >web/public/release.txt
	cc deploy web --tag "$1"
}

setup() {
	step "docker 29+ with the containerd image store"
	local major
	major="$(docker version --format '{{.Server.Version}}' 2>/dev/null | cut -d. -f1 || true)"
	if [ "${major:-0}" -lt 29 ]; then
		# a fresh VM runs unattended-upgrades on boot and holds the dpkg lock for a while
		local i
		for i in $(seq 120); do
			fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock >/dev/null 2>&1 || break
			sleep 5
		done
		curl -fsSL https://get.docker.com | sh
	fi
	if ! docker info --format '{{json .DriverStatus}}' | grep -q io.containerd.snapshotter; then
		mkdir -p /etc/docker
		[ -s /etc/docker/daemon.json ] || echo '{}' >/etc/docker/daemon.json
		jq '.features["containerd-snapshotter"] = true' /etc/docker/daemon.json >"$WORK/daemon.json"
		mv "$WORK/daemon.json" /etc/docker/daemon.json
		systemctl restart docker
	fi
	docker version --format 'docker {{.Server.Version}}'

	step "build and install"
	BINARY="${CLOWDR_BINARY:-}"
	if [ -z "$BINARY" ]; then
		BINARY="$WORK/clowdrd"
		(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=e2e" -o "$BINARY" ./cmd/clowdr)
	fi
	sh "$ROOT/install.sh" --binary "$BINARY"
	ADMIN="$(cat /var/lib/clowdr/admin-token)"
	FP="$(cat /var/lib/clowdr/tls/fingerprint)"
	clowctl context add e2e https://127.0.0.1:7443 --token "$ADMIN" --fingerprint "$FP"
	clowctl --context e2e version
	clowctl --context e2e platform status
	# small hosts drop connections under load with the kernel's default conntrack table
	[ "$(sysctl -n net.netfilter.nf_conntrack_max)" -ge 131072 ] || fail "nf_conntrack_max was not raised"

	step "project and token"
	clowctl --context e2e project create shop --domains "$DOMAIN" --memory 256m
	clowctl --context e2e project ls
	TOKEN="$(clowctl --context e2e token create --project shop --env prod --name e2e)"
	[ -n "$TOKEN" ] || fail "empty project token"
	export CLOWDR_TOKEN="$TOKEN"

	cp -r "$ROOT/examples/shop" "$WORK/shop"
	cd "$WORK/shop"
}
