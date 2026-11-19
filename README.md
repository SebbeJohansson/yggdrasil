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
| **Loki** | tests the watchers: finds blind spots, and tricks a part for a few minutes to see who notices | 8780 | container |
| **Heimdall** | the security watchman: vulnerable packages, ports open beyond loopback, keys in commits, loose token files | 8781 | host unit |

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

Settings and tokens are in `~/.config/yggdrasil/<part>.env`. Norn's models
run on XeStrata (`CHAT_URL`). `OLLAMA_URL=off` in `norn.env` keeps Ollama out
completely: no fallback, the inbox judge uses XeStrata when Laya doesn't
answer (or always, with `INBOX_JUDGE=xestrata`). Any OpenAI-compatible
server works as `CHAT_URL` (llama.cpp, LM Studio…). On a machine without one,
`CHAT_URL=off` runs every call on Ollama, tool calls too, and
`LOCAL_MODEL=qwen3:4b` (say) names the one model they all use; per-call
settings such as `SUMMARY_MODEL` still win. Take `xestrata` out of
`MUNIN_TARGETS` and `VEDRFOLNIR_PROBES` there too. Embeddings for memory
recall come from the compose `embeddings` service (llama.cpp with
nomic-embed-text v1.5 on `127.0.0.1:8096`, `EMBED_URL` in `norn.env`); start it
with `docker compose --profile embeddings up -d embeddings`. Memories embedded
by another model are embedded again in the background. Your owner token
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

