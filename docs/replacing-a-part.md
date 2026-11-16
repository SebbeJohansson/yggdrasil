# Replacing a part

A part is its contract, not its Go code. Each part's contract is an OpenAPI
3.1 spec in [`api/`](../api), and every built-in part serves its own at
`GET /openapi.yaml`. Any program, in any language, can take a part's place
if it meets that contract. The other parts never know the difference: they
only ever reach a part at its URL.

[`examples/munin-python/munin.py`](../examples/munin-python/munin.py) is a
Munin in about 150 lines of standard-library Python, and it passes the same
check as the Go one.

## The steps

1. **Read the contract.** `api/<part>.yaml`. Its description lists the rules
   the routes alone can't show: who may do what, which alerts it raises, and
   what it must never do.
2. **Build your part.** It must:
   - answer `GET /health` without a token, in the shared shape (`ok`, `part`,
     `version`, `startedAt`, `loops`), naming the part, with every loop it
     runs on a timer and when that loop last ran. Ratatoskr reads this to
     tell a part that answers from one that still does its job.
   - answer `GET /openapi.yaml` without a token. Serve the file from `api/`.
   - refuse every other route without a valid bearer token (`401`).
   - take tokens as `name=token` pairs, at least 32 characters each. `owner`
     is you. Any other name is a part, and on Gjallar that name is the source
     of what it raises.
   - send its alerts to Gjallar at `YGG_GJALLAR_URL` with its own
     `GJALLAR_TOKEN`, if the part raises alerts.
3. **Check it.** Run it on a spare port, then:

   ```sh
   ygg check munin http://127.0.0.1:8786
   ```

   The owner token comes from `YGG_TOKEN`, or `~/.config/yggdrasil/owner.token`.
   For Gjallar, also set `YGG_SENDER_TOKEN` to a part's token: the check then
   raises, updates, reads and resolves a test alert. Every route is checked
   against the spec's shapes. Fix every `FAIL`.
4. **Swap it in.**
   - Stop the built-in part: `systemctl --user disable --now ygg-<part>` for
     a host unit, or leave it out of `COMPOSE_PROFILES` in `.env` for a container.
   - Point the others at yours in `~/.config/yggdrasil/parts.env`:
     `YGG_MUNIN_URL=http://127.0.0.1:8786`, or run yours on the part's
     default port and change nothing.
   - Restart the parts that read it: the host units
     (`systemctl --user restart ygg-ratatoskr ygg-vedrfolnir`) and
     `docker compose up -d`.
5. **Watch it.** Ratatoskr now checks your part's `/health` like any other.
   `ygg check <part>` with no URL checks whatever `YGG_<PART>_URL` points at.

## Where each part is found

| Part | Setting | Default |
| --- | --- | --- |
| Norn | `YGG_NORN_URL` | `http://127.0.0.1:8771` |
| Sindri | `YGG_SINDRI_URL` | `http://127.0.0.1:8772` |
| Veðrfölnir | `YGG_VEDRFOLNIR_URL` | `http://127.0.0.1:8773` |
| Gjallar | `YGG_GJALLAR_URL` | `http://127.0.0.1:8774` |
| Ratatoskr | `YGG_RATATOSKR_URL` | `http://127.0.0.1:8775` |
| Munin | `YGG_MUNIN_URL` | `http://127.0.0.1:8776` |
| Huginn | `YGG_HUGINN_URL` | `http://127.0.0.1:8777` |

## Changing a contract

A contract changes in the same commit as the code. Each built-in part's test
fails when it serves a route its spec lacks, or the other way round, and
when an answer doesn't match the spec's shape (`internal/contract`). Keep
changes additive where you can (new optional fields, new routes), so a
replacement that met the old contract still does.
