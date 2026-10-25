#!/bin/sh
# sign-checksums.sh <tag> <checksums.txt> <key.pem> <signature>: ed25519 over "<tag>\n" +
# checksums.txt, as install.sh and `clowctl self-update` verify it. Called by goreleaser.
set -eu
signed="$2.signed"
trap 'rm -f "$signed"' EXIT
{ printf '%s\n' "$1"; cat "$2"; } >"$signed"
openssl pkeyutl -sign -rawin -inkey "$3" -in "$signed" -out "$4"
