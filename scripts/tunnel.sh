#!/bin/sh
# Reaches another machine's Yggdrasil from this one, over SSH: each of its
# parts, which listen on its 127.0.0.1 only, on this machine's 127.0.0.1 at
# its port + 10000 (Sindri 8772 -> 18772), so they never clash with the
# parts this machine runs itself. Nothing is opened on either network.
#
#   scripts/tunnel.sh vps                     # the dev profile's parts
#   scripts/tunnel.sh vps --offset 20000      # a second remote node
#   scripts/tunnel.sh vps --parts sindri,gjallar
#
# The widget then finds that node at those ports (my-widget's README, Nodes).
# Runs in the foreground until stopped; ssh's own options (a user, a key, a
# jump host) belong in ~/.ssh/config under the host's name. docs/vps.md has
# a systemd user unit that keeps it up.
set -eu

usage() {
	echo "usage: scripts/tunnel.sh <ssh host> [--offset N] [--parts a,b,...]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
host="$1"
shift
offset=10000
parts=sindri,gjallar,ratatoskr,dellingr,heimdall
while [ $# -gt 0 ]; do
	case "$1" in
	--offset) offset="${2:-}"; shift 2 ;;
	--parts) parts="${2:-}"; shift 2 ;;
	*) usage ;;
	esac
done

port() {
	case "$1" in
	norn) echo 8771 ;;
	sindri) echo 8772 ;;
	vedrfolnir) echo 8773 ;;
	gjallar) echo 8774 ;;
	ratatoskr) echo 8775 ;;
	munin) echo 8776 ;;
	huginn) echo 8777 ;;
	dellingr) echo 8778 ;;
	hermodr) echo 8779 ;;
	loki) echo 8780 ;;
	heimdall) echo 8781 ;;
	saga) echo 8782 ;;
	*) echo "tunnel.sh: no part called $1" >&2; exit 2 ;;
	esac
}

forwards=""
for part in $(echo "$parts" | tr ',' ' '); do
	remote=$(port "$part")
	local_port=$((remote + offset))
	forwards="$forwards -L 127.0.0.1:$local_port:127.0.0.1:$remote"
	echo "  $part: 127.0.0.1:$local_port -> $host:$remote"
done

# shellcheck disable=SC2086 # one -L per part
exec ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 $forwards "$host"