Names stay on this machine. Before Norn hands anything to Claude (a
question that needs tools, a coding task, your reply to an agent, a Pennvass
rewrite), it swaps people's names and email addresses for placeholders
(Person 1, Person 2…) and turns them back into names in what the agent
answers. Names are found from what Norn already knows (you, the calendars,
the inbox), by the local model (`NAMES_MODEL`, qwen2.5:7b), and by the shape
of an address. The local model gets 90 s per 4,000 characters, and a second
try after a timeout (a paged-out XeStrata is slow until that first call brings
it back). When it still does not answer, or a known name is still
there after the swap, nothing is sent. To get by without the local model for a
while, `PUT /v1/settings/send-names` with `{"hours": 2}` (at most 24) sends
names to Claude as they are until then; `{"hours": 0}` swaps them again. A Claude meeting brief is gathered by
Norn itself (the invitation, and your email and Teams chats with the people
invited, from the last 30 days) and given to a Claude agent with no tools.
A brief for a meeting with another company (by the invitees' address domains) adds an About line on what that company does, and says when there is no earlier email or chat with it.
A local brief is written by `BRIEF_MODEL` (qwen2.5:7b). With the chat's
debug view on ("Show the model calls"), each handover to Claude is listed
under your message: the text exactly as it went, each placeholder with the
real name and how it was found, and Claude's answer before and after the
names were put back.

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

Norn, Gjallar, Munin, Huginn, Dellingr, Hermóðr and Loki then run in the `dev` container (compose profile
`dev`), and Sindri, Veðrfölnir, Ratatoskr and Heimdall in `ygg-dev.service`
on the host, since they need your repos and toolchain, the host's `/proc` and
systemd, or must outlive Docker. `on` disables the installed units and stops the
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
| `GET /v1/you` | `{ on, patterns, checkedAt?, errors? }`: each of `stretch`, `late` and `week` with `active`, `since`, `nextGap` (and `nextGapEnds`) and `toldAt`, for a widget to show on hover |
| `GET`, `PATCH /v1/settings/you` | `{ on }`: watching you on or off, from the dashboard. Changing it is owner only, and it outlives a restart (`RATATOSKR_STATE`); `RATATOSKR_YOU` is the default until you set it |

A part Loki's proxy stands in front of (`LOKI_PROXY`, with Loki watched too) is asked on its own port (`LOKI_UPSTREAM_<PART>`, else its default) while Loki is down: if it answers, it is cut off, not down, and the one alert is Loki's, "Loki is down: Dellingr, Huginn and Munin are cut off", instead of one per part. It checks the parts in `RATATOSKR_WATCH` every 30 s, at their `YGG_<PART>_URL`. Down is `down:<part>`
(critical). A loop that hasn't run within three of its intervals plus a
minute is `stalled:<part>:<loop>` (warning). A loop whose last run failed is
`failing:<part>:<loop>` (warning), with the error as the detail; it resolves
after the loop runs cleanly again.

It watches you the same way, every 5 minutes, and none of it is a fault:
nothing goes to Gjallar.

| Pattern | Active when |
| --- | --- |
| `stretch` | you are inside a run of meetings with no gap of `RATATOSKR_YOU_BREAK` (15m) or more, `RATATOSKR_YOU_STRETCH` (4h) long or more. A break you kept free (Norn's `POST /v1/calendar/breaks`) ends a run like such a gap, however short. A focus block you booked from the week's plan does not: it is not a meeting, but not time away from the screen either, so it carries the run on (focus alone is no stretch) |
| `late` | it is past `RATATOSKR_YOU_LATE` (23:00, until 05:00) and one of your own Claude Code sessions changed in the last 10 minutes, or you just answered a Sindri agent that asked you something (an agent working on its own does not count) |
| `week` | meetings fill `RATATOSKR_YOU_PACKED` (60) percent or more of the working hours (`WORK_HOURS`, 09:00-17:00, Monday to Friday) from the start of today to Friday. Focus blocks you booked do not count, and are never named as its free time |

When one starts, it sends one quiet message through Hermóðr: priority low,
held through meetings, listed in the widget only, worded as a suggestion
("The next gap is at 14:00, a good moment to step away from the screen").
At most one per pattern per day (`week`: per week), read back from
Hermóðr's history after a restart, and none while Hermóðr says it is quiet
hours (a late evening inside `QUIET_HOURS` is seen on `GET /v1/you` but never
sent). No counts, no scores. Messages carry times only: meeting subjects and
people never leave Norn. It reads Norn's `GET /v1/calendar` (the work
calendar; declined, free, out-of-office and all-day events, breaks you
kept free and focus blocks you booked left out) and
Sindri's `GET /v1/sessions/recent` and `GET /v1/agents`, with `NORN_TOKEN` and
`SINDRI_TOKEN` in `ratatoskr.env` (`scripts/install.sh` adds them; restart
Norn and Sindri after).

| Setting (`ratatoskr.env`) | Default | What it does |
| --- | --- | --- |
| `RATATOSKR_YOU` | `on` | `off`: reads nothing, sends nothing, and `GET /v1/you` says `on: false`. Only the default: what you set with `PATCH /v1/settings/you` wins |
| `RATATOSKR_STATE` | `~/.local/state/yggdrasil/ratatoskr.json` | where that is kept |
| `RATATOSKR_YOU_EVERY` | `5m` | how often it looks; the `you` loop on `/health` |
| `RATATOSKR_YOU_STRETCH` | `4h` | how long a run of meetings is a stretch |
| `RATATOSKR_YOU_BREAK` | `15m` | the shortest gap that counts as a real break |
| `RATATOSKR_YOU_LATE` | `23:00` | when working late starts |
| `RATATOSKR_YOU_PACKED` | `60` | the percent of working hours in meetings that makes a week packed |
| `WORK_HOURS` | `09:00-17:00` | your working hours, Monday to Friday. Set it in `parts.env`, so Norn's gap reads the same day; `RATATOSKR_YOU_HOURS` still wins when set |
| `RATATOSKR_YOU_PUSH` | `off` | `on` lets Hermóðr push these to your phone too (with `NTFY_MIN=low` in `hermodr.env`, since they are low) |
| `TIME_ZONE` | `Europe/Stockholm` | yours |

### Munin

| Route | What it does |
| --- | --- |
| `GET /v1/status` | `{ targets, sources, checks }` |

Targets are `name=kind:url` in `MUNIN_TARGETS`. The kinds are `http` (up when
anything answers), `openai` (an OpenAI-compatible `/v1/models` has a model
loaded: XeStrata), `ollama` (also has the models in `MUNIN_OLLAMA_MODELS`) and
`statuspage` (a provider's Statuspage says there is no incident). Ollama is
not checked by default: Yggdrasil runs on XeStrata alone. Local
targets are checked every minute and internet ones every 5 minutes. When this
machine is offline, that is one `offline` alert.

Sources are modules for one outside system each, for what a URL can't tell.
A machine runs the ones named in `MUNIN_SOURCES`, and each reads its own
`MUNIN_<SOURCE>_*` settings; one not named does not run and shows nowhere.
Munin gives each one a loop on `/health`, damps its checks like a target's,
and keeps its alert feed's place in `MUNIN_STATE` (`data/munin.json`), so a
restart raises nothing twice.

| Source | What it watches | Settings |
| --- | --- | --- |
| `tcgpriser` | tcgpriser.se: `/health` (`tcgpriser:down`, critical), `/admin/status` (`tcgpriser:status`), every alert the site posts to Discord except the shop scraping's (`shop-import`) (`feed:tcgpriser:<key>`, its own severity, closed when the site resolves it or after 24 h), and `/admin/usage` for the last 7 days | `MUNIN_TCGPRISER_TOKEN`, a service token from the API's `API_AUTH_TOKENS` (without it, `/health` only); `MUNIN_TCGPRISER_URL` (`https://api.tcgpriser.se`) |

A new source is a package under `munin/` that implements `munin/source`, and
one line in `Sources` in `munin/sources.go`.

### Sindri

Coding agents, worktrees, goals and the night shift; `api/sindri.yaml` has
every route. Races:

| Route | Who | What it does |
| --- | --- | --- |
| `PUT /v1/goals/{id}/race` | owner | `{ race }`: a draft goal races. Marked "run tonight" too, the night shift starts it twice, once on each conduit of the pair, each in its own worktree and branch |
| `POST /v1/goals/{id}/race/pick` | owner | `{ winner: <conduit>\|neither, merge? }` once both lanes have finished: the winner's branches merge through the usual merge (`merge: false` keeps them to review), the loser's worktrees and branches are removed. `neither` removes both and closes the goal |
| `GET /v1/races` | anyone | `{ races }`: every goal that raced, with its lanes (`racing`) and your pick |
| `GET /v1/races/stats` | anyone | wins per kind per conduit, from every pick |
| `PUT /v1/goals/{id}/conduit` | anyone | `{ conduit }`: what a goal runs on when it starts as usual; `""` is the default |
| `GET /v1/settings/races`, `PATCH` | owner to change | `{ pair, minPicks }` (Claude Code against OpenCode, 5), and which conduits are up |

The conduits are Claude Code, OpenCode and Copilot CLI. An OpenCode agent is
a session of the `opencode serve` at `OPENCODE_URL`, in its own worktree;
OpenCode's own config decides its tool permissions. `OPENCODE_URL=off` in
`sindri.env` turns OpenCode off completely: Sindri neither reads its
sessions nor starts agents in it, and it is not listed, raced, suggested or
offered for a goal (a race pair naming it becomes the first two conduits
that are on). `COPILOT_BIN=off` does the same for GitHub Copilot CLI: no
agent starts or carries on in it, it is not a choice for the coding agent
(`GET /v1/settings/coding-agent` lists the `engines` that are on, and a
saved `copilot` reads as `claude`), and it is not raced. `CLAUDE_BIN=off` turns
Claude Code off the same way, herdr starts included: then every Claude task,
Norn's question agents and Claude meeting briefs among them, is refused with
"Claude is off here" (your own Claude Code sessions are still read). When only one conduit
of the pair is up, the goal runs once on it, and the goal (`raceNote`) and
the morning report say so. The morning report lists the races ready to pick,
and picks nothing itself.

Each pick is kept with the goal's kind (ui, bugfix, refactor, tests, docs,
backend, other), judged from the goal's text by Norn's local model (asked
with `/local`, so it stays on this machine), and by keywords until it
answers or when Norn is out of reach. Once a kind has `minPicks` picks with
a winner, a draft goal of that kind that does not race shows the conduit
that wins it most (`suggestion`; none on a tie). It only suggests: you
choose with `PUT /v1/goals/{id}/conduit`.

### Sindri: your prompts, kept

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/sessions/prompts?from=&to=` | anyone | `{ prompts, oldest }`: your own prompts in your Claude Code sessions in a range (epoch ms, a day by default, at most 93 days), oldest first: `{ at, session, title, cwd?, project?, branch?, prompt }` |

Claude Code deletes its transcripts after `cleanupPeriodDays` (30 by
default), so Sindri keeps a small index of what you typed in its own store
(`session_prompts`): the time from each transcript line, the session, its
title, folder and branch, and the prompt clipped to 200 characters. Never
Claude's answers or tool output, and not Sindri's own agents. A loop reads
new transcript lines every 3 minutes, from where it left off; the first run
after start reads every transcript still on disk. The same session and time
is kept once. Norn's Rewind reads it. It is kept forever unless
`SINDRI_PROMPTS_KEEP` (a duration, e.g. `8760h`) is set in `sindri.env`.
Goal tasks on `GET /v1/goals` carry `startedAt` and `mergedAt`.

### Sindri: a goal's why

| Route | Who | What it does |
| --- | --- | --- |
| `GET`, `PATCH /v1/settings/why` | owner changes it | `{ on }`: whether a goal's agents get the why; on until turned off |

When a goal's coding agents start (Start, a restart, the night shift, each
lane of a race), Sindri asks Norn's `POST /v1/why` with the goal's title,
background, repos and id, and puts what comes back in the prompt under
`## Why this goal exists`: one line per source with its day and kind,
summarized by Norn's local model, names as placeholders. Where it is told
whose words to follow, the goal wins. The `ref` of each source stays with
you. Sindri waits at most `SINDRI_WHY_TIMEOUT` (30s) in `sindri.env`; when
Norn does not answer, the agents start without it. A pack is used again for
an hour, and Norn not answering for 5 minutes, so a race's two lanes wait
once. A self-repair goal is never asked about: its alert is its why.

What was sent is kept on the goal: `why` on `GET /v1/goals` is
`{ at, state, sources }`, `state` being `sent`, `none` (Norn found nothing it
was sure of), `unreachable` or `off` (Norn was not asked). Sindri asks with
its `NORN_TOKEN`; without one it never asks.

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
| `POST /v1/observations` | owner: `{ action, target?, at? }`, what you just did on the dashboard: `open:/dashboard/<page>`, `copy-draft`, or `ack-alert`, `snooze-alert`, `mute-alert` with target `<source>:<key>` → `204` |
| `GET /v1/suggestions` | rules and mutes it suggests from what you do, waiting for you |
| `POST /v1/suggestions/{id}/accept` | owner: makes the rule, enabled, or the mute on Gjallar |
| `POST /v1/suggestions/{id}/reject` | owner: no thanks, and that pattern is never suggested again |
| `GET`, `PATCH /v1/settings/learning` | `{ on, windowMinutes }`: on and 10 until set; off forgets what it saw |

It follows Norn's `GET /v1/events/stream` (`inbox.arrived`, `calendar.added`,
`calendar.moved`, `calendar.removed`, `calendar.ended`, `calendar.duplicate`,
`calendar.stale`, `savepoint.ready`, `promise.due`, `weave.ready`) and Gjallar's alert
stream, which it turns into `alert.opened`, `alert.escalated` and
`alert.resolved`. Built-in rules: a message from your manager or above, an
urgent direct message, a brief for a meeting added or moved into the next
hour (asked of Norn), what a critical alert stops, and a Linear ticket of
yours in Todo that no goal has, when Norn judges it development work (looked
at every 5 minutes through Sindri; the suggestion links to Goals, where you
add it), and a meeting you put in your calendar yourself that an invite now
covers, or whose invite was cancelled (widget only; Norn's
`GET /v1/calendar/suggestions`, and the dashboard deletes your copy with
`POST /v1/calendar/suggestions/{id}/apply`, which needs the Microsoft sign-in
with the calendar, now `Calendars.ReadWrite`), and, when a meeting ends, what
you were on before it ("You were on: …" with your last prompt, linking to the
goal or session; widget only and not held, so the prompt never reaches ntfy),
a promise you made that is due (below), and the week's plan when Norn made
it (`week-plan`: "The week's plan" with the week's line, linking to the plan;
widget only and not held). Each reaction goes
to Hermóðr, with the rule's `hold` so it waits while you are in a meeting. A
stream down for 5 minutes is `stream:<part>` (warning).

