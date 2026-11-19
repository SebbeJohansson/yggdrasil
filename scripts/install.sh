#!/bin/sh
# Builds ygg, writes each part's settings that are missing (with fresh tokens,
# keeping every token already there), and installs and starts the host parts
# as systemd user units. Safe to run again: nothing you set is overwritten.
#
#   scripts/install.sh
#   docker compose up -d --build     # then the container parts
#
# Settings live in ~/.config/yggdrasil/<part>.env, readable by you only.
set -eu
cd "$(dirname "$0")/.."
config="$HOME/.config/yggdrasil"
origins="http://localhost:17420,http://127.0.0.1:17420"

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
for part in munin vedrfolnir ratatoskr norn sindri huginn dellingr hermodr; do
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
write ratatoskr.env <<EOF
RATATOSKR_TOKENS=owner=$owner
GJALLAR_TOKEN=$(gjallar_token ratatoskr)
# Used only when Gjallar is down and Hermóðr can't take it. The same topic as hermodr.env's.
NTFY_URL=
NTFY_TOKEN=
YGG_CORS_ORIGINS=$origins
EOF

# Norn and Sindri call each other, so they are written together.
if [ ! -e "$config/norn.env" ] || [ ! -e "$config/sindri.env" ]; then
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
# through a meeting.
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

# ensure <file> <name> <value>: adds name=value to a settings file that
# lacks it, keeping everything else.
ensure() {
	[ -e "$config/$1" ] || return 0
	grep -q "^$2=" "$config/$1" && return 0
	printf '%s=%s\n' "$2" "$3" >>"$config/$1"
	echo "  added $2 to $1"
}
for part in gjallar ratatoskr huginn dellingr sindri; do
	ensure "$part.env" HERMODR_TOKEN "$(part_token hermodr "$part")"
done
# Ratatoskr also watches your own rhythm: your work calendar from Norn, your
# sessions and agents from Sindri (README, Ratatoskr). RATATOSKR_YOU=off stops it.
ensure ratatoskr.env NORN_TOKEN "$(part_token norn ratatoskr)"
ensure ratatoskr.env SINDRI_TOKEN "$(part_token sindri ratatoskr)"
ensure ratatoskr.env TIME_ZONE Europe/Stockholm

# Huginn and Dellingr call Norn and Sindri with tokens of their own. After a
# token is added there, restart Norn and Sindri (docker compose up -d, and
# systemctl --user restart ygg-sindri).
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

write parts.env <<'EOF'
# Where every part lives, read by every part. Leave a line commented to use
# the built-in part on its default port. To replace a part with your own
# program, set its URL here, stop the built-in one, and run
# `ygg check <part>` (docs/replacing-a-part.md).
#YGG_NORN_URL=http://127.0.0.1:8771
#YGG_SINDRI_URL=http://127.0.0.1:8772
#YGG_VEDRFOLNIR_URL=http://127.0.0.1:8773
#YGG_GJALLAR_URL=http://127.0.0.1:8774
#YGG_RATATOSKR_URL=http://127.0.0.1:8775
#YGG_MUNIN_URL=http://127.0.0.1:8776
#YGG_HUGINN_URL=http://127.0.0.1:8777
#YGG_DELLINGR_URL=http://127.0.0.1:8778
#YGG_HERMODR_URL=http://127.0.0.1:8779

# The parts Ratatoskr watches. Leave out the ones this machine doesn't run.
RATATOSKR_WATCH=norn,sindri,vedrfolnir,gjallar,munin,huginn,dellingr,hermodr
EOF
if ! grep -q '^RATATOSKR_WATCH=.*hermodr' "$config/parts.env"; then
	echo "  Note: add huginn,dellingr,hermodr to RATATOSKR_WATCH in $config/parts.env if this machine runs them"
fi

umask 022
mkdir -p profile data
if [ ! -e profile/profile.md ] && [ -e ../my-agent/profile/profile.md ]; then
	cp ../my-agent/profile/profile.md profile/profile.md
	echo "  copied your profile from my-agent"
fi

echo "Installing the systemd user units..."
mkdir -p "$HOME/.config/systemd/user"
for unit in deploy/systemd/*.service; do
	install -m 0644 "$unit" "$HOME/.config/systemd/user/"
done
systemctl --user daemon-reload
systemctl --user enable ygg-vedrfolnir.service ygg-ratatoskr.service ygg-sindri.service
systemctl --user restart ygg-vedrfolnir.service ygg-ratatoskr.service ygg-sindri.service

echo
echo "Done. Your owner token (for the widget and curl) is in $config/owner.token."
echo "Next: docker compose up -d --build"
