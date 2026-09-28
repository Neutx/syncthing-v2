// app.js - SyncThing V2 dashboard: Status, Pair, Settings/About and Welcome views.
// Chrome (backdrop, scrim, sweep, rim), the status glyph and the progress bar
// follow the prototype's Dashboard.cs; motion uses anim.js (a port of Anim.cs).
// Hover is pure CSS and only changes fill and rim brightness.
(function () {
  'use strict';

  const A = window.Anim;
  const Ease = A.Ease;

  const W = 460, H = 640, WIN_RADIUS = 40;
  const METER_FULL = 8 * 1024 * 1024; // D8: meters are full at 8 MB/s
  const BAR_W = 368, BAR_H = 10;

  // ---------- page parameters ----------
  const params = new URLSearchParams(location.search);
  const MODES = ['glass', 'vibrancy', 'browser'];
  const MODE = MODES.indexOf(params.get('mode')) >= 0 ? params.get('mode') : 'browser';
  const DEMO = /^[a-z]{1,16}$/.test(params.get('demo') || '') ? params.get('demo') : '';
  const CORNERS = { br: '100% 100%', bl: '0% 100%', tr: '100% 0%', tl: '0% 0%' };
  const CORNER = CORNERS[params.get('corner')] || (MODE === 'vibrancy' ? CORNERS.tr : CORNERS.br);
  const reduceMQ = window.matchMedia ? window.matchMedia('(prefers-reduced-motion: reduce)') : null;
  const reduced = () => !!(reduceMQ && reduceMQ.matches);

  const $ = (id) => document.getElementById(id);
  const rootStyle = getComputedStyle(document.documentElement);
  function tokenColor(name, fallback) {
    const h = (rootStyle.getPropertyValue(name).trim() || fallback).replace('#', '');
    return { a: 255, r: parseInt(h.substr(0, 2), 16), g: parseInt(h.substr(2, 2), 16), b: parseInt(h.substr(4, 2), 16) };
  }
  const rgba = (c, alpha) => 'rgba(' + c.r + ',' + c.g + ',' + c.b + ',' + Math.max(0, Math.min(1, alpha)).toFixed(3) + ')';

  const COL = {
    fg: tokenColor('--fg', '#F6F8FC'),
    faint: tokenColor('--faint', '#9AA3A8'),
    accent: tokenColor('--accent', '#60A6FF'),
    insync: tokenColor('--st-insync', '#3AC672'),
    syncing: tokenColor('--st-syncing', '#569CF6'),
    scanning: tokenColor('--st-scanning', '#F0B838'),
    error: tokenColor('--st-error', '#EE5C52'),
    paused: tokenColor('--st-paused', '#96969E'),
    down: tokenColor('--st-down', '#787C86'),
    barIdle: tokenColor('--bar-idle', '#8C92A0')
  };

  function stateColor(st) {
    switch (st) {
      case 'insync': return COL.insync;
      case 'syncing': return COL.syncing;
      case 'scanning': return COL.scanning;
      case 'nopeer':
      case 'error': return COL.error;
      case 'paused': return COL.paused;
      default: return COL.down;
    }
  }

  // ---------- elements ----------
  const stage = $('stage'), backdropEl = $('backdrop'), scrimEl = $('scrim'), contentEl = $('content');
  const fxUnder = $('fx-under'), fxOver = $('fx-over'), glyphCv = $('glyph'), barCv = $('bar');
  const views = { status: $('view-status'), pair: $('view-pair'), settings: $('view-settings'), welcome: $('view-welcome') };

  // ---------- animation state (Dashboard.cs fields) ----------
  const enter = new A.Tween();
  const sweep = new A.Tween();
  const pct = new A.Spring(0, 120, 20);
  const dn = new A.Spring(0, 150, 22);
  const up = new A.Spring(0, 150, 22);
  const shimmer = new A.Loop(2.2);
  const breathe = new A.Loop(6.0);
  const spin = new A.Loop(2.4);
  let colNow = COL.insync, colTarget = COL.insync;
  let closing = false;
  let raf = 0, lastTick = 0;
  let hasBackdrop = false;

  let snap = null;            // latest snapshot from /api/state
  let info = { atLogin: 'at login', os: '', name: 'SyncThing V2', version: '', commit: '', disclaimer: '', repoURL: '' };
  let view = 'status';
  const ages = new Map();     // activity key -> 0..1 slide-in progress
  const busyBtns = new Set(); // status buttons with an action in flight

  // ---------- formatters (ports of the prototype's Grp, Rate) ----------
  const grp = (n) => Math.trunc(Number(n) || 0).toLocaleString('en-US');
  function fmtRate(bps) {
    if (!(bps >= 1024)) return '0 KB/s';
    const u = ['B', 'KB', 'MB', 'GB'];
    let v = bps, i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return (v < 10 ? v.toFixed(1) : Math.round(v).toString()) + ' ' + u[i] + '/s';
  }
  const shortID = (id) => String(id || '').slice(0, 7);
  const pad2 = (n) => (n < 10 ? '0' : '') + n;
  function fmtTime(at) {
    const d = new Date(at);
    if (isNaN(d.getTime())) return '--:--:--';
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  }
  function osLabel(os) {
    const k = String(os || '').toLowerCase();
    return { windows: 'Windows', macos: 'macOS', darwin: 'macOS', linux: 'Linux' }[k] || String(os || '');
  }
  const stripScheme = (a) => String(a || '').replace(/^[a-z]+:\/\//i, '');

  // ---------- DOM helpers (no innerHTML: every string is text) ----------
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }
  function segs(parent, parts) {
    parts.forEach((p) => parent.appendChild(p.b ? el('b', null, p.t) : document.createTextNode(p.t)));
  }
  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  function setText(id, text) { const n = $(id); if (n.textContent !== text) n.textContent = text; }

  // ---------- squircle (port of Glass.SquirclePath) ----------
  function squircle(x, y, w, h, radius) {
    const rad = Math.min(radius, Math.min(w, h) * 0.5);
    if (rad <= 0.5) return [[x, y], [x + w, y], [x + w, y + h], [x, y + h]];
    const inv = 1 / 4;
    const steps = Math.max(6, Math.trunc(rad * 0.7));
    const arc = [];
    for (let i = 0; i <= steps; i++) {
      const a = (i / steps) * Math.PI / 2;
      const cs = Math.cos(a), sn = Math.sin(a);
      arc.push([rad * Math.pow(Math.abs(cs), inv) * Math.sign(cs), rad * Math.pow(Math.abs(sn), inv) * Math.sign(sn)]);
    }
    const l = x, t = y, r = x + w, b = y + h, pts = [];
    for (let i = steps; i >= 0; i--) pts.push([r - rad + arc[i][0], t + rad - arc[i][1]]);
    for (let i = 0; i <= steps; i++) pts.push([r - rad + arc[i][0], b - rad + arc[i][1]]);
    for (let i = steps; i >= 0; i--) pts.push([l + rad - arc[i][0], b - rad + arc[i][1]]);
    for (let i = 0; i <= steps; i++) pts.push([l + rad - arc[i][0], t + rad - arc[i][1]]);
    return pts;
  }
  function tracePath(ctx, pts) {
    ctx.beginPath();
    ctx.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i][0], pts[i][1]);
    ctx.closePath();
  }
  const WIN_PATH = squircle(0, 0, W, H, WIN_RADIUS);
  const RIM_PATH = squircle(0.5, 0.5, W - 1, H - 1, WIN_RADIUS);

  // Canvases are sized in CSS pixels and backed at devicePixelRatio.
  function ctxFor(cv, w, h) {
    const r = window.devicePixelRatio || 1;
    const pw = Math.round(w * r), ph = Math.round(h * r);
    if (cv.width !== pw || cv.height !== ph) { cv.width = pw; cv.height = ph; }
    const ctx = cv.getContext('2d');
    ctx.setTransform(r, 0, 0, r, 0, 0);
    ctx.clearRect(0, 0, w, h);
    return ctx;
  }

  // ---------- frame loop (OnAnimTick) ----------
  function ensureAnimating() {
    if (!raf) {
      lastTick = performance.now();
      raf = requestAnimationFrame(tick);
    }
  }

  function tick(now) {
    raf = 0;
    let dt = (now - lastTick) / 1000;
    lastTick = now;
    if (dt <= 0) dt = 0.016;

    let busy = false;
    busy = enter.Step(dt) || busy;
    busy = sweep.Step(dt) || busy;
    busy = pct.Step(dt) || busy;
    busy = dn.Step(dt) || busy;
    busy = up.Step(dt) || busy;
    presses.forEach((sp) => { busy = sp.Step(dt) || busy; });

    ages.forEach((age, key) => {
      if (age < 1) { ages.set(key, Math.min(1, age + dt * 3.2)); busy = true; }
    });

    if (colNow.r !== colTarget.r || colNow.g !== colTarget.g || colNow.b !== colTarget.b) {
      colNow = A.ColorLerp.Mix(colNow, colTarget, Math.min(1, dt * 5));
      if (Math.abs(colNow.r - colTarget.r) < 2 && Math.abs(colNow.g - colTarget.g) < 2 &&
          Math.abs(colNow.b - colTarget.b) < 2) colNow = colTarget;
      busy = true;
    }

    const st = snap ? snap.StateName : '';
    if (!reduced()) {
      breathe.Step(dt);
      if (st === 'syncing' || st === 'scanning') {
        shimmer.Step(dt);
        spin.Step(dt);
        busy = true;
      }
      if (peersUp() > 0) busy = true; // presence pulse
    }

    if (closing && !enter.Running) {
      closing = false;
      render();
      finishHide();
      return;
    }
    render();
    if (busy || closing) raf = requestAnimationFrame(tick);
  }

  function peersUp() {
    return snap && snap.Peers ? snap.Peers.filter((p) => p.Connected).length : 0;
  }

  // ---------- rendering ----------
  function render() {
    const ent = Math.max(0.001, enter.Eased);
    const e1 = Math.min(1, ent);
    const scale = 0.965 + 0.035 * Math.min(1.06, ent);
    stage.style.transform = 'scale(' + scale.toFixed(4) + ')';

    if (MODE === 'glass') {
      backdropEl.style.opacity = hasBackdrop ? String(Math.min(1, ent * 1.2)) : String(0.941 * e1);
    } else if (MODE === 'browser') {
      backdropEl.style.opacity = String(e1);
    }
    scrimEl.style.opacity = String(e1);
    contentEl.style.opacity = String(e1);
    contentEl.style.transform = 'translateY(' + ((1 - e1) * 12).toFixed(2) + 'px)';

    drawSweep();
    drawRim(e1);
    drawGlyph();
    drawBar();
    drawLive();
  }

  // Entrance specular sweep (under the content).
  function drawSweep() {
    const ctx = ctxFor(fxUnder, W, H);
    if (!(sweep.T > 0 && sweep.T < 1)) return;
    const e = sweep.Eased;
    const sx = -170 + e * (W + 340);
    const g = ctx.createLinearGradient(sx - 150, 0, sx + 150, 0);
    g.addColorStop(0, 'rgba(255,255,255,0)');
    g.addColorStop(0.5, 'rgba(255,255,255,' + ((44 * (1 - e * 0.5)) / 255).toFixed(3) + ')');
    g.addColorStop(1, 'rgba(255,255,255,0)');
    ctx.fillStyle = g;
    ctx.fillRect(sx - 150, 0, 300, H);
  }

  // One rim on the window edge plus the breathing specular arc (D4).
  function drawRim(e1) {
    const ctx = ctxFor(fxOver, W, H);
    const drift = reduced() ? 0 : (breathe.Sin01 - 0.5) * 2;
    // The arc is the rim stroked brighter, then masked by an ellipse around
    // the upper-left corner (72 % of the width along the top, 120 px down the
    // left side) that fades to nothing, so the arc has soft ends. A hard
    // rectangular clip left it ending in abrupt steps that read as a
    // rendering glitch.
    tracePath(ctx, RIM_PATH);
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = 'rgba(255,255,255,' + ((120 * e1) / 255).toFixed(3) + ')';
    ctx.stroke();
    const rx = W * 0.72, ry = 120 + drift * 6;
    ctx.save();
    ctx.globalCompositeOperation = 'destination-in';
    ctx.scale(1, ry / rx);
    const fade = ctx.createRadialGradient(0, 0, 0, 0, 0, rx);
    fade.addColorStop(0, 'rgba(255,255,255,1)');
    fade.addColorStop(0.45, 'rgba(255,255,255,1)');
    fade.addColorStop(1, 'rgba(255,255,255,0)');
    ctx.fillStyle = fade;
    ctx.fillRect(0, 0, W, (H * rx) / ry);
    ctx.restore();
    tracePath(ctx, RIM_PATH);
    ctx.lineWidth = 1;
    ctx.strokeStyle = 'rgba(255,255,255,' + ((46 * e1) / 255).toFixed(3) + ')';
    ctx.stroke();
  }

  // Header status glyph (D5): 36 px ring, per-state mark, specular glint.
  function drawGlyph() {
    const ctx = ctxFor(glyphCv, 40, 40);
    const c = colNow, st = snap ? snap.StateName : 'down';
    const x0 = 2, y0 = 2, cx = 20, cy = 20, R = 18;
    const rad = (deg) => (deg * Math.PI) / 180;

    ctx.lineWidth = 3.6;
    ctx.strokeStyle = rgba(c, 64 / 255);
    ctx.beginPath(); ctx.arc(cx, cy, R, 0, Math.PI * 2); ctx.stroke();

    ctx.strokeStyle = rgba(c, 1);
    if (st === 'syncing') {
      const sweepDeg = 360 * Math.max(0.04, pct.Value);
      ctx.lineCap = 'round';
      ctx.beginPath(); ctx.arc(cx, cy, R, rad(-90), rad(-90 + sweepDeg)); ctx.stroke();
    } else if (st === 'scanning') {
      const start = spin.Phase * 360;
      ctx.lineCap = 'round';
      ctx.beginPath(); ctx.arc(cx, cy, R, rad(start), rad(start + 96)); ctx.stroke();
    } else {
      ctx.lineCap = 'butt';
      ctx.beginPath(); ctx.arc(cx, cy, R, 0, Math.PI * 2); ctx.stroke();
      ctx.lineWidth = 3.2;
      ctx.lineCap = 'round';
      const line = (ax, ay, bx, by) => { ctx.beginPath(); ctx.moveTo(x0 + ax, y0 + ay); ctx.lineTo(x0 + bx, y0 + by); ctx.stroke(); };
      if (st === 'insync') {
        ctx.lineJoin = 'round';
        ctx.beginPath(); ctx.moveTo(x0 + 9, y0 + 18); ctx.lineTo(x0 + 15, y0 + 24); ctx.lineTo(x0 + 26, y0 + 11); ctx.stroke();
      } else if (st === 'nopeer' || st === 'error' || st === 'unauthorized') {
        line(11, 11, 25, 25);
        line(25, 11, 11, 25);
      } else if (st === 'paused') {
        line(14, 12, 14, 24);
        line(22, 12, 22, 24);
      } else {
        ctx.fillStyle = rgba(c, 1);
        ctx.beginPath(); ctx.arc(x0 + 18, y0 + 18, 4, 0, Math.PI * 2); ctx.fill();
      }
    }

    // specular glint: one consistent light source, upper-left
    ctx.lineWidth = 1.6;
    ctx.lineCap = 'round';
    ctx.strokeStyle = 'rgba(255,255,255,' + (125 / 255).toFixed(3) + ')';
    ctx.beginPath(); ctx.arc(cx, cy, R - 1.5, rad(-142), rad(-96)); ctx.stroke();
  }

  function errorCount(s) {
    return (s && s.Folders ? s.Folders : []).reduce((n, f) => n + (f.Errors || 0), 0);
  }

  // Sync Progress bar (D7): spring value, shimmer while moving, top highlight.
  function drawBar() {
    const ctx = ctxFor(barCv, BAR_W, BAR_H);
    const st = snap ? snap.StateName : 'down';
    tracePath(ctx, squircle(0, 0, BAR_W, BAR_H, 5));
    ctx.fillStyle = 'rgba(10,12,18,' + (150 / 255).toFixed(3) + ')';
    ctx.fill();

    // The bar reports completeness, so it never borrows the connection's red.
    const barCol = st === 'syncing' ? COL.accent
      : st === 'scanning' ? COL.scanning
      : (st === 'paused' || offline(snap)) ? COL.barIdle
      : errorCount(snap) > 0 ? COL.error
      : COL.insync;

    const pv = Math.max(0, Math.min(1, pct.Value));
    const fw = BAR_W * pv;
    if (fw <= 8) return;
    const fill = squircle(0, 0, fw, BAR_H, 5);
    tracePath(ctx, fill);
    ctx.fillStyle = rgba(barCol, 1);
    ctx.fill();

    if ((st === 'syncing' || st === 'scanning') && !reduced()) {
      ctx.save();
      tracePath(ctx, fill);
      ctx.clip();
      const sx = -70 + shimmer.Phase * (fw + 140);
      const g = ctx.createLinearGradient(sx - 35, 0, sx + 35, 0);
      g.addColorStop(0, 'rgba(255,255,255,0)');
      g.addColorStop(0.5, 'rgba(255,255,255,' + (150 / 255).toFixed(3) + ')');
      g.addColorStop(1, 'rgba(255,255,255,0)');
      ctx.fillStyle = g;
      ctx.fillRect(sx - 35, 0, 70, BAR_H);
      ctx.restore();
    }

    ctx.lineWidth = 1;
    ctx.strokeStyle = 'rgba(255,255,255,' + (95 / 255).toFixed(3) + ')';
    ctx.beginPath(); ctx.moveTo(3, 1.5); ctx.lineTo(fw - 3, 1.5); ctx.stroke();
  }

  // Per-frame DOM values: meters, presence pulse, activity slide-in, gel press.
  function drawLive() {
    const s = snap;
    const inR = s ? s.InRate || 0 : 0, outR = s ? s.OutRate || 0 : 0;
    meter($('meter-dn'), dn.Value, inR > 1024 ? COL.accent : COL.faint);
    meter($('meter-up'), up.Value, outR > 1024 ? COL.insync : COL.faint);

    const on = peersUp() > 0;
    const pulse = on && !reduced() ? 0.55 + 0.45 * breathe.Sin01 : 1;
    $('presence-halo').style.opacity = ((70 / 255) * pulse).toFixed(3);

    const rows = $('activity').children;
    for (let i = 0; i < rows.length; i++) {
      const age = ages.has(rows[i].dataset.key) ? ages.get(rows[i].dataset.key) : 1;
      const ag = Ease.OutCubic(age);
      rows[i].style.opacity = ag.toFixed(3);
      rows[i].style.transform = 'translateX(' + ((1 - ag) * 10).toFixed(2) + 'px)';
    }

    presses.forEach((sp, btn) => {
      btn.style.transform = sp.Value > 0.0005 ? 'scale(' + (1 - sp.Value * 0.05).toFixed(4) + ')' : '';
    });
  }

  function meter(node, norm, c) {
    node.style.width = norm > 0.01 ? Math.max(4, 160 * norm).toFixed(1) + 'px' : '0px';
    node.style.backgroundColor = rgba(c, 1);
  }

  function setSpring(sp, v) {
    if (reduced()) sp.Snap(v); else sp.Set(v);
  }

  // ---------- snapshot → DOM ----------
  const HEADLINES = {
    insync: 'In sync', syncing: 'Syncing', scanning: 'Checking for changes', nopeer: 'Disconnected',
    error: 'Needs attention', paused: 'Paused', down: 'Syncthing not running', unauthorized: 'API key rejected'
  };
  function sublineFor(s) {
    switch (s.StateName) {
      case 'insync': return 'All devices hold the same files.';
      case 'syncing': return (s.Pct || 0) + '% complete' + (s.ETA ? '  -  about ' + s.ETA + ' left' : '');
      case 'scanning': return 'Routine verification - nothing is wrong.';
      case 'nopeer': return 'No paired device is reachable right now.';
      case 'error': return errorCount(s) + ' item(s) could not be synced.';
      case 'paused': return 'Syncing is paused.';
      case 'unauthorized': return s.Detail || 'Syncthing rejected the API key.';
      default: return 'Start Syncthing to resume syncing.';
    }
  }

  function apply(s) {
    const prev = snap;
    snap = s;
    const st = s.StateName || 'down';
    colTarget = stateColor(st);
    if (!prev || reduced()) colNow = colTarget;

    const p = (s.Pct || 0) / 100;
    if (!prev) pct.Snap(p); else setSpring(pct, p);
    setSpring(dn, Math.min(1, (s.InRate || 0) / METER_FULL));
    setSpring(up, Math.min(1, (s.OutRate || 0) / METER_FULL));

    setText('headline', s.Headline || HEADLINES[st] || HEADLINES.down);
    setText('subline', s.Subline || sublineFor(s));
    renderConnection(s);
    renderSync(s);
    renderTransfer(s);
    renderActivity(s, !prev);
    renderButtons();
    renderNotice();
    const pend = (s.Pending || []).length + (s.PendingFolders || []).length;
    const badge = $('pair-badge');
    badge.hidden = pend === 0;
    badge.textContent = String(pend);
    if (view === 'pair') renderPair();
    if (view === 'settings') renderStartup();
    if (view === 'welcome') renderWelcome();

    if (prev && prev.StateName !== st && !reduced() && document.visibilityState === 'visible') {
      sweep.Start(0.62, Ease.OutCubic);
    }
    ensureAnimating();
  }

  function renderConnection(s) {
    const peers = (s.Peers || []).filter((p) => p.Connected);
    const on = peers.length > 0;
    const first = peers[0];
    const nameEl = $('peer-name');
    setText('peer-name', on ? (first.Name || shortID(first.ID)) : 'No device connected');
    nameEl.title = peers.map((p) => (p.Name || shortID(p.ID)) + ' (' + (p.Transport || 'direct') + ')').join('\n');
    setText('peer-more', peers.length > 1 ? '+' + (peers.length - 1) + ' more' : '');
    setText('peer-via', on ? 'Connected via ' + (first.Transport || 'direct') : 'Waiting for a device');
    setText('peer-addr', on ? (stripScheme(first.Addr) || 'address unknown') : 'not connected');
    const pill = $('peer-pill');
    pill.textContent = on ? 'ONLINE' : 'OFFLINE';
    pill.classList.toggle('on', on);
    const c = rgba(on ? COL.insync : COL.error, 1);
    $('presence-dot').style.backgroundColor = c;
    $('presence-halo').style.backgroundColor = c;
  }

  // offline reports whether s holds no sync progress to show: Syncthing is
  // down or refused us, or the error came before any folder was read.
  function offline(s) {
    const st = s ? s.StateName : 'down';
    return st === 'down' || st === 'unauthorized' || (st === 'error' && !(s.Folders || []).length);
  }

  function renderSync(s) {
    const st = s.StateName;
    setText('sync-big', st === 'insync' ? 'Up to date' : s.Connecting ? '-' : offline(s) ? 'Offline' : (s.Pct || 0) + '%');
    setText('files-line', s.FilesLine || '');
    setText('size-line', s.SizeLine || '');
    setText('need-line', s.NeedItems > 0 ? grp(s.NeedItems) + ' items pending' : 'nothing queued');
    barCv.setAttribute('aria-valuenow', String(s.Pct || 0));
  }

  function renderTransfer(s) {
    const inR = s.InRate || 0, outR = s.OutRate || 0;
    setText('rate-dn', fmtRate(inR));
    setText('rate-up', fmtRate(outR));
    $('rate-dn').style.color = rgba(inR > 1024 ? COL.accent : COL.faint, 1);
    $('rate-up').style.color = rgba(outR > 1024 ? COL.insync : COL.faint, 1);
    setText('xfer-state', inR > 1024 || outR > 1024 ? 'active' : 'idle');
    const su = s.Startup || {};
    $('sd-st').parentNode.classList.toggle('on', !!su.Syncthing);
    $('sd-tray').parentNode.classList.toggle('on', !!su.Tray);
  }

  function actKey(a) { return String(a.At) + '|' + a.Text; }

  // Recent Activity (D9): newest 4 of up to 40; new rows slide in 10 px.
  function renderActivity(s, initial) {
    const all = s.Activity || [];
    const items = all.slice(0, 4);
    const list = $('activity');
    const keys = items.map(actKey);
    const current = Array.prototype.map.call(list.children, (li) => li.dataset.key);
    const known = new Set(all.map(actKey));
    ages.forEach((_, k) => { if (!known.has(k)) ages.delete(k); });
    items.forEach((a, i) => {
      if (!ages.has(keys[i])) ages.set(keys[i], initial || reduced() ? 1 : 0);
    });
    $('act-empty').hidden = items.length > 0;
    if (keys.join('\n') === current.join('\n')) return fitActivity();
    clear(list);
    items.forEach((a, i) => {
      const li = el('li', 'act-item');
      li.dataset.key = keys[i];
      const tint = ['green', 'red', 'blue', 'amber', 'grey'].indexOf(a.Tint) >= 0 ? a.Tint : 'grey';
      li.appendChild(el('span', 'act-halo tint-' + tint));
      li.appendChild(el('span', 'act-dot tint-' + tint));
      li.appendChild(el('span', 'act-time mono', fmtTime(a.At)));
      let t = String(a.Text || '');
      if (t.length > 44) t = t.substring(0, 41) + '...';
      const tx = el('span', 'act-text', t);
      if (t !== a.Text) tx.title = a.Text;
      li.appendChild(tx);
      list.appendChild(li);
    });
    fitActivity();
  }

  // Show only whole rows: a notice banner shrinks the card, and a row cut in
  // half reads as a rendering fault. While a notice is up the status view
  // tightens step by step (compact-1..4 in app.css) until at least two rows
  // fit, so Recent Activity (D9) stays visible in the Down and Unauthorized
  // states too. With no room for a row the card keeps its place (so the
  // buttons stay put) but is not drawn.
  const MAX_COMPACT = 4;
  function fitActivity() {
    const status = views.status;
    if (status.hidden) return; // nothing to measure; setView refits on return
    const card = $('activity').parentNode;
    const rows = $('activity').children;
    const noticeOn = !$('notice').hidden;
    const want = Math.max(1, Math.min(2, rows.length));
    let level = 0, fit = 0;
    for (;;) {
      for (let i = 1; i <= MAX_COMPACT; i++) status.classList.toggle('compact-' + i, i <= level);
      // Rows start 34 px down with a 24 px pitch; a row's text needs 18 px.
      fit = Math.max(0, Math.floor((card.clientHeight - 34 - 18) / 24) + 1);
      if (!noticeOn || fit >= want || level >= MAX_COMPACT) break;
      level++;
    }
    for (let i = 0; i < rows.length; i++) rows[i].hidden = i >= fit;
    card.classList.toggle('cramped', fit < 1);
  }

  // ---------- status buttons (D10) ----------
  const presses = new Map();
  document.querySelectorAll('.btn').forEach((btn) => {
    const sp = new A.Spring(0, 420, 30);
    presses.set(btn, sp);
    const release = () => {
      btn.classList.remove('pressed');
      if (reduced()) sp.Snap(0); else sp.Set(0);
      ensureAnimating();
    };
    btn.addEventListener('pointerdown', (ev) => {
      if (btn.disabled || ev.button !== 0) return;
      btn.classList.add('pressed');
      if (reduced()) sp.Snap(1); else sp.Set(1);
      ensureAnimating();
    });
    btn.addEventListener('pointerup', release);
    btn.addEventListener('pointercancel', release);
    btn.addEventListener('pointerleave', release);
    // A native click fires only when the press is released inside the button.
    btn.addEventListener('click', () => onCommand(btn.dataset.cmd, btn));
  });

  function renderButtons() {
    const st = snap ? snap.StateName : 'down';
    const off = st === 'down' || st === 'unauthorized';
    document.querySelectorAll('.btn').forEach((btn) => {
      const cmd = btn.dataset.cmd;
      if (cmd === 'pause') btn.textContent = st === 'paused' ? 'Resume' : 'Pause';
      const gated = cmd === 'rescan' || cmd === 'pause' || cmd === 'restart';
      btn.disabled = busyBtns.has(btn) || (gated && off);
    });
  }

  function onCommand(cmd, btn) {
    const folders = snap && snap.Folders ? snap.Folders : [];
    switch (cmd) {
      case 'rescan': return runBtn(btn, 'rescan');
      case 'pause': return runBtn(btn, snap && snap.StateName === 'paused' ? 'resume' : 'pause');
      case 'folder':
        if (folders.length > 1) return toggleFolderMenu();
        return runBtn(btn, 'open-folder', folders.length ? { id: folders[0].ID } : {});
      case 'web': return runBtn(btn, 'open-webui');
      case 'restart': return runBtn(btn, 'restart');
    }
  }

  function runBtn(btn, name, args) {
    busyBtns.add(btn);
    renderButtons();
    return act(name, args)
      .catch((e) => toast(e.message, true))
      .then(() => { busyBtns.delete(btn); renderButtons(); });
  }

  // Folder list popover for several folders.
  const folderMenu = $('folder-menu');
  function toggleFolderMenu() {
    if (!folderMenu.hidden) return closeFolderMenu();
    clear(folderMenu);
    (snap && snap.Folders ? snap.Folders : []).forEach((f) => {
      const b = el('button', 'menu-item');
      b.type = 'button';
      b.setAttribute('role', 'menuitem');
      b.appendChild(el('span', 'mi-label', f.Label || f.ID));
      if (f.Path) b.appendChild(el('span', 'mi-path', f.Path));
      b.addEventListener('click', () => {
        closeFolderMenu();
        act('open-folder', { id: f.ID }).catch((e) => toast(e.message, true));
      });
      folderMenu.appendChild(b);
    });
    folderMenu.hidden = false;
    $('btn-folder').setAttribute('aria-expanded', 'true');
    const firstItem = folderMenu.querySelector('button');
    if (firstItem) firstItem.focus();
  }
  function closeFolderMenu() {
    folderMenu.hidden = true;
    $('btn-folder').setAttribute('aria-expanded', 'false');
  }
  document.addEventListener('pointerdown', (ev) => {
    if (!folderMenu.hidden && !folderMenu.contains(ev.target) && ev.target !== $('btn-folder')) closeFolderMenu();
  });

  // ---------- actions ----------
  function act(name, args) {
    return fetch('/api/action', {
      method: 'POST',
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { 'Content-Type': 'application/json', 'X-STV2': '1' },
      body: JSON.stringify({ name: name, args: args === undefined ? null : args })
    }).then((r) => r.json().catch(() => ({ ok: false, error: 'HTTP ' + r.status })).then((j) => {
      if (r.status === 403) { showExpired(); throw new Error('Session ended'); }
      if (!r.ok || !j || !j.ok) {
        const err = new Error((j && j.error) || 'HTTP ' + r.status);
        if (j && typeof j.docs === 'string') err.docs = j.docs; // a page for open-docs
        throw err;
      }
      return j.result;
    }));
  }

  let toastTimer = 0;
  function toast(msg, isError) {
    const t = $('toast');
    t.textContent = msg;
    t.classList.toggle('error', !!isError);
    t.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove('show'), isError ? 4200 : 2600);
  }

  // A help link to one of the fixed documentation pages (ui.DocsURL). The
  // backend opens it in the default browser, so the dashboard never
  // navigates away.
  function docsLink(page, label) {
    const b = el('button', 'link-btn', label);
    b.type = 'button';
    b.addEventListener('click', () => act('open-docs', { page: page }).catch((e) => toast(e.message, true)));
    return b;
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).catch(() => copyFallback(text));
    }
    return copyFallback(text);
  }
  function copyFallback(text) {
    return new Promise((resolve, reject) => {
      const ta = el('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.left = '-9999px';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      if (ok) resolve(); else reject(new Error('Copy failed'));
    });
  }

  function copyDiagnostics() {
    return act('copy-diagnostics')
      .then((r) => copyText(typeof r === 'string' ? r : (r && r.text) || ''))
      .then(() => toast('Diagnostics copied'))
      .catch((e) => toast(e.message, true));
  }

  // ---------- notices (one banner at a time, with a pager) ----------
  const handled = new Set(); // banners answered in this page session
  let noticeIdx = 0;

  function peerName(id) {
    const p = (snap && snap.Peers ? snap.Peers : []).find((x) => x.ID === id);
    return p && p.Name ? p.Name : shortID(id);
  }

  function pendingDeviceNotice(pd) {
    const node = pd.Node || null;
    const host = (node && node.NodeName) || pd.Name || shortID(pd.DeviceID);
    const meta = [node && node.OS ? osLabel(node.OS) : '', 'ID ' + shortID(pd.DeviceID)].filter(Boolean).join(' · ');
    const other = !!(node && !node.SameOwner);
    return {
      key: 'dev:' + pd.DeviceID,
      kind: other ? 'warn' : 'info',
      title: [{ b: true, t: host }, { t: ' wants to sync with this computer' }],
      body: meta,
      warn: other ? 'Owned by ' + (node.LoginName || 'another Tailscale user') + ' — only accept if you know this person' : '',
      actions: [
        { label: 'Accept', primary: true, run: () => act('accept-device', { deviceID: pd.DeviceID }) },
        { label: 'Decline', run: () => act('decline-device', { deviceID: pd.DeviceID }) }
      ]
    };
  }

  function pendingFolderNotice(pf) {
    const base = { folderID: pf.FolderID, fromDevice: pf.FromDevice };
    return {
      key: 'fold:' + pf.FolderID + ':' + pf.FromDevice,
      kind: 'info',
      title: [{ b: true, t: peerName(pf.FromDevice) }, { t: ' shares "' + (pf.Label || pf.FolderID) + '"' }],
      actions: [
        { label: 'Accept', primary: true, run: () => act('accept-folder', base) },
        {
          label: 'Choose location…',
          run: () => chooseFolder('Where should "' + (pf.Label || pf.FolderID) + '" be saved?').then((path) => {
            if (!path) throw new Error('No folder chosen');
            return act('accept-folder', Object.assign({ path: path }, base));
          })
        },
        { label: 'Decline', run: () => act('decline-folder', base) }
      ]
    };
  }

  function buildNotices(s) {
    const out = [];
    const st = s.StateName;
    if (st === 'down' && !s.Connecting) {
      out.push({
        key: 'state:down', kind: 'warn', title: [{ t: 'Syncthing is not running' }], body: s.Detail || '',
        actions: [{ label: 'Start Syncthing', primary: true, run: () => act('start-syncthing') }]
      });
    } else if (st === 'unauthorized') {
      out.push({
        key: 'state:unauth', kind: 'warn', title: [{ t: 'Syncthing rejected the API key' }],
        body: 'Run "stv2 doctor" to find the cause.',
        actions: [{ label: 'Copy diagnostics', run: copyDiagnostics, keep: true }]
      });
    }
    (s.Pending || []).forEach((pd) => out.push(pendingDeviceNotice(pd)));
    (s.PendingFolders || []).forEach((pf) => out.push(pendingFolderNotice(pf)));
    (s.Notices || []).forEach((id) => {
      if (id === 'gui-exposed') {
        out.push({
          key: 'n:gui-exposed', kind: 'warn',
          title: [{ t: 'Your Syncthing control panel is reachable from other devices without a password. Restrict it to this computer?' }],
          actions: [
            { label: 'Restrict', primary: true, run: () => act('fix-gui-exposure') },
            { label: 'Not now', run: () => act('dismiss-notice', { id: id, forever: false }) },
            { label: "Don't ask again", run: () => act('dismiss-notice', { id: id, forever: true }) }
          ]
        });
      } else if (id === 'legacy-tray') {
        out.push({
          key: 'n:legacy-tray', kind: 'amber',
          title: [{ t: 'An older Syncthing tray app is running. Replace it with SyncThing V2?' }],
          actions: [
            { label: 'Replace', primary: true, run: () => act('migrate-legacy', { yes: true }) },
            { label: 'No', run: () => act('migrate-legacy', { yes: false }) }
          ]
        });
      } else if (id === 'tailscale-missing') {
        const acts = [];
        if (info.os === 'windows') acts.push({ label: 'Install with winget', primary: true, run: () => act('install-tailscale') });
        acts.push({ label: 'Not now', run: () => act('dismiss-notice', { id: id, forever: false }) });
        out.push({
          key: 'n:tailscale-missing', kind: 'amber',
          title: [{ t: 'Tailscale is not installed' }],
          body: 'SyncThing V2 pairs your computers over Tailscale. Install it and sign in on each computer.',
          actions: acts
        });
      }
    });
    if (s.UpdateAvailable) {
      out.push({
        key: 'upd:' + s.UpdateAvailable, kind: 'info',
        title: [{ t: info.name + ' ' + s.UpdateAvailable + ' is available' }],
        actions: [
          { label: 'View release', primary: true, run: () => act('open-update') },
          { label: 'Not now', run: () => act('dismiss-notice', { id: 'update', forever: false }) }
        ]
      });
    }
    return out.filter((n) => !handled.has(n.key));
  }

  let noticeSig = '';
  function renderNotice() {
    const box = $('notice');
    const list = snap ? buildNotices(snap) : [];
    if (!list.length) { box.hidden = true; noticeSig = ''; fitActivity(); return; }
    if (noticeIdx >= list.length) noticeIdx = 0;
    const n = list[noticeIdx];
    const sig = list.length + '|' + noticeIdx + '|' + n.key + '|' + (n.body || '') + '|' + (n.warn || '');
    if (sig === noticeSig && !box.hidden) return;
    noticeSig = sig;
    box.className = 'notice kind-' + n.kind;
    const title = $('notice-title');
    clear(title);
    segs(title, n.title);
    title.title = n.title.map((p) => p.t).join(''); // full text when compact-3 clamps it
    const body = $('notice-body');
    body.hidden = !n.body;
    body.textContent = n.body || '';
    body.title = n.body || '';
    const warn = $('notice-warn');
    warn.hidden = !n.warn;
    warn.textContent = n.warn || '';
    const next = $('notice-next');
    next.hidden = list.length < 2;
    next.textContent = (noticeIdx + 1) + '/' + list.length + ' ›';
    const actions = $('notice-actions');
    clear(actions);
    n.actions.forEach((a) => {
      const b = el('button', 'small-btn' + (a.primary ? ' primary' : ''), a.label);
      b.type = 'button';
      b.addEventListener('click', () => runNoticeAction(n, a, actions));
      actions.appendChild(b);
    });
    box.hidden = false;
    fitActivity();
  }

  function runNoticeAction(n, a, container) {
    const btns = container.querySelectorAll('button');
    btns.forEach((b) => { b.disabled = true; });
    Promise.resolve()
      .then(a.run)
      .then(() => {
        if (!a.keep) handled.add(n.key);
        noticeSig = '';
        renderNotice();
        if (view === 'pair') renderPair();
      })
      .catch((e) => {
        toast(e.message, true);
        btns.forEach((b) => { b.disabled = false; });
      });
  }

  $('notice-next').addEventListener('click', () => { noticeIdx++; noticeSig = ''; renderNotice(); });

  // ---------- Pair view ----------
  let cands = null, discovering = false, discoverErr = '', discoverDocs = '', pairedOnce = false;
  const share = { open: false, folderID: '', newPath: '', devices: new Set() };

  function discover() {
    if (discovering) return;
    discovering = true;
    discoverErr = '';
    discoverDocs = '';
    renderPair();
    act('discover')
      .then((r) => { cands = Array.isArray(r) ? r : (r && Array.isArray(r.Candidates) ? r.Candidates : []); })
      .catch((e) => { discoverErr = e.message; discoverDocs = e.docs || ''; if (!cands) cands = []; })
      .then(() => { discovering = false; renderPair(); });
  }

  function candidateItem(c) {
    const item = el('div', 'item');
    const main = el('div', 'item-main');
    const name = el('p', 'item-name', c.NodeName || c.DNSName || String(c.IP || ''));
    if (!c.SameOwner) name.appendChild(el('span', 'badge', 'OTHER OWNER'));
    main.appendChild(name);
    const meta = [osLabel(c.OS), c.IP ? String(c.IP) : '', c.DeviceID ? 'ID ' + shortID(c.DeviceID) : ''].filter(Boolean).join(' · ');
    main.appendChild(el('p', 'item-meta', meta));
    if (!c.SameOwner && c.Status === 'ready') {
      main.appendChild(el('p', 'item-warn', 'Owned by ' + (c.LoginName || 'another Tailscale user') + ' — only pair if you know this person'));
    }
    if (c.Status === 'blocked') {
      const note = el('p', 'item-note', 'Syncthing not reachable — is SyncThing V2 running there? Check ACLs/firewall ');
      note.appendChild(docsLink('blocked', 'Help'));
      main.appendChild(note);
    } else if (c.Status === 'no-syncthing') {
      main.appendChild(el('p', 'item-note', 'Install SyncThing V2 on this device (or it uses a non-default port)'));
    }
    item.appendChild(main);
    const side = el('div', 'item-side');
    if (c.Status === 'ready') {
      const b = el('button', 'small-btn primary', 'Pair');
      b.type = 'button';
      b.setAttribute('aria-label', 'Pair with ' + (c.NodeName || 'this device'));
      b.addEventListener('click', () => {
        b.disabled = true;
        act('pair', { deviceID: c.DeviceID })
          .then(() => {
            c.Status = 'paired';
            toast('Pairing started. Accept it on ' + (c.NodeName || 'the other device') + '.');
            if (!pairedOnce) { pairedOnce = true; openShare(); } else renderPair();
          })
          .catch((e) => { b.disabled = false; toast(e.message, true); });
      });
      side.appendChild(b);
    } else if (c.Status === 'paired') {
      side.appendChild(el('span', 'item-state', 'Paired'));
    }
    item.appendChild(side);
    return item;
  }

  function requestItem(n) {
    const item = el('div', 'item item-col');
    const title = el('p', 'item-note');
    segs(title, n.title);
    item.appendChild(title);
    if (n.body) item.appendChild(el('p', 'item-meta', n.body));
    if (n.warn) item.appendChild(el('p', 'item-warn', n.warn));
    const acts = el('div', 'flow-actions');
    n.actions.forEach((a) => {
      const b = el('button', 'small-btn' + (a.primary ? ' primary' : ''), a.label);
      b.type = 'button';
      b.addEventListener('click', () => runNoticeAction(n, a, acts));
      acts.appendChild(b);
    });
    item.appendChild(acts);
    return item;
  }

  let requestsSig = '';
  function renderPair() {
    const s = snap || {};
    const tsMissing = (s.Notices || []).indexOf('tailscale-missing') >= 0;
    $('ts-card').hidden = !tsMissing;
    $('ts-install').hidden = info.os !== 'windows';

    const msg = $('pair-msg');
    const text = discovering ? 'Looking for your devices on the tailnet…' : discoverErr;
    const docs = discovering ? '' : discoverDocs;
    msg.hidden = !text;
    const msgSig = text + '|' + docs;
    if (msg.dataset.sig !== msgSig) { // rebuilt only on change, so the link keeps focus
      msg.dataset.sig = msgSig;
      clear(msg);
      msg.appendChild(document.createTextNode(text));
      if (docs) {
        msg.appendChild(document.createTextNode(' '));
        msg.appendChild(docsLink(docs, 'How to fix this'));
      }
    }
    $('pair-refresh').disabled = discovering;

    const reqs = (s.Pending || []).map(pendingDeviceNotice)
      .concat((s.PendingFolders || []).map(pendingFolderNotice))
      .filter((n) => !handled.has(n.key));
    const sig = reqs.map((n) => n.key).join(',');
    if (sig !== requestsSig) {
      requestsSig = sig;
      const box = $('list-requests');
      clear(box);
      reqs.forEach((n) => box.appendChild(requestItem(n)));
    }
    $('sec-requests').hidden = reqs.length === 0;

    $('sec-share').hidden = !share.open;
    $('pair-lists').hidden = share.open;
    if (share.open) { renderShare(); return; }

    const list = (cands || []).filter((c) => c.Status !== 'self');
    fillList($('list-own'), list.filter((c) => c.SameOwner),
      cands === null ? '' : 'No other computers of yours are online in Tailscale.');
    fillList($('list-other'), list.filter((c) => !c.SameOwner), cands === null ? '' : 'None found.');
  }

  function fillList(box, items, emptyText) {
    clear(box);
    if (!items.length) {
      box.appendChild(el('p', 'empty-note', emptyText || 'Searching…'));
      return;
    }
    items.forEach((c) => box.appendChild(candidateItem(c)));
  }

  function openShare() {
    share.open = true;
    share.folderID = '';
    share.newPath = '';
    share.devices = new Set();
    shareSig = '';
    renderPair();
  }

  let shareSig = '';
  function renderShare() {
    const s = snap || {};
    const folders = s.Folders || [];
    const peers = s.Peers || [];
    const sig = folders.map((f) => f.ID + '=' + f.Label).join(',') + '|' + peers.map((p) => p.ID + '=' + p.Name).join(',') +
      '|' + share.folderID + '|' + share.newPath;
    if (sig !== shareSig) {
      shareSig = sig;
      const fbox = $('share-folders');
      clear(fbox);
      folders.forEach((f) => fbox.appendChild(choice('radio', 'share-folder', f.Label || f.ID, f.Path, share.folderID === f.ID, () => {
        share.folderID = f.ID;
        share.newPath = '';
        shareSig = '';
        renderShare();
      })));
      fbox.appendChild(choice('radio', 'share-folder', 'New folder…', share.newPath || 'Choose a location', !!share.newPath, () => {
        chooseFolder('Choose a folder to share').then((path) => {
          if (path) { share.newPath = path; share.folderID = ''; }
        }).catch((e) => toast(e.message, true)).then(() => { shareSig = ''; renderShare(); });
      }));
      const dbox = $('share-devices');
      clear(dbox);
      if (!peers.length) dbox.appendChild(el('p', 'empty-note', 'Pair a device first.'));
      peers.forEach((p) => dbox.appendChild(choice('checkbox', 'share-dev', p.Name || shortID(p.ID), 'ID ' + shortID(p.ID), share.devices.has(p.ID), (ev) => {
        if (ev.target.checked) share.devices.add(p.ID); else share.devices.delete(p.ID);
        $('share-go').disabled = !shareReady();
      })));
    }
    $('share-go').disabled = !shareReady();
  }

  function shareReady() { return (share.folderID || share.newPath) && share.devices.size > 0; }

  function choice(type, name, label, sub, checked, onChange) {
    const lab = el('label', 'choice');
    const input = el('input');
    input.type = type;
    input.name = name;
    input.checked = checked;
    input.addEventListener('change', onChange);
    lab.appendChild(input);
    const txt = el('span', 'choice-text', label);
    if (sub) txt.appendChild(el('span', 'choice-sub', sub));
    lab.appendChild(txt);
    return lab;
  }

  // ---------- folder choice: native picker, or a typed path without one ----------
  let pathResolve = null;

  // chooseFolder resolves to the chosen absolute path, or '' if cancelled.
  // Where no native picker is installed (Linux without zenity or kdialog)
  // it asks for a typed path instead.
  function chooseFolder(title) {
    return act('pick-folder').then((r) => {
      if (r && r.unavailable) return askPath(title);
      return typeof r === 'string' ? r : (r && r.path) || '';
    });
  }

  function askPath(title) {
    closePathEntry('');
    return new Promise((resolve) => {
      pathResolve = resolve;
      $('path-entry-title').textContent = title;
      $('path-entry-input').value = '';
      $('path-entry').hidden = false;
      $('path-entry-input').focus();
    });
  }

  function closePathEntry(value) {
    $('path-entry').hidden = true;
    const resolve = pathResolve;
    pathResolve = null;
    if (resolve) resolve(value);
  }

  function submitPathEntry() {
    const v = $('path-entry-input').value.trim();
    if (!v) { $('path-entry-input').focus(); return; }
    closePathEntry(v);
  }

  $('path-entry-ok').addEventListener('click', submitPathEntry);
  $('path-entry-cancel').addEventListener('click', () => closePathEntry(''));
  $('path-entry-input').addEventListener('keydown', (ev) => {
    if (ev.key === 'Enter') { ev.preventDefault(); submitPathEntry(); }
  });

  $('pair-refresh').addEventListener('click', discover);
  $('share-open').addEventListener('click', openShare);
  $('share-cancel').addEventListener('click', () => { share.open = false; renderPair(); });
  $('ts-install').addEventListener('click', () => act('install-tailscale').catch((e) => toast(e.message, true)));
  $('share-go').addEventListener('click', () => {
    if (!shareReady()) return;
    const args = { deviceIDs: Array.from(share.devices) };
    if (share.folderID) args.folderID = share.folderID; else args.path = share.newPath;
    $('share-go').disabled = true;
    act('share-folder', args)
      .then(() => { toast('Folder shared. Accept it on the other device.'); share.open = false; renderPair(); })
      .catch((e) => { toast(e.message, true); $('share-go').disabled = !shareReady(); });
  });

  // ---------- Settings / About ----------
  let settings = null;
  const holds = {}; // optimistic switch values until the snapshot catches up

  const PROFILE_TEXT = {
    tailnet: 'Tailnet only: global discovery, relays and NAT traversal are off. Sync needs Tailscale up, or both computers on the same local network.',
    hybrid: 'Hybrid: Syncthing\'s defaults. Devices can also find each other through global discovery and relays when Tailscale is down.'
  };

  function setSwitch(id, on, disabled) {
    const sw = $(id);
    sw.setAttribute('aria-checked', on ? 'true' : 'false');
    if (disabled !== undefined) sw.disabled = disabled;
  }
  function held(key, actual) {
    const h = holds[key];
    if (h && h.until > Date.now()) return h.value;
    delete holds[key];
    return actual;
  }

  function renderStartup() {
    const su = (snap && snap.Startup) || {};
    const foreign = !!su.Syncthing && !su.SyncthingManagedByUs;
    setSwitch('as-tray', held('tray', !!su.Tray), false);
    setSwitch('wel-as-tray', held('tray', !!su.Tray), false);
    setSwitch('as-st', held('syncthing', !!su.Syncthing), foreign);
    $('as-st-note').hidden = !foreign;
  }

  function renderSettings() {
    const s = settings || {};
    setSwitch('pref-notif', !!s.notifications, false);
    setSwitch('pref-upd', !!s.checkForUpdates, false);
    ['tailnet', 'hybrid'].forEach((p) => $('prof-' + p).setAttribute('aria-checked', s.profile === p ? 'true' : 'false'));
    $('prof-text').textContent = PROFILE_TEXT[s.profile] || (PROFILE_TEXT.tailnet + ' ' + PROFILE_TEXT.hybrid);
  }

  function openSettings() {
    renderStartup();
    act('get-settings')
      .then((r) => { settings = r || {}; renderSettings(); })
      .catch((e) => toast(e.message, true));
  }

  function toggleAutostart(target, id) {
    const sw = $(id);
    const on = sw.getAttribute('aria-checked') !== 'true';
    holds[target] = { value: on, until: Date.now() + 5000 };
    setSwitch(id, on);
    sw.disabled = true;
    act('set-autostart', { target: target, on: on })
      .catch((e) => { delete holds[target]; toast(e.message, true); })
      .then(() => { sw.disabled = false; renderStartup(); });
  }
  $('as-tray').addEventListener('click', () => toggleAutostart('tray', 'as-tray'));
  $('as-st').addEventListener('click', () => toggleAutostart('syncthing', 'as-st'));

  function togglePref(key, id) {
    const sw = $(id);
    const on = sw.getAttribute('aria-checked') !== 'true';
    setSwitch(id, on);
    sw.disabled = true;
    act('set-pref', { key: key, on: on })
      .then((r) => { if (r && typeof r === 'object') settings = r; else (settings = settings || {})[key] = on; })
      .catch((e) => toast(e.message, true))
      .then(() => { sw.disabled = false; renderSettings(); });
  }
  $('pref-notif').addEventListener('click', () => togglePref('notifications', 'pref-notif'));
  $('pref-upd').addEventListener('click', () => togglePref('checkForUpdates', 'pref-upd'));

  let pendingProfile = '';
  document.querySelectorAll('.seg').forEach((seg) => seg.addEventListener('click', () => {
    const p = seg.dataset.profile;
    $('prof-text').textContent = PROFILE_TEXT[p];
    if (settings && settings.profile === p) { $('prof-confirm').hidden = true; return; }
    pendingProfile = p;
    $('prof-confirm-text').textContent = 'Apply the ' + seg.textContent + ' profile to Syncthing now? It changes discovery and relay settings.';
    $('prof-confirm').hidden = false;
  }));
  $('prof-cancel').addEventListener('click', () => { $('prof-confirm').hidden = true; renderSettings(); });
  $('prof-apply').addEventListener('click', () => {
    const b = $('prof-apply');
    b.disabled = true;
    act('set-profile', { profile: pendingProfile })
      .then((r) => {
        if (r && typeof r === 'object') settings = r; else (settings = settings || {}).profile = pendingProfile;
        $('prof-confirm').hidden = true;
        toast('Profile applied');
      })
      .catch((e) => toast(e.message, true))
      .then(() => { b.disabled = false; renderSettings(); });
  });
  $('copy-diag').addEventListener('click', copyDiagnostics);
  $('open-logs').addEventListener('click', () => act('open-logs').catch((e) => toast(e.message, true)));

  // ---------- Welcome (--setup and first run: §6.1, §6.2) ----------
  // Syncthing's state (with the bootstrap download progress), Tailscale, the
  // login toggle and, on Windows, the firewall rule; then on to Pair.
  function renderWelcome() {
    const s = snap || {};
    const st = s.StateName || 'down';
    const off = st === 'down' || st === 'unauthorized';
    setText('wel-st', off ? (s.Detail || s.Subline || 'Syncthing is not running.') : 'Syncthing is running.');
    $('wel-st-actions').hidden = st !== 'down' || !!s.Connecting;
    const tsMissing = (s.Notices || []).indexOf('tailscale-missing') >= 0;
    setText('wel-ts', tsMissing
      ? 'Tailscale is not installed. ' + info.name + ' finds your other computers through it: install it and sign in on each of them.'
      : 'Sign in to Tailscale on each computer you want to sync, then pair them.');
    $('wel-ts-actions').hidden = !tsMissing || info.os !== 'windows';
    $('wel-fw-sec').hidden = info.os !== 'windows';
    renderStartup();
  }

  function runWelcome(id, name, done) {
    const b = $(id);
    b.disabled = true;
    act(name)
      .then((r) => { if (done) done(r); })
      .catch((e) => toast(e.message, true))
      .then(() => { b.disabled = false; });
  }
  $('wel-st-start').addEventListener('click', () => runWelcome('wel-st-start', 'start-syncthing'));
  $('wel-ts-install').addEventListener('click', () => runWelcome('wel-ts-install', 'install-tailscale'));
  $('wel-as-tray').addEventListener('click', () => toggleAutostart('tray', 'wel-as-tray'));
  $('wel-fw').addEventListener('click', () => runWelcome('wel-fw', 'allow-firewall',
    (r) => toast(r && r.already ? 'The firewall rule is already in place' : 'Firewall rule added')));
  $('wel-done').addEventListener('click', () => setView('pair'));

  function applyInfo() {
    const at = info.atLogin || 'at login';
    setText('startup-label', 'Starts ' + at);
    setText('lbl-as-tray', 'Start ' + info.name + ' ' + at);
    setText('lbl-wel-as-tray', 'Start ' + info.name + ' ' + at);
    setText('lbl-as-st', 'Start Syncthing ' + at);
    setText('about-name', info.name + ' ' + (info.version || '') + (info.commit && info.commit !== 'unknown' ? ' (' + info.commit + ')' : ''));
    setText('about-repo', info.repoURL || '');
    setText('about-disclaimer', info.disclaimer || '');
    if (snap) renderNotice();
    if (view === 'pair') renderPair();
    if (view === 'welcome') renderWelcome();
  }

  // ---------- views ----------
  function setView(v, fromHash) {
    if (!views[v]) v = 'status';
    view = v;
    Object.keys(views).forEach((k) => { views[k].hidden = k !== v; });
    closeFolderMenu();
    if (v === 'pair') {
      if (cands === null) discover();
      renderPair();
    } else if (v === 'settings') {
      openSettings();
    } else if (v === 'welcome') {
      renderWelcome();
    } else {
      fitActivity();
    }
    if (!fromHash && location.hash !== '#' + v) history.replaceState(null, '', '#' + v);
    const focusable = views[v].querySelector(v === 'status' ? '#nav-pair' : '[data-back]');
    if (focusable && document.activeElement && document.activeElement !== document.body) focusable.focus();
  }
  $('nav-pair').addEventListener('click', () => setView('pair'));
  $('nav-settings').addEventListener('click', () => setView('settings'));
  document.querySelectorAll('[data-back]').forEach((b) => b.addEventListener('click', () => setView('status')));
  window.addEventListener('hashchange', () => setView(location.hash.slice(1), true));

  // ---------- show / hide (D2, D11) ----------
  function loadBackdrop() {
    if (MODE !== 'glass') return;
    const url = '/api/backdrop.png?t=' + Date.now();
    const img = new Image();
    img.onload = () => {
      backdropEl.style.backgroundImage = 'url("' + url + '")';
      hasBackdrop = true;
      ensureAnimating();
    };
    img.onerror = () => {
      backdropEl.style.backgroundImage = 'none';
      hasBackdrop = false;
      ensureAnimating();
    };
    img.src = url;
  }

  function show() {
    closing = false;
    loadBackdrop();
    if (reduced()) {
      enter.Finish();
      sweep.Finish();
    } else {
      enter.Start(0.34, Ease.OutBack);
      sweep.Start(0.62, Ease.OutCubic);
    }
    if (snap) pct.Snap((snap.Pct || 0) / 100);
    ages.forEach((_, k) => ages.set(k, 1));
    ensureAnimating();
  }

  function beginHide() {
    if (closing || MODE === 'browser') return;
    closing = true;
    if (reduced()) {
      closing = false;
      enter.Start(0.016, Ease.OutCubic, true);
      enter.Finish();
      render();
      finishHide();
      return;
    }
    enter.Start(0.18, Ease.OutCubic, true);
    ensureAnimating();
  }

  function finishHide() {
    act('hide').catch(() => {});
  }

  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') show();
  });
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    if (!$('path-entry').hidden) { closePathEntry(''); return; }
    if (!folderMenu.hidden) { closeFolderMenu(); $('btn-folder').focus(); return; }
    if (!$('prof-confirm').hidden) { $('prof-confirm').hidden = true; renderSettings(); return; }
    if (view !== 'status') { setView('status'); return; }
    beginHide();
  });

  // setCorner anchors the entrance scale at the tray corner the host placed
  // the popup in (spec §8.1 Motion).
  function setCorner(c) {
    if (MODE !== 'browser' && CORNERS[c]) stage.style.transformOrigin = CORNERS[c];
  }

  // Hosts may drive the page directly (WebView2 ExecuteScript, WKWebView).
  window.stv2 = { show: show, hide: beginHide, view: (v) => setView(v), corner: setCorner };

  // ---------- state stream ----------
  function showExpired() {
    $('expired').hidden = false;
  }

  function connect() {
    const es = new EventSource('/api/state' + (DEMO ? '?demo=' + encodeURIComponent(DEMO) : ''));
    es.addEventListener('snapshot', (ev) => {
      let s;
      try { s = JSON.parse(ev.data); } catch (e) { return; }
      if (s && typeof s === 'object') apply(s);
    });
    es.onerror = () => {
      if (es.readyState !== EventSource.CLOSED) return; // the browser retries by itself
      fetch('/api/info', { credentials: 'same-origin', cache: 'no-store' })
        .then((r) => { if (r.status === 403) showExpired(); else setTimeout(connect, 2000); })
        .catch(() => setTimeout(connect, 3000));
    };
  }

  // ---------- start ----------
  document.body.className = 'mode-' + MODE;
  stage.style.clipPath = "path('M" + WIN_PATH.map((p) => p[0].toFixed(2) + ' ' + p[1].toFixed(2)).join(' L') + " Z')";
  if (MODE !== 'browser') stage.style.transformOrigin = CORNER;
  if (reduceMQ && reduceMQ.addEventListener) reduceMQ.addEventListener('change', ensureAnimating);
  window.addEventListener('resize', () => { fitActivity(); ensureAnimating(); });

  fetch('/api/info', { credentials: 'same-origin', cache: 'no-store' })
    .then((r) => (r.status === 403 ? (showExpired(), null) : r.json()))
    .then((j) => { if (j) { info = Object.assign(info, j); applyInfo(); } })
    .catch(() => {});

  const initialView = DEMO === 'pair' || DEMO === 'settings' ? DEMO : location.hash.slice(1);
  setView(views[initialView] ? initialView : 'status');
  renderButtons();
  connect();
  show();
})();
