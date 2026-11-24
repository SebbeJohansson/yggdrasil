#!/bin/sh
# Builds one release bundle: everything a machine needs to run Yggdrasil at
# that version without the source (docs/releases.md).
#
#   scripts/release-bundle.sh v0.1.0 amd64 dist/
#   -> dist/yggdrasil-v0.1.0-linux-amd64.tar.gz, holding yggdrasil/:
#        VERSION, bin/ygg, compose.yaml (the release's image, no build),
#        .env.example, scripts/install.sh, scripts/tunnel.sh,
#        deploy/systemd/*, docs/, README.md
#
# Go comes from GO (default `go`); the CI release workflow runs this once
# per architecture. Run it from a clean checkout at the tag.
set -eu
cd "$(dirname "$0")/.."

[ $# -eq 3 ] || { echo "usage: scripts/release-bundle.sh <version> <amd64|arm64> <out dir>" >&2; exit 2; }
version=$1 arch=$2 out=$3
echo "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "release-bundle: $version is not a vX.Y.Z version" >&2; exit 2; }
case "$arch" in amd64 | arm64) ;; *) echo "release-bundle: the architecture is amd64 or arm64, not $arch" >&2; exit 2 ;; esac

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
root="$stage/yggdrasil"
mkdir -p "$root/bin" "$root/scripts" "$root/deploy" "$out"

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "${GO:-go}" build -trimpath \
	-ldflags "-s -w -X yggdrasil/internal/part.Version=$version" -o "$root/bin/ygg" ./cmd/ygg

echo "$version" >"$root/VERSION"
# The parts' image is the release's, from .env (install.sh writes it); there is nothing to build.
sed -e '/^  build: \.$/d' \
	-e 's|^  image: \${YGG_IMAGE:-yggdrasil}$|  image: ${YGG_IMAGE:?install.sh names the image in .env}|' \
	compose.yaml >"$root/compose.yaml"
grep -q 'YGG_IMAGE:?' "$root/compose.yaml" || { echo "release-bundle: compose.yaml's part image line has changed; update the sed here" >&2; exit 1; }
cp .env.example README.md "$root/"
cp scripts/install.sh scripts/tunnel.sh scripts/tailscale-serve.sh "$root/scripts/"
cp -R deploy/systemd "$root/deploy/"
cp -R docs "$root/docs"

name="yggdrasil-$version-linux-$arch.tar.gz"
tar -C "$stage" -czf "$out/$name" yggdrasil
echo "$out/$name"
