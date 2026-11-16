# Yggdrasil

my-agent split into parts, each with one job, its own API, and its own
process. One Go binary, `ygg`, runs any part: `ygg gjallar`, `ygg munin`…
Why one process per part: [docs/architecture.md](docs/architecture.md).

| Part | What it does | Port | Runs as |
| --- | --- | --- | --- |
| **Gjallar** | alerts and warnings from every part, in one place | 8774 | container |
| **Ratatoskr** | watchdog: is each part alive and doing its job | 8775 | host unit |
| **Munin** | checks the systems outside Yggdrasil the parts depend on | 8776 | container |
| **Veðrfölnir** | reports on the machine: CPU, GPU, disks, services, containers | 8773 | host unit |
| **Norn** | assistant: inbox, calendars, sign-ins, memory, the front desk | 8771 | container |
| **Sindri** | coding: agents, worktrees, review, checks | 8772 | host unit |
| **Huginn** | reacts to what just happened: messages, meetings, alerts | 8777 | container |
| **Dellingr** | runs the jobs you put on a clock | 8778 | container |
| **Hermóðr** | carries every message for you to the widget and your phone | 8779 | container |

None of the watching parts fixes anything. Huginn only tells you things;
Dellingr acts only as far as you set a job up to. Everything listens on 127.0.0.1 only.

## Contracts

Each part's contract is an OpenAPI spec in [`api/`](api), and each part
serves its own at `GET /openapi.yaml`. A part can be replaced by any program
that meets its contract, in any language:
[docs/replacing-a-part.md](docs/replacing-a-part.md). Check one, built-in or
not:

```sh
ygg check munin                          # whatever YGG_MUNIN_URL points at
ygg check munin http://127.0.0.1:8786    # or any URL
```

Parts find each other only through `YGG_<PART>_URL`
(`~/.config/yggdrasil/parts.env`), never by assuming the built-in one.

## Run

The host has no Go; `scripts/go` runs the toolchain in Docker.

```sh
scripts/install.sh                 # build, write settings and tokens, start the host units
cp .env.example .env               # pick the container parts (COMPOSE_PROFILES)
docker compose up -d --build       # Norn, Gjallar, Munin, Huginn, Dellingr, and LanguageTool for Norn
curl localhost:8775/health
```

Settings and tokens are in `~/.config/yggdrasil/<part>.env`. Your owner token
is in `~/.config/yggdrasil/owner.token`:

```sh
T=$(cat ~/.config/yggdrasil/owner.token)
curl -H "Authorization: Bearer $T" localhost:8774/v1/alerts
curl -H "Authorization: Bearer $T" localhost:8775/v1/status
```

Every message meant for you goes through Hermóðr, and the widget's ask window
lists it. To also get it on your phone, set `NTFY_URL` in `hermodr.env`, and
the same in `gjallar.env` and `ratatoskr.env`: they push straight to it when
Hermóðr can't take a message. ntfy.sh is a public server; run your own ntfy
if what reaches your phone must not pass through it.

## Develop

```sh
scripts/dev.sh                 # every part from your working tree
scripts/dev.sh norn sindri     # just these; the rest keep running as installed
```

