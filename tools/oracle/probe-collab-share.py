#!/usr/bin/env python
"""Probe the collab WebSocket LOGIN + LIMIT paths against the ORACLE.

probe0012 pinned the owner handshake and the bad-frame close codes. This probe
pins the paths the M11 scenarios still need:

  L1  edit link (no password)      -> auth frame -> ready role=edit canWrite
  L2  view link                    -> auth frame -> ready role=view canWrite=false
  L3  password link, no password   -> error password_required (stays open)
  L3b password link, wrong pw      -> error password_required again
  L3c password link, right pw      -> ready role=edit
  L4  forged token                 -> close 4403 forbidden
  L5  non-owner session (pending)  -> close 4408 auth_timeout (REALTIME_AUTH_TIMEOUT_SECONDS=2)
  L6  trashed design (owner)       -> close 4404 design_trashed
  L7  foreign Origin               -> handshake refused HTTP 403
  L8  oversize frame               -> close 4413 frame_too_large (REALTIME_MAX_FRAME_BYTES=64)
  L9  message rate                 -> close 4429 rate_limited (REALTIME_RATE_MESSAGES_PER_SECOND=1, BURST=1)
  L10 client sync/step1            -> sync/step2 (full-state update, hex payload)

Raw transcripts: test/parity/probes/0014-collab-share/{meta.txt,frames.json}
"""
import asyncio
import json
import os
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request

import websockets  # noqa: E402  (oracle venv)

REPO = os.environ["REPO_DIR"]
OUT = os.environ["OUT_DIR"]
BACKEND = os.path.join(REPO, ".upstream/current/backend")
SHA = subprocess.check_output(["git", "-C", os.path.join(REPO, ".upstream/current"), "rev-parse", "HEAD"]).decode().strip()

ENV_BASE = {
    "PYTHONDONTWRITEBYTECODE": "1",
    "DJANGO_SETTINGS_MODULE": "doory.settings.prod",
    "DJANGO_SECRET_KEY": "parity-oracle-secret-key-0123456789",
    "SECURE_SSL_REDIRECT": "false",
    "SECURE_COOKIES": "false",
    "REALTIME_ENABLED": "true",
    "DATABASE_URL": f"sqlite:///{REPO}/.oracle/oracle-share.sqlite3",
    "MEDIA_ROOT": os.path.join(REPO, ".oracle/media"),
    "EXPORT_STORAGE_ROOT": os.path.join(REPO, ".oracle/exports"),
    "SITE_URL": "http://localhost:5173",
}

frames_log = []


def log(phase, direction, kind, payload=b"", note=""):
    entry = {
        "phase": phase,
        "dir": direction,
        "kind": kind,
        "len": len(payload),
        "hex": payload[:512].hex(),
        "note": note,
    }
    if note == "json" and payload:
        try:
            entry["json"] = json.loads(payload)
        except Exception:
            pass
    frames_log.append(entry)