Huginn also learns your habits and offers to write the rule for you
(`huginn/learn.go`). The dashboard reports what you do, and what you do
within 10 minutes after an event (`windowMinutes`) is a pair: a meeting Anna
organizes ends, and you open the meeting note. It learns from messages from
someone with a known relation, meetings with an organizer (`calendar.soon`,
`calendar.ended`) and opened alerts. Five pairs of the same kind of event
and action in 30 days, done after at least 70 % of those events, become a
suggested rule, "Sync with Anna Berg ended: open the meeting note?",
delivered to the widget only and linking to that page (the rule's
`action.link`, filtered on `on.organizer` or, for an alert, `on.key`). The
same alert acknowledged, snoozed or muted five times, an hour apart, becomes
a suggested Gjallar mute; the dashboard mutes it on Gjallar itself and then
accepts it, so Huginn never holds your owner token. A new suggestion is told through Hermóðr at most
once a day, listed and never pushed (`push: false`, `hold: true`). Only kinds
and ids are kept, for 30 days in `huginn.db`: page names, an alert's source
and key, an event's kind with its relation, organizer and calendar. Never a
message's text.

### Norn: keeping a break free

| Route | Who | What it does |
| --- | --- | --- |
| `POST /v1/calendar/breaks` | owner | `{ start, end }` (epoch ms, 15 min to 2 h, at most 7 days ahead) → `201 { event }`, or `200 { event }` when that slot already is a break |

