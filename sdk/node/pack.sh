#!/bin/sh
# Builds, tests and packs clowdr-node into internal/sdk/files, where clowdrd embeds it and serves
# it at /sdk/node/. Run after changing the package (bump "version" in package.json first): older
# tarballs stay, so services pinned to them keep installing.
set -eu
cd "$(dirname "$0")"
cp ../../docs/tracing.md README.md
npm install --no-audit --no-fund
rm -rf dist
npm run build
npm test
npm pack --pack-destination ../../internal/sdk/files
cp README.md ../../internal/sdk/files/README.md
