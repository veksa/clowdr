#!/usr/bin/env bash
# Chaos test on a real Linux host (nightly; same requirements as test/e2e/run.sh): break
# things in the middle of operations and check that clowdr comes back consistent.
#   - clowdrd killed (SIGKILL) while a release waits for health, while the old one drains,
#     and in a stop-first deploy after the old one was stopped;
#   - `docker push` interrupted from the client side and by a dying clowdrd;
#   - a service running out of memory;
#   - nginx crashing, stopped, removed;
#   - the disk at 95%, then full during a push;
#   - a "reboot" (docker, containerd and /run wiped) in the middle of a deploy.
# Consistent means: the last good release serves, no release is stuck in "deploying", no
# stray containers, and the next deploy works.
#
#   sudo test/chaos/run.sh
#   sudo CLOWDR_BINARY=./clowdrd test/chaos/run.sh
set -euo pipefail
# shellcheck source=../e2e/lib.sh
. "$(dirname "$0")/../e2e/lib.sh"
CHAOS="$(cd "$(dirname "$0")" && pwd)"

command -v ctr >/dev/null || fail "ctr (containerd) is required"
setup
cp "$CHAOS/deploy.yml" deploy.yml
cp -r "$CHAOS/hog" hog
REGISTRY=127.0.0.1:7443

admin() { CLOWDR_TOKEN= clowctl --context e2e "$@"; }
served() { [ "$(get release.txt)" = "$1" ]; }
must_serve() { served "$1" || fail "$2: expected $1, got $(get release.txt || echo nothing)"; }

# SIGKILL the daemon; systemd starts it again after 2s, as after a crash.
kill_clowdrd() { systemctl kill -s KILL --kill-whom=main clowdrd; }
clowdrd_back() { within 60 clowctl --context e2e version || fail "clowdrd did not come back"; }

# deploy_bg TAG [ARGS...]: deploy web in the background; its output goes to $WORK/deploy.out.
deploy_bg() {
	local tag=$1
	shift
	UPLOADS_BEFORE="$(uploads)"
	echo "$tag" >web/public/release.txt
	cc deploy web --tag "$tag" "$@" >"$WORK/deploy.out" 2>&1 &
	DEPLOY_PID=$!
}
# wait_for PATTERN: until the background deploy printed PATTERN.
wait_for() {
	local i
	for i in $(seq 1800); do
		if grep -q -- "$1" "$WORK/deploy.out"; then return 0; fi
		if ! kill -0 "$DEPLOY_PID" 2>/dev/null; then
			grep -q -- "$1" "$WORK/deploy.out" && return 0
			cat "$WORK/deploy.out"
			fail "the deploy ended before printing \"$1\""
		fi
		sleep 0.1
	done
	fail "the deploy never printed \"$1\""
}
deploy_done() { # the background deploy's outcome does not matter after a kill
	if wait "$DEPLOY_PID"; then echo "deploy finished"; else echo "deploy failed"; fi
	tail -n 5 "$WORK/deploy.out"
}
# uploads: active registry uploads (clowdr ingests in containerd). Those of an interrupted push
# stay until their lease expires.
uploads() { ctr -n moby content active 2>/dev/null | awk '/^clowdr-upload-/ { print $1 }' | sort || true; }
# wait_upload: until the background deploy's push sends a blob (a new upload appears).
wait_upload() {
	local i
	for i in $(seq 1800); do
		if [ -n "$(comm -13 <(echo "$UPLOADS_BEFORE") <(uploads))" ]; then return 0; fi
		kill -0 "$DEPLOY_PID" 2>/dev/null || fail "the deploy ended before the upload started"
		sleep 0.1
	done
	fail "no upload seen"
}

web_containers() { docker ps -aq --filter label=clowdr.project=shop --filter label=clowdr.service=web | wc -l; }
deploying() { cc releases web -o json | jq '[.[] | select(.status == "deploying")] | length'; }
# consistent: no release left deploying and exactly one web container (one replica).
consistent() { [ "$(deploying)" -eq 0 ] && [ "$(web_containers)" -eq 1 ]; }
check_consistent() { # check_consistent WHAT LIVE
	within 60 consistent || {
		cc releases web || true
		docker ps -a --filter label=clowdr.project=shop
		fail "$1: inconsistent after recovery"
	}
	must_serve "$2" "$1"
}

step "baseline: web and hog"
release v1
cc deploy hog --tag h1
must_serve v1 "first deploy"
LIVE=v1

step "clowdrd killed while the new release waits for health"
load_start
deploy_bg k1
wait_for "started shop-web-"
kill_clowdrd
deploy_done
clowdrd_back
load_stop "kill during health wait"
check_consistent "kill during health wait" "$LIVE"
cc releases web -o json | jq -e 'any(.[]; .version == "k1" and .status == "failed" and (.error | test("interrupted")))' >/dev/null ||
	fail "the interrupted release is not marked failed"
release v2
must_serve v2 "deploy after the kill"
LIVE=v2

step "clowdrd killed while the old release drains"
load_start
deploy_bg k2
wait_for "draining r"
kill_clowdrd
deploy_done
clowdrd_back
load_stop "kill during drain"
must_serve k2 "kill during drain"
LIVE=k2
check_consistent "kill during drain" "$LIVE"

