#!/usr/bin/env bash
# The stand scenario. Runs as root inside the stand container (test/stand/stand.sh), a fresh
# Ubuntu host with systemd and Docker, the working tree mounted read-only at /src:
#   install the last release and deploy the example project; upgrade to a build of the working
#   tree and check that releases, tokens, images and the site survive; npm login, publish and
#   install with yarn 4 and npm; a full clowctl deploy with the package check; token rotation;
#   roll back to the release and upgrade again.
set -euo pipefail

SRC=/src
# shellcheck source=../e2e/pebble.sh
. "$SRC/test/e2e/pebble.sh"

OLD="${STAND_OLD_VERSION:-v0.3.1}"
# STAND_NEW_VERSION=v0.4.0 upgrades to that release with install.sh (download, signature check)
# instead of a build of the working tree. v0.4.0 itself fails nginx_follows_main_conf: it needs
# a hand reload of nginx after the upgrade.
NEW="${STAND_NEW_VERSION:-}"
BASE=127-0-0-1.sslip.io
API="https://deploy.$BASE"
REG="$API/npm/"
SITE="shop.$BASE"
W=/root/stand
export CLOWDR_CONFIG="$W/clowctl-config" CLOWDR_TOKEN="" COREPACK_ENABLE_DOWNLOAD_PROMPT=0 HOME=/root

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() {
	printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2
	journalctl -u clowdrd -n 60 --no-pager >&2 || true
	restore_nginx_conf
	exit 1
}
# restore_nginx_conf: puts back and loads the nginx.conf nginx_follows_main_conf saved, if any,
# so a failed run leaves a working stand to look at.
restore_nginx_conf() {
	[ -f "$W/nginx.conf.good" ] || return 0
	cp "$W/nginx.conf.good" /etc/clowdr/nginx/nginx.conf &&
		docker exec clowdr-nginx nginx -s reload -c /etc/clowdr/nginx/nginx.conf >&2 || true
	rm -f "$W/nginx.conf.good"
}
within() { # within SECONDS CMD...: retry once a second
	local n=$1 i
	shift
	for i in $(seq "$n"); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	"$@"
}
site() { curl -fsS --max-time 5 "https://$SITE/$1"; }
served() { [ "$(site release.txt 2>/dev/null)" = "$1" ]; }
admin() { clowctl --context stand "$@"; }
cc() { CLOWDR_TOKEN="$PROJECT_TOKEN" clowctl --context stand -e prod "$@"; }
code() { # code TOKEN URL: HTTP status of a GET with that bearer token ("" = none)
	if [ -n "$1" ]; then
		curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" "$2"
	else
		curl -s -o /dev/null -w '%{http_code}' "$2"
	fi
}
is_version() { [ "$(server_version 2>/dev/null)" = "$1" ]; }
server_version() { curl -fsS "$API/v1/version" -H "Authorization: Bearer $ADMIN" | jq -r .version; }
release() { # release TAG [deploy flags]: deploy web with release.txt = TAG
	local tag=$1
	shift
	echo "$tag" >web/public/release.txt
	cc deploy web --tag "$tag" "$@"
	within 30 served "$tag" || fail "$tag is not served"
}
# npm_block on|off: the npm section of platform.yml; releases before npm refuse unknown keys.
npm_block() {
	sed -i '/^npm:/,/^  publish:/d' /etc/clowdr/platform.yml
	[ "$1" = off ] || printf 'npm:\n  publish: true\n' >>/etc/clowdr/platform.yml
}
# install_and_wait VERSION INSTALL_ARGS... (VERSION as /v1/version says it: 0.3.1). The version
# is read through nginx on the deploy host, so this also checks that nginx passes the API to
# the new daemon; see nginx_follows_main_conf for what it cannot see.
install_and_wait() {
	local want=$1
	shift
	sh "$SRC/install.sh" --domain "$BASE" "$@" >/dev/null
	within 60 is_version "$want" || fail "the server is not $want"
}
publish_version() { # publish_version VERSION: with clowctl npm publish from $W/pkg
	(cd "$W/pkg" && jq --arg v "$1" '.version = $v' package.json >p.json && mv p.json package.json && clowctl --context stand npm publish)
}

rm -rf "$W"
mkdir -p "$W"
cd "$W"

step "pebble as the ACME server"
pebble_up "deploy.$BASE"
pebble_platform "$BASE"

step "install $OLD"
sh "$SRC/install.sh" --version "$OLD" --domain "$BASE" >/dev/null
ADMIN="$(cat /var/lib/clowdr/admin-token)"
within 60 curl -fsS -o /dev/null "$API/v1/version" -H "Authorization: Bearer $ADMIN" || fail "no certificate for deploy.$BASE"
[ "$(server_version)" = "${OLD#v}" ] || fail "the server is not $OLD"
clowctl context add stand "$API" --token "$ADMIN"

