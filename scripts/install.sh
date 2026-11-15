#!/bin/sh
# Builds ygg, writes each part's settings with fresh tokens (only the first
# time), and installs the host parts as systemd user units.
#
#   scripts/install.sh
#   docker compose up -d --build     # then the container parts
#
# Settings live in ~/.config/yggdrasil/<part>.env, readable by you only.
# Existing files are never overwritten: delete them all to get new tokens.
set -eu
cd "$(dirname "$0")/.."
config="$HOME/.config/yggdrasil"

echo "Building ygg..."
scripts/go build -trimpath -ldflags "-s -w -X yggdrasil/internal/part.Version=$(git describe --always --dirty 2>/dev/null || echo dev)" -o bin/ygg ./cmd/ygg
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/ygg "$HOME/.local/bin/ygg"

token() {
	head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48
}

mkdir -p "$config"
chmod 700 "$config"
if [ ! -e "$config/gjallar.env" ]; then
	echo "Writing settings with new tokens to $config..."
	owner=$(token)
	ratatoskr=$(token)
	munin=$(token)
	vedrfolnir=$(token)
	origins="http://localhost:17420,http://127.0.0.1:17420"
	umask 077
	printf '%s\n' "$owner" >"$config/owner.token"
	cat >"$config/gjallar.env" <<EOF
GJALLAR_TOKENS=owner=$owner,ratatoskr=$ratatoskr,munin=$munin,vedrfolnir=$vedrfolnir
# Senders that must beat, and how long a silence may last.
GJALLAR_HEARTBEATS=ratatoskr=3m
# Your phone push topic for critical alerts, e.g. https://ntfy.sh/<a secret topic>. Empty is off.
NTFY_URL=
NTFY_TOKEN=
DASHBOARD_URL=http://localhost:17420
YGG_CORS_ORIGINS=$origins
EOF
	cat >"$config/munin.env" <<EOF
MUNIN_TOKENS=owner=$owner
GJALLAR_TOKEN=$munin
YGG_CORS_ORIGINS=$origins
EOF
	cat >"$config/vedrfolnir.env" <<EOF
VEDRFOLNIR_TOKENS=owner=$owner
GJALLAR_TOKEN=$vedrfolnir
YGG_CORS_ORIGINS=$origins
EOF
	cat >"$config/ratatoskr.env" <<EOF
RATATOSKR_TOKENS=owner=$owner
GJALLAR_TOKEN=$ratatoskr
# Used only when Gjallar itself is down. The same topic as Gjallar's.
NTFY_URL=
NTFY_TOKEN=
YGG_CORS_ORIGINS=$origins
EOF
fi

if [ ! -e "$config/parts.env" ]; then
	umask 077
	cat >"$config/parts.env" <<'EOF'
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

# The parts Ratatoskr watches. Leave out the ones this machine doesn't run.
RATATOSKR_WATCH=norn,sindri,vedrfolnir,gjallar,munin
EOF
fi

echo "Installing the systemd user units..."
mkdir -p "$HOME/.config/systemd/user"
for unit in deploy/systemd/*.service; do
	install -m 0644 "$unit" "$HOME/.config/systemd/user/"
done
systemctl --user daemon-reload
systemctl --user enable --now ygg-vedrfolnir.service ygg-ratatoskr.service
systemctl --user restart ygg-vedrfolnir.service ygg-ratatoskr.service

echo
echo "Done. Your owner token (for the widget and curl) is in $config/owner.token."
echo "Next: docker compose up -d --build"
