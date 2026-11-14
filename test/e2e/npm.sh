#!/usr/bin/env bash
# End-to-end test of the npm registry on a real Linux host (a GitHub Actions ubuntu runner or a
# throwaway VM; it installs clowdrd system-wide, takes ports 80, 443, 14000 and 15000 and adds
# deploy.clowdr.test to /etc/hosts): install.sh with a domain, the deploy host's certificate
# from Pebble through clowdrd's own ACME flow, then real npm and yarn 4 through nginx and TLS,
# and the access matrix with curl.
#
#   sudo CLOWDR_BINARY=./clowdrd test/e2e/npm.sh
#
# Needs node (LTS) with npm and corepack, jq, zstd.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
# shellcheck source=pebble.sh
. "$(dirname "$0")/pebble.sh"

BASE=clowdr.test
HOST="deploy.$BASE"
API="https://$HOST"
REG="$API/npm/"
export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
# npm and yarn read their user config from here, never from the invoking user's home
export HOME="$WORK/home"
mkdir -p "$HOME"
TOKENS=() # every token handed out, for the log check

for c in node npm corepack zstdcat ip; do command -v "$c" >/dev/null || fail "$c is required"; done
grep -q " $HOST\$" /etc/hosts || echo "127.0.0.1 $HOST" >>/etc/hosts

