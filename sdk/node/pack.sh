#!/bin/sh
# Builds, tests and packs clowdr-node (published to npm as @veksa/clowdr-node) into internal/sdk/files,
# where clowdrd embeds it and serves it at /sdk/node/. Run after changing the package (bump "version" in
# package.json first): older tarballs stay, so services pinned to them keep installing. The embedded file
# keeps the name clowdr-node-<version>.tgz that `clowctl sdk install` and older CLIs look for; the npm
# release of the same version is made by .github/workflows/release.yml.
set -eu
cd "$(dirname "$0")"
version=$(node -p "require('./package.json').version")

# A released version never changes: services pin it and clowdrd serves it as immutable.
if git -C ../.. ls-files --error-unmatch "internal/sdk/files/clowdr-node-$version.tgz" >/dev/null 2>&1; then
    echo "clowdr-node $version is already released: bump version in package.json" >&2
    exit 1
fi
cat ../../docs/tracing.md ../../docs/metrics.md > README.md
npm install --no-audit --no-fund
rm -rf dist
npm run build
npm test
npm pack --pack-destination ../../internal/sdk/files
# npm names a scoped package's tarball veksa-clowdr-node-<version>.tgz.
mv "../../internal/sdk/files/veksa-clowdr-node-$version.tgz" "../../internal/sdk/files/clowdr-node-$version.tgz"
cp README.md ../../internal/sdk/files/README.md
