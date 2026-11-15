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

None of them fixes anything. Everything listens on 127.0.0.1 only.

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
docker compose up -d --build       # Gjallar and Munin
curl localhost:8775/health
```

Settings and tokens are in `~/.config/yggdrasil/<part>.env`. Your owner token
is in `~/.config/yggdrasil/owner.token`:

```sh
T=$(cat ~/.config/yggdrasil/owner.token)
curl -H "Authorization: Bearer $T" localhost:8774/v1/alerts
curl -H "Authorization: Bearer $T" localhost:8775/v1/status
```

To push critical alerts to your phone, set `NTFY_URL` in `gjallar.env` and
`ratatoskr.env` to the same ntfy topic.

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
| `GET /v1/alerts/stream` | anyone | server-sent events, one `alert` per change. It needs the token, so read it with `fetch`, not `EventSource` |
| `GET /v1/alerts/{id}` | anyone | `{ alert, events }`: the alert and its history |
| `POST /v1/alerts/{id}/ack` | owner | seen it |
| `POST /v1/alerts/{id}/snooze` | owner | `{ until }`, at most 30 days ahead: no push until then |
| `GET /v1/mutes`, `POST /v1/mutes` | owner | `{ source, key, muted }`: a muted alert is listed, never pushed |
| `POST /v1/heartbeat` | a part | I am alive. A part named in `GJALLAR_HEARTBEATS` that goes silent is raised as `silent:<name>` |

How loud: `info` shows on the dashboard, `warning` adds a widget badge,
`critical` adds a widget toast and a phone push (ntfy), at most once per alert
every 15 minutes.

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
