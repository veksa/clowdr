#!/usr/bin/env bash
# End-to-end test on a real Linux host (a GitHub Actions ubuntu runner or a throwaway VM; it
# installs clowdrd system-wide and takes ports 80, 443 and 7443):
#   Docker 29 with the containerd image store → install.sh with a local build → a project's
#   whole life cycle through clowctl, with traffic going through nginx.
#
#   sudo test/e2e/run.sh                          (builds with go)
#   sudo CLOWDR_BINARY=./clowdrd test/e2e/run.sh  (a prebuilt linux binary)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
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
trap 'code=$?; [ $code -eq 0 ] || dump; rm -f "$WORK/stop"; exit $code' EXIT

[ "$(id -u)" = 0 ] || fail "run as root"
command -v jq >/dev/null || fail "jq is required"

# get PATH: a request through nginx with the project's Host header.
get() { curl -fsS --max-time 5 -H "Host: $DOMAIN" "http://127.0.0.1/$1"; }
# eventually CMD...: retry for up to 30s (logs are flushed asynchronously).
eventually() {
	local i
	for i in $(seq 30); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	"$@"
}
has() { # has PATTERN CMD...: CMD's output matches PATTERN
	local pattern=$1
	shift
	local out
	out="$("$@" 2>&1)" || return 1
	grep -q -- "$pattern" <<<"$out"
}

not_served() { [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Host: $DOMAIN" http://127.0.0.1/release.txt)" != 200 ]; }
no_containers() { [ -z "$(docker ps -aq --filter label=clowdr.project=shop --filter label=clowdr.role=service)" ]; }

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
load_stop() { # load_stop WHAT: fails if any request failed
	touch "$WORK/stop"
	wait "$LOAD_PID"
	read -r total failed <"$WORK/load"
	echo "$1: $total requests, $failed failed"
	[ "$total" -gt 10 ] || fail "$1: load generator made only $total requests"
	[ "$failed" -eq 0 ] || fail "$1: $failed of $total requests failed"
}

step "docker 29+ with the containerd image store"
major="$(docker version --format '{{.Server.Version}}' 2>/dev/null | cut -d. -f1 || true)"
if [ "${major:-0}" -lt 29 ]; then
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

step "project and token"
clowctl --context e2e project create shop --domains "$DOMAIN" --memory 256m
clowctl --context e2e project ls
TOKEN="$(clowctl --context e2e token create --project shop --env prod --name e2e)"
[ -n "$TOKEN" ] || fail "empty project token"
export CLOWDR_TOKEN="$TOKEN"
if clowctl --context e2e project create other --domains other.example.com 2>/dev/null; then
	fail "a project token created a project"
fi

cp -r "$ROOT/examples/shop" "$WORK/shop"
cd "$WORK/shop"
cc() { clowctl --context e2e -e prod "$@"; }
release() { # release TAG: deploy web with release.txt = TAG
	echo "$1" >web/public/release.txt
	cc deploy web --tag "$1"
}

step "first deploy"
release v1
[ "$(get release.txt)" = v1 ] || fail "v1 is not served"
cc ps
cc routes
has "$DOMAIN" cc routes || fail "routes do not list $DOMAIN"

step "deploy under load: no failed requests"
load_start
release v2
load_stop "deploy v2"
[ "$(get release.txt)" = v2 ] || fail "v2 is not served"
[ "$(cc releases web -o json | jq length)" -ge 2 ] || fail "releases: expected 2"

step "a release that never becomes healthy keeps the old one serving"
rm web/public/health
load_start
if cc deploy web --tag v3-broken; then
	fail "an unhealthy release was switched to"
fi
load_stop "failed deploy v3"
[ "$(get release.txt)" = v2 ] || fail "v2 is not served after the failed deploy"
echo ok >web/public/health
has v3-broken cc releases web || fail "the failed release is not in the history"

step "rollback"
cc rollback web
[ "$(get release.txt)" = v1 ] || fail "rollback did not bring back v1"

step "exec"
[ "$(cc exec web -- cat /www/release.txt)" = v1 ] || fail "exec: wrong release in the container"

step "scale"
load_start
cc scale web=2
load_stop "scale to 2"
[ "$(cc status web -o json | jq '.containers | length')" -eq 2 ] || fail "expected 2 containers"
for _ in $(seq 10); do get release.txt >/dev/null; done
cc scale web=1

step "restart under load"
load_start
cc restart web
load_stop "restart"

step "clowdrd restart does not touch traffic"
load_start
systemctl restart clowdrd
eventually clowctl --context e2e version
load_stop "clowdrd restart"
[ "$(get release.txt)" = v1 ] || fail "v1 is not served after clowdrd restart"

step "logs, access log and events"
get release.txt >/dev/null
eventually has GET cc logs web --since 10m
eventually has /release.txt cc access "$DOMAIN" --since 10m
eventually has deploy cc events
cc top || true

step "stop and start"
cc stop web
eventually not_served || fail "a stopped service still answers"
cc start web
eventually get release.txt
[ "$(get release.txt)" = v1 ] || fail "v1 is not served after start"

step "diff and gc"
cc diff || true
CLOWDR_TOKEN= clowctl --context e2e gc

step "remove the service"
cc rm web --yes
eventually not_served || fail "a removed service still answers"
eventually no_containers || fail "containers left after rm"

step "e2e passed"