It puts a "Break" event in your work calendar (Outlook), shown as busy so
nobody books over it, with nobody invited and the Outlook category
`Yggdrasil break`, which is how Yggdrasil knows it as its own: on
`GET /v1/calendar` it has `break: true`. Norn reads the calendar first and
answers `409` when something you did not decline overlaps the slot. It needs
the Microsoft sign-in with `Calendars.ReadWrite`. A break is not a meeting:
Norn sends no `calendar.*` events for it, Ratatoskr ends a stretch at it
and does not count it toward a packed week, and Hermóðr holds messages
through it and says `inBreak` rather than `inMeeting`.

### Norn: the week's plan (the weave)

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/weave` | anyone | `{ week, madeAt, words, waiting, days: [{ date, words, blocks: [{ id, kind: focus\|break, start, end, for?, link?, booked, eventId?, skipped }] }] }`; `week` is `""` before the first plan |
| `POST /v1/weave/book` | owner | `{ ids? }` (every open block when empty) → `{ results: [{ id, booked, eventId?, error? }], weave }` |
| `POST /v1/weave/remake` | owner | make it again now; blocks already booked stay |
| `POST /v1/weave/blocks/{id}/skip` | owner | leave one block out (`409` once booked) |
| `GET`, `PUT /v1/settings/weave` | owner to change | `{ on, makeAt, focusMinutes }`: on, `18:00` and `90` until set |

Once a week Norn proposes the coming Monday to Friday, within `WORK_HOURS`. It
looks every 3 minutes, and on start: from Sunday's `makeAt` (18:00 in
`TIME_ZONE`) the next week is due, and on a weekday the current one, so a
machine that was off on Sunday plans on Monday morning, and one started on
Wednesday plans the rest of the week. A week is planned once; only Make it
again plans it anew. The plan waits for a fresh read of the work calendar.

It is made by Norn's own code, not a model. Busy is what the gap reads as
busy, and breaks and focus blocks Yggdrasil booked count as booked. A day is
packed when meetings fill 60% of its working hours, as Ratatoskr's packed
week counts them. Then:

- **Focus blocks**: at most one a day, never on a packed day or one with a
  focus block booked already, in the longest free stretch of `focusMinutes`
  or more, mornings (before 12:00) first, two hours at most. Each gets a
  suggested use from what waits: a promise due on or after its day (or late
  already), soonest first ("the promise to Anna due Thursday: send the
  deck"); what a draft that runs tonight did, on the first day; then a review,
  a race to pick, a draft to start, an agent to answer.
- **Breaks**: after each run of meetings of 3 hours or more (gaps under 15
  minutes do not end a run), 15 to 30 minutes in the first free time after
  it, unless a break you kept is there already.
- One line per day ("Tuesday is heavy: a break after lunch, no focus
  block.") and one for the week. The local model (`SUMMARY_MODEL`) may say
  the week's line more warmly; a line that drops a day, adds a number or
  exclaims is thrown away, and without the model the plain line stands.

When it is made with a block in it, Norn sends `weave.ready` once, and
Huginn's `week-plan` rule tells the widget. Nothing is booked by itself: Book
keeps each block the way a break is kept, a break as "Break", a focus block as
"Focus" with the Outlook category `Yggdrasil focus` (`focus: true` on
`GET /v1/calendar`), busy, one `transactionId` per slot, so a double click is
one event. A slot no longer free gets its reason. A focus block is not a
meeting: no `calendar.*` events or save points for it, Hermóðr holds what asks
to hold through it (`inFocusBlock`), the gap offers nothing in it, Ratatoskr
does not count it toward a packed week, and passive Loki does not strike in
it. Calendar, goals and promises stay on this machine: nothing here goes to
Claude.

### Norn: save points

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/savepoints?limit=` | any | `{ savepoints }`, newest first (20, at most 100) |
| `POST /v1/savepoints/{id}/done` | owner | put one away → the save point with `doneAt` |

