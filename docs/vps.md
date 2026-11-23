# Yggdrasil on a VPS

A VPS runs the **dev profile**: Sindri and the parts that watch it, always on,
so coding agents and the night shift keep going while your own machines sleep.
It has no inbox, calendar or model, so Norn and the parts that need it stay on
your desktop (the **full profile**).

| Profile | Parts | For |
| --- | --- | --- |
| full | every part | a desktop: the work laptop, the home PC |
| dev | Sindri, Ratatoskr, Heimdall (host units), Gjallar, Dellingr (containers) | an always-on VPS |

Parts already run without Norn when they have no `NORN_TOKEN`: Sindri posts
nothing back to a conversation and asks no goal's kind or why, Heimdall skips
the sign-ins check, Dellingr runs no ask jobs. Ratatoskr's rhythm
(`RATATOSKR_YOU`) is off, with no calendar to read.

## Install

On the VPS, as the user the agents should run as (not root):

```sh
# Docker, git, and Claude Code (or OpenCode) logged in: Sindri runs them.
git clone https://github.com/SebbeJohansson/yggdrasil.git ~/projects/yggdrasil/yggdrasil-desktop
cd ~/projects/yggdrasil/yggdrasil-desktop
scripts/install.sh --profile dev
echo COMPOSE_PROFILES=gjallar,dellingr >.env
docker compose up -d --build
loginctl enable-linger "$USER"     # keep the host units up after you log out
ygg status
```

The profile is remembered in `~/.config/yggdrasil/profile`; running
`scripts/install.sh` again keeps it. Personal repos for Sindri go anywhere
you like; add them as projects from the widget or `POST /v1/projects`.

Only personal repos and personal accounts belong here. Work stays on the work
laptop's own node.

## Reach it

Every part listens on the VPS's 127.0.0.1 only. Two ways in, neither opens a
port to the internet:

**SSH tunnel** (nothing to install on the VPS):

```sh
scripts/tunnel.sh vps      # Sindri on 127.0.0.1:18772, Gjallar 18774, Ratatoskr 18775, Dellingr 18778, Heimdall 18781
```

Each part appears at its own port + 10000, so it never clashes with the parts
this machine runs. To keep it up, a user unit
(`~/.config/systemd/user/ygg-tunnel-vps.service`):

```ini
[Unit]
Description=Tunnel to Yggdrasil on the VPS
After=network-online.target

[Service]
ExecStart=%h/projects/yggdrasil/yggdrasil-desktop/scripts/tunnel.sh vps
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
```

**Tailscale serve** (the VPS on your tailnet): `tailscale serve --bg --https=8772 http://127.0.0.1:8772`
for each part, then the node's URLs are `https://<vps>.<tailnet>.ts.net:8772`
and so on. Add those origins to `YGG_CORS_ORIGINS` if a browser tab calls them.

## In the widget

Add the VPS as a node in `~/.config/yggdrasil/nodes.json` on each machine
whose widget should see it (my-widget's README, Nodes), with the VPS's owner
token (`~/.config/yggdrasil/owner.token` there). The widget then lists its
agents and goals beside this machine's, reads its sessions through Sindri
(`GET /v1/sessions/claude/{id}/transcript`, the owner only), and can send new
goals to it.

## Work and personal

The work laptop may read the personal nodes; nothing personal reads the work
laptop. So: the VPS's token never goes on the work laptop's Sindri or Norn,
and on the work laptop, Sága stays off or points at a work-owned Mímir, since
it shares that machine's coding sessions with Mímir.
