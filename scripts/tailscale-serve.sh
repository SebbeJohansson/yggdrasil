#!/bin/sh
# Publishes this machine's parts on your tailnet, over HTTPS, so your other
# machines reach them at https://<this machine>.<tailnet>.ts.net:<port>
# (docs/tailscale.md). The parts keep listening on 127.0.0.1 only; Tailscale
# forwards each port to them. Only the parts this machine's profile runs.
#
#   scripts/tailscale-serve.sh          publish
#   scripts/tailscale-serve.sh --off    stop publishing
#
# Never on a work machine (YGG_WORK=on): it refuses, so nothing there is ever
# reachable from the tailnet. Once, as root, let this user run `tailscale
# serve`: sudo tailscale set --operator=$USER
set -eu

config="$HOME/.config/yggdrasil"
die() { printf 'tailscale-serve: %s\n' "$*" >&2; exit 1; }

if grep -q '^YGG_WORK=on' "$config/parts.env" 2>/dev/null || [ "${YGG_WORK:-}" = on ]; then
	die "this is a work machine (YGG_WORK=on): it is never published on the tailnet"
fi
# In WSL, Tailscale runs on Windows: its tailscale.exe, through interop. With
# WSL's mirrored networking, Windows' 127.0.0.1 is WSL's, so it reaches the parts.
if command -v tailscale >/dev/null 2>&1; then
	ts=tailscale
elif command -v tailscale.exe >/dev/null 2>&1; then
	ts=tailscale.exe
else
	die "tailscale is not installed (docs/tailscale.md)"
fi

off=""
case "${1:-}" in
"") ;;
--off) off=1 ;;
*) die "usage: scripts/tailscale-serve.sh [--off]" ;;
esac

# name:port for the parts this profile runs.
case "$(cat "$config/profile" 2>/dev/null || echo full)" in
dev) parts="sindri:8772 gjallar:8774 ratatoskr:8775 dellingr:8778 heimdall:8781" ;;
*) parts="norn:8771 sindri:8772 vedrfolnir:8773 gjallar:8774 ratatoskr:8775 munin:8776 huginn:8777 dellingr:8778 hermodr:8779 loki:8780 heimdall:8781" ;;
esac

for entry in $parts; do
	name=${entry%%:*} port=${entry##*:}
	if [ -n "$off" ]; then
		"$ts" serve --https="$port" off >/dev/null 2>&1 || true
		echo "  $name: no longer on the tailnet"
	elif "$ts" serve --bg --https="$port" "http://127.0.0.1:$port" >/dev/null; then
		echo "  $name: https port $port"
	else
		die "tailscale serve refused $name; once, as root: sudo tailscale set --operator=$(id -un)"
	fi
done

[ -n "$off" ] && exit 0
host=$("$ts" status --json 2>/dev/null | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -n 1)
echo
echo "Published on the tailnet${host:+ as https://$host:<port>}."
echo "In the widget's nodes.json on your other machines: { \"id\": \"...\", \"label\": \"...\", \"token\": \"<owner.token here>\", \"tailnet\": \"${host:-<this machine>.<tailnet>.ts.net}\" }"