step "deploy the example on $OLD"
admin project create shop --domains "$SITE" --memory 256m
PROJECT_TOKEN="$(admin token create --project shop --env prod --name stand)"
cp -r "$SRC/examples/shop" "$W/shop"
cd "$W/shop"
sed -i "s|https://deploy.example.com|$API|; s|shop.example.com|$SITE|" deploy.yml
# the stand's architecture (arm64 on Apple silicon); not behind Cloudflare, so Pebble certifies it
sed -i "s|^    port: 8080|    port: 8080\n    platform: linux/$(dpkg --print-architecture)\n    tls: direct|" deploy.yml
release v1
release v2
RELEASES_BEFORE="$(cc releases web -o json | jq length)"
TOKENS_BEFORE="$(admin token ls -o json | jq -r '.[].name' | sort | tr '\n' ' ')"

# upgrade: to $NEW from GitHub Releases, or to a build of the working tree.
upgrade() {
	npm_block on
	if [ -n "$NEW" ]; then
		install_and_wait "${NEW#v}" --version "$NEW"
	else
		install_and_wait stand-dev --binary "$W/clowdrd"
	fi
}
# nginx_follows_main_conf: the upgrade from $OLD reloads nginx anyway, because the project file
# of a direct-TLS domain changes too; a host whose projects render the same (no direct-TLS
# domains) gets nginx.conf changed alone, and 0.4.0 left nginx running the old one, so the API
# answered 404 through nginx until a hand reload. Put nginx on the deploy-host location of a
# release before 0.4.0 (no X-Clowdr-Via header), restart clowdrd and expect the API through
# nginx again with nothing else reloading it.
api_refused() { [ "$(code "$ADMIN" "$API/v1/version")" = 404 ]; }
nginx_follows_main_conf() {
	local want
	want="$(server_version)"
	cp /etc/clowdr/nginx/nginx.conf "$W/nginx.conf.good"
	sed -i '/X-Clowdr-Via deploy-host;/d' /etc/clowdr/nginx/nginx.conf
	docker exec clowdr-nginx nginx -s reload -c /etc/clowdr/nginx/nginx.conf || fail "hand reload failed"
	within 10 api_refused || fail "the API answers through a deploy host without X-Clowdr-Via: the check proves nothing"
	systemctl restart clowdrd
	within 60 is_version "$want" || fail "nginx still runs the old nginx.conf after clowdrd restarted (no reload)"
	grep -q 'X-Clowdr-Via deploy-host;' /etc/clowdr/nginx/nginx.conf || fail "clowdrd did not rewrite nginx.conf"
	rm -f "$W/nginx.conf.good"
}
if [ -n "$NEW" ]; then
	step "upgrade to $NEW"
