#!/usr/bin/env python
"""Probe the collab WebSocket against the ORACLE (uvicorn + doory.asgi).

Phases (each boots the oracle on its own port):
  A: owner connect  -> frame sequence (sync step1, ready shape), ping/pong,
                       a client sync step1 answered by the server state
  B: handshake denials: bad Origin (403), no session (accept + close4401)
  C: frame-level checks after join: oversized frame close4413? (skipped if
                       slow), unknown text frame -> close4400, awareness
                       exchange

Raw transcripts: test/parity/probes/0012-collab/{meta.txt,NNN-*.txt}
Frames are recorded as direction + type + payload hex (first512 bytes).
"""
import asyncio
import base64
import json
import os
import signal
import subprocess
import sys
import time
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
    "DATABASE_URL": f"sqlite:///{REPO}/.oracle/oracle.sqlite3",
    "MEDIA_ROOT": os.path.join(REPO, ".oracle/media"),
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
    proc = subprocess.Popen(
        [sys.executable, "-m", "uvicorn", "doory.asgi:application", "--host", "127.0.0.1",
         "--port", str(port), "--log-level", "warning"],
        cwd=BACKEND, env=env,
        stdout=open(os.path.join(OUT, f"uvicorn-{port}.log"), "w"),
        stderr=subprocess.STDOUT,
    )
    for _ in range(100):
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


def http(port, method, path, body=None, headers=None, jar={}):
    url = f"http://127.0.0.1:{port}{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    csrf = jar.get("csrftoken")
    if csrf:
        req.add_header("X-CSRFToken", csrf)
        req.add_header("Referer", f"http://127.0.0.1:{port}/")
    cookie = "; ".join(f"{k}={v}" for k, v in jar.items())
    if cookie:
        req.add_header("Cookie", cookie)
    try:
        resp = urllib.request.urlopen(req)
        status, payload, hdrs = resp.status, resp.read(), resp.headers
    except urllib.error.HTTPError as exc:
        status, payload, hdrs = exc.code, exc.read(), exc.headers
    _store_cookies(jar, hdrs)
    return status, payload, dict(hdrs)


def _store_cookies(jar, headers):
    values = []
    getter = getattr(headers, "get_all", None)
    if getter:
        values = getter("Set-Cookie") or []
    if not values and hasattr(headers, "get"):
        v = headers.get("Set-Cookie")
        if v:
            values = [v]
    for value in values:
        kv = value.split(";")[0]
        if "=" in kv:
            k, v = kv.split("=", 1)
            jar[k.strip()] = v.strip()


_seq = [0]


def setup(port):
    """csrf -> register -> login -> create design; returns (jar, design_id)."""
    jar = {}
    _seq[0] += 1
    email = f"collabprobe{os.getpid()}x{_seq[0]}@example.com"
    http(port, "GET", "/api/v1/auth/csrf/", jar=jar)
    st, body, _ = http(port, "POST", "/api/v1/auth/register/",
                       {"email": email, "password": "Tr0ub4dor&3xkcd9"},
                       jar=jar)
    assert st == 201, (st, body)
    st, body, _ = http(port, "POST", "/api/v1/auth/login/",
                       {"email": email, "password": "Tr0ub4dor&3xkcd9"},
                       jar=jar)
    assert st == 200, (st, body)
    st, body, _ = http(port, "POST", "/api/v1/designs/", {"title": "WS Probe"}, jar=jar)
    assert st == 201, (st, body)
    design_id = json.loads(body)["id"]
    return jar, design_id


def cookie_header(jar):
    return "; ".join(f"{k}={v}" for k, v in jar.items())


async def recv_frame(ws, timeout=4.0, count=1):
    out = []
    try:
        for _ in range(count):
            msg = await asyncio.wait_for(ws.recv(), timeout)
            out.append(msg)
    except asyncio.TimeoutError:
        pass
    except Exception as exc:  # ConnectionClosed* carries the server's code
        out.append(f"<closed:{getattr(ws, 'close_code', '?')}:{str(exc)[:80]}>")
    return out


def frame_type_char(data):
    # y-websocket: first varuint = message type (0 sync,1 awareness,2 auth,3 query)
    if not data:
        return "empty"
    msg = data[0] & 0x7F
    names = {0: "sync", 1: "awareness", 2: "auth", 3: "query-awareness"}
    name = names.get(msg, f"msg{msg}")
    if msg == 0 and len(data) > 1:
        sub = data[1] & 0x7F
        name += {0: "/step1", 1: "/step2", 2: "/update"}.get(sub, f"/{sub}")
    return name


