# Three ideas for Yggdrasil

Each fits the rules in [architecture.md](architecture.md): one job, its own
API and spec in `api/`, talks to the others over HTTP only, and tells you
through Gjallar and Hermóðr. None is built yet.

## 1. Backups of every part's state

**Gap:** the parts keep state in `norn.db`, `gjallar.db`, `huginn.db`,
`dellingr.db`, `hermodr.db`, `saga.db`, `heimdall.json`, `loki.json` and
Sindri's agents. Only `ygg migrate` copies any of it, and only between
my-agent and Yggdrasil. A bad disk or a bad migration loses memory,
promises and sign-ins.

**Shape:** a small host part, **Hirðir** (port 8783). A loop snapshots each
file (SQLite through `VACUUM INTO`, JSON by copy) to a folder, keeps the last
7 daily and 4 weekly copies, and optionally copies them to the VPS over
Tailscale, the way Sága reaches Mímir. `GET /v1/backups` lists what exists;
`POST /v1/restore` is owner only and refuses to write over data unless given
`replace`, like `ygg migrate`. A backup older than 36 hours raises
`backup-stale` in Gjallar (warning), and Heimdall already checks the file
modes of the folder.

**Size:** about 30 minutes of agent time.

## 2. Release and deploy watch

**Gap:** Munin checks the systems Yggdrasil depends on, Heimdall your repos'
security. Nothing tells you that a deploy or the release build of one of
your repos failed, or that production is unhealthy. These are the alerts
that matter most and today arrive only by email.

**Shape:** a Munin check kind (or a part of its own, **Vörðr**) that reads,
for the repos in `RELEASE_WATCH`, the latest default-branch workflow run and
latest deployment from GitHub (token in `munin.env`), plus optional health
URLs for production. A failed run on the default branch raises
`release:<repo>` as `warning`; a failing production URL raises
`prod:<url>` as `critical`, so it reaches the phone through Hermóðr. Both
resolve when the next run or probe passes. Huginn can react: a failed
release becomes a Needs-you item with the run's link, and the dashboard's
agent view can offer to start a Sindri agent on the failing log.

**Size:** about 20 minutes of agent time for the check, 20 more for the
Huginn rule and dashboard card.

## 3. A Claude usage ledger

**Gap:** Sindri starts many Claude Code agents, and cloud mode sends every
model call to Claude through `POST /v1/complete`. Sindri waits when usage
runs out, but you can't see what used it, so you only learn a limit is near
when work stops.

**Shape:** Sindri records, per agent turn and per `/v1/complete` call, the
model, tokens in and out and the goal or Norn feature that asked, in a
small SQLite file. `GET /v1/usage?since=` answers totals by day, goal and
caller. When the share used of the plan's window passes 80%, Sindri raises
`claude-usage` (warning) in Gjallar, and Dellingr can run a job that pauses
low-priority agents until it clears. `ygg usage` prints the same in plain
lines next to `ygg agents`.

**Size:** about 25 minutes of agent time.

## Order

Do 2 first: it catches the incidents you care about most. Then 1, because it
protects everything else. Then 3.