else
	step "upgrade to the working tree"
	(cd "$SRC" && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-X main.version=stand-dev" -o "$W/clowdrd" ./cmd/clowdr)
fi
upgrade
step "the API through nginx after clowdrd restarts on a changed nginx.conf alone"
nginx_follows_main_conf
served v2 || fail "v2 is not served after the upgrade"
[ "$(cc releases web -o json | jq length)" = "$RELEASES_BEFORE" ] || fail "releases lost in the upgrade"
[ "$(admin token ls -o json | jq -r '.[].name' | sort | tr '\n' ' ')" = "$TOKENS_BEFORE" ] || fail "tokens changed in the upgrade"
cc rollback web
within 30 served v1 || fail "the previous image is gone after the upgrade (rollback to v1)"
release v3

step "npm login and publish"
clowctl --context stand npm login --scope @stand --scope 'npm:publish:@stand/*'
grep -q "$REG" /root/.yarnrc.yml || fail "~/.yarnrc.yml has no token"
mkdir -p "$W/pkg"
cat >"$W/pkg/package.json" <<EOF
{"name": "@stand/hello", "version": "1.0.0", "main": "index.js", "publishConfig": {"registry": "$REG"}}
EOF
echo 'module.exports = "hello from the stand";' >"$W/pkg/index.js"
(cd "$W/pkg" && clowctl --context stand npm publish)
if (cd "$W/pkg" && clowctl --context stand npm publish >"$W/out" 2>&1); then fail "a re-publish of 1.0.0 was accepted"; fi
grep -q "already published" "$W/out" || fail "re-publish: $(cat "$W/out")"
(cd "$W/pkg" && jq '.version = "1.0.1"' package.json >p.json && mv p.json package.json && npm publish --loglevel=warn >/dev/null)

step "install with yarn 4 and with npm"
mkdir -p "$W/app"
cd "$W/app"
echo '{"name": "app", "private": true}' >package.json
corepack use yarn@4 >/dev/null
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >.yarnrc.yml
clowctl --context stand npm setup --scope @stand
yarn add @stand/hello@^1.0.0 >/dev/null
[ "$(node -p 'require("@stand/hello")')" = "hello from the stand" ] || fail "yarn 4 did not install the package"
mkdir -p "$W/app-npm"
cd "$W/app-npm"
printf '@stand:registry=%s\n' "$REG" >.npmrc
npm init -y >/dev/null
npm install @stand/hello --no-audit --no-fund >/dev/null
[ "$(node -p 'require("@stand/hello")')" = "hello from the stand" ] || fail "npm did not install the package"

step "a project pinned to yarn 4.14"
mkdir -p "$W/app-414"
cd "$W/app-414"
echo '{"name": "app-414", "private": true, "packageManager": "yarn@4.14.0"}' >package.json
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >.yarnrc.yml
clowctl --context stand npm setup --scope @stand
[ "$(yarn --version)" = 4.14.0 ] || fail "corepack did not run yarn 4.14.0"
yarn add @stand/hello@^1.0.0 >/dev/null || fail "yarn 4.14 with the settings npm setup wrote"

step "a full clowctl deploy checks the packages"
cd "$W/shop"
echo '{"name": "shop", "private": true}' >package.json
corepack use yarn@4 >/dev/null
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >.yarnrc.yml
clowctl --context stand npm setup --scope @stand
yarn add @stand/hello@1.0.0 >/dev/null
echo v4 >web/public/release.txt
cc deploy --tag v4
within 30 served v4 || fail "v4 is not served"
for v in 1.1.0 1.2.0 1.3.0; do publish_version "$v" >/dev/null; done
if cc npm check >"$W/out" 2>&1; then fail "npm check passed with a deprecated version locked"; fi
grep -q "@stand/hello@1.0.0 is deprecated.*yarn up @stand/hello" "$W/out" || fail "npm check: $(cat "$W/out")"
echo v5 >web/public/release.txt
if cc deploy --tag v5 >"$W/out" 2>&1; then fail "deploy went ahead with a deprecated version locked"; fi
grep -q "deploy anyway with --allow-outdated" "$W/out" || fail "deploy: $(cat "$W/out")"
cc deploy --tag v5 --allow-outdated
within 30 served v5 || fail "v5 is not served"
cc releases web | grep -q "v5.*(allow-outdated)" || fail "the release does not show --allow-outdated"

step "token rotation"
OLD_TOKEN="$(grep -A1 "$REG" /root/.yarnrc.yml | sed -n 's/.*npmAuthToken: *//p' | tr -d '"')"
clowctl --context stand npm token rotate
NEW_TOKEN="$(grep -A1 "$REG" /root/.yarnrc.yml | sed -n 's/.*npmAuthToken: *//p' | tr -d '"')"
[ "$NEW_TOKEN" != "$OLD_TOKEN" ] || fail "rotation did not rewrite ~/.yarnrc.yml"
[ "$(code "$OLD_TOKEN" "$REG-/whoami")" = 200 ] || fail "the old token stopped before its grace ended"
[ "$(code "$NEW_TOKEN" "$REG-/whoami")" = 200 ] || fail "the new token is refused"
(cd "$W/app" && rm -rf node_modules && yarn cache clean --all >/dev/null && yarn install >/dev/null) || fail "yarn install with the rotated token"
clowctl --context stand npm token rotate --now
[ "$(code "$NEW_TOKEN" "$REG-/whoami")" = 401 ] || fail "--now did not stop the previous token"
TOKEN="$(grep -A1 "$REG" /root/.yarnrc.yml | sed -n 's/.*npmAuthToken: *//p' | tr -d '"')"

step "roll back to $OLD"
npm_block off
install_and_wait "${OLD#v}" --version "$OLD"
served v5 || fail "v5 is not served after the rollback"
[ "$(cc releases web -o json | jq length)" -ge 5 ] || fail "releases lost in the rollback"
release v6
[ "$(code "$TOKEN" "$REG-/whoami")" != 200 ] || fail "$OLD serves npm"

step "upgrade again"
upgrade
nginx_follows_main_conf
served v6 || fail "v6 is not served after the second upgrade"
[ "$(code "$TOKEN" "$REG-/whoami")" = 200 ] || fail "the npm token did not survive the rollback"
# $OLD rewrote the history with its v6 deploy: its own fields only, so the v5 mark is gone.
[ "$(cc releases web -o json | jq length)" -ge 6 ] || fail "releases lost in the second upgrade"
(cd "$W/app" && rm -rf node_modules && yarn cache clean --all >/dev/null && yarn install >/dev/null) || fail "yarn install after the second upgrade"
release v7 --allow-outdated

step "stand scenario passed"
