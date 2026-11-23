# Architecture

Yggdrasil is my-agent split into parts, each with one job and its own API.
The full plan, and why, is in my-agent's `docs/roadmap.md`, sections 3–6.
This page records how the split is built.

## One binary, one process per part

**Decided:** one Go module and one binary, `ygg`, with each part run as its
own process: `ygg gjallar`, `ygg ratatoskr`, …

| | One process for all parts | One process per part (chosen) |
| --- | --- | --- |
| A crash or a hang | takes every part down | takes one part down; Ratatoskr notices and says so |
| The watchdog | can't watch the process it lives in | watches the others from outside |
| Choosing parts per machine | needs flags inside one process | start only the units and containers you want (work: no Glim tokens, no home-only parts) |
| Running on the host vs in Docker | all in one place | Veðrfölnir needs the host's /proc, systemd and docker; Gjallar and Munin are fine in containers |
| Swapping a part out | the boundary is a function call, easy to blur | the boundary is HTTP, so a part can be replaced, or moved to Glim, without the others noticing |
| Cost | one port, one token | a port, a token list and a unit/container per part |

Ratatoskr is the deciding reason: a watchdog in the same process as what it
watches can't report that process dead. The cost is small with Go: one
static binary of about 16 MB serves every part, so a host unit and a
container run the same file.

Shared code lives in `internal/` and is compiled into every part, but parts
never call each other's Go code. They talk over HTTP only:

- `internal/part`: the server every part runs: loopback HTTP, named bearer
  tokens, `/health` with each loop's last run, graceful shutdown.
- `internal/alert`: the Gjallar client.
- `internal/watch`: two failures to go down, two passes to come back,
  re-sending what Gjallar didn't take, and, after a restart, taking back the
  alerts Gjallar still holds (`GET /v1/alerts/mine`) so they get resolved.
  Used by Ratatoskr, Munin and Veðrfölnir.
- `internal/ntfy`: phone pushes, for Hermóðr and for the fallbacks below.
- `internal/tell`: telling you something through Hermóðr, chained with a
  direct ntfy push where Hermóðr itself may be down (Gjallar, Ratatoskr).
- `internal/env`: settings.
- `internal/where`: where each part is found (`YGG_<PART>_URL`).
- `internal/contract`: reads a part's spec and checks a running service
  against it (`ygg check`).
- `internal/call`: one part calling another's JSON routes with its token,
  and asking Norn (Huginn, Dellingr).
- `internal/sse`: following another part's event stream, reconnecting with
  `Last-Event-ID` (Huginn).

## Contracts

Each part's API is an OpenAPI 3.1 spec in `api/`, embedded in the binary and
served by each part at `GET /openapi.yaml`. The spec is the contract: a part
reaches another only at `YGG_<PART>_URL` and only through its spec, so any
program that meets it can replace a built-in part
([replacing-a-part.md](replacing-a-part.md)). Each built-in part's tests
hold it to its spec: the same routes, and every answer in the spec's shape.

## The parts

| Part | Port | Runs as | State | Status |
| --- | --- | --- | --- | --- |
| Norn: assistant (inbox, calendars, memory, ask) | 8771 | container | `norn.db`, the sign-ins | memory, sign-ins, calendars and inbox moved; ask, dispatch and writing next |
| Sindri: coding (agents, worktrees, goals) | 8772 | host unit (it starts agents on the host) | the agents (JSON) | the runner's agent half moved; goals, approvals and Linear next |
| Veðrfölnir: reports on the machine | 8773 | host unit | the hour of samples | built, ported from the runner |
| Gjallar: alerts | 8774 | container | `gjallar.db` | built |
| Ratatoskr: watchdog | 8775 | host unit, outside compose | none | built |
| Munin: external checks | 8776 | container | none | built |
| Huginn: reacts to events | 8777 | container | `huginn.db`: rules, reactions, gossip | built |
| Dellingr: scheduled jobs | 8778 | container | `dellingr.db`: jobs, runs | built |
| Hermóðr: messages to you | 8779 | container | `hermodr.db`: the last week of messages | built |
| Loki: tests the watchers | 8780 | container | `loki.json`: settings, strikes; `loki-strikes.jsonl`: every ended strike | built |
| Heimdall: the security watchman | 8781 | host unit (the host's sockets, your repos, the real file modes) | `heimdall.json`: settings, findings | built |
| Sága: syncs with your other devices through Mímir | 8782 | host unit (reaches Mímir at its public HTTPS URL) | `saga.db`: outbox, read mark, mirrors | alerts built |

## Tokens

Every route but `/health` needs `Authorization: Bearer <token>`. Each part
reads `name=token` pairs (`GJALLAR_TOKENS=owner=…,munin=…`), and a handler
knows the caller's name:

- `owner` is you: the widget, the dashboard, curl. One owner token for all parts.
- Every other name is a part. On Gjallar, that name *is* the alert's source,
  so a part can't raise or resolve another part's alerts.
- `glim` (later, home only) may send `info` and `warning`, never `critical`.

## Who reports which problem

| Problem | Reported by |
| --- | --- |
| a Yggdrasil part is down, or one of its loops stopped or failed | Ratatoskr |
| something outside Yggdrasil doesn't answer (Ollama, Laya, Slack…) | Munin |
| a disk is filling, a unit failed, a container crashed (not Yggdrasil's own) | Veðrfölnir |
| a vulnerable package, a port open beyond loopback, a key in a commit, a token file others can read | Heimdall |
| a sign-in expired | the part holding it (Norn, Sindri) |
| the local model fails most calls for an hour | Norn |
| Ratatoskr went silent | Gjallar |
| Gjallar is down | Ratatoskr, around Gjallar: through Hermóðr or straight to ntfy, and `/v1/status` for the widget |

## Moving from my-agent

my-agent keeps running while parts move over. Each part keeps my-agent's
JSON shapes where it can, so my-widget can switch one page at a time from
my-agent's routes to the part's.

How Norn and Sindri split my-agent's message handling: Norn holds the
conversation, decides what each message is, and builds the hand-over
(your words, profile, memories, inbox). Sindri owns the agents, the coding
prompts and tool rules, tasks, goals and approvals, and posts what agents
say back into the conversation (`POST /v1/conversations/{session}/agent-turns`).

Moved:
- **Veðrfölnir**: the runner's `/system` and `/services` (without `stop`:
  Veðrfölnir fixes nothing).
- **Norn**: memory and learning, profile, jokes, widget settings, search,
  the debug view, the Laya pass-through, the Microsoft, Google and Slack
  sign-ins, both calendars, the inbox (Outlook, Teams, Slack, Gmail, the org
  chart, the judge, the summaries), `/v1/ask` with the dispatcher and its
  guard rails, chat, Needs you and decisions with the shadow Laya
  dispatcher, Pennvass's writing checks (`/v1/write/*`), read-only lookups
  through MCP servers (Linear) before an agent, ticket answers from Linear,
  the activity heatmap, `/part`, and naming tasks with the dispatch model.
- **Sindri**: the runner's agents (Claude Code and Copilot CLI, worktrees,
  review, checks, screenshots, merge, pull requests, uploads, projects),
  tasks, goals with pictures, parts across repos, automatic approvals,
  follow-through on failed tests, waiting for room or for Claude usage, and
  the coding-agent and automation settings, Linear goal candidates and
  moving an issue as its goal moves, `/v1/activity`, and every coding-agent
  session on the machine (`/v1/sessions`: Claude Code's live sessions,
  OpenCode's, herdr's panes, with prompts, keys and dialog answers). Question
  agents run in herdr panes while herdr runs, as my-agent's did. The
  agent-conduit npm package these used is ported into Sindri
  (`sindri/conduit_*.go`).

