#!/bin/sh
# Installs a Yggdrasil release on this machine in one go: the VPS, or later
# the work laptop (docs/releases.md). Run it as the user the parts should run
# as, not root:
#
#   curl -fsSL https://raw.githubusercontent.com/SebbeJohansson/yggdrasil/main/scripts/get.sh | sh -s -- --profile dev
#
#   --profile dev|full     which parts (required the first time; then remembered)
#   --version v0.1.0       a release (default, or `latest`: the newest)
#   --from-checkout DIR    first stop the parts a checkout in DIR runs, and move
#                          its data/, profile/ and .env into the install
#   --home DIR             where to install (default ~/.local/share/yggdrasil,
#                          or YGG_HOME)
#
# It downloads the bundle for this machine (amd64 or arm64) and the
# release's SHA256SUMS, checks the one against the other, unpacks the bundle
# into the install and runs its install.sh, which installs ygg, writes the
# settings and starts the parts. Running it again reinstalls; for later
# versions, `ygg upgrade <version>` does the same from the installed ygg.
set -eu

repo="SebbeJohansson/yggdrasil"
# Where the release files are, for testing against a local copy: <base>/<version>/<file>.
base="${YGG_RELEASES_URL:-https://github.com/$repo/releases/download}"

say() { printf '%s\n' "$*"; }
die() { printf 'get.sh: %s\n' "$*" >&2; exit 1; }

profile="" version="" checkout="" home="${YGG_HOME:-$HOME/.local/share/yggdrasil}"
while [ $# -gt 0 ]; do
	case "$1" in
	--profile) profile="${2:-}"; shift 2 ;;
	--version) version="${2:-}"; shift 2 ;;
	--from-checkout) checkout="${2:-}"; shift 2 ;;
	--home) home="${2:-}"; shift 2 ;;
	-h | --help) sed -n '2,20p' "$0" 2>/dev/null || true; exit 0 ;;
	*) die "unknown argument $1 (try --profile dev|full [--version v0.1.0] [--from-checkout DIR])" ;;
	esac
done

[ "$(id -u)" -ne 0 ] || [ "${YGG_ALLOW_ROOT:-}" = 1 ] || die "run it as the user the parts should run as, not root (YGG_ALLOW_ROOT=1 if you mean it)"
case "$profile" in "" | dev | full) ;; *) die "the profile is dev or full, not $profile" ;; esac
if [ -z "$profile" ] && [ ! -s "$HOME/.config/yggdrasil/profile" ]; then
	die "say which parts this machine runs: --profile dev (a VPS) or --profile full"
fi

case "$(uname -s)" in Linux) ;; *) die "releases are built for Linux, not $(uname -s)" ;; esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "releases are built for amd64 and arm64, not $(uname -m)" ;;
esac

for tool in tar sha256sum; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is missing"
done
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
	read_url() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	read_url() { wget -qO- "$1"; }
else
	die "curl or wget is needed"
fi
command -v docker >/dev/null 2>&1 || say "Note: no docker here; the container parts start once it is installed (docker compose up -d in $home)."

[ "$version" != latest ] || version=""
if [ -z "$version" ]; then
	[ -z "${YGG_RELEASES_URL:-}" ] || die "name the --version when YGG_RELEASES_URL is set"
	version=$(read_url "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$version" ] || die "could not find the latest release; name one with --version"
fi
echo "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "$version is not a version like v0.1.0"

asset="yggdrasil-$version-linux-$arch.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

say "Downloading Yggdrasil $version for $arch..."
fetch "$base/$version/SHA256SUMS" "$work/SHA256SUMS" || die "no SHA256SUMS for $version ($base/$version/SHA256SUMS)"
fetch "$base/$version/$asset" "$work/$asset" || die "no $asset in $version"
(cd "$work" && grep "  $asset\$" SHA256SUMS | sha256sum --check --status) || die "$asset does not match the release's SHA256SUMS"
say "Checked against SHA256SUMS."

# Unpacked beside the install first, so a broken bundle leaves it as it was.
mkdir -p "$work/bundle"
tar -xzf "$work/$asset" -C "$work/bundle"
[ "$(cat "$work/bundle/yggdrasil/VERSION" 2>/dev/null)" = "$version" ] || die "the bundle is not $version"

if [ -n "$checkout" ]; then
	[ -f "$checkout/compose.yaml" ] || die "$checkout has no compose.yaml: not a Yggdrasil checkout"
	say "Stopping the parts the checkout in $checkout runs..."
	if command -v docker >/dev/null 2>&1; then
		(cd "$checkout" && docker compose down) || die "could not stop the checkout's containers"
	fi
	mkdir -p "$home"
	for item in data profile .env; do
		if [ -e "$checkout/$item" ] && [ ! -e "$home/$item" ]; then
			mv "$checkout/$item" "$home/$item"
			say "  moved $item"
		elif [ -e "$checkout/$item" ]; then
			say "  kept $home/$item; $checkout/$item is left where it is"
		fi
	done
fi

mkdir -p "$home"
# Over the previous release's files; data/, profile/ and .env are not in a bundle.
(cd "$work/bundle/yggdrasil" && tar -cf - .) | (cd "$home" && tar -xf -)
say "Installed the files into $home."

if [ -n "$profile" ]; then
	sh "$home/scripts/install.sh" --profile "$profile"
else
	sh "$home/scripts/install.sh"
fi

say ""
say "Yggdrasil $version is installed. Later versions: ygg upgrade latest"
