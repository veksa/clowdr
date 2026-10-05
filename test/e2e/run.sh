#!/usr/bin/env bash
# End-to-end test on a real Linux host (a GitHub Actions ubuntu runner or a throwaway VM; it
# installs clowdrd system-wide and takes ports 80, 443 and 7443):
#   Docker 29 with the containerd image store → install.sh with a local build → a project's
#   whole life cycle through clowctl, with traffic going through nginx.
#
#   sudo test/e2e/run.sh                          (builds with go)
#   sudo CLOWDR_BINARY=./clowdrd test/e2e/run.sh  (a prebuilt linux binary)
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

setup
if clowctl --context e2e project create other --domains other.example.com 2>/dev/null; then
	fail "a project token created a project"
fi

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
cc top --once

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