def boot(port, extra_env=None):
    env = dict(os.environ)
    env.update(ENV_BASE)
    if extra_env:
        env.update(extra_env)
    # A fresh sqlite DB needs the migrations (probe0012 reused an existing one).
    db_path = env["DATABASE_URL"].replace("sqlite:///", "")
    if not os.path.exists(db_path):
        subprocess.run([sys.executable, "manage.py", "migrate", "--noinput"],
                       cwd=BACKEND, env=env, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    proc = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "doory.asgi:application", "--host", "127.0.0.1",
         "--port", str(port), "--log-level", "warning"],
        cwd=BACKEND, env=env,
        stdout=open(os.path.join(OUT, f"uvicorn-{port}.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    for _ in range(120):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/api/v1/health/", timeout=1)
            return proc
        except Exception:
            time.sleep(0.3)
    raise RuntimeError(f"oracle did not start on {port}")


def stop(proc):
    proc.send_signal(signal.SIGTERM)
    try:
        proc.wait(timeout=10)
    except Exception:
        proc.kill()


def http(port, method, path, body=None, jar=None):
    jar = {} if jar is None else jar
    url = f"http://127.0.0.1:{port}{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if jar.get("csrftoken"):
        req.add_header("X-CSRFToken", jar["csrftoken"])
        req.add_header("Referer", f"http://127.0.0.1:{port}/")
    if jar:
        req.add_header("Cookie", "; ".join(f"{k}={v}" for k, v in jar.items()))
    try:
        resp = urllib.request.urlopen(req)
        status, payload, hdrs = resp.status, resp.read(), resp.headers
    except urllib.error.HTTPError as exc:
        status, payload, hdrs = exc.code, exc.read(), exc.headers
    for value in (hdrs.get_all("Set-Cookie") or []):
        kv = value.split(";")[0]
        if "=" in kv:
            k, v = kv.split("=", 1)
            jar[k.strip()] = v.strip()
    return status, payload, jar


_seq = [0]


def account(port):
    """A fresh registered+logged-in account (session cookie in the jar)."""
    _seq[0] += 1
    email = f"shareprobe{os.getpid()}x{_seq[0]}@example.com"
    jar = {}
    http(port, "GET", "/api/v1/auth/csrf/", jar=jar)
    st, body, jar = http(port, "POST", "/api/v1/auth/register/",
                         {"email": email, "password": "Tr0ub4dor&3xkcd9"}, jar=jar)
    assert st == 201, (st, body)
    st, body, jar = http(port, "POST", "/api/v1/auth/login/",
                         {"email": email, "password": "Tr0ub4dor&3xkcd9"}, jar=jar)
    assert st == 200, (st, body)
    return jar


def design(port, jar, title="Share WS"):
    st, body, jar = http(port, "POST", "/api/v1/designs/", {"title": title}, jar=jar)
    assert st == 201, (st, body)
    return json.loads(body)["id"]


def link(port, jar, design_id, body):
    st, payload, jar = http(port, "POST", f"/api/v1/designs/{design_id}/share-links/", body, jar=jar)
    assert st == 201, (st, payload)
    return json.loads(payload)["token"]


def cookie_header(jar):
    return "; ".join(f"{k}={v}" for k, v in jar.items())


def frame_type_char(data):
    if not data:
        return "empty"
    msg = data[0] & 0x7F
    names = {0: "sync", 1: "awareness", 2: "auth", 3: "query-awareness"}
    name = names.get(msg, f"msg{msg}")
    if msg == 0 and len(data) > 1:
        sub = data[1] & 0x7F
        name += {0: "/step1", 1: "/step2", 2: "/update"}.get(sub, f"/{sub}")
    return name


async def recv_frames(ws, count=2, timeout=4.0):
    out = []
    try:
        for _ in range(count):
            out.append(await asyncio.wait_for(ws.recv(), timeout))
    except asyncio.TimeoutError:
        pass
    except Exception as exc:  # ConnectionClosed* carries the close code
        out.append(f"<closed:{getattr(ws, 'close_code', '?')}:{str(exc)[:80]}>")
    return out


def record(phase, frames):
    for data in frames:
        text = data if isinstance(data, str) else None
        payload = data.encode() if isinstance(data, str) else data
        note = "json" if text and text.lstrip().startswith(("{", "[")) else ""
        kind = "text" if text is not None else frame_type_char(payload)
        log(phase, "recv", kind, payload, note)


async def with_ws(port, design_id, jar, fn, extra_headers=None, phase=""):
    origin = f"http://127.0.0.1:{port}"
    url = f"ws://127.0.0.1:{port}/ws/designs/{design_id}/"
    headers = {"Origin": origin}
    if jar:
        headers["Cookie"] = cookie_header(jar)
    if extra_headers:
        headers.update(extra_headers)
    try:
        async with websockets.connect(url, additional_headers=headers, max_size=None) as ws:
            await fn(ws)
            code = ws.close_code
            log(phase, "close-code", "close-code", b"", str(code) if code is not None else "")
    except Exception as exc:
        log(phase, "dial", "refused", b"", str(exc)[:160])


def run(coro_fn, port):
    return asyncio.get_event_loop().run_until_complete(coro_fn(port))


# ------------------------------------------------------------------ phases


def phase_login(port, env_extra):
    """L1-L3: link logins (edit / view / password) + the ready shape."""
    proc = boot(port, env_extra)
    try:
        owner = account(port)
        did = design(port, owner)
        tok_e = link(port, owner, did, {"role": "edit"})
        tok_v = link(port, owner, did, {"role": "view"})
        tok_p = link(port, owner, did, {"role": "edit", "password": "s3cret-pass"})
        # A share-link login needs a SESSION: consumers.connect refuses 4401
        # for an unauthenticated socket before auth frames are even read
        # (probe L1 originally dialed anonymous and got exactly that). Use a
        # second logged-in account that does not own the design.
        guest = account(port)

        async def edit_login(ws):
            record("L1", await recv_frames(ws, 1))  # nothing before auth? (controller checks)
            await ws.send(json.dumps({"type": "auth", "token": tok_e}))
            record("L1", await recv_frames(ws, 3))
        run(lambda p: with_ws(p, did, guest, edit_login, phase="L1"), port)

        async def view_login(ws):
            await ws.send(json.dumps({"type": "auth", "token": tok_v}))
            record("L2", await recv_frames(ws, 3))
        run(lambda p: with_ws(p, did, guest, view_login, phase="L2"), port)

        async def password_login(ws):
            await ws.send(json.dumps({"type": "auth", "token": tok_p}))
            record("L3", await recv_frames(ws, 2))
            await ws.send(json.dumps({"type": "auth", "token": tok_p, "password": "wrong"}))
            record("L3b", await recv_frames(ws, 2))
            await ws.send(json.dumps({"type": "auth", "token": tok_p, "password": "s3cret-pass"}))
            record("L3c", await recv_frames(ws, 3))
        run(lambda p: with_ws(p, did, guest, password_login, phase="L3"), port)

        async def forged(ws):
            await ws.send(json.dumps({"type": "auth", "token": "x" * 32}))
            record("L4", await recv_frames(ws, 2))
        run(lambda p: with_ws(p, did, guest, forged, phase="L4"), port)

        async def sync_roundtrip(ws):
            record("L10", await recv_frames(ws, 2))
            await ws.send(bytes([0, 0, 1, 0]))  # sync/step1 with an empty state vector
            record("L10", await recv_frames(ws, 2))
        run(lambda p: with_ws(p, did, owner, sync_roundtrip, phase="L10"), port)
    finally:
        stop(proc)


def phase_pending(port, env_extra):
    """L5: a logged-in NON-owner connection pends then closes 4408."""
    proc = boot(port, env_extra)
    try:
        owner = account(port)
        did = design(port, owner)
        other = account(port)

        async def passerby(ws):
            record("L5", await recv_frames(ws, 2, timeout=6.0))
        run(lambda p: with_ws(p, did, other, passerby, phase="L5"), port)
    finally:
        stop(proc)


def phase_trashed(port, env_extra):
    """L6: the owner of a trashed design gets close 4404."""
    proc = boot(port, env_extra)
    try:
        owner = account(port)
        did = design(port, owner)
        st, body, owner = http(port, "DELETE", f"/api/v1/designs/{did}/", jar=owner)
        assert st == 204, (st, body)

        async def trashed(ws):
            record("L6", await recv_frames(ws, 2))
        run(lambda p: with_ws(p, did, owner, trashed, phase="L6"), port)
    finally:
        stop(proc)


def phase_limits(port, env_extra):
    """L7-L9: foreign origin, oversize frame, message rate."""
    proc = boot(port, env_extra)
    try:
        owner = account(port)
        did = design(port, owner)

        async def nothing(ws):
            record("L7", await recv_frames(ws, 1, timeout=1.5))
        run(lambda p: with_ws(p, did, owner, nothing,
                              extra_headers={"Origin": "http://evil.example"}, phase="L7"), port)

        async def oversize(ws):
            await recv_frames(ws, 2)
            await ws.send(b"x" * 200)
            record("L8", await recv_frames(ws, 2))
        run(lambda p: with_ws(p, did, owner, oversize, phase="L8"), port)

        async def ratelimit(ws):
            await recv_frames(ws, 2)
            for _ in range(6):
                await ws.send('{"type":"ping"}')
                await asyncio.sleep(0.05)
            record("L9", await recv_frames(ws, 3))
        run(lambda p: with_ws(p, did, owner, ratelimit, phase="L9"), port)
    finally:
        stop(proc)


def main():
    os.makedirs(OUT, exist_ok=True)
    try:
        os.remove(ENV_BASE["DATABASE_URL"].replace("sqlite:///", ""))
    except OSError:
        pass
    phase_login(8034, {})
    phase_pending(8035, {"REALTIME_AUTH_TIMEOUT_SECONDS": "2"})
    phase_trashed(8036, {})
    phase_limits(8037, {
        "REALTIME_MAX_FRAME_BYTES": "64",
        "REALTIME_RATE_MESSAGES_PER_SECOND": "1",
        "REALTIME_RATE_BURST": "1",
    })
    with open(os.path.join(OUT, "frames.json"), "w") as fh:
        json.dump(frames_log, fh, ensure_ascii=False, separators=(",", ":"))
    with open(os.path.join(OUT, "meta.txt"), "w") as fh:
        fh.write(
            "probe=0014-collab-share\n"
            f"upstream_commit={SHA}\n"
            f"probed_at={time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}\n"
            "settings=doory.settings.prod via uvicorn doory.asgi:application\n"
            "note=raw evidence; never edit these files, only add new probe dirs\n"
        )
    print(f"collab share probe: {len(frames_log)} frame records")


if __name__ == "__main__":
    main()
