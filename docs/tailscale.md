# Yggdrasil on a tailnet

Your machines reach each other's Yggdrasil over Tailscale: the VPS, the home
PC, the phone and the work laptop on one tailnet. The SSH tunnel
(`scripts/tunnel.sh`, [vps.md](vps.md)) still works as a second way in.

One rule holds everywhere: **the work laptop may read the personal machines;
nothing may reach the work laptop, and nothing of its work leaves it.** Three
layers keep it, so one slip does not undo it:

| Layer | What it does |
| --- | --- |
| Tailnet policy | No rule lets anything connect to `tag:work`. Policy tests fail the save if one ever does. |
| Shields up on the work laptop | Tailscale on the laptop drops every incoming connection, whatever the policy says. |
| `YGG_WORK=on` on the work laptop | Its parts say `"work": true` on `/health` (other widgets refuse it as a node), refuse to connect to any tailnet address (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`), never run Sága, and are never published on the tailnet. Its widget only reads from personal machines: no goals, prompts or answers sent to them. |

| Device | Tag | Yggdrasil | Published on the tailnet |
| --- | --- | --- | --- |
| VPS | `tag:personal` | release, dev profile | yes |
| Home PC | `tag:personal` | checkout, full profile | yes |
| Phone | none (your own device) | none | no |
| Work laptop | `tag:work` | release, full profile, `--work` | never |

## 1. The tailnet

1. Sign up at tailscale.com with a **personal** account (GitHub or Gmail), not
   the work address: the free Personal plan covers this.
2. Admin console → **DNS**: turn on **MagicDNS** and **HTTPS certificates**.
3. Admin console → **Access controls**: replace the policy with this one.

```json
{
  "tagOwners": {
    "tag:personal": ["autogroup:admin"],
    "tag:work":     ["autogroup:admin"]
  },

  // Everything may reach the personal machines. Nothing reaches tag:work:
  // no rule names it as a destination.
  "acls": [
    { "action": "accept",
      "src": ["autogroup:member", "tag:personal", "tag:work"],
      "dst": ["tag:personal:*"] }
  ],

  // Checked by Tailscale on every save: a policy that lets anything reach
  // the work laptop is refused.
  "tests": [
    { "src": "tag:personal", "accept": ["tag:personal:8772"], "deny": ["tag:work:22", "tag:work:8771", "tag:work:8772"] },
    { "src": "tag:work",     "accept": ["tag:personal:8772"], "deny": ["tag:work:8772"] }
  ]
}
```

Your phone is your own device (`autogroup:member`): it reaches the personal
machines and, like everything else, not the work laptop.

## 2. The devices

**VPS** (as root; the parts themselves run as `ygg`):

```sh
curl -fsSL https://tailscale.com/install.sh | sh
tailscale up --advertise-tags=tag:personal
tailscale set --operator=ygg              # lets ygg publish its parts
```

then as `ygg`, publish the parts (or install with `get.sh … --tailscale`):

```sh
~/.local/share/yggdrasil/scripts/tailscale-serve.sh
```

**Home PC**: install Tailscale for Windows, sign in, and tag it
`tag:personal` (admin console → the machine → Edit ACL tags). WSL reaches the
tailnet through Windows with mirrored networking. Publish the parts from the
checkout in WSL with `scripts/tailscale-serve.sh`: it runs Windows'
`tailscale.exe`, which reaches the parts on the shared 127.0.0.1.

**Phone**: install the Tailscale app and sign in. Nothing else.

**Work laptop**: install Tailscale for Windows, sign in, then:

1. Tag it `tag:work` (admin console → the machine → Edit ACL tags).
2. Shields up: Tailscale menu → Preferences → turn **off** *Allow incoming
   connections* (CLI: `tailscale set --shields-up`).
3. Install Yggdrasil as a work machine, never with `--tailscale`:

```sh
curl -fsSL https://raw.githubusercontent.com/SebbeJohansson/yggdrasil/main/scripts/get.sh | sh -s -- --profile full --work
```

## 3. Telling the widget

On each machine whose widget should see another one, add it to
`~/.config/yggdrasil/nodes.json` with its tailnet name and its owner token
(`~/.config/yggdrasil/owner.token` there):

```json
{ "label": "Home PC", "nodes": [
  { "id": "vps", "label": "VPS", "token": "<the VPS's token>",
    "tailnet": "vps.<your-tailnet>.ts.net", "ssh": "ygg@vps.<your-tailnet>.ts.net" }
] }
```

`"tailnet"` reaches every part at `https://<name>:<port>`; `"tunnel": 10000`
instead goes through the SSH tunnel. Never add the work laptop to a personal
machine's `nodes.json`: its widget would refuse it anyway, since its
`/health` says `work`, and the tailnet would not let it through.

## 4. Checking it

Run these after setting up, and again after any change to the policy.

| Where | Run | Expect |
| --- | --- | --- |
| Admin console | Save the policy | The tests pass |
| VPS or home PC | `curl -m 5 http://<work-laptop>:8771/health` | Times out |
| VPS or home PC | `nc -vz -w 5 <work-laptop> 22` | Fails |
| Phone (browser) | `http://<work-laptop>:8771/health` | Does not load |
| Work laptop | `curl https://vps.<tailnet>.ts.net:8772/health` | Answers |
| Work laptop | `curl -s http://127.0.0.1:8772/health` | Has `"work":true` |
| Work laptop | `systemctl --user status ygg-saga` | Inactive |
| Work laptop | `ygg status` | Every part up |
| Home PC widget | Health → Machines | VPS listed; the work laptop only if you added it, then as "left out: it is a work machine" |
| Work laptop widget | Create a goal for the VPS | Refused: read only from a work machine |

`<work-laptop>` is its tailnet name or 100.x address (`tailscale status`).