admin() { clowctl --context npm "$@"; }
# FROM=() sends requests from this host's address; FROM=("${NS[@]}") from a second network
# namespace with its own address, which nginx sees as another client.
FROM=()
NS_NAME=clowdr-e2e-client
NS=(ip netns exec "$NS_NAME" curl --resolve "$HOST:443:10.213.0.1" --resolve "$HOST:80:10.213.0.1")
# status TOKEN URL [curl args]: HTTP status through nginx ("" = no token; 000 = connection
# closed); the body goes to $WORK/body and the headers to $WORK/headers.
status() {
	local tok=$1 url=$2
	shift 2
	local cmd=(curl)
	[ ${#FROM[@]} -eq 0 ] || cmd=("${FROM[@]}")
	[ -z "$tok" ] || set -- -H "Authorization: Bearer $tok" "$@"
	"${cmd[@]}" -s -o "$WORK/body" -D "$WORK/headers" -w '%{http_code}' "$@" "$url" || true
}
# same_as_missing WHAT TOKEN URL MISSING_URL [curl args]: URL answers exactly like a package that
# does not exist: status, headers (but Date) and body, byte for byte.
same_as_missing() {
	local what=$1 tok=$2 url=$3 missing=$4 a b
	shift 4
	a="$(status "$tok" "$url" "$@")"
	grep -iv '^date:' "$WORK/headers" >"$WORK/h1"
	cp "$WORK/body" "$WORK/b1"
	b="$(status "$tok" "$missing" "$@")"
	grep -iv '^date:' "$WORK/headers" >"$WORK/h2"
	[ "$a" = 404 ] || fail "$what: $url answered $a, want 404"
	[ "$a" = "$b" ] && cmp -s "$WORK/h1" "$WORK/h2" && cmp -s "$WORK/b1" "$WORK/body" ||
		fail "$what: differs from a missing package: $(diff "$WORK/h1" "$WORK/h2"; diff "$WORK/b1" "$WORK/body")"
	echo "  ok  $what → 404, same as a missing package"
}
ns_up() {
	ip netns add "$NS_NAME"
	ON_EXIT+=("ip netns del $NS_NAME; ip link del clowdr-e2e-h 2>/dev/null")
	ip link add clowdr-e2e-h type veth peer name clowdr-e2e-n
	ip link set clowdr-e2e-n netns "$NS_NAME"
	ip addr add 10.213.0.1/30 dev clowdr-e2e-h
	ip link set clowdr-e2e-h up
	ip netns exec "$NS_NAME" ip addr add 10.213.0.2/30 dev clowdr-e2e-n
	ip netns exec "$NS_NAME" ip link set clowdr-e2e-n up
	ip netns exec "$NS_NAME" ip link set lo up
	ip netns exec "$NS_NAME" ip route add default via 10.213.0.1
}
# denied WHAT TOKEN URL WANT [curl args]: the request gets a status matching WANT (e.g. 404 or
# "301|404") and no package data.
denied() {
	local what=$1 tok=$2 url=$3 want=$4 got
	shift 4
	got="$(status "$tok" "$url" "$@")"
	[[ $got =~ ^($want)$ ]] || fail "$what: $url answered $got, want $want: $(head -c 300 "$WORK/body")"
	# a document, or a tarball (gzip)
	if grep -q -e '"versions"' -e 'dist-tags' "$WORK/body" || [ "$(head -c2 "$WORK/body" | od -An -tx1 | tr -d ' ')" = 1f8b ]; then
		fail "$what: $url leaked data: $(head -c 300 "$WORK/body")"
	fi
	[ -z "$tok" ] || ! grep -qF "$tok" "$WORK/body" || fail "$what: the error body echoes the token"
	echo "  ok  $what → $got"
}
token_of() { # token_of NAME EXPIRES_IN SCOPE... : an npm token created through the admin API
	local name=$1 expires=$2
	shift 2
	jq -nc --arg n "$name" --arg e "$expires" '{name: $n, expires_in: $e, scopes: $ARGS.positional}' --args "$@" |
		curl -fsS -H "Authorization: Bearer $ADMIN" -d @- "$API/v1/npm/tokens" | jq -r .token
}
home_token() { grep -A1 -F "$REG" "$HOME/.yarnrc.yml" | sed -n 's/.*npmAuthToken: *//p' | tr -d '"'; }
set_version() { jq --arg v "$1" '.version = $v' package.json >p.json && mv p.json package.json; }

setup_docker
step "pebble and install with a domain"
pebble_up "$HOST"
pebble_platform "$BASE"
printf 'npm:\n  publish: true\n' >>/etc/clowdr/platform.yml
install_clowdrd --domain "$BASE"
TOKENS+=("$ADMIN")
within 60 curl -fsS -o /dev/null -H "Authorization: Bearer $ADMIN" "$API/v1/version" || fail "no certificate for $HOST"
clowctl context add npm "$API" --token "$ADMIN"
admin version

step "npm login, publish with npm and with clowctl"
admin npm login --scope @e2e --scope 'npm:publish:@e2e/*' --name e2e-machine
LOGIN="$(home_token)"
[ -n "$LOGIN" ] || fail "no token in ~/.yarnrc.yml"
TOKENS+=("$LOGIN")
grep -q "^//$HOST/npm/:_authToken=$LOGIN\$" "$HOME/.npmrc" || fail "no token in ~/.npmrc"
[ "$(npm whoami --registry "$REG")" = e2e-machine ] || fail "npm whoami"
mkdir -p "$WORK/pkg" && cd "$WORK/pkg"
cat >package.json <<EOF
{"name": "@e2e/hello", "version": "1.0.0", "main": "index.js", "publishConfig": {"registry": "$REG"}}
EOF
echo 'module.exports = "hello";' >index.js
npm publish --loglevel=warn
set_version 1.0.1
admin npm publish
# npm refuses on its own when the document lists the version; clowctl sends it and gets the 409
if npm publish --loglevel=warn >/dev/null 2>&1; then fail "npm re-published 1.0.1"; fi
if admin npm publish >"$WORK/republish" 2>&1; then fail "a re-publish was accepted"; fi
grep -q "already published" "$WORK/republish" || fail "re-publish: $(cat "$WORK/republish")"
admin npm audit --package @e2e/hello --action publish -o json | jq -e 'any(.[]; .status == 409)' >/dev/null ||
	fail "the audit log has no 409 publish"
set_version 2.0.0-beta.1
admin npm publish --tag beta

step "dist-tags"
npm dist-tag add @e2e/hello@1.0.0 stable --registry "$REG" >/dev/null
npm dist-tag ls @e2e/hello --registry "$REG" | tee "$WORK/tags"
grep -q '^stable: 1.0.0$' "$WORK/tags" || fail "dist-tag stable"
grep -q '^beta: 2.0.0-beta.1$' "$WORK/tags" || fail "dist-tag beta"
grep -q '^latest: 1.0.1$' "$WORK/tags" || fail "a beta moved latest"
npm dist-tag rm @e2e/hello stable --registry "$REG" >/dev/null

step "install into fresh projects with yarn 4 and npm"
mkdir -p "$WORK/app" && cd "$WORK/app"
echo '{"name": "app", "private": true}' >package.json
corepack use yarn@4 >/dev/null
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >.yarnrc.yml
admin npm setup --scope @e2e
yarn add @e2e/hello@1.0.0 >/dev/null
[ "$(node -p 'require("@e2e/hello")')" = hello ] || fail "yarn 4 install"
YARN_NPM_AUDIT_REGISTRY="$REG" yarn npm audit --environment production || fail "yarn npm audit"
admin npm check
# a project pinned to a yarn before 4.17, which rejects the scoped age gate setting
mkdir -p "$WORK/app-414" && cd "$WORK/app-414"
echo '{"name": "app-414", "private": true, "packageManager": "yarn@4.14.0"}' >package.json
printf 'nodeLinker: node-modules\nenableTelemetry: false\n' >.yarnrc.yml
admin npm setup --scope @e2e
[ "$(yarn --version)" = 4.14.0 ] || fail "corepack did not run yarn 4.14.0"
grep -q '"@e2e/\*"' .yarnrc.yml || fail "npm setup did not preapprove @e2e for yarn 4.14: $(cat .yarnrc.yml)"
yarn add @e2e/hello@1.0.1 >/dev/null || fail "yarn 4.14 install (a minute-old version, preapproved scope)"
[ "$(node -p 'require("@e2e/hello")')" = hello ] || fail "yarn 4.14 install"
mkdir -p "$WORK/app-npm" && cd "$WORK/app-npm"
printf '@e2e:registry=%s\n' "$REG" >.npmrc
npm init -y >/dev/null
npm install @e2e/hello --no-audit --no-fund >/dev/null
[ "$(node -p 'require("@e2e/hello/package.json").version')" = 1.0.1 ] || fail "npm install took the wrong version"

step "retention: newer versions deprecate the oldest"
cd "$WORK/pkg"
for v in 1.1.0 1.2.0 1.3.0; do set_version "$v" && admin npm publish >/dev/null; done
cd "$WORK/app"
yarn npm info @e2e/hello@1.0.0 --fields deprecated >"$WORK/yarn-out" 2>&1
grep -q "removed on .*upgrade to 1.3.0" "$WORK/yarn-out" || fail "yarn does not show the deprecation: $(cat "$WORK/yarn-out")"
cd "$WORK/app-npm"
npm install @e2e/hello@1.0.0 --no-audit --no-fund >"$WORK/npm-out" 2>&1
grep -q "deprecated @e2e/hello@1.0.0" "$WORK/npm-out" || fail "npm did not warn about the deprecated version: $(cat "$WORK/npm-out")"
cd "$WORK/app"
if admin npm check 2>"$WORK/check"; then fail "npm check passed with a deprecated version locked"; fi
grep -q "yarn up @e2e/hello" "$WORK/check" || fail "npm check: $(cat "$WORK/check")"

step "rotation and revocation"
admin npm token rotate
ROTATED="$(home_token)"
TOKENS+=("$ROTATED")
[ "$ROTATED" != "$LOGIN" ] || fail "rotation did not rewrite ~/.yarnrc.yml"
[ "$(status "$LOGIN" "$REG-/whoami")" = 200 ] || fail "the old token stopped within its grace"
[ "$(npm whoami --registry "$REG")" = e2e-machine ] || fail "npm whoami with the rotated token"
admin npm token rotate --now
CURRENT="$(home_token)"
TOKENS+=("$CURRENT")
[ "$(status "$ROTATED" "$REG-/whoami")" = 401 ] || fail "--now did not stop the previous token"
(cd "$WORK/app" && rm -rf node_modules && yarn cache clean --all >/dev/null && yarn install >/dev/null) || fail "yarn install with the current token"
admin npm token revoke e2e-machine
[ "$(status "$CURRENT" "$REG-/whoami")" = 401 ] || fail "a revoked token still works"
[ "$(status "$LOGIN" "$REG-/whoami")" = 401 ] || fail "revoking did not stop the token in its grace period"

step "access matrix over nginx and TLS"
READER="$(token_of reader 30d 'npm:read:@e2e/*')"
OTHER_PUB="$(token_of other-publisher 30d 'npm:publish:@other/*')"
SHORT="$(token_of short 1s 'npm:read:@e2e/*')"
GONE="$(token_of gone 30d 'npm:read:@e2e/*')"
curl -fsS -X DELETE -H "Authorization: Bearer $ADMIN" "$API/v1/npm/tokens/gone" >/dev/null
admin project create matrix --domains matrix.example.com --memory 64m >/dev/null
PROJECT="$(admin token create --project matrix --env prod --name matrix)"
TOKENS+=("$READER" "$OTHER_PUB" "$SHORT" "$GONE" "$PROJECT")
# a package in another scope, which the reader must not be able to find
mkdir -p "$WORK/other" && cd "$WORK/other"
echo '{"name": "@other/secret", "version": "1.0.0"}' >package.json
echo 'module.exports = "secret";' >index.js
npm publish --registry "$REG" "--//$HOST/npm/:_authToken=$OTHER_PUB" --loglevel=warn
sleep 2 # SHORT expires

DOC="${REG}@e2e%2fhello"
TGZ="${REG}@e2e/hello/-/hello-1.3.0.tgz"
[ "$(status "$READER" "$DOC")" = 200 ] || fail "the reader cannot read its own scope"
[ "$(status "$READER" "$TGZ")" = 200 ] || fail "the reader cannot download its own scope"

# Refused tokens come from a second client address: failed authentications are limited per
# address, and this host's address must stay usable whatever the count below.
ns_up
FROM=("${NS[@]}")
[ "$(status "$READER" "$DOC")" = 200 ] || fail "the second client address cannot reach the registry"
for target in "$DOC" "$TGZ"; do
	denied "no token" "" "$target" 401
	denied "garbage token" "garbage" "$target" 401
	denied "well-formed unknown token" "clwn_0123456789ab_0123456789abcdef0123456789abcdef" "$target" 401
	denied "expired token" "$SHORT" "$target" 401
	denied "revoked token" "$GONE" "$target" 401
	denied "project deploy token" "$PROJECT" "$target" 401
	denied "admin token" "$ADMIN" "$target" 401
done
denied "no token: whoami" "" "${REG}-/whoami" 401
denied "garbage token: whoami" "garbage" "${REG}-/whoami" 401
FROM=()

# A token without rights on a package gets exactly what a missing package gets.
same_as_missing "wrong scope: document" "$OTHER_PUB" "$DOC" "${REG}@other%2fnope"
same_as_missing "wrong scope: HEAD" "$OTHER_PUB" "$DOC" "${REG}@other%2fnope" -I
same_as_missing "wrong scope: abbreviated document" "$OTHER_PUB" "$DOC" "${REG}@other%2fnope" -H "Accept: application/vnd.npm.install-v1+json"
same_as_missing "wrong scope: tarball" "$OTHER_PUB" "$TGZ" "${REG}@other/nope/-/nope-1.3.0.tgz"
same_as_missing "wrong scope: dist-tags" "$OTHER_PUB" "${REG}-/package/@e2e%2fhello/dist-tags" "${REG}-/package/@other%2fnope/dist-tags"
denied "other scope by %2f" "$READER" "${REG}@other%2fsecret" 404
denied "other scope by path" "$READER" "${REG}@other/secret" 404
denied "guessed tarball of another scope" "$READER" "${REG}@other/secret/-/secret-1.0.0.tgz" 404
denied "guessed tarball under the own scope" "$READER" "${REG}@e2e/hello/-/secret-1.0.0.tgz" 404
denied "double encoding" "$READER" "${REG}@other%252fsecret" "400|404"
# Go's mux answers a path with dot segments by redirecting to the cleaned path: followed, still nothing
denied "dot segments" "$READER" "${REG}@e2e/hello/-/../../@other/secret/-/secret-1.0.0.tgz" "307|400|404" --path-as-is
denied "dot segments, redirect followed" "$READER" "${REG}@e2e/hello/-/../../@other/secret/-/secret-1.0.0.tgz" "400|404" --path-as-is -L
denied "encoded dot segments" "$READER" "${REG}@e2e%2f..%2f..%2f@other%2fsecret" "400|404"
denied "npm token on the API" "$READER" "$API/v1/version" 401

# The ACME location on port 80: "/npm/x/..%2F..%2F.well-known/acme-challenge/t" matches it in
# nginx, which must not let it reach the registry with a client-chosen X-Forwarded-For (the npm
# 401 body, failures counted against the forged address). Two layers stop it: nginx passes only
# the normalised challenge path, and Go's mux cleans the path before routing, so even the older
# nginx block that forwarded the raw URI got the challenge handler's 404, not the registry.
# This guards the outcome if either layer changes.
ACME_PROBE="http://$HOST/npm/x/..%2F..%2F.well-known/acme-challenge/t"
code="$(status "" "$ACME_PROBE" --path-as-is -H "X-Forwarded-For: 203.0.113.9")"
[[ $code =~ ^(000|400|404)$ ]] || fail "ACME traversal answered $code: $(head -c 300 "$WORK/body")"
! grep -q "clowctl npm login" "$WORK/body" || fail "ACME traversal reached the registry"
echo "  ok  ACME traversal with a forged X-Forwarded-For → $code, not the registry"
for _ in $(seq 20); do status "" "$ACME_PROBE" --path-as-is -H "X-Forwarded-For: 203.0.113.9" >/dev/null; done
[ "$(status "$READER" "$DOC")" = 200 ] || fail "requests through the ACME location counted against this address"
echo "  ok  20 more through the ACME location: this address is not limited"

# Brute force, from the second address: refused until the limit, then even a valid token.
FROM=("${NS[@]}")
blocked=""
for _ in $(seq 30); do
	if [ "$(status garbage "$DOC")" = 429 ]; then blocked=1 && break; fi
done
[ -n "$blocked" ] || fail "failed authentications are not limited"
denied "a valid token from a blocked address" "$READER" "$DOC" 429
FROM=()
[ "$(status "$READER" "$DOC")" = 200 ] || fail "the limit of another address blocked this one"
echo "  ok  brute force: the address is blocked after its failures, others are not"

step "no token in the logs"
systemctl restart clowdrd # flushes the log store
eventually curl -fsS -o /dev/null -H "Authorization: Bearer $ADMIN" "$API/v1/version"
zstdcat /var/lib/clowdr/logs/_platform/nginx/_/*.zst >"$WORK/access.log"
grep -q '"/npm/' "$WORK/access.log" || fail "the access log has no /npm/ requests: the check below would prove nothing"
journalctl -u clowdrd --no-pager >"$WORK/journal.log"
docker logs clowdr-nginx >"$WORK/nginx.log" 2>&1
find /var/lib/clowdr/logs -name '*.zst' -exec zstdcat {} + >"$WORK/stored.log"
for t in "${TOKENS[@]}"; do
	secret="${t##*_}" # the part after the id: enough to use a token
	for f in access journal nginx stored; do
		! grep -qF "$secret" "$WORK/$f.log" || fail "a token is in the $f log"
	done
done
echo "  ok  ${#TOKENS[@]} tokens absent from the access log, journal, nginx output and the log store"

step "npm e2e passed"
