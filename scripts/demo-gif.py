#!/usr/bin/env python3
"""Records assets/demo.gif from demo mode (synthetic data only).

What it does:
  1. Builds a console stv2 from this checkout into a temporary directory
     (or uses --exe) and starts "stv2 demo --port <port>". Demo mode serves
     made-up devices, folders and activity over a synthetic wallpaper; it
     never reads Syncthing, Tailscale or the screen.
  2. Drives the real dashboard in Microsoft Edge headless (fresh temporary
     profile, DevTools protocol on loopback) through a short story:
       tray states -> status (in sync) -> Pair view discovering the tailnet
       -> "Pair" on a discovered device -> syncing (the demo's progress
       climbs about 1 % every 1.5 s, so the script waits for 88, 92, 96 and
       99 %) -> in sync again. The views with Recent Activity are captured
       before the Pair click, so its note never shows out of time order.
     Clicks are real mouse events sent to the page; every dashboard frame is
     a capture of the running app.
  3. Renders the tray icons with the app's own icon code
     ("go run ./internal/icon/cmd/genicons -strip").
  4. Lays the frames out on a dark canvas with a caption per scene, a pointer
     for the clicks and short crossfades between views, and encodes the GIF
     with ffmpeg (one palette for the whole loop, ordered dither, changed
     rectangles only). Without ffmpeg on PATH, Pillow encodes it instead.

Nothing in the output comes from this computer: no user or host names, no
addresses, no files and no desktop content. The clock times in Recent
Activity are the time of the recording.

Requires Python 3.9+, Pillow (pip install --user pillow), Go (for the tray
icons, and to build stv2 unless --exe is given), Microsoft Edge, and
preferably ffmpeg.

Usage:
  python scripts/demo-gif.py [--port 18998] [--exe path\\to\\console-stv2.exe]
                             [--edge path\\to\\msedge.exe] [--out assets/demo.gif]
                             [--keep-frames DIR]
"""

import argparse
import base64
import io
import json
import math
import os
import re
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request

try:
    from PIL import Image, ImageDraw, ImageFont
except ImportError:
    sys.exit("Pillow is required: python -m pip install --user pillow")

MODULE = "github.com/Neutx/syncthing-v2"
VERSION = "1.0.0"
VIEW_W, VIEW_H, SCALE = 460, 640, 2          # dashboard viewport, capture scale

# GIF layout: the dashboard at DASH_W wide, centred under a caption band.
CANVAS_W, CANVAS_H = 720, 780
DASH_W = 500
DASH_H = round(DASH_W * VIEW_H / VIEW_W)     # 696
DASH_X = (CANVAS_W - DASH_W) // 2
DASH_Y = 64
K = DASH_W / VIEW_W                          # CSS px -> canvas px
MAX_BYTES = 4 * 1024 * 1024

# Tray states, in the order genicons draws its strip, with the labels the
# dashboard uses for them.
TRAY_LABELS = ["In sync", "Syncing", "Checking", "Paused",
               "Disconnected", "Error", "API key rejected", "Not running"]


def log(msg):
    print(msg, flush=True)


# ---------- tools ----------

def find_go():
    p = shutil.which("go")
    if p:
        return p
    for base in (os.environ.get("LOCALAPPDATA"), os.environ.get("ProgramFiles")):
        if not base:
            continue
        for sub in (r"Programs\Go\bin\go.exe", r"Go\bin\go.exe"):
            c = os.path.join(base, sub)
            if os.path.isfile(c):
                return c
    sys.exit("Go was not found. Install it from https://go.dev/dl/ and run this script again.")