When a meeting starts, Norn keeps your place: the open goal (Sindri's
`GET /v1/goals`, by its agents' last activity) and your own Claude Code
session (`GET /v1/sessions/recent`: title, folder, branch, your last prompt
clipped to 200 characters) that moved last in the 30 minutes before. Nothing
is kept when nothing moved, and not for a declined, all-day or free meeting,
a break you kept free, or a meeting already running when Norn started. Two
overlapping meetings share the first one's save point. When the meeting ends
(as the calendar says by then), Norn sends `savepoint.ready` with it, Huginn
tells the widget, and the dashboard's overview shows "You were here" with
Pick up until you put it away or two hours pass. It all stays on this
machine: nothing here goes to Claude.

### Norn: promises you made

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/promises` | anyone | `?state=open\|done\|dropped` (open) → `{ promises }`: `{ id, what, to, toAddress?, due?, state, source, message, link?, subject?, thread, createdAt, changedAt, doneBy?, doneWhy?, doneLink?, remindedAt?, dev? }` |
| `PATCH /v1/promises/{id}` | owner | `{ state?, due? }`: Done, Drop, open again, or a new day (`YYYY-MM-DD`, `""` for none), which is reminded again |
| `PUT /v1/promises/{id}/dev` | a part | `{ dev }`: Huginn's judgement that it is development work |

Every 2 minutes Norn reads what you sent in the last 3 days: Outlook's Sent
Items (the Microsoft sign-in with mail), Gmail's Sent (the Google sign-in with
Gmail), your own last message in each Teams chat, your Slack messages the
inbox's sweep sees, and the meeting notes you save on the dashboard (After the
meeting, and Done with the notes taken during it). A message with no promise
words ("I'll", "jag ska", "återkommer"…) is skipped. The rest go to the local
model only (`CHAT_URL`), never to Claude or Sindri, four a pass, and it
answers with each promise: what, to whom (matched to the message's
recipients), and the words for when, which Norn turns into a day in
`TIME_ZONE` ("Friday" is the next Friday, "i morgon" tomorrow, "24/11";
"soon" and "next week" give none). Only your own words: a reply's quoted
thread is cut off. A promise is kept once per message (or meeting) and text;
the message itself is not kept.

On the working day before its day, from 08:00, or on the day when it was made
too late for that, Norn publishes `promise.due` once. Before that, for a
promise made in a thread, it writes a follow-up with the reply drafter (on
`GET /v1/drafts` as item `promise:<id>`; `POST /v1/drafts` with that item
writes one whenever you ask). Huginn's built-in `promise-due` rule
then says "You promised Anna: Send the Q3 deck, by tomorrow", held through
meetings, with a link to the dashboard. When a later message of yours to the
same person keeps an open promise (the local model judges, strictly), it is
marked done by `norn` with why; you can open it again. `PUT /v1/settings/promises`
`{ on: false }` (owner) turns the ledger off: nothing you send or save is read
for promises, no follow-ups are written and no reminders sent; what it kept
stays listed (`GET /v1/settings/promises` says whether it runs).

### Norn: a task sized to your gap

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/gap` | anyone | `{ free, freeUntil?, item? }`: `item` is `{ kind, title, text, minutes, link, goalId?, promiseId? }` |

You are free when nothing in the work calendar holds you now (a meeting not
declined, not all day, not shown as free; a break you kept free counts as
busy, since a break is not for work, and so does a focus block you booked)
within the working day, `WORK_HOURS` in
`parts.env` (09:00-17:00, Monday to Friday), the one Ratatoskr reads too. The gap lasts until the next such
meeting or the end of the day. What waits: Sindri's goals by their next step,
and the open promises due by tomorrow. Each kind has a rough size: answer an
agent 3 minutes, start a draft 2, pick a race 10, a promise 10, review a
branch 20. The item is the one that fits with 5 minutes to spare, what blocks
an agent first, then what has waited longest; none when the gap is under 10
minutes, nothing fits, or a save point still waits to be picked up. `link` is
the dashboard path that does it: the goal with its agent open (on its review
for a review), `/dashboard/races/<goal>`, or `/dashboard/inbox#promises`. It
all stays on this machine: nothing here goes to Claude.

### Norn: Rewind

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/rewind?from=&to=` | anyone | `{ from, to, entries, unavailable }`: one timeline (epoch ms; today in `TIME_ZONE` by default, at most 31 days): `{ at, end?, kind, title, detail?, link?, source }` in time order |
| `POST /v1/rewind/ask` | anyone | `{ q }`: "Tuesday afternoon", "yesterday at 3", "when did I last work on races?" → `{ from, to, focus, entries, said, words, fromModel, unavailable }`, the entries that match marked `match` |

Built when asked from what Yggdrasil already keeps; nothing is copied. How
far back each source reaches:

| Source | Kinds | How far back |
| --- | --- | --- |
| Outlook and Google calendars | `meeting`, `break` | any range: Graph's calendarView and Google Calendar are asked for it when it is older than the day Norn keeps (kept 5 minutes per range); declined, all-day and free meetings left out |
| Sindri's prompt index | `session`, `prompt` | from the oldest transcript on disk when Sindri first indexed, then forever (`SINDRI_PROMPTS_KEEP`) |
| Sindri's goals and agents | `goal`, `merge`, `agent` | forever: goals made, started, merged, closed; agents outside a goal |
| your messages to Norn and its agents | `chat` | forever |
| what Norn learned | `memory` | until you delete it |
| promises | `promise` | open ones forever, done or dropped ones 60 days |
| save points | `savepoint` | the last 200 |
| reply drafts you used | `reply` | the last 500 |
| Obsidian notes changed | `note` | titles and links only, never note text. A note's last change always; earlier changes from when Norn first ran with Rewind (it notes each note's modification time every 5 minutes) |

A burst (your prompts in one session, or messages in one conversation, with
pauses of at most 10 minutes) is one entry with the first and the last, each
clipped to 200 characters. A question goes to the local model only
(`CHAT_URL`, never Claude or Sindri), which only splits it into the words for
when and the words to look for; Norn turns the when into a range in
`TIME_ZONE` ("Tuesday" is the latest Tuesday, "i tisdags" and "last Tuesday"
the one before today, "at 3" is 14:00-16:00). Without a model the question is
read as it is. Words with no day look back 31 days and send only the matches,
`focus` on the latest.

### Norn: a goal's why

| Route | Who | What it does |
| --- | --- | --- |
| `POST /v1/why` | anyone (Sindri) | `{ goal, context?, repos?, goalId? }` → `{ sources }`: at most 5 `{ kind, at, summary, ref }`, `kind` mail, meeting or rewind |

When Sindri starts coding agents on a goal, it asks Norn what lies behind
it. Norn looks on this machine for what holds at least two of the goal's
words (the one, for a one-word goal; words like "add", "fix" or "page" left
out): the inbox (Outlook, Teams, Slack, Gmail), Outlook mail searched through
Graph (with the Microsoft sign-in), the Obsidian meeting notes, and Rewind's
timeline of the last 31 days (meetings, your messages to Norn, what it
learned, promises, reply drafts, notes changed; Sindri's goals left out).
The best 8 go to the local model only (`CHAT_URL`, never Claude or Sindri),
which keeps what is clearly about the goal as one line each. The lines then
have names swapped for placeholders as for any handover to Claude, and a line
that may hold a key or a token is dropped. The pack is empty when nothing
matches surely, either model does not answer, or names are being sent as they
are (`PUT /v1/settings/send-names`). `ref` says where each was found
(`inbox:<id>`, `outlook:<id>`, `obsidian:<path>`, `rewind:<kind>:<at>`), for
you; it is never sent to an agent. It takes at most 25 seconds.

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

### Loki

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/plan` | anyone | `{ checkedAt, blindSpots, tales }`. Blind spots, rebuilt every hour: `{ id, part, kind, text, severity: low\|medium\|high, trick? }`, where `trick` is the strike that would test it. Tales: `{ id, alertId, part, title, text, openedAt, noticedAfterMs?, healedAfterMs? }`, one per real alert that opened and resolved in the last 14 days |
| `GET /v1/targets` | anyone | `{ parts: [{ name, up, lokiOn, proxied, freezable, vias: [{ via, available, reason? }], tricks: [{ via, kind, loop?, delayMs?, status?, label, expect }] }] }`: each part's tricks by each way to strike it (switch tricks from its `/health`; proxy tricks only when proxied; freeze only when freezable), why a way is closed, and what should catch each (`ratatoskr:down:<part>`, `ratatoskr:failing:<part>:<loop>`, `gjallar:alert`…) |
| `POST /v1/strikes` | owner | `{ part, trick: { via?: switch\|proxy\|freeze, kind: health\|loop\|slow\|error\|drop\|hang\|freeze, loop?, delayMs?, status?, path? }, minutes? }` (3, at most 10) → `201` Strike. No `via`: `freeze` for a freeze, `proxy` for error, drop and hang, else `switch` |
| `GET /v1/strikes` | anyone | `?limit=` (50) → `{ strikes }`, newest first |
| `GET /v1/strikes/{id}` | anyone | `{ id, part, trick, by: manual\|passive, startedAt, endsAt, healedAt?, state: striking\|watching\|caught\|missed\|stopped, checks: [{ watcher, expected, noticedAt?, ok? }], tale?, pids? }` |
| `POST /v1/strikes/{id}/stop` | owner | end it now: the trick comes off, and it ends `stopped`, with no verdict |
| `GET`, `PATCH /v1/settings` | owner changes it | `{ passive, perDay, allowWatchers, allowFreeze }`: off, 1, off and off until set; kept in `LOKI_STATE` |

A trick strikes one of three ways, its `via`: `switch`, from inside, with the
part's own help; `proxy`, from outside, where the part never knows; and
`freeze`, which stops the part's process. Every one ends at its `until`, at
most 10 minutes ahead, even if Loki dies.

**Switch.** Every part has a trick switch
(`internal/part`), which answers only with `LOKI=on` in that part's
environment: set it in `parts.env` in dev mode, never on a machine you rely
on. Without it, `/v1/loki/trick` is `404` and nothing can be tricked. Only
the owner and Loki's token may use it. A trick is an in-process behavior
with an `until`, at most 10 minutes ahead, and it ends then on its own,
even if Loki dies:

| Trick | What the part does | Should be caught by |
| --- | --- | --- |
| `health` | `/health` answers `503`, `ok: false` | `ratatoskr:down:<part>`, and Ratatoskr's alert on Gjallar (`gjallar:alert`) |
| `loop` | the named loop's runs fail with "Loki's trick" | `ratatoskr:failing:<part>:<loop>` and `gjallar:alert`; Ratatoskr's `heartbeat` loop: `gjallar:silent:ratatoskr` |
| `slow` | every route but `/health` and the trick switch waits `delayMs` (3000, at most 10000) | `ratatoskr:slow:<part>`, which nothing raises yet: a strike shows the blind spot |

**Proxy (dev only).** Parts find each other only through `YGG_<PART>_URL`, so
Loki can stand at that address. With a part in `LOKI_PROXY`, Loki listens
on a port of its own for it, the part's port + 100 (Norn 8871, Sindri 8872 …
Hermóðr 8879), and passes every call through to the real part untouched:
method, path, query, headers (Authorization too), body, status and response
headers, server-sent events (Norn's `/v1/events/stream`, Gjallar's
`/v1/alerts/stream`) streamed as they come and kept open, long calls left
to run. Nothing shows the proxy to a caller. Point `YGG_<PART>_URL` in
`parts.env` at the proxy and every part, Ratatoskr included, reaches it
through Loki; Loki itself calls the real part at `LOKI_UPSTREAM_<PART>`
(never `YGG_<PART>_URL`), and refuses an upstream that would reach itself.