async def phase_a(port):
    jar, design_id = setup(port)
    origin = f"http://127.0.0.1:{port}"
    url = f"ws://127.0.0.1:{port}/ws/designs/{design_id}/"
    async with websockets.connect(url, additional_headers={
        "Cookie": cookie_header(jar), "Origin": origin,
    }) as ws:
        got = await recv_frame(ws, count=6)
        for data in got:
            if isinstance(data, str):
                log("A", "recv", "text", data.encode(), "json")
            else:
                log("A", "recv", frame_type_char(data), data,
                    "json" if data[:1] in (b"{", b"[") else "")
        # text ping -> pong
        await ws.send('{"type":"ping"}')
        got = await recv_frame(ws, count=2)
        for data in got:
            payload = data.encode() if isinstance(data, str) else data
            log("A", "recv", "text", payload, "json")
        # client sync step1 (empty state vector): varuint(0)+varuint(0)... build:
        # message=0, kind=0, payload=varbytes(sv) with sv = b"\x00" (0 clients)
        step1 = bytes([0, 0, 1, 0])
        await ws.send(step1)
        got = await recv_frame(ws, count=3)
        for data in got:
            if isinstance(data, str):
                log("A", "recv", "text", data.encode(), "json")
            else:
                log("A", "recv", frame_type_char(data), data,
                    "json" if data[:1] in (b"{", b"[") else "")
        # awareness query
        await ws.send(bytes([3]))
        got = await recv_frame(ws, count=3)
        for data in got:
            if isinstance(data, str):
                log("A", "recv", "text", data.encode(), "")
            else:
                log("A", "recv", frame_type_char(data), data, "")
        await ws.close()
        log("A", "close", "local-close")


async def phase_b(port):
    jar, design_id = setup(port)
    url = f"ws://127.0.0.1:{port}/ws/designs/{design_id}/"
    # B1: bad origin
    try:
        async with websockets.connect(url, additional_headers={
            "Cookie": cookie_header(jar), "Origin": "http://evil.example",
        }) as ws:
            await ws.send(b"x")
            log("B", "bad-origin", "CONNECTED-UNEXPECTED")
    except Exception as exc:
        log("B", "bad-origin", "refused", b"", str(exc)[:200])
    # B2: no cookie
    try:
        async with websockets.connect(url, additional_headers={
            "Origin": f"http://127.0.0.1:{port}",
        }) as ws:
            got = await recv_frame(ws, count=3)
            for data in got:
                payload = data.encode() if isinstance(data, str) else data
                log("B", "no-session", frame_type_char(payload), payload, "json" if payload[:1] in (b"{", b"[") else "")
            code = ws.close_code
            log("B", "no-session", "close-code", b"", str(code))
    except Exception as exc:
        log("B", "no-session", "error", b"", str(exc)[:200])


async def phase_c(port):
    jar, design_id = setup(port)
    origin = f"http://127.0.0.1:{port}"
    url = f"ws://127.0.0.1:{port}/ws/designs/{design_id}/"
    async with websockets.connect(url, additional_headers={
        "Cookie": cookie_header(jar), "Origin": origin,
    }, max_size=None) as ws:
        await recv_frame(ws, count=4)
        # C1: unknown text frame type -> expect close4400
        await ws.send('{"type":"bogus"}')
        got = await recv_frame(ws, count=3)
        for data in got:
            payload = data.encode() if isinstance(data, str) else data
            log("C", "bogus-text", frame_type_char(payload), payload, "json" if payload[:1] in (b"{", b"[") else "")
        log("C", "bogus-text", "close-code", b"", str(ws.close_code))
    # C2: binary frame with an unknown message type ->4400
    try:
        async with websockets.connect(url, additional_headers={
            "Cookie": cookie_header(jar), "Origin": origin,
        }, max_size=None) as ws:
            await recv_frame(ws, count=4)
            await ws.send(bytes([9]))
            got = await recv_frame(ws, count=3)
            for data in got:
                payload = data.encode() if isinstance(data, str) else data
                log("C", "bad-binary", frame_type_char(payload), payload, "")
            log("C", "bad-binary", "close-code", b"", str(ws.close_code))
    except Exception as exc:
        log("C", "bad-binary", "error", b"", str(exc)[:200])


def main():
    os.makedirs(OUT, exist_ok=True)
    port_a, port_b, port_c = 8031, 8032, 8033
    procs = []
    loop = asyncio.get_event_loop()
    for port, name, fn in ((port_a, "A", phase_a), (port_b, "B", phase_b), (port_c, "C", phase_c)):
        proc = boot(port)
        procs.append(proc)
        try:
            loop.run_until_complete(fn(port))
        except Exception as exc:
            frames_log.append({"phase": name, "dir": "crash", "kind": "crash",
                               "len": 0, "hex": "", "note": str(exc)[:300]})
        stop(proc)
        procs.remove(proc)
    try:
        for p in procs:
            stop(p)
    except Exception:
        pass
    with open(os.path.join(OUT, "frames.json"), "w", encoding="utf-8") as fh:
        json.dump(frames_log, fh, ensure_ascii=False, separators=(",", ":"))
    with open(os.path.join(OUT, "meta.txt"), "w") as fh:
        fh.write(f"probe=0012-collab\nupstream_commit={SHA}\n"
                 f"probed_at={time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}\n"
                 "settings=doory.settings.prod via uvicorn doory.asgi:application\n"
                 "note=raw evidence; never edit these files, only add new probe dirs\n")
    print(f"collab ws probe: {len(frames_log)} frame records")


if __name__ == "__main__":
    main()
