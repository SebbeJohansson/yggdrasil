#!/bin/sh
# Builds ygg, writes each part's settings that are missing (with fresh tokens,
# keeping every token already there), and installs and starts the host parts
# as systemd user units. Safe to run again: nothing you set is overwritten.
#
#   scripts/install.sh                  # every part (the full profile)
#   scripts/install.sh --profile dev    # the coding parts only, for a VPS
#   docker compose up -d --build        # then the container parts
#
# Profiles (docs/vps.md):
#   full  every part: Norn, the inbox, calendars, local models. Your desktop.
#   dev   Sindri, Ratatoskr, Gjallar, Dellingr and Heimdall: an always-on
#         machine for coding agents, with no inbox, calendar or model.
# The profile is remembered in ~/.config/yggdrasil/profile, so running it
# again keeps the one you picked. YGG_PROFILE works as well as --profile.
#
# Settings live in ~/.config/yggdrasil/<part>.env, readable by you only.
set -eu
cd "$(dirname "$0")/.."
config="$HOME/.config/yggdrasil"
origins="http://localhost:17420,http://127.0.0.1:17420"

profile="${YGG_PROFILE:-}"
while [ $# -gt 0 ]; do
	case "$1" in
	--profile) profile="${2:-}"; shift 2 ;;
	--profile=*) profile="${1#--profile=}"; shift ;;
	*) echo "install.sh: unknown argument $1 (try --profile full|dev)" >&2; exit 2 ;;
	esac
done
[ -n "$profile" ] || profile=$(cat "$config/profile" 2>/dev/null || echo full)
case "$profile" in
full | dev) ;;
*) echo "install.sh: the profile is full or dev, not $profile" >&2; exit 2 ;;
esac
# full: whether this machine runs every part.
full() {
	[ "$profile" = full ]
}
echo "Profile: $profile"

echo "Building ygg..."
scripts/go build -trimpath -ldflags "-s -w -X yggdrasil/internal/part.Version=$(git describe --always --dirty 2>/dev/null || echo dev)" -o bin/ygg ./cmd/ygg
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/ygg "$HOME/.local/bin/ygg"

token() {
	head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48
}

mkdir -p "$config"
chmod 700 "$config"
umask 077

echo "$profile" >"$config/profile"
[ -s "$config/owner.token" ] || token >"$config/owner.token"
owner=$(cat "$config/owner.token")

if [ ! -e "$config/gjallar.env" ]; then
	cat >"$config/gjallar.env" <<EOF
GJALLAR_TOKENS=owner=$owner
# Senders that must beat, and how long a silence may last.
GJALLAR_HEARTBEATS=ratatoskr=3m
# A phone push for critical alerts when Hermóðr can't take them: the same topic as hermodr.env's. Empty is off.
NTFY_URL=
NTFY_TOKEN=
DASHBOARD_URL=http://localhost:17420
YGG_CORS_ORIGINS=$origins
EOF
fi

# part_token <part> <caller>: the caller's token at the part, read from
# <PART>_TOKENS in <part>.env and added when missing.
part_token() {
	file="$config/$1.env"
	variable="$(echo "$1" | tr a-z A-Z)_TOKENS"
	found=$(grep "^$variable=" "$file" | cut -d= -f2- | tr ',' '\n' | grep "^$2=" | cut -d= -f2- || true)
	if [ -z "$found" ]; then
		found=$(token)
		sed -i "s|^$variable=.*|&,$2=$found|" "$file"
		echo "  gave $2 a token at $1" >&2
	fi
	echo "$found"
}
# A part's token at Gjallar.
gjallar_token() {
	part_token gjallar "$1"
}
if full; then
	senders="munin vedrfolnir ratatoskr norn sindri huginn dellingr hermodr loki heimdall phone"
else
	senders="ratatoskr sindri dellingr heimdall"
fi
for part in $senders; do
	gjallar_token "$part" >/dev/null
done

# write <file>: the text on stdin, only when the file is not there yet.
write() {
	if [ -e "$config/$1" ]; then
		cat >/dev/null
		return 0
	fi
	cat >"$config/$1"
	echo "  wrote $1"
}

# ensure <file> <name> <value>: adds name=value to a settings file that
# lacks it, keeping everything else.
ensure() {
	[ -e "$config/$1" ] || return 0
	grep -q "^$2=" "$config/$1" && return 0
	printf '%s=%s\n' "$2" "$3" >>"$config/$1"
	echo "  added $2 to $1"
}

if full; then
	write munin.env <<EOF
MUNIN_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token munin)
YGG_CORS_ORIGINS=$origins
# Sources this machine runs, e.g. tcgpriser (README, Munin). None by default.
MUNIN_SOURCES=
# MUNIN_TCGPRISER_TOKEN=
EOF
	write vedrfolnir.env <<EOF