While Loki is down, a proxied part is cut off from the others: Ratatoskr's
`/v1/status` shows it down, and Gjallar raises it. Keep the proxy to dev
mode, and take a part out of `LOKI_PROXY` and its `YGG_<PART>_URL` back to
its own port before you rely on it.

| Trick (via proxy) | What callers get | Should be caught by |
| --- | --- | --- |
| `health` | `/health` answers `503`, the part's own answer with `ok: false` | `ratatoskr:down:<part>` and `gjallar:alert` |
| `slow` | every call but `/health` waits `delayMs` | `ratatoskr:slow:<part>` (nothing yet) |
| `error` | every call but `/health` answers `status` (500, or 502); with `path`, only paths under it | `ratatoskr:errors:<part>` (nothing yet) |
| `drop` | the connection is closed unanswered | `ratatoskr:down:<part>` and `gjallar:alert` |
| `hang` | calls are held unanswered until the trick ends (then they go through) or the caller gives up | `ratatoskr:down:<part>` and `gjallar:alert` |

**Freeze.** Loki sends the part's process `SIGSTOP`, and `SIGCONT` at
`until`. The thaw is scheduled when the strike starts, in a detached
`sh -c 'sleep N; kill -CONT <pid>'` in a session of its own, so it comes
even if Loki dies; Loki also thaws at once when the strike ends or is
stopped and when it shuts down, and calls the detached thaw off. A part is
freezable when Loki sees exactly one process running as `ygg <part>` that it
may signal: its own PID namespace, its own user. In dev mode Loki runs in
the dev container, so it can freeze Norn, Gjallar, Munin, Huginn, Dellingr
and Hermóðr, but not Sindri, Veðrfölnir and Ratatoskr, which run on the
host; with the installed containers it can freeze none. Expected:
`ratatoskr:down:<part>` and `gjallar:alert` (Ratatoskr's `heartbeat` to
Gjallar for Ratatoskr itself). Never Loki itself.