Everything in my-agent now has a place in Yggdrasil.

Moving data between the two: `ygg migrate from-my-agent` copies my-agent's
database, sign-ins and agents into Norn and Sindri; `ygg migrate
to-my-agent` copies them back. Both back up the target first and refuse to
write over data unless given `--replace`. my-agent's herdr question agents
(`data/herdr-agents.json`) become Sindri agents with the herdr engine, and
go back the same way.

## Testing the watchers

Loki checks that the watching works. Every part's server (`internal/part`)
has a trick switch, `/v1/loki/trick`, only with `LOKI=on` in its
environment: for at most 10 minutes the part answers `/health` with 503,
fails one loop, or answers slowly. A trick lives in the part's memory with
its own end time, so it stops even if Loki dies, and it touches no data,
file, process or container. Loki can also strike from outside, with no help
from the part: as a proxy (dev only) standing at the address the others call,
`YGG_<PART>_URL`, it passes every call through untouched until a trick makes
it fail `/health`, answer errors, slow down, drop or hang; and it can freeze
a part's process with SIGSTOP, thawed at the end by a detached process that
outlives Loki. While Loki is down, a proxied part is cut off. Loki strikes
one part, reads Ratatoskr's status and Gjallar's alerts to see who noticed,
and raises `missed:<part>:<kind>` when a watcher didn't, for self-repair. It
never tricks itself, Ratatoskr only when you allow it, and Gjallar only
alone.

## Doing things without being asked

Two parts act without a message from you, and both are built so that Glim
can replace them later through their contracts.

- **Huginn** reacts to events. Norn sends what changed on
  `GET /v1/events/stream`: summaries and links only, since raw inbox text
  never leaves Norn. Gjallar's alert stream is the other source. Huginn
  suggests and tells; it never sends, starts or changes anything. It
  also learns what you do after an event and suggests a rule for it, made
  only when you accept it. Its
  questions to Norn go with `to: norn` and `/local`, so Norn answers itself
  and never hands them to an agent. For fun it also keeps Ratatoskr's
  gossip, the parts talking in character about system events only, written
  by the local model on this machine and sent nowhere.
- **Dellingr** runs jobs on a clock. my-agent's roadmap (section 2) planned
  one scheduler per part; one part replaced that, since a job's work lives in
  Norn or Sindri anyway, and Ratatoskr reports Dellingr down like any other
  part. Each part's own fixed loops stay in that part.

A rule or job made by anyone but you starts disabled, and only you can
enable it. Only you can let a goal job start agents (`autoStart`).

## One way to tell you

Hermóðr is the only part that sends to you: one history the widget lists,
and ntfy when it is set up. Slack and Discord were left out so nothing
from your inbox lands where others could read it. Gjallar, Huginn and Dellingr send their
messages there, with a priority, and Hermóðr decides when and where:
urgent at once, the rest after quiet hours and, if asked, after your
meeting. Holding through meetings moved from Huginn to here, so every
sender gets it.

Two senders keep a way around it. Gjallar pushes a critical alert straight
to ntfy when Hermóðr can't take it, and Ratatoskr does the same for
"Gjallar is down": the alert path must not depend on one more part.