VEDRFOLNIR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token vedrfolnir)
YGG_CORS_ORIGINS=$origins
EOF
fi
write ratatoskr.env <<EOF
RATATOSKR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token ratatoskr)
# Used only when Gjallar is down and Hermóðr can't take it. The same topic as hermodr.env's.
NTFY_URL=
NTFY_TOKEN=
YGG_CORS_ORIGINS=$origins
EOF

# The dev profile has no calendar to watch you by (Ratatoskr's rhythm).
full || ensure ratatoskr.env RATATOSKR_YOU off

# The dev profile runs Sindri alone: no Norn to post to or ask, so no
# NORN_TOKEN, which turns those calls off (sindri/main.go).
if ! full; then
	write sindri.env <<EOF
SINDRI_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token sindri)
YGG_CORS_ORIGINS=$origins
EOF
fi

# Norn and Sindri call each other, so they are written together.
if full && { [ ! -e "$config/norn.env" ] || [ ! -e "$config/sindri.env" ]; }; then
	norn_at_sindri=$(token)
	sindri_at_norn=$(token)
	rm -f "$config/norn.env" "$config/sindri.env"
	write norn.env <<EOF
NORN_TOKENS=owner=$owner,sindri=$sindri_at_norn
GJALLAR_TOKEN=$(gjallar_token norn)
# Norn's token at Sindri, to hand work to coding agents.
SINDRI_TOKEN=$norn_at_sindri
TIME_ZONE=Europe/Stockholm
# Laya (judge, tone) and your own Google "Desktop app" OAuth client; empty is off.
LAYA_KEY=
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
YGG_CORS_ORIGINS=$origins
EOF
	write sindri.env <<EOF
SINDRI_TOKENS=owner=$owner,norn=$norn_at_sindri
GJALLAR_TOKEN=$(gjallar_token sindri)
# Sindri's token at Norn, to post what agents say into your conversations.
NORN_TOKEN=$sindri_at_norn
YGG_CORS_ORIGINS=$origins
EOF
	# Settings my-agent already has, carried over once: the Google client,
	# Laya, Linear and the models. Values are copied, never printed.
	legacy="${MY_AGENT_ENV:-$(dirname "$PWD")/my-agent/.env}"
	carry() {
		# Without an inline "# comment", spaces or quotes: dotenv drops them, the parts don't.
		value=$(grep -E "^$1=" "$legacy" 2>/dev/null | tail -1 | cut -d= -f2- | sed -E 's/[[:space:]]+#.*$//; s/^[[:space:]]+//; s/[[:space:]]+$//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/')
		[ -n "$value" ] || return 0
		grep -vE "^$1=" "$2" >"$2.tmp" || true
		printf '%s=%s\n' "$1" "$value" >>"$2.tmp"
		mv "$2.tmp" "$2"
		echo "  carried $1 over from my-agent into $(basename "$2")"
	}
	for key in GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET LAYA_KEY TYPESAFE_KEY TIME_ZONE INBOX_JUDGE INBOX_JUDGE_MODEL SUMMARY_MODEL ASK_MODEL DISPATCH_MODEL EMBED_MODEL CHAT_URL OLLAMA_URL LAYA_URL LANGUAGETOOL_URL WRITE_MODEL REWRITE_MODEL LINEAR_KEY LAYA_SHADOW_URL LAYA_SHADOW_KEY; do
		carry "$key" "$config/norn.env"
	done
	for key in LINEAR_KEY DELEGATE_MODEL AUTO_ALLOW; do
		carry "$key" "$config/sindri.env"
	done
fi

# Hermóðr carries every message for you: into the widget, and to your phone
# when NTFY_URL is set. It reads your calendars from Norn, to hold messages
# through a meeting. Hermóðr, Huginn, Loki and Sága are the full profile's.
if full; then
	write hermodr.env <<EOF
HERMODR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token hermodr)
NORN_TOKEN=$(part_token norn hermodr)
TIME_ZONE=Europe/Stockholm
# Your phone: an ntfy topic, e.g. https://ntfy.sh/<a long secret topic>. Empty is off.
NTFY_URL=
NTFY_TOKEN=
# The lowest priority it sends: low, normal, high or urgent.
NTFY_MIN=normal
# Only urgent messages go out between these, e.g. 22:00-07:00. Empty is none.
QUIET_HOURS=
YGG_CORS_ORIGINS=$origins
EOF

	for part in gjallar ratatoskr huginn dellingr sindri; do
		ensure "$part.env" HERMODR_TOKEN "$(part_token hermodr "$part")"
	done
	# Ratatoskr also watches your own rhythm: your work calendar from Norn, your
	# sessions and agents from Sindri (README, Ratatoskr). RATATOSKR_YOU=off stops it.
	ensure ratatoskr.env NORN_TOKEN "$(part_token norn ratatoskr)"
