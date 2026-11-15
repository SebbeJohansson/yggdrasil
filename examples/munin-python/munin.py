#!/usr/bin/env python3
"""A stand-in for Munin, outside the ygg binary: Python, standard library only.

It exists to show that a part is its contract (api/munin.yaml), not its Go
code. It checks plain HTTP targets only (no ollama or statuspage kinds), but
it keeps the rules a replacement must keep:

  - /health in the shared shape, with each loop's last run, for Ratatoskr
  - /openapi.yaml, and /v1/status behind a named bearer token
  - two failures in a row to go down, two passes to come back
  - raise and resolve on Gjallar with its own token, and retry what Gjallar
    didn't take

Run it, then check it:

  MUNIN_TOKENS=owner=<token> MUNIN_ADDR=127.0.0.1:8786 python3 munin.py
  YGG_TOKEN=<token> ygg check munin http://127.0.0.1:8786

Settings: MUNIN_ADDR, MUNIN_TOKENS (name=token,...), MUNIN_TARGETS
(name=http:url,...), YGG_GJALLAR_URL, GJALLAR_TOKEN.
"""

import hmac
import json
import os
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

EVERY = 60
SPEC = Path(__file__).resolve().parents[2] / "api" / "munin.yaml"


def now():
    return datetime.now(timezone.utc).isoformat()


def pairs(text):
    for entry in text.split(","):
        name, _, value = entry.strip().partition("=")
        if name and value:
            yield name.strip(), value.strip()


TOKENS = {token: name for name, token in pairs(os.environ.get("MUNIN_TOKENS", ""))}
TARGETS = [
    {"name": name, "kind": "http", "url": value.removeprefix("http:"), "local": "127.0.0.1" in value}
    for name, value in pairs(os.environ.get("MUNIN_TARGETS", "laya=http:http://127.0.0.1:8000/"))
]
GJALLAR = os.environ.get("YGG_GJALLAR_URL", "http://127.0.0.1:8774").rstrip("/")
GJALLAR_TOKEN = os.environ.get("GJALLAR_TOKEN", "")
STARTED = now()

lock = threading.Lock()
last_run = None
seen = {}    # target name -> what /v1/status shows
checks = {}  # check key -> state


def gjallar(path, body):
    if not GJALLAR_TOKEN:
        return False
    request = urllib.request.Request(GJALLAR + path, json.dumps(body).encode(), method="POST",
                                     headers={"Content-Type": "application/json", "Authorization": "Bearer " + GJALLAR_TOKEN})
    try:
        with urllib.request.urlopen(request, timeout=5):
            return True
    except (urllib.error.URLError, OSError):
        return False


def observe(key, ok, title, detail):
    state = checks.setdefault(key, {"key": key, "down": False, "fails": 0, "passes": 0,
                                    "since": now(), "checkedAt": now(), "raised": False})
    state["checkedAt"] = now()
    if ok:
        state["fails"], state["passes"] = 0, state["passes"] + 1
        if state["down"] and state["passes"] >= 2:
            state["down"], state["since"] = False, now()
    else:
        state["passes"], state["fails"] = 0, state["fails"] + 1
        state.update(severity="warning", title=title, detail=detail)
        if not state["down"] and state["fails"] >= 2:
            state["down"], state["since"] = True, now()
    if state["down"] and not state["raised"]:
        state["raised"] = gjallar("/v1/alerts", {"key": key, "severity": "warning", "title": title, "detail": detail})
    elif not state["down"] and state["raised"]:
        state["raised"] = not gjallar("/v1/alerts/resolve", {"key": key})


def check_once():
    global last_run
    for target in TARGETS:
        try:
            with urllib.request.urlopen(target["url"], timeout=5) as response:
                ok, detail = True, f"answers {response.status}"
        except urllib.error.HTTPError as error:
            ok, detail = True, f"answers {error.code}"  # any answer is a server that is there
        except (urllib.error.URLError, OSError) as error:
            ok, detail = False, str(error)
        with lock:
            seen[target["name"]] = {**target, "up": ok, "detail": detail, "checkedAt": now()}
            observe("down:" + target["name"], ok, target["name"] + " doesn't answer", "" if ok else detail)
    with lock:
        last_run = now()


def loop():
    while True:
        check_once()
        time.sleep(EVERY)


class Handler(BaseHTTPRequestHandler):
    def send(self, status, body, kind="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", kind)
        self.end_headers()
        self.wfile.write(data)

    def caller(self):
        given = self.headers.get("Authorization", "").removeprefix("Bearer ")
        return next((name for token, name in TOKENS.items() if hmac.compare_digest(given, token)), None)

    def do_GET(self):
        if self.path == "/health":
            with lock:
                loop_state = {"everySeconds": EVERY, **({"lastRunAt": last_run} if last_run else {})}
            return self.send(200, {"ok": True, "part": "munin", "version": "python-example", "startedAt": STARTED, "loops": {"local": loop_state}})
        if self.path == "/openapi.yaml":
            return self.send(200, SPEC.read_bytes(), "application/yaml")
        if self.caller() is None:
            return self.send(401, {"error": "missing or wrong bearer token"})
        if self.path == "/v1/status":
            with lock:
                targets = [seen.get(t["name"], {**t, "up": False}) for t in TARGETS]
                return self.send(200, {"targets": targets, "checks": sorted(checks.values(), key=lambda c: (not c["down"], c["key"]))})
        self.send(404, {"error": "not found"})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    host, _, port = os.environ.get("MUNIN_ADDR", "127.0.0.1:8776").rpartition(":")
    threading.Thread(target=loop, daemon=True).start()
    print(f"munin (python) on {host}:{port}")
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
