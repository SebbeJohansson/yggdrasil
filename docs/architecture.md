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
- `internal/watch`: two failures to go down, two passes to come back, and
  re-sending what Gjallar didn't take. Used by Ratatoskr, Munin and Veðrfölnir.
- `internal/ntfy`: phone pushes.
- `internal/env`: settings.

## The parts

| Part | Port | Runs as | State | Status |
| --- | --- | --- | --- | --- |
| Norn: assistant (inbox, calendar, memory, ask) | 8771 | container | `norn.db` | not built yet; still in my-agent |
| Sindri: coding (goals, agents, worktrees) | 8772 | host unit (it starts agents on the host) | `sindri.db` | not built yet; still in my-agent and its runner |
| Veðrfölnir: reports on the machine | 8773 | host unit | the hour of samples | built, ported from the runner |
| Gjallar: alerts | 8774 | container | `gjallar.db` | built |
| Ratatoskr: watchdog | 8775 | host unit, outside compose | none | built |
| Munin: external checks | 8776 | container | none | built |

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
| a Yggdrasil part is down, or one of its loops stopped | Ratatoskr |
| something outside Yggdrasil doesn't answer (Ollama, Laya, Slack…) | Munin |
| a disk is filling, a unit failed, a container crashed (not Yggdrasil's own) | Veðrfölnir |
| a sign-in expired | the part holding it (Norn, Sindri) |
| Ratatoskr went silent | Gjallar |
| Gjallar is down | Ratatoskr, around Gjallar: phone push, and `/v1/status` for the widget |

## Moving from my-agent

my-agent keeps running while parts move over. Each part copies its piece of
my-agent and keeps the same JSON shapes, so my-widget can switch one page at a
time from my-agent's routes to the part's.

- Done: Veðrfölnir has the runner's `/system` and `/services` routes, under
  `/v1/`. The runner's `POST /services/:id/stop` was left out, because
  Veðrfölnir fixes nothing.
- Next: Norn (the largest), then Sindri together with the runner's agent half.