[air](https://github.com/air-verse/air), Go's usual live reloader, watches the
Go files and the specs, rebuilds `ygg` on every save and restarts the parts
you named, each with its own settings from `~/.config/yggdrasil` and its log
lines marked with its name. Their installed copies (the systemd units, the
containers) are stopped first, so the dev ones take the same ports and data,
and started again when you press Ctrl-C. The first run installs air into
`bin/tools`, through `scripts/go` like every other Go command here.

To keep dev mode running without a terminal, and after a reboot:

```sh
scripts/dev-mode.sh on      # every part from your working tree, rebuilt on every save
scripts/dev-mode.sh logs    # follow it
scripts/dev-mode.sh off     # the installed parts again
```

Norn, Gjallar, Munin, Huginn and Dellingr then run in the `dev` container (compose profile
`dev`), and Sindri, Veðrfölnir and Ratatoskr in `ygg-dev.service` on the host,
since they need your repos and toolchain, the host's `/proc` and systemd, or
must outlive Docker. `on` disables the installed units and stops the
containers; `off` enables the units again.

## Test

```sh
scripts/go vet ./...
scripts/go test ./...
```

Every built-in part's tests check it against its own contract: the routes it
serves match its spec, and every answer has the spec's shape.

## Routes

The specs in `api/` are the full reference; this is the short version.

Every part answers `GET /health` and `GET /openapi.yaml` without a token:
`{ ok, part, version, startedAt, loops: { <name>: { everySeconds, lastRunAt, lastError? } } }`.
Ratatoskr reads it. Everything else needs `Authorization: Bearer <token>`.

### Gjallar

| Route | Who | What it does |
| --- | --- | --- |
| `POST /v1/alerts` | a part | `{ key, severity: info\|warning\|critical, title, detail?, link?, traceId?, expiresAt? }` → `201` opened, or `200` updated. The source is the caller's token name. The same source and key updates the open alert; a resolved one opens again, and an acknowledged one opens again when its severity goes up |
| `POST /v1/alerts/resolve` | a part | `{ key }` → `{ resolved }`. Only the part's own alerts |
| `GET /v1/alerts` | anyone | `?state=active` (default: open and acknowledged), `open`, `acknowledged`, `resolved`, `all`; `?limit=` (200) → `{ alerts }`, most recently seen first |
| `GET /v1/alerts/mine` | a part | `{ alerts }`: the caller's own open and acknowledged alerts. A part reads it on start, so it resolves what it raised before a restart. The owner has none |
| `GET /v1/alerts/stream` | anyone | server-sent events, one `alert` per change. It needs the token, so read it with `fetch`, not `EventSource` |
| `GET /v1/alerts/{id}` | anyone | `{ alert, events }`: the alert and its history |
| `POST /v1/alerts/{id}/ack` | owner | seen it |
| `POST /v1/alerts/{id}/snooze` | owner | `{ until }`, at most 30 days ahead: no push until then |
| `GET /v1/mutes`, `POST /v1/mutes` | owner | `{ source, key, muted }`: a muted alert is listed, never pushed |
| `POST /v1/heartbeat` | a part | I am alive. A part named in `GJALLAR_HEARTBEATS` that goes silent is raised as `silent:<name>` |

How loud: `info` shows on the dashboard, `warning` adds a widget badge,
`critical` adds a widget toast and an urgent message through Hermóðr (straight
to ntfy when Hermóðr can't take it), at most once per alert every 15 minutes.

### Ratatoskr

| Route | What it does |
| --- | --- |
| `GET /v1/status` | `{ parts, checks, gjallarUp }`: every part as last seen, and every check. The widget reads this directly, so it can show "Gjallar is down" without Gjallar |

It checks the parts in `RATATOSKR_WATCH` every 30 s, at their `YGG_<PART>_URL`. Down is `down:<part>`
(critical). A loop that hasn't run within three of its intervals plus a
minute is `stalled:<part>:<loop>` (warning).

### Munin

| Route | What it does |
| --- | --- |
| `GET /v1/status` | `{ targets, checks }` |

Targets are `name=kind:url` in `MUNIN_TARGETS`. The kinds are `http` (up when
anything answers), `ollama` (also has the models in `MUNIN_OLLAMA_MODELS`) and
`statuspage` (a provider's Statuspage says there is no incident). Local
targets are checked every minute and internet ones every 5 minutes. When this
machine is offline, that is one `offline` alert.

### Veðrfölnir

The same shapes as my-agent's runner, so my-widget's pages read either.

| Route | What it does |
| --- | --- |
| `GET /v1/system` | CPU, memory, disks and pressure, with an hour of 5 s samples |
| `GET /v1/system/windows` | the host as Windows sees it: memory, the WSL VM, CPU |
| `GET /v1/system/gpu` | GPU use, from Windows and from WSL |
| `GET /v1/system/consumers` | the 12 biggest consumers, grouped by service |
| `GET /v1/services` | systemd user units, compose containers and local HTTP servers, with what each uses |
| `GET /v1/services/{id}/logs` | `?lines=` (60): the last lines one of them logged |

Its alerts: a disk 90 % full (warning) or 95 % full (critical), a failed unit,
and a container that crashed or keeps restarting. Containers in the
`yggdrasil` compose project and `ygg-*` units are left out, because
Ratatoskr watches those.

### Huginn

| Route | What it does |
| --- | --- |
| `GET /v1/status` | `{ streams }`: Norn's and Gjallar's event streams, connected or not |
| `GET /v1/rules` | every rule, the built-in ones first |
| `POST /v1/rules` | `{ name, on: { event, …filters }, action: { kind: notify\|ask\|alert-impact, text }, deliver?, hold? }`. Disabled unless the owner makes it |
| `PATCH /v1/rules/{id}` | change it; enabling is owner only, and a built-in rule can only be switched on or off |
| `DELETE /v1/rules/{id}` | not a built-in one |
| `GET /v1/reactions` | `?state=held\|delivered\|failed\|skipped`: what it did in the last 7 days. The widget reads this |

It follows Norn's `GET /v1/events/stream` (`inbox.arrived`, `calendar.added`,
`calendar.moved`, `calendar.removed`, `calendar.ended`) and Gjallar's alert
stream, which it turns into `alert.opened`, `alert.escalated` and
`alert.resolved`. Built-in rules: a message from your manager or above, an
urgent direct message, a brief for a meeting added or moved into the next
hour (asked of Norn), what a critical alert stops, and a Linear ticket of
yours in Todo that no goal has, when Norn judges it development work (looked
at every 5 minutes through Sindri; the suggestion links to Goals, where you
add it). Each reaction goes
to Hermóðr, with the rule's `hold` so it waits while you are in a meeting. A
stream down for 5 minutes is `stream:<part>` (warning).

### Dellingr

| Route | What it does |
| --- | --- |
| `POST /v1/parse` | `{ text: "every weekday at 8" }` → `{ cron, timezone, said, next }`, to show back before saving |
| `GET /v1/jobs` | every job, by next run |
| `POST /v1/jobs` | `{ name, cron, timezone?, action: { kind: notify\|ask\|goal, text, title?, repos?, autoStart? }, deliver? }`. Disabled unless the owner makes it |
| `GET /v1/jobs/{id}` | the job and its last 20 runs |
| `PATCH /v1/jobs/{id}` | change it; enabling and `autoStart` are owner only |
| `DELETE /v1/jobs/{id}` | remove it |
| `POST /v1/jobs/{id}/run` | run it now, once. Owner only |
| `GET /v1/runs` | every job's runs in the last 30 days. The widget reads this |

A goal job drafts the goal in Sindri, and starts it only with `autoStart`.
Runs the machine slept through run once on wake, never once per missed tick.
A job that fails 3 times in a row is `failing:<job id>` (warning).

### Hermóðr

| Route | Who | What it does |
| --- | --- | --- |
| `POST /v1/messages` | a part | `{ title, text?, link?, priority?: low\|normal\|high\|urgent, hold?, push? }` → `201`. The sender is the caller's token name |
| `GET /v1/messages` | anyone | `?state=held\|sent\|listed\|failed`, `?unseen=true`: the last 7 days. The widget reads this |
| `POST /v1/messages/{id}/seen` | owner | the widget stops listing it |
| `GET /v1/channels` | anyone | ntfy: set up or not, the lowest priority each sends, the last send; quiet hours; whether you are in a meeting |
| `POST /v1/channels/{name}/test` | owner | a test message to one channel, past every rule |

Urgent messages go out at once. Others wait through `QUIET_HOURS`, and, when
they ask to `hold`, while you are in a meeting (your calendars, from Norn).
`push: false` lists a message for the widget only. Each channel sends what is
at or above its `<CHANNEL>_MIN`. Slack and Discord were left out on
purpose: nothing from your inbox should land where others in a workspace or
server could read it. A channel failing 3 sends in a row is
`channel:<name>` (warning).