A strike reads Ratatoskr's `/v1/status` and Gjallar's alerts every 10 s and
notes when each watcher noticed. After the trick ends it waits up to 5
minutes for the part to be well and the alerts resolved (`healedAt`); a
watcher noticing then still counts. `caught` is every watcher noticing;
`missed` raises `missed:<part>:<kind>` (warning) on Gjallar, which
self-repair picks up, and the next caught strike of that kind resolves it.
One strike per part at a time, never Loki, never Gjallar while another strike
runs, and never Ratatoskr unless `allowWatchers` is on. A `health` strike
on a part (or a freeze, drop or hang) raises a real critical alert, so it
reaches your phone like one.

With `passive` on, Loki strikes on its own at most `perDay` times, at random
times in your working hours (`WORK_HOURS` in `parts.env`, 09:00-17:00, Monday
to Friday), never at night, and never while Hermóðr says you are in a
meeting, on a break, in focus or in a focus block you booked: then it tries again 15 minutes later. It
picks a blind spot that has a trick, or else a random part and trick, never
Gjallar: switch tricks on parts whose switch answers, proxy tricks on
proxied parts, and freezes only with `allowFreeze` on. Afterwards one quiet Hermóðr message (`push: false`) tells the tale.

Blind spots are parts that run but aren't in Ratatoskr's watch list, no
heartbeat from Ratatoskr in `GJALLAR_HEARTBEATS`, critical alerts with no
phone channel on Hermóðr, loops never seen failing, parts never yet proven
going down by a caught strike, slowness, which nothing times, and a part
answering errors on its routes while its `/health` is fine, which nobody
would notice (its trick is a proxy `error` once a part is proxied). Tales
leave out Loki's own alerts and those a strike caused; `noticedAfterMs` is
when you acknowledged the alert.

| Setting (`loki.env`) | Default | What it does |
| --- | --- | --- |
| `LOKI_TOKENS` | | `owner=…` |
| `<PART>_TOKEN` | | Loki's token at each part: `NORN_TOKEN`, `GJALLAR_TOKEN`, `RATATOSKR_TOKEN`, `HERMODR_TOKEN`… (`scripts/install.sh` gives them) |
| `LOKI_STATE` | `data/loki.json` | settings and the last 200 strikes |
| `LOKI_PARTS` | every part but Loki | the parts it may trick |
| `GJALLAR_HEARTBEATS` | `ratatoskr=3m` | read to find the heartbeat blind spot; set it in `parts.env` if you changed Gjallar's |
| `LOKI_PROXY` | none | the parts Loki's proxy stands in front of, `norn,sindri,…` (dev only) |
| `LOKI_PROXY_ADDR_<PART>` | `127.0.0.1:<port + 100>` | where that part's proxy listens |
| `LOKI_UPSTREAM_<PART>` | `http://127.0.0.1:<port>` | where the real part is; never read from `YGG_<PART>_URL` |

To put Norn and Hermóðr behind the proxy, in `parts.env` (read by Loki and by
every part that calls them):

```
LOKI_PROXY=norn,hermodr
YGG_NORN_URL=http://127.0.0.1:8871
YGG_HERMODR_URL=http://127.0.0.1:8879
```

### Heimdall

The security watchman, on 8781. Once a night (03:00 in `TIME_ZONE`, or as
soon as the machine is on after it) and when you ask, it runs five small
checks and keeps what they find.

| Route | Who | What it does |
| --- | --- | --- |
| `GET /v1/report` | anyone | `?state=active\|resolved` → `{ checkedAt, running, findings: [{ id, check, severity: low\|medium\|high, title, detail, where?, fix?, repo?, firstSeen, lastSeen, resolvedAt?, critical?, commit? }], checks: [{ name, ok, lastRun, error?, skipped? }] }`, high first |
| `POST /v1/report/run` | owner | `202 { running: true }`: run every check now, in the background |
| `GET`, `PATCH /v1/settings` | owner changes it | `{ at, repos, tokenAgeDays }`: 03:00, `HEIMDALL_REPOS` (else `~/projects/yggdrasil` and `~/projects/my-widget`), 180; kept in `HEIMDALL_STATE` |

| Check | What it looks at | Severity |
| --- | --- | --- |
| `deps` | each repo's `go.mod`, `yarn.lock`, `package-lock.json` (or exact versions in a `package.json` with no lockfile), up to three folders down, asked of [osv.dev](https://osv.dev) | the advisory's own (GitHub's critical and high are high), medium when it has none |
| `listening` | every TCP listener in `/proc/net/tcp` and `tcp6` not on 127.0.0.1 or ::1, with its process when Heimdall may see it | Yggdrasil's own ports (8771-8781, and Loki's proxies at port + 100) high; every address (0.0.0.0, ::) medium; one address low |
| `secrets` | the lines added by each repo's commits since the last run (the first: the last 200), on every branch and tag: GitHub, Slack, OpenAI, Anthropic and AWS keys, private key blocks, and high-entropy values set to a `*TOKEN`, `*KEY` or `*SECRET`; and untracked files named like `.env` that `.gitignore` doesn't cover | a key in a commit high, an unignored `.env` medium |
| `files` | `~/.config/yggdrasil`, each repo's `.env` files and `HEIMDALL_FILES`: readable by the group or others, or a token file unchanged for longer than `tokenAgeDays` | others high, the group medium, old low |
| `scopes` | the access Norn's Microsoft and Google sign-ins hold for a feature the widget hides both at home and away | low |

