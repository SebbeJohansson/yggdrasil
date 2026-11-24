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

**Done:** the ledger (in `sindri.db`), `GET /v1/usage?since=`, the `claude-usage` alert
(needs `SINDRI_USAGE_BUDGET`, tokens per `SINDRI_USAGE_WINDOW`, default 5h) and
`ygg usage`. **Left:** Dellingr's job that pauses low-priority agents, and Norn
sending a `feature` with each `/v1/complete` call (the ledger has the caller).

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

# Security hardening

From the security review on 2026-11-24, before the repo went public. The
three High findings are fixed: prompts go to the claude CLI after `--`, only
you can give a Sindri agent tools or answer its approvals, and another part's
`/v1/ask` never starts or steers an agent. These Medium findings are left.

## 4. Coding agents in a sandbox

**Gap:** coding agents run in `auto` permission mode (`sindri/work.go`), may
read GitHub, Linear and Outlook/Teams through MCP (`sindri/prompts.go`), and
are allowed to run `npm test`, `go test` and `npx vitest` without asking.
Sindri also runs `npm run <script>` itself (`sindri/files.go`), worktrees run
`postinstall` (`sindri/git.go`), and self-repair works unattended overnight.
A GitHub issue, comment, ticket or mail the agent reads can tell it to change
`package.json`'s `test` or a `_test.go` file; the next allowed test run then
runs that on the host as you, with your SSH keys and
`~/.config/yggdrasil/*.env`.

**Shape:** run each coding agent and its test runs in a container (or Claude
Code's sandbox) that sees only its worktree, with no home directory and the
network limited to what the repo needs. Coding agents lose the Outlook/Teams
tools; question agents keep them. Sindri's own `npm run` and `postinstall`
run in the same sandbox.

**Size:** about 60 minutes of agent time, most of it the container and its
caches.

## 5. A token per device

**Gap:** `scripts/install.sh` writes one `owner.token` into every part's
`*_TOKENS`, the VPS's owner token is copied into other machines'
`nodes.json` (`docs/vps.md`), and the phone uses the owner token for Sindri.
A lost phone or a leaked `nodes.json` controls every part on every machine:
start agents, answer their approvals, read transcripts.

**Shape:** each device gets its own token with a name (`owner:phone`,
`owner:laptop`), and parts check the scope that name has. The phone's token
reads and acknowledges but can't use `POST /v1/agents` or answer
permissions. `ygg token rotate <device>` makes a new one and drops the old,
and Heimdall raises a token older than 90 days.

**Size:** about 40 minutes of agent time.

## 6. Signed requests through Mímir

**Gap:** with `SAGA_DRIVE_FROM` set, Sága lets requests that come through
Mímir type into local agent sessions and start cloud sessions
(`saga/sessions.go`), and it trusts the device name Mímir gives. A
compromised Mímir, or a stolen device token, can forge a request "from the
phone". Off by default.

**Shape:** each device keeps an ed25519 key and signs its requests (kind,
target, text, time); Sága keeps the public keys of the devices allowed to
drive it and refuses anything unsigned, old or replayed. Mímir only carries
the requests. The phone keeps its key in the Android Keystore.

**Size:** about 45 minutes of agent time, both repos.

## Smaller ones

- A "yes" answers the first pending request of the most recent agent
  (`norn/ask.go`): bind each approval to its `requestId`.
- Parts don't check the `Host` header, so a page can read `/health` and
  `/openapi.yaml` through DNS rebinding (`internal/part/part.go`): refuse
  any `Host` but `127.0.0.1:<port>` and `localhost:<port>`.
- Question agents may `cat`, `head`, `tail`, `grep` and `rg` any file
  (`sindri/prompts.go`): limit them to `Read(<roots>/**)`.