step "clowdrd killed in a stop-first deploy after the old release was stopped"
cp deploy.yml "$WORK/deploy.yml"
sed -i 's/^    drain: 20s$/    drain: 20s\n    switch: stop-first/' deploy.yml
grep -q 'switch: stop-first' deploy.yml || fail "could not switch web to stop-first"
deploy_bg k3
wait_for "stop-first: stopping"
kill_clowdrd
deploy_done
clowdrd_back
# The old release is down until clowdrd starts it again: that is what stop-first means.
within 60 served "$LIVE" || fail "stop-first: $LIVE did not come back"
check_consistent "kill in stop-first" "$LIVE"
cp "$WORK/deploy.yml" deploy.yml

step "docker push interrupted by the client"
head -c 128M /dev/urandom >web/public/blob
load_start
deploy_bg big1
wait_upload
pkill -KILL -f "docker push $REGISTRY/shop/web:big1" || fail "no docker push to kill"
deploy_done
load_stop "interrupted push"
must_serve "$LIVE" "interrupted push"
cc deploy web --tag big1 --no-build
must_serve big1 "push after the interrupted one"
LIVE=big1

step "docker push interrupted by a dying clowdrd"
head -c 128M /dev/urandom >web/public/blob
load_start
deploy_bg big2
wait_upload
kill_clowdrd
deploy_done
clowdrd_back
load_stop "clowdrd killed during a push"
served big2 || {
	must_serve "$LIVE" "clowdrd killed during a push"
	cc deploy web --tag big2 --no-build
}
must_serve big2 "push after clowdrd came back"
LIVE=big2
check_consistent "clowdrd killed during a push" "$LIVE"
rm web/public/blob
release v3
LIVE=v3

step "a service runs out of memory"
# The limit must not spill into swap, or the service thrashes instead of being killed.
# CHAOS_ALLOW_SWAP=1 skips the step on hosts that ignore swap limits (docker in docker).
swap="$(cc exec hog -- cat /sys/fs/cgroup/memory.swap.max)"
if [ "$swap" != 0 ] && [ "${CHAOS_ALLOW_SWAP:-}" = 1 ]; then
	echo "SKIP: memory.swap.max is $swap on this host"
else
	[ "$swap" = 0 ] || fail "hog may swap: memory.swap.max is $swap"
	cc exec hog -- touch /tmp/oom
	within 60 has "out of memory" cc events || fail "no oom event"
	within 60 cc exec hog -- cat /www/health || fail "hog did not come back after the OOM"
fi
must_serve "$LIVE" "web after the OOM of hog"

step "nginx crashes"
kill -KILL "$(docker inspect -f '{{.State.Pid}}' clowdr-nginx)"
eventually served "$LIVE" || fail "nginx did not come back after a crash"
within 30 has "nginx exited" admin events --platform || fail "no nginx event"

step "nginx stopped"
docker stop clowdr-nginx
within 60 served "$LIVE" || fail "clowdrd did not start a stopped nginx"

step "nginx removed"
docker rm -f clowdr-nginx
within 60 served "$LIVE" || fail "clowdrd did not recreate a removed nginx"
has "nginx container was removed" admin events --platform || fail "no nginx removal event"

step "disk at 95%"
FILL=/var/lib/clowdr/chaos-fill
ON_EXIT+=("rm -f $FILL")
# fill_to BYTES: leave about BYTES free on the state directory's filesystem.
fill_to() {
	local avail
	rm -f "$FILL"
	avail="$(df -B1 --output=avail /var/lib/clowdr | tail -1)"
	[ "$avail" -gt "$1" ] || return 0
	fallocate -l $((avail - $1)) "$FILL"
	df -h /var/lib/clowdr
}
total="$(df -B1 --output=size /var/lib/clowdr | tail -1)"
fill_to $((total * 5 / 100))
within 90 has "disk free" admin events --platform || fail "no disk alert at 95%"
load_start
release v4
load_stop "deploy at 95% disk"
LIVE=v4

step "disk full during a push"
head -c 128M /dev/urandom >web/public/blob
echo full >web/public/release.txt
docker build -q -t "$REGISTRY/shop/web:full" web
fill_to $((64 << 20))
load_start
if timeout 600 cc deploy web --tag full --no-build; then
	fail "a push to a full disk succeeded"
fi
load_stop "push to a full disk"
must_serve "$LIVE" "push to a full disk"
clowctl --context e2e version >/dev/null || fail "clowdrd died on a full disk"
rm -f "$FILL"
cc deploy web --tag full --no-build
must_serve full "push after the disk was freed"
LIVE=full
check_consistent "disk full" "$LIVE"
rm web/public/blob
release v5
LIVE=v5

step "reboot in the middle of a deploy"
deploy_bg r1
wait_for "started shop-web-"
kill_clowdrd
systemctl stop clowdrd
deploy_done
systemctl stop docker.socket docker containerd
rm -rf /run/clowdr
start=$(date +%s)
systemctl start containerd docker
systemctl start clowdrd
within 90 served "$LIVE" || fail "$LIVE is not served after the reboot"
echo "serving again $(($(date +%s) - start))s after docker started"
clowdrd_back
check_consistent "reboot" "$LIVE"
within 60 cc exec hog -- cat /www/health || fail "hog did not come back after the reboot"

step "everything still works"
load_start
release v6
load_stop "deploy after the chaos"
cc ps
[ "$(deploying)" -eq 0 ] || fail "releases left deploying"
CLOWDR_TOKEN= clowctl --context e2e gc

step "chaos passed"
