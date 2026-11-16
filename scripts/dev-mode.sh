#!/bin/sh
# Switches this machine between the installed parts and dev mode, which keeps
# running without a terminal and comes back after a reboot:
#
#   scripts/dev-mode.sh on       every part from your working tree, rebuilt on every save
#   scripts/dev-mode.sh off      the installed parts again
#   scripts/dev-mode.sh logs     follow both halves' logs
#
# Dev mode is two halves: the dev container (Norn, Gjallar, Munin) and
# ygg-dev.service (Sindri, Veðrfölnir, Ratatoskr, which need the host).
set -eu
cd "$(dirname "$0")/.."
host_units="ygg-sindri.service ygg-vedrfolnir.service ygg-ratatoskr.service"

case "${1:-}" in
on)
	install -m 0644 deploy/systemd/ygg-dev.service "$HOME/.config/systemd/user/"
	systemctl --user daemon-reload
	systemctl --user disable --now $host_units
	systemctl --user enable --now ygg-dev.service
	docker compose stop norn gjallar munin >/dev/null 2>&1 || true
	docker compose --profile dev up -d --build dev
	echo "Dev mode on. Logs: scripts/dev-mode.sh logs"
	;;
off)
	docker compose --profile dev stop dev
	systemctl --user disable --now ygg-dev.service
	systemctl --user enable --now $host_units
	echo "Dev mode off. Start the container parts with: docker compose up -d --build"
	;;
logs)
	docker compose --profile dev logs -f --tail 50 dev &
	journalctl --user -u ygg-dev.service -f -n 50 -o cat &
	trap 'kill 0' INT TERM
	wait
	;;
*)
	echo "Usage: scripts/dev-mode.sh on|off|logs" >&2
	exit 2
	;;
esac
