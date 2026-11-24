# Releases

A machine that should run a known, fixed version of Yggdrasil (the VPS, the
work laptop) installs a **release** instead of building from a checkout. The
machine you develop on keeps building from source with `scripts/install.sh`.

| Machine | Runs | Profile |
| --- | --- | --- |
| VPS | a release, `ygg upgrade` | dev |
| Work laptop | a release, `ygg upgrade --from` if it cannot download | full, `YGG_WORK=on` |
| Home PC | a checkout, `scripts/install.sh` | full |

A release is a version tag, `v0.2.0` (or `v0.2.0-rc.1`, a prerelease). For
each one, `.github/workflows/release.yml` publishes:

- `yggdrasil-<version>-linux-amd64.tar.gz` and `…-arm64.tar.gz`: the bundle
  a machine installs (`scripts/release-bundle.sh`): `bin/ygg` built with the
  version in it, `install.sh`, the systemd units, a `compose.yaml` that runs
  the release's image, `.env.example`, the docs and `VERSION`;
- `SHA256SUMS` over both bundles;
- `ghcr.io/sebbejohansson/yggdrasil:<version>`, the container parts' image, for
  amd64 and arm64.

Every part reports its version on `/health` (`ygg version` says it too), so
the widget's Parts page shows which version each machine runs.

## Cutting a release

```sh
git switch main && git pull
git tag v0.2.0
git push origin v0.2.0
```

The workflow runs the tests first; a failing test publishes nothing. Once the
first release's image is on ghcr.io, make the package public once
(github.com/SebbeJohansson → Packages → yggdrasil → Package settings → Change
visibility), or `docker compose pull` on the machines is refused.

To try the bundle before tagging: `scripts/release-bundle.sh v0.2.0 amd64 dist/`.

## Installing a machine

One command, as the user the parts should run as (not root), with Docker
installed, and for the dev profile Claude Code (or OpenCode) logged in:

```sh
curl -fsSL https://raw.githubusercontent.com/SebbeJohansson/yggdrasil/main/scripts/get.sh | sh -s -- --profile dev
loginctl enable-linger "$USER"          # keep the parts up after you log out
ygg status
```

`scripts/get.sh` downloads the newest release's bundle for this machine
(amd64 or arm64) and its `SHA256SUMS`, checks the one against the other,
unpacks the bundle into `~/.local/share/yggdrasil` and runs its `install.sh`.
`--version v0.1.0` installs a given release, `--profile full` every part,
`--tailscale` publishes the parts on your tailnet, and `--work` makes it a
work machine that never shares with your personal ones
([tailscale.md](tailscale.md)). Running it again reinstalls.

The install lives in `~/.local/share/yggdrasil` (`--home` or `YGG_HOME` to
move it): the bundle's files, plus `data/`, `profile/` and `.env`, which no
release touches. `install.sh` sees the bundle (a `VERSION` and a built
`bin/ygg`, no source), builds nothing, writes
`YGG_IMAGE=ghcr.io/sebbejohansson/yggdrasil:<version>` and `COMPOSE_PROFILES`
into `.env`, and pulls and starts the container parts. LanguageTool is built
from a sibling checkout, so a release starts it only when its image
(`pennvass-languagetool`) was built on that machine already, and never tries
to download it.

By hand, the same steps:

```sh
version=v0.1.0
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p ~/.local/share/yggdrasil && cd ~/.local/share/yggdrasil
curl -fLO "https://github.com/SebbeJohansson/yggdrasil/releases/download/$version/yggdrasil-$version-linux-$arch.tar.gz"
curl -fLO "https://github.com/SebbeJohansson/yggdrasil/releases/download/$version/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf "yggdrasil-$version-linux-$arch.tar.gz" --strip-components=1
scripts/install.sh --profile dev
```

## Upgrading, and going back

```sh
ygg upgrade v0.3.0      # or: ygg upgrade latest
ygg upgrade v0.2.0      # back: the same command with the older version
```

It downloads the bundle for this machine and the release's `SHA256SUMS`,
checks the one against the other, unpacks it into the install (staged first:
a bad bundle leaves the install as it was), and runs its `install.sh`, which
keeps the profile, installs the new `ygg`, restarts the host parts and pulls
the release's image for the container parts. Settings and tokens in
`~/.config/yggdrasil` stay; the installed version is in
`~/.config/yggdrasil/version`.

Without a download (a machine that should not reach GitHub), copy the bundle
and `SHA256SUMS` over and:

```sh
ygg upgrade v0.3.0 --from ~/yggdrasil-v0.3.0-linux-amd64.tar.gz   # SHA256SUMS beside it, or --sums
```

## From a checkout to a release

A machine that ran from a checkout (the VPS, before releases) moves over in
the same command: `--from-checkout` stops the parts the checkout runs and
moves its `data/`, `profile/` and `.env` into the install first.

```sh
curl -fsSL https://raw.githubusercontent.com/SebbeJohansson/yggdrasil/main/scripts/get.sh \
  | sh -s -- --profile dev --from-checkout ~/projects/yggdrasil/yggdrasil-desktop
```

Settings and tokens in `~/.config/yggdrasil` stay where they are, and so does
the host parts' own state in `~/.local/state/yggdrasil` (Sindri's goals, tasks
and agents). The checkout itself is left in place; remove it once the release
runs.