fi
ensure ratatoskr.env SINDRI_TOKEN "$(part_token sindri ratatoskr)"
ensure ratatoskr.env TIME_ZONE Europe/Stockholm

# Huginn and Dellingr call Norn and Sindri with tokens of their own. After a
# token is added there, restart Norn and Sindri (docker compose up -d, and
# systemctl --user restart ygg-sindri).
if full; then
	write huginn.env <<EOF
HUGINN_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token huginn)
NORN_TOKEN=$(part_token norn huginn)
SINDRI_TOKEN=$(part_token sindri huginn)
HERMODR_TOKEN=$(part_token hermodr huginn)
TIME_ZONE=Europe/Stockholm
YGG_CORS_ORIGINS=$origins
EOF
	write dellingr.env <<EOF
DELLINGR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token dellingr)
NORN_TOKEN=$(part_token norn dellingr)
SINDRI_TOKEN=$(part_token sindri dellingr)
HERMODR_TOKEN=$(part_token hermodr dellingr)
TIME_ZONE=Europe/Stockholm
YGG_CORS_ORIGINS=$origins
EOF
else
	# Dev: jobs that run agents through Sindri; no Norn to ask, no Hermóðr to tell.
	write dellingr.env <<EOF
DELLINGR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token dellingr)
SINDRI_TOKEN=$(part_token sindri dellingr)
TIME_ZONE=Europe/Stockholm
YGG_CORS_ORIGINS=$origins
EOF
fi

# Heimdall, the security watchman, runs on the host: it reads the host's
# listening sockets, your repos and this folder. It asks Norn which sign-ins
# it holds, and Sindri which projects it knows.
write heimdall.env <<EOF
HEIMDALL_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token heimdall)
$(full && echo "NORN_TOKEN=$(part_token norn heimdall)" || echo "# No Norn on the dev profile: the sign-ins check is skipped.")
SINDRI_TOKEN=$(part_token sindri heimdall)
TIME_ZONE=Europe/Stockholm
# The repos to check, comma separated; Sindri's projects are added. Empty: ~/projects/yggdrasil/yggdrasil-desktop, ~/projects/yggdrasil/mimir and ~/projects/my-widget.
HEIMDALL_REPOS=
YGG_CORS_ORIGINS=$origins
EOF

# Loki tests the watchers by tricking a part for a few minutes, through each
# part's trick switch, which answers only with LOKI=on (parts.env, dev mode).
# It reads Ratatoskr's status, Gjallar's alerts and Hermóðr's channels, and
# tricks every part, each with a token of its own there. After a token is
# added at a part, restart that part.
if full; then
loki_env="LOKI_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token loki)"
for part in norn sindri vedrfolnir ratatoskr munin huginn dellingr hermodr heimdall; do
	loki_env="$loki_env
$(echo "$part" | tr a-z A-Z)_TOKEN=$(part_token "$part" loki)"
done
write loki.env <<EOF
$loki_env
TIME_ZONE=Europe/Stockholm
YGG_CORS_ORIGINS=$origins
EOF
ensure loki.env HEIMDALL_TOKEN "$(part_token heimdall loki)"

# Ratatoskr's gossip in Huginn reads Loki's strikes and Dellingr's runs, with
# tokens of its own there (README, Huginn). Restart Loki and Dellingr after.
ensure huginn.env LOKI_TOKEN "$(part_token loki huginn)"
ensure huginn.env DELLINGR_TOKEN "$(part_token dellingr huginn)"

# Sága syncs alerts with your phone through Mímir, the event log on your VPS
# (github.com/SebbeJohansson/mimir), at its public HTTPS URL. It runs on the
# host. It reads and acts on alerts as you, and raises the
# phone's alerts as `phone`, a sender Gjallar never pushes (GJALLAR_QUIET).
# Restart Gjallar after the phone's token is added there.
write saga.env <<EOF
SAGA_TOKENS=owner=$owner
GJALLAR_OWNER_TOKEN=$owner
SAGA_GJALLAR_TOKENS=phone=$(gjallar_token phone)
SAGA_DEVICE=desktop
# Mímir on your VPS, and this machine's token there (Mímir's deploy/install.sh prints both). Empty is off.
MIMIR_URL=
MIMIR_TOKEN=
YGG_CORS_ORIGINS=$origins
EOF
# Sága also shares this machine's coding sessions (Sindri's) with your other
# devices, and carries out what the devices in SAGA_DRIVE_FROM ask of them:
# type a prompt, answer a dialog, start or follow up a cloud session. Empty
# SAGA_DRIVE_FROM is look only. Restart Sindri after its token is added.
ensure saga.env SINDRI_TOKEN "$(part_token sindri saga)"
ensure saga.env SAGA_DRIVE_FROM "phone"
fi