def find_edge():
    if os.name == "nt":
        import winreg
        for hive, key in ((winreg.HKEY_CURRENT_USER, r"SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe"),
                          (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe"),
                          (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe")):
            try:
                with winreg.OpenKey(hive, key) as k:
                    p = winreg.QueryValue(k, None).strip('"')
                    if p and os.path.isfile(p):
                        return p
            except OSError:
                pass
        for base in (os.environ.get("ProgramFiles(x86)"), os.environ.get("ProgramFiles"), os.environ.get("LOCALAPPDATA")):
            if base:
                p = os.path.join(base, r"Microsoft\Edge\Application\msedge.exe")
                if os.path.isfile(p):
                    return p
    for name in ("msedge", "microsoft-edge", "microsoft-edge-stable"):
        p = shutil.which(name)
        if p:
            return p
    sys.exit("Microsoft Edge was not found. Pass its path with --edge.")


def port_free(port):
    with socket.socket() as s:
        try:
            s.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def read_text(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


# ---------- a minimal WebSocket + DevTools protocol client ----------

class Cdp:
    def __init__(self, ws_url, timeout=30):
        u = urllib.parse.urlparse(ws_url)
        self.sock = socket.create_connection((u.hostname, u.port), timeout=timeout)
        key = base64.b64encode(os.urandom(16)).decode()
        req = (f"GET {u.path} HTTP/1.1\r\nHost: {u.hostname}:{u.port}\r\nUpgrade: websocket\r\n"
               f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.sock.sendall(req.encode())
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("Edge closed the DevTools connection during the handshake.")
            head += chunk
        status = head.split(b"\r\n", 1)[0]
        if b" 101 " not in status:
            raise RuntimeError(f"Edge refused the DevTools WebSocket ({status.decode(errors='replace')}).")
        self.buf = head.split(b"\r\n\r\n", 1)[1]
        self.next_id = 0

    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(max(65536, n - len(self.buf)))
            if not chunk:
                raise RuntimeError("Edge closed the DevTools connection.")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def _send_frame(self, opcode, payload):
        mask = os.urandom(4)
        n = len(payload)
        head = bytes([0x80 | opcode])
        if n < 126:
            head += bytes([0x80 | n])
        elif n < 65536:
            head += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", n)
        body = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(head + mask + body)

    def _recv_message(self):
        parts = []
        while True:
            b0, b1 = self._read(2)
            opcode, n = b0 & 0x0F, b1 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._read(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._read(8))[0]
            mask = self._read(4) if b1 & 0x80 else None
            data = self._read(n)
            if mask:
                data = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
            if opcode == 0x8:
                raise RuntimeError("Edge closed the DevTools connection.")
            if opcode == 0x9:
                self._send_frame(0xA, data)
                continue
            if opcode in (0x0, 0x1, 0x2):
                parts.append(data)
                if b0 & 0x80:
                    return b"".join(parts).decode("utf-8")

    def call(self, method, params=None, timeout=30):
        self.next_id += 1
        mid = self.next_id
        self._send_frame(0x1, json.dumps({"id": mid, "method": method, "params": params or {}}).encode())
        deadline = time.time() + timeout
        self.sock.settimeout(timeout)
        while True:
            msg = json.loads(self._recv_message())
            if msg.get("id") == mid:
                if "error" in msg:
                    raise RuntimeError(f"DevTools {method} failed: {msg['error']}")
                return msg.get("result", {})
            if time.time() > deadline:
                raise RuntimeError(f"DevTools {method} timed out.")

    def eval(self, expr):
        r = self.call("Runtime.evaluate", {"expression": expr, "returnByValue": True, "awaitPromise": True})
        if "exceptionDetails" in r:
            raise RuntimeError(f"Page script failed: {r['exceptionDetails'].get('text')}")
        return r.get("result", {}).get("value")

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


# ---------- the recording ----------

class Recorder:
    def __init__(self, base, token, cdp):
        self.base, self.token, self.cdp = base, token, cdp

    def open_view(self, view):
        body = json.dumps({"next": f"/?mode=glass&demo={view}"}).encode()
        req = urllib.request.Request(f"{self.base}/api/launch", data=body, method="POST", headers={
            "Authorization": f"Bearer {self.token}", "X-STV2": "1", "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=15) as r:
            launch = json.loads(r.read())
        if not launch.get("ok") or not launch.get("url"):
            raise RuntimeError(f"The demo server returned no login link for '{view}'.")
        self.cdp.call("Page.navigate", {"url": launch["url"]})
        self.wait(f"location.pathname === '/' && location.search.includes('demo={view}') && "
                  "document.readyState === 'complete' && document.body.classList.contains('mode-glass')",
                  f"the '{view}' view to load")

    def wait(self, expr, what, timeout=20):
        deadline = time.time() + timeout
        while not self.cdp.eval(f"Boolean({expr})"):
            if time.time() > deadline:
                raise RuntimeError(f"Timed out waiting for {what}.")
            time.sleep(0.05)

    def shot(self):
        r = self.cdp.call("Page.captureScreenshot", {"format": "png", "fromSurface": True}, timeout=60)
        img = Image.open(io.BytesIO(base64.b64decode(r["data"]))).convert("RGBA")
        if img.size != (VIEW_W * SCALE, VIEW_H * SCALE):
            raise RuntimeError(f"Capture is {img.size}, expected {(VIEW_W * SCALE, VIEW_H * SCALE)}.")
        return img

    def center(self, selector):
        v = self.cdp.eval(f"(() => {{ const e = document.querySelector({json.dumps(selector)}); if (!e) return null;"
                          " const r = e.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; })()")
        if not v:
            raise RuntimeError(f"Element {selector} is not on the page.")
        return v

    def mouse(self, kind, x, y):
        p = {"type": kind, "x": x, "y": y, "button": "left" if kind != "mouseMoved" else "none"}
        if kind != "mouseMoved":
            p["clickCount"] = 1
        self.cdp.call("Input.dispatchMouseEvent", p)

    def scroll_top(self):
        self.cdp.eval("(() => { const sc = document.querySelector('.view:not([hidden]) .scroll');"
                      " sc.style.scrollBehavior = 'auto'; sc.scrollTop = 0; return 0; })()")

    def pct(self):
        t = self.cdp.eval("document.getElementById('sync-big').textContent") or ""
        m = re.match(r"^(\d+)%$", t.strip())
        return int(m.group(1)) if m else -1

    def after_render(self, selector):
        # Resolves just after the list holding selector is next rebuilt (or
        # after 2 s), which leaves most of a second before the next one.
        self.cdp.eval("new Promise((done) => { const e = document.querySelector(%s);"
                      " const box = (e && e.closest('[id^=list-]')) || document.body;"
                      " const o = new MutationObserver(() => { o.disconnect(); done(true); });"
                      " o.observe(box, { childList: true }); setTimeout(() => { o.disconnect(); done(false); }, 2000); })"
                      % json.dumps(selector))

    def click(self, selector, landed):
        """Returns (point, hover capture, pressed capture) and completes the click.

        The dashboard rebuilds its lists on every snapshot (once a second), so
        a button can be replaced between the press and the release, and then
        no click happens. The press is timed right after a rebuild, and the
        click is retried until the landed expression holds."""
        for _ in range(4):
            x, y = self.center(selector)
            self.mouse("mouseMoved", x, y)
            time.sleep(0.25)
            hover = self.shot()
            self.after_render(selector)
            self.mouse("mousePressed", x, y)
            time.sleep(0.12)
            pressed = self.shot()
            self.mouse("mouseReleased", x, y)
            deadline = time.time() + 3
            while time.time() < deadline:
                if self.cdp.eval(f"Boolean({landed})"):
                    return (x, y), hover, pressed
                time.sleep(0.05)
            log(f"The click on {selector} did not land; trying again")
        raise RuntimeError(f"The click on {selector} never landed.")

    def pointer_away(self):
        # Parks the pointer in the bottom corner, where nothing reacts to it.
        self.mouse("mouseMoved", VIEW_W - 2, VIEW_H - 2)

    def record(self):
        s = {}
        # The views that show Recent Activity are captured before anything is
        # clicked: a click adds a note to the activity, and a note taken
        # during the Pair scene would sit above the newer syncing entries.
        # Status, in sync.
        self.open_view("status")
        time.sleep(1.5)
        s["status"] = self.shot()

        # Syncing: the demo's progress climbs from 38 % to 99 % and starts over.
        self.open_view("syncing")
        for target in (88, 92, 96, 99):
            deadline = time.time() + 120
            log(f"Waiting for {target} % (now {self.pct()} %)")
            while self.pct() != target:
                if time.time() > deadline:
                    raise RuntimeError(f"The syncing view never showed {target} %.")
                time.sleep(0.1)
            time.sleep(0.35)                        # the rest of the snapshot lands
            s[f"sync{target}"] = self.shot()

        # In sync again.
        self.open_view("status")
        time.sleep(1.5)
        s["insync"] = self.shot()

        # Pair: the discovery round (600 ms in demo mode), then its results.
        self.open_view("pair")
        if self.cdp.eval("document.getElementById('pair-msg').hidden"):
            # The first round ended while the page loaded: start another one.
            self.cdp.eval("document.getElementById('pair-refresh').click()")
        s["discovering"] = self.shot()
        if "Looking for your devices" not in (self.cdp.eval("document.getElementById('pair-msg').textContent") or ""):
            raise RuntimeError("The discovery message was gone before the capture.")
        self.wait("document.querySelector('[aria-label=\"Pair with workshop-mini\"]')", "the discovered devices")
        time.sleep(0.6)
        s["found"] = self.shot()
        s["pair_pt"], s["pair_hover"], s["pair_press"] = self.click(
            '[aria-label="Pair with workshop-mini"]', "document.getElementById('toast').classList.contains('show')")
        self.pointer_away()
        self.scroll_top()
        time.sleep(0.4)
        s["paired"] = self.shot()                   # toast, requests and the share panel
        return s


# ---------- composition ----------

def font(size, bold=False):
    names = ["seguisb.ttf", "segoeuib.ttf"] if bold else ["segoeui.ttf"]
    names += ["DejaVuSans-Bold.ttf" if bold else "DejaVuSans.ttf", "Arial.ttf"]
    for n in names:
        for d in ("", os.path.join(os.environ.get("WINDIR", r"C:\Windows"), "Fonts")):
            try:
                return ImageFont.truetype(os.path.join(d, n) if d else n, size)
            except OSError:
                pass
    return ImageFont.load_default(size)


def background():
    # A dark, quiet version of the demo's synthetic wallpaper (ui.DemoWallpaper).
    w, h = CANVAS_W // 4, CANVAS_H // 4
    img = Image.new("RGB", (w, h))
    px = img.load()
    blobs = [(0.18, 0.12, 0.55, 64, 120, 255), (0.92, 0.46, 0.50, 40, 200, 170), (0.30, 0.95, 0.60, 170, 80, 230)]
    for y in range(h):
        for x in range(w):
            u, v = x / w, y / h
            t = (u + v) / 2
            r, g, b = 14 + 10 * t, 18 + 8 * t, 34 + 20 * (1 - t)
            for bx, by, br, cr, cg, cb in blobs:
                dx, dy = u - bx, (v - by) * h / w
                k = math.exp(-(dx * dx + dy * dy) / (br * br * 0.35)) * 0.3
                r, g, b = r + cr * k, g + cg * k, b + cb * k
            px[x, y] = (int(min(r, 255)), int(min(g, 255)), int(min(b, 255)))
    return img.resize((CANVAS_W, CANVAS_H), Image.BICUBIC)


def pointer(z=1.3):
    # A plain arrow pointer, z times the usual 12x20 px, drawn at 4x and
    # scaled down for smooth edges.
    s = 4 * z
    pts = [(0, 0), (0, 17), (4.2, 13.2), (7, 19.6), (9.8, 18.4), (7.1, 12.2), (12.4, 12.2)]
    w, h = round(15 * z), round(23 * z)
    img = Image.new("RGBA", (w * 4, h * 4), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.polygon([((x + 1.2) * s, (y + 1.2) * s) for x, y in pts], fill=(255, 255, 255, 255),
              outline=(10, 12, 18, 255), width=round(1.3 * s))
    return img.resize((w, h), Image.LANCZOS)


class Composer:
    def __init__(self):
        self.bg = background()
        self.cap_font = font(22, bold=True)
        self.ptr = pointer()

    def dash(self, shot):
        return shot.resize((DASH_W, DASH_H), Image.LANCZOS)

    def frame(self, shot, caption, ptr=None):
        img = self.bg.copy()
        self.caption(img, caption)
        dash = self.dash(shot)
        img.paste(dash, (DASH_X, DASH_Y), dash)
        if ptr:
            x, y = ptr
            img.paste(self.ptr, (round(DASH_X + x * K) - 1, round(DASH_Y + y * K) - 1), self.ptr)
        return img

    def caption(self, img, text):
        d = ImageDraw.Draw(img)
        w = d.textlength(text, font=self.cap_font)
        d.text(((CANVAS_W - w) / 2, 18), text, font=self.cap_font, fill=(236, 240, 248))

    def tray(self, strip, caption):
        img = self.bg.copy()
        self.caption(img, caption)
        size = strip.height
        gap = size // 4
        lab = font(17)
        cols, cell_w, cell_h = 4, 160, 190
        x0 = (CANVAS_W - cols * cell_w) // 2
        y0 = (CANVAS_H - 2 * cell_h) // 2 + 20
        d = ImageDraw.Draw(img)
        for i, text in enumerate(TRAY_LABELS):
            icon = strip.crop((i * (size + gap), 0, i * (size + gap) + size, size))
            cx = x0 + (i % cols) * cell_w + cell_w // 2
            cy = y0 + (i // cols) * cell_h
            img.paste(icon, (cx - size // 2, cy), icon)
            w = d.textlength(text, font=lab)
            d.text((cx - w / 2, cy + size + 16), text, font=lab, fill=(200, 208, 222))
        return img


def build_story(s, strip):
    """Returns [(image, milliseconds)]."""
    c = Composer()
    out = []

    def hold(img, ms):
        out.append((img, ms))

    # Durations are multiples of 40 ms: ffmpeg's concat input runs on a 25 fps
    # clock and would round anything else unevenly.
    def fade(a, b, steps=5, ms=40):
        for i in range(1, steps + 1):
            out.append((Image.blend(a, b, i / (steps + 1)), ms))

    def glide(shot, caption, p0, p1, steps=6, ms=40):
        for i in range(1, steps + 1):
            t = i / steps
            t = 1 - (1 - t) ** 3                    # ease out
            p = (p0[0] + (p1[0] - p0[0]) * t, p0[1] + (p1[1] - p0[1]) * t)
            out.append((c.frame(shot, caption, p), ms))

    cap_tray = "Every sync state, right in the tray"
    cap_status = "Your computers, in sync"
    cap_find = "Finds your devices on your tailnet"
    cap_pair = "Pair in one click"
    cap_sync = "Watch the sync, live"
    cap_done = "In sync again"

    tray = c.tray(strip, cap_tray)
    status = c.frame(s["status"], cap_status)
    hold(tray, 1520)
    fade(tray, status)
    hold(status, 1320)

    disc = c.frame(s["discovering"], cap_find)
    fade(status, disc)
    hold(disc, 600)
    hold(c.frame(s["found"], cap_find), 1200)

    start = (VIEW_W * 0.72, VIEW_H * 0.86)
    glide(s["found"], cap_pair, start, s["pair_pt"], steps=5)
    out[-1] = (c.frame(s["pair_hover"], cap_pair, s["pair_pt"]), 320)
    hold(c.frame(s["pair_press"], cap_pair, s["pair_pt"]), 160)
    paired = c.frame(s["paired"], cap_pair)
    hold(paired, 1320)

    syncs = [c.frame(s[f"sync{p}"], cap_sync) for p in (88, 92, 96, 99)]
    fade(paired, syncs[0])
    for img in syncs:
        hold(img, 640)
    done = c.frame(s["insync"], cap_done)
    fade(syncs[-1], done)
    hold(done, 1520)
    fade(done, tray)
    return out


def encode(frames, out_path, tmp):
    total = sum(ms for _, ms in frames)
    if any(ms % 40 for _, ms in frames):
        raise ValueError("Frame durations must be multiples of 40 ms.")
    ffmpeg = shutil.which("ffmpeg")
    if ffmpeg:
        d = os.path.join(tmp, "frames")
        os.makedirs(d, exist_ok=True)
        lines = []
        for i, (img, ms) in enumerate(frames):
            p = os.path.join(d, f"f{i:03d}.png")
            img.save(p)
            lines.append(f"file '{p.replace(os.sep, '/')}'\nduration {ms / 1000:.3f}\n")
        lines.append(f"file '{os.path.join(d, f'f{len(frames) - 1:03d}.png').replace(os.sep, '/')}'\n")
        lst = os.path.join(tmp, "frames.txt")
        with open(lst, "w", encoding="utf-8") as f:
            f.writelines(lines)
        subprocess.run([ffmpeg, "-y", "-v", "error", "-f", "concat", "-safe", "0", "-i", lst,
                        "-filter_complex",
                        "[0:v]split[a][b];[a]palettegen=max_colors=256:stats_mode=full:reserve_transparent=0[p];"
                        "[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle",
                        "-fps_mode", "vfr", "-loop", "0", out_path], check=True)
    else:
        log("ffmpeg not found; encoding with Pillow")
        imgs = [img for img, _ in frames]
        imgs[0].save(out_path, save_all=True, append_images=imgs[1:], duration=[ms for _, ms in frames],
                     loop=0, optimize=True, disposal=1)
    size = os.path.getsize(out_path)
    with Image.open(out_path) as g:
        n, w, h = g.n_frames, g.width, g.height
    log(f"Wrote {out_path}: {w}x{h}, {n} frames, {total / 1000:.1f} s loop, {size / 1024 / 1024:.2f} MB")
    if size > MAX_BYTES:
        raise RuntimeError(f"{out_path} is {size} bytes, over the {MAX_BYTES} byte budget.")


# ---------- main ----------

def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", type=int, default=18998, help="port for the demo server on 127.0.0.1")
    ap.add_argument("--exe", default="", help="an existing console build of stv2 to use")
    ap.add_argument("--edge", default="", help="path of msedge.exe")
    ap.add_argument("--out", default="", help="output GIF (default assets/demo.gif)")
    ap.add_argument("--keep-frames", default="", help="also save the raw captures to this directory")
    a = ap.parse_args()

    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    out_path = os.path.abspath(a.out or os.path.join(root, "assets", "demo.gif"))
    edge = a.edge or find_edge()
    go = find_go()
    if not (1024 <= a.port <= 65535) or not port_free(a.port):
        sys.exit(f"Port {a.port} on 127.0.0.1 is not usable. Pick another one with --port.")

    tmp = tempfile.mkdtemp(prefix="stv2-gif-")
    demo = browser = cdp = out_file = None
    profile = os.path.join(tmp, "edge-profile")
    try:
        env = dict(os.environ, CGO_ENABLED="0")
        exe = os.path.abspath(a.exe) if a.exe else os.path.join(tmp, "stv2.exe" if os.name == "nt" else "stv2")
        if not a.exe:
            log(f"Building stv2 {VERSION} for the recording")
            subprocess.run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags",
                            f"-X {MODULE}/internal/brand.Version={VERSION}", "-o", exe, "./cmd/stv2"],
                           cwd=root, env=env, check=True)
        strip_png = os.path.join(tmp, "tray-states.png")
        subprocess.run([go, "run", "./internal/icon/cmd/genicons", "-out", os.path.join(tmp, "icons"),
                        "-strip", strip_png, "-strip-size", "80"], cwd=root, env=env, check=True)
        strip = Image.open(strip_png).convert("RGBA")

        # 1. The demo server; its output carries the control token for /api/launch.
        out_file = open(os.path.join(tmp, "demo.out"), "w")
        demo = subprocess.Popen([exe, "demo", "--port", str(a.port)], stdout=out_file, stderr=subprocess.STDOUT,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
        token, deadline = None, time.time() + 30
        while not token:
            if demo.poll() is not None:
                raise RuntimeError(f"stv2 demo exited with code {demo.returncode}: {read_text(out_file.name)}")
            if time.time() > deadline:
                raise RuntimeError("stv2 demo did not start within 30 s.")
            time.sleep(0.2)
            m = re.search(r"Authorization: Bearer ([A-Za-z0-9_-]+)", read_text(out_file.name))
            token = m and m.group(1)
        base = f"http://127.0.0.1:{a.port}"
        log(f"Demo server (synthetic data) on {base}")

        # 2. Headless Edge with a throwaway profile and DevTools on loopback.
        browser = subprocess.Popen([edge, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--mute-audio",
                                    "--no-first-run", "--no-default-browser-check", "--disable-extensions",
                                    "--disable-sync", f"--user-data-dir={profile}",
                                    "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0",
                                    f"--window-size={VIEW_W},{VIEW_H}", f"--force-device-scale-factor={SCALE}",
                                    "--default-background-color=00000000", "--force-prefers-reduced-motion",
                                    "about:blank"],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        port_file = os.path.join(profile, "DevToolsActivePort")
        deadline = time.time() + 30
        while True:
            lines = read_text(port_file).split("\n")
            if len(lines) >= 2 and lines[0].strip().isdigit() and lines[1].strip().startswith("/devtools/browser/"):
                dev_port = lines[0].strip()
                break
            if time.time() > deadline:
                raise RuntimeError("Edge did not open its DevTools endpoint within 30 s.")
            time.sleep(0.2)

        # A tab of our own, opened on the demo's origin so later navigations
        # keep the same renderer. A fresh profile may still replace it while
        # it initialises, so the whole recording is retried in a new tab.
        shots = None
        for attempt in range(1, 4):
            try:
                if cdp:
                    cdp.close()
                req = urllib.request.Request(f"http://127.0.0.1:{dev_port}/json/new?" +
                                             urllib.parse.quote(f"{base}/login", safe=""), method="PUT")
                with urllib.request.urlopen(req, timeout=15) as r:
                    tab = json.loads(r.read())
                time.sleep(0.5)
                cdp = Cdp(tab["webSocketDebuggerUrl"])
                cdp.call("Emulation.setDeviceMetricsOverride",
                         {"width": VIEW_W, "height": VIEW_H, "deviceScaleFactor": SCALE, "mobile": False})
                cdp.call("Emulation.setDefaultBackgroundColorOverride", {"color": {"r": 0, "g": 0, "b": 0, "a": 0}})
                cdp.call("Emulation.setEmulatedMedia",
                         {"features": [{"name": "prefers-reduced-motion", "value": "reduce"}]})
                shots = Recorder(base, token, cdp).record()
                break
            except (RuntimeError, OSError) as e:
                if attempt == 3:
                    raise
                log(f"Edge was not ready; trying again in a new tab ({e})")
                time.sleep(1)

        if a.keep_frames:
            os.makedirs(a.keep_frames, exist_ok=True)
            for k, v in shots.items():
                if isinstance(v, Image.Image):
                    v.save(os.path.join(a.keep_frames, f"{k}.png"))

        # 3. The GIF.
        encode(build_story(shots, strip), out_path, tmp)
    finally:
        if cdp:
            cdp.close()
        for p in (browser, demo):
            if p and p.poll() is None:
                p.kill()
                try:
                    p.wait(10)
                except subprocess.TimeoutExpired:
                    pass
        if os.name == "nt":
            # Edge helper processes that outlive the main one hold the profile.
            subprocess.run(["powershell", "-NoProfile", "-Command",
                            "Get-CimInstance Win32_Process -Filter \"Name='msedge.exe'\" | "
                            f"Where-Object {{ $_.CommandLine -and $_.CommandLine.Contains('{profile}') }} | "
                            "ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if out_file:
            out_file.close()
        for _ in range(10):
            shutil.rmtree(tmp, ignore_errors=True)
            if not os.path.exists(tmp):
                break
            time.sleep(0.5)
    log("Done. Review the GIF before committing it: it must show demo data only.")


if __name__ == "__main__":
    main()
