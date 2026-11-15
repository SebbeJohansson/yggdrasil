#!/bin/sh
# What air runs after each build (scripts/dev.sh), from its copy in bin/dev:
# every part named, from bin/dev/ygg, each with its settings from
# ~/.config/yggdrasil. Each part marks its own log lines with its name. air
# stops the whole process group on the next change.
set -u
config="$HOME/.config/yggdrasil"

# Reads KEY=value lines into the environment, as systemd's EnvironmentFile and
# compose's env_file do: no shell quoting or expansion, comments skipped.
load() {
	[ -r "$1" ] || return 0
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in '' | '#'*) continue ;; esac
		export "${line%%=*}=${line#*=}"
	done <"$1"
}

for part in "$@"; do
	(
		load "$config/parts.env"
		load "$config/$part.env"
		exec bin/dev/ygg "$part"
	) &
done
wait