# The parts Ratatoskr watches: the ones this profile runs.
if full; then
	watch=norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr,hermodr,loki,heimdall
else
	watch=sindri,gjallar,dellingr,heimdall
fi

write parts.env <<EOF
# Where every part lives, read by every part. Leave a line commented to use
# the built-in part on its default port. To replace a part with your own
# program, set its URL here, stop the built-in one, and run
# \`ygg check <part>\` (docs/replacing-a-part.md).
#YGG_NORN_URL=http://127.0.0.1:8771
#YGG_SINDRI_URL=http://127.0.0.1:8772
#YGG_VEDRFOLNIR_URL=http://127.0.0.1:8773
#YGG_GJALLAR_URL=http://127.0.0.1:8774
#YGG_RATATOSKR_URL=http://127.0.0.1:8775
#YGG_MUNIN_URL=http://127.0.0.1:8776
#YGG_HUGINN_URL=http://127.0.0.1:8777
#YGG_DELLINGR_URL=http://127.0.0.1:8778
#YGG_HERMODR_URL=http://127.0.0.1:8779
#YGG_LOKI_URL=http://127.0.0.1:8780
#YGG_HEIMDALL_URL=http://127.0.0.1:8781
#YGG_SAGA_URL=http://127.0.0.1:8782

# Your working day, Monday to Friday: Ratatoskr's rhythm and Norn's gap read it.
#WORK_HOURS=09:00-17:00

# On the machine that holds work data (the work laptop): every part says so on
# /health, and a widget on another machine refuses to add this one as a node.
#YGG_WORK=on

# The parts Ratatoskr watches. Leave out the ones this machine doesn't run.
RATATOSKR_WATCH=$watch

# Lets Loki trick the parts for a few minutes, to test the watchers. Dev mode
# only, never on a machine you rely on. Unset, the trick switch is not there.
#LOKI=on
EOF
if full && ! grep -q '^RATATOSKR_WATCH=.*hermodr' "$config/parts.env"; then
	echo "  Note: add huginn,dellingr,hermodr to RATATOSKR_WATCH in $config/parts.env if this machine runs them"
fi
if full && ! grep -q '^RATATOSKR_WATCH=.*loki' "$config/parts.env"; then
	echo "  Note: add loki to RATATOSKR_WATCH in $config/parts.env if this machine runs it"
fi
if full && ! grep -q '^RATATOSKR_WATCH=.*heimdall' "$config/parts.env"; then
	echo "  Note: add heimdall to RATATOSKR_WATCH in $config/parts.env"
fi
if full && ! grep -q '^RATATOSKR_WATCH=.*saga' "$config/parts.env"; then
	echo "  Note: once Sága is set up (MIMIR_URL in saga.env), add saga to RATATOSKR_WATCH in $config/parts.env"
fi

umask 022
mkdir -p profile data
if full && [ ! -e profile/profile.md ] && [ -e ../my-agent/profile/profile.md ]; then
	cp ../my-agent/profile/profile.md profile/profile.md
	echo "  copied your profile from my-agent"
fi

echo "Installing the systemd user units..."
mkdir -p "$HOME/.config/systemd/user"
for unit in deploy/systemd/*.service; do
	install -m 0644 "$unit" "$HOME/.config/systemd/user/"
done
if full; then
	units="ygg-vedrfolnir.service ygg-ratatoskr.service ygg-sindri.service ygg-heimdall.service ygg-saga.service"
	containers=norn,gjallar,munin,huginn,dellingr,hermodr,loki,languagetool,embeddings
else
	units="ygg-ratatoskr.service ygg-sindri.service ygg-heimdall.service"
	containers=gjallar,dellingr
fi
systemctl --user daemon-reload
# shellcheck disable=SC2086 # a list of units
systemctl --user enable $units
# shellcheck disable=SC2086
systemctl --user restart $units

echo
echo "Done. Your owner token (for the widget and curl) is in $config/owner.token."
if [ ! -e .env ]; then
	echo "Next: echo COMPOSE_PROFILES=$containers >.env && docker compose up -d --build"
else
	echo "Next: docker compose up -d --build (COMPOSE_PROFILES in .env: $containers for the $profile profile)"
fi
if ! full; then
	echo "On a VPS, keep the user units running after you log out: loginctl enable-linger $(id -un)"
fi
