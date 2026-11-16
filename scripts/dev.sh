#!/bin/sh
# Runs parts from your working tree and rebuilds and restarts them on every
# change, with air (github.com/air-verse/air), Go's usual live reloader.
#
#   scripts/dev.sh                  every part
#   scripts/dev.sh norn sindri      just these; the rest keep running as installed
#
# The installed copies of those parts (the systemd units, the containers) are
# stopped first, so the dev ones take their ports, settings and data, and
# started again when you stop this with Ctrl-C.
#
# To keep dev mode running without a terminal, use scripts/dev-mode.sh on: it
# runs this in the dev container and in ygg-dev.service, with
#   YGG_DEV_MANAGED=1   whoever started this handles the installed copies
#   YGG_GO=go           the Go command (default scripts/go, Go in Docker)
#   YGG_DEV_DIR=<dir>   where the build goes (default bin/dev), one per runner
set -eu
cd "$(dirname "$0")/.."
ALL="norn sindri vedrfolnir gjallar ratatoskr munin huginn dellingr"
parts="${*:-$ALL}"
for part in $parts; do
	case " $ALL " in *" $part "*) ;; *) echo "No part called $part. Parts: $ALL" >&2; exit 2 ;; esac
done
managed="${YGG_DEV_MANAGED:-}"
go="${YGG_GO:-scripts/go}"
dir="${YGG_DEV_DIR:-bin/dev}"

if [ ! -x bin/tools/air ]; then
	echo "Installing air into bin/tools..."
	GOBIN="$PWD/bin/tools" $go install github.com/air-verse/air@v1.61.7
fi

# The installed copies: systemd units for these, containers for the rest.
unit_of() {
	case "$1" in sindri | vedrfolnir | ratatoskr) echo "ygg-$1.service" ;; *) echo "" ;; esac
}
stopped_units=""
stopped_containers=""
if [ -z "$managed" ]; then
	for part in $parts; do
		unit=$(unit_of "$part")
		if [ -n "$unit" ]; then
			if systemctl --user is-active --quiet "$unit"; then
				systemctl --user stop "$unit"
				stopped_units="$stopped_units $unit"
			fi
		elif [ -n "$(docker compose ps -q "$part" 2>/dev/null)" ]; then
			docker compose stop "$part" >/dev/null 2>&1
			stopped_containers="$stopped_containers $part"
		fi
	done
fi
restore() {
	trap - EXIT INT TERM
	echo
	[ -z "$stopped_units" ] || { echo "Starting the installed units again:$stopped_units"; systemctl --user start $stopped_units; }
	[ -z "$stopped_containers" ] || { echo "Starting the containers again:$stopped_containers"; docker compose start $stopped_containers >/dev/null; }
}
trap restore EXIT
trap 'exit 130' INT TERM

mkdir -p "$dir"
cat >"$dir/air.toml" <<TOML
root = "."
tmp_dir = "$dir"

[build]
  # air deletes what it runs before each build, so it runs a copy of
  # scripts/dev-run.sh that the build puts back.
  cmd = "$go build -o $dir/ygg ./cmd/ygg && cp scripts/dev-run.sh $dir/run.sh"
  bin = "$dir/run.sh"
  full_bin = "$dir/run.sh $parts"
  include_ext = ["go", "yaml"]
  exclude_dir = [".git", "bin", "data", "profile", "workspace", "deploy", "examples"]
  exclude_regex = ["_test\\\\.go$"]
  delay = 500
  # Ctrl-C to the parts first, so they close their databases; killed after 3 s.
  send_interrupt = true
  kill_delay = "3s"
  stop_on_error = true

[log]
  time = true

[misc]
  clean_on_exit = false
TOML
echo "Running $parts from source; every save rebuilds and restarts them. Ctrl-C stops."
bin/tools/air -c "$dir/air.toml"