Only package names, versions and ecosystems leave the machine, in one
`querybatch` to osv.dev, then the ids it named to read each one, with the
User-Agent `heimdall-deps-check/1`: no name, email, repo or path. No finding,
alert or log holds a secret's value, only the file, line, kind and a masked
preview such as `ghp_…4f2a`. A line with `heimdall:ignore` on it is skipped,
and so are values that look made up (`0123456789`, `example`, `fake`…) or are
names (`calendar-suggestions-kept`). A secret in a commit stays a finding
until the commit is gone from every branch and tag; the fix is to rotate the
key, then remove it from history.

A finding not found again is resolved and kept 30 days. A check that fails
or is skipped leaves its findings as they were. The high findings of a check
raise one alert on Gjallar, `<check>:summary`, with how many and the first
ten: a warning, critical while one of them is a secret in a pushed commit or
a part listening beyond loopback; it resolves when the check has none left.
One alert per check, never one per finding: a first run over a long history
finds hundreds. A secret is a value with digits in it, not a name or a
reference (`config.API_TOKEN`, `keys.move_left`). Self-repair takes `deps:` and `listening:` alerts; a secret is never
handed to a coding agent.

It runs as a host unit, `ygg-heimdall.service`, like Veðrfölnir: it needs the
host's sockets and processes (a container sees only its own network), your
repos with their real history and git, and the real modes of the files in
`~/.config/yggdrasil`.

| Setting (`heimdall.env`) | Default | What it does |
| --- | --- | --- |
| `HEIMDALL_TOKENS` | | `owner=…` |
| `HEIMDALL_REPOS` | `~/projects/yggdrasil,~/projects/my-widget` | the repos, until `PATCH /v1/settings` sets them |
| `HEIMDALL_SINDRI_REPOS` | `off` | `on` also reads every repo Sindri knows (needs `SINDRI_TOKEN`) |
| `HEIMDALL_FILES` | none | more files to check the mode and age of, as globs (`~/projects/yggdrasil/data/*.json` for Norn's sign-ins) |
| `HEIMDALL_STATE` | `~/.local/state/yggdrasil/heimdall.json` | settings, findings, what each repo was read up to, the alerts raised |
| `HEIMDALL_OSV_URL` | `https://api.osv.dev` | osv.dev's API |
| `GJALLAR_TOKEN`, `NORN_TOKEN`, `SINDRI_TOKEN` | | Heimdall's token at each (`scripts/install.sh` gives them); without `NORN_TOKEN` the scopes check is skipped |
| `TIME_ZONE` | `Europe/Stockholm` | for the nightly run |

### Sindri: the night shift and self-repair

| Route | Who | What it does |
| --- | --- | --- |
| `GET`, `PATCH /v1/settings/night-shift` | anyone | `{ start, report }` as HH:MM in `TIME_ZONE` (22:00 and 07:00) |
| `GET`, `PATCH /v1/settings/self-repair` | owner changes it | `{ on, perNight }`: off and 3 until set; `SINDRI_SELF_REPAIR=on` in `sindri.env` turns it on by default |

Goals marked "run tonight" start at `start`, and at `report` one message
through Hermóðr says what is ready to review and what got stuck.

With self-repair on, Sindri first drafts goals in the yggdrasil repo for
what is wrong with Yggdrasil itself, marked to run tonight, with a source
starting `self-repair:`:

- Gjallar's active alerts from its own parts (Ratatoskr's `down:`,
  `stalled:` and `failing:`, Loki's `missed:`, Heimdall's `deps:` and
  `listening:`, and the parts' own), with the key, title, error and history.
  Munin, Veðrfölnir, sign-ins, the local model, ntfy and muted alerts are left
  out: nothing in this repo fixes them. So are Heimdall's `secrets:`,
  `files:` and `scopes:`: a secret never goes to a coding agent.
- Norn's dispatch picks you corrected the same way at least 3 times in a week
  (`GET /v1/decisions`), as "picked answer, you said code:yggdrasil, 4
  times". Only kinds, agent ids, projects and counts go in, never a
  message's text or a name.

No goal is drafted while one for the same alert or correction is open, within
24 hours of its last fix being merged (counted from that goal's last
activity), or for an alert that has resolved; a waiting draft whose alert resolved is closed. At
most `perNight` a night, critical alerts first. Each agent is told to
reproduce the problem, fix its cause and add a test, or, when not confident,
change nothing and say `NOT SURE:`. The morning report sorts them into fixed
(ready to review), not sure, still running and left alone. Nothing merges on its own: every
fix waits for you. Sindri reads Gjallar with its `GJALLAR_TOKEN` and Norn with
its `NORN_TOKEN`.

### Hermóðr

| Route | Who | What it does |
| --- | --- | --- |
| `POST /v1/messages` | a part | `{ title, text?, link?, priority?: low\|normal\|high\|urgent, hold?, push? }` → `201`. The sender is the caller's token name |
| `GET /v1/messages` | anyone | `?state=held\|sent\|listed\|failed`, `?unseen=true`: the last 7 days. The widget reads this |
| `POST /v1/messages/{id}/seen` | owner | the widget stops listing it |
| `GET /v1/channels` | anyone | ntfy: set up or not, the lowest priority each sends, the last send; quiet hours; whether you are in a meeting (`inMeeting`, `meetingEndsAt`) on a break you kept free (`inBreak`, `breakEndsAt`) or in a focus block you booked (`inFocusBlock`, `focusEndsAt`) |
| `POST /v1/channels/{name}/test` | owner | a test message to one channel, past every rule |

Urgent messages go out at once. Others wait through `QUIET_HOURS`, and, when
they ask to `hold`, while you are in a meeting, on a break you kept free or
in a focus block you booked (your calendars, from Norn).
`push: false` lists a message for the widget only. Each channel sends what is
at or above its `<CHANNEL>_MIN`. Slack and Discord were left out on
purpose: nothing from your inbox should land where others in a workspace or
server could read it. A channel failing 3 sends in a row is
`channel:<name>` (warning).
