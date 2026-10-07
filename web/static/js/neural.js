// Shared drawing code: deterministic RNG, the hero spiking network, raster thumbnails
// and the results raster. No dependencies.

export const COLORS = {
  magenta: [255, 63, 210],
  green: [46, 242, 154],
  cyan: [139, 234, 247],
  grey: [120, 132, 126],
  red: [255, 93, 108],
};
const rgba = (c, a) => `rgba(${c[0]},${c[1]},${c[2]},${a})`;

export function hashString(str) {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < str.length; i++) {
    h ^= str.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

export function rng(seed) {
  let a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function sizeCanvas(canvas, maxDpr = 2) {
  const dpr = Math.min(window.devicePixelRatio || 1, maxDpr);
  const rect = canvas.getBoundingClientRect();
  const w = Math.max(1, Math.round(rect.width));
  const h = Math.max(1, Math.round(rect.height));
  if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx, w, h, dpr };
}

function glowSprite(color, size = 48) {
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const g = c.getContext("2d");
  const grad = g.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  grad.addColorStop(0, rgba([255, 255, 255], 1));
  grad.addColorStop(0.12, rgba(color, 0.95));
  grad.addColorStop(0.4, rgba(color, 0.25));
  grad.addColorStop(1, rgba(color, 0));
  g.fillStyle = grad;
  g.fillRect(0, 0, size, size);
  return c;
}

/* ------------------------------------------------------------------ */
/* Hero spiking network: leaky units on a sparse directed graph.       */
/* Sensory units (green) get Poisson drive, pulses travel along edges  */
/* with a delay proportional to length, motor units are magenta.       */
/* The pointer acts as a stimulation electrode.                        */
/* ------------------------------------------------------------------ */
/* Motion: on first start the network "ignites" — a wavefront leaves one point and every unit */
/* fires its first spike as the front reaches it, so the field visibly comes alive in <1 s.    */
/* The pointer leaves ripples; a press is a stronger stimulation pulse.                        */
export class SpikingNet {
  constructor(canvas, { ribbon = null, reduced = false, seed = 630, ignite = false, igniteOrigin = null } = {}) {
    this.canvas = canvas;
    this.ribbon = ribbon;
    this.reduced = reduced;
    this.seed = seed;
    this.running = false;
    this.visible = true;
    this.pointer = null;
    this.spikeLog = [];
    this.time = 0;
    this.ripples = [];
    this.lastRipple = null;
    this.igniteWanted = ignite && !reduced;
    this.igniteOrigin = igniteOrigin; // fractions of the canvas: { x: 0..1, y: 0..1 }
    this.wave = null;
    this.sprites = {
      sensory: glowSprite(COLORS.green),
      inter: glowSprite(COLORS.cyan),
      motor: glowSprite(COLORS.magenta),
    };
    this._frame = this._frame.bind(this);
    this.build();
  }

  build() {
    const { w, h } = sizeCanvas(this.canvas);
    this.w = w; this.h = h;
    const R = rng(this.seed);
    // Phones and small tablets get a lighter field (fewer units, fewer pulses to draw).
    const narrow = w <= 768;
    const count = narrow ? 34 : Math.min(120, Math.round((w * h) / 14000));
    const nodes = [];
    for (let i = 0; i < count; i++) {
      // Bias toward the right half, where the plate image is dense; keep text side calm.
      const bx = narrow ? R() : 0.4 + 0.6 * Math.pow(R(), 0.8);
      const x = bx * w;
      const y = (0.06 + 0.88 * R()) * h;
      const roll = R();
      const type = roll < 0.14 ? "sensory" : roll > 0.9 ? "motor" : "inter";
      nodes.push({ x, y, type, v: R() * 0.5, ref: 0, flash: 0, out: [], lit: 1, r: type === "inter" ? 1.3 + R() * 0.9 : 1.8 + R() * 1.1 });
    }
    // Directed edges to nearest neighbours (sparse, like a local connectome patch).
    const edges = [];
    const maxD = Math.max(w, h) * (narrow ? 0.26 : 0.16);
    for (let i = 0; i < nodes.length; i++) {
      const a = nodes[i];
      const cand = [];
      for (let j = 0; j < nodes.length; j++) {
        if (i === j) continue;
        const d = Math.hypot(nodes[j].x - a.x, nodes[j].y - a.y);
        if (d < maxD) cand.push([d, j]);
      }
      cand.sort((p, q) => p[0] - q[0]);
      const k = 2 + Math.floor(R() * 2);
      for (let n = 0; n < Math.min(k, cand.length); n++) {
        const [d, j] = cand[n];
        const weight = (R() < 0.12 ? -0.5 : 0.42 + R() * 0.45);
        const e = { from: i, to: j, len: d, weight };
        a.out.push(edges.length);
        edges.push(e);
      }
    }
    this.nodes = nodes;
    this.edges = edges;
    this.pulses = [];
    const path = new Path2D();
    for (const e of edges) {
      const a = nodes[e.from], b = nodes[e.to];
      // gentle curve so the field reads as neurites, not a graph diagram
      const mx = (a.x + b.x) / 2 + (a.y - b.y) * 0.12;
      const my = (a.y + b.y) / 2 + (b.x - a.x) * 0.12;
      e.mx = mx; e.my = my;
      path.moveTo(a.x, a.y);
      path.quadraticCurveTo(mx, my, b.x, b.y);
    }
    this.edgePath = path;
    // Rows shown in the raster ribbon: sensory first, then a sample of interneurons, motor last.
    const order = { sensory: 0, inter: 1, motor: 2 };
    const picked = nodes
      .map((n, i) => ({ n, i }))
      .filter(({ n }, idx) => n.type !== "inter" || idx % 3 === 0)
      .sort((p, q) => order[p.n.type] - order[q.n.type] || p.n.y - q.n.y);
    this.rowOf = new Map();
    picked.slice(0, 36).forEach(({ i }, row) => this.rowOf.set(i, row));
    this.rows = Math.min(36, picked.length);
    if (this.ribbon) sizeCanvas(this.ribbon);
  }

  setPointer(x, y) {
    this.pointer = x === null ? null : { x, y };
    // Phones and small tablets (lite field) get no pointer ripples at all.
    if (!this.pointer || this.reduced || this.w <= 768) return;
    // A sparse trail of faint ripples, spaced in distance and time, so the electrode is visible.
    // At most ~10 per second and 5 alive: each one is a stroke per frame under "lighter".
    const lr = this.lastRipple;
    const dt = lr ? this.time - lr.t : Infinity;
    if (!lr || (Math.hypot(x - lr.x, y - lr.y) > 90 && dt > 0.1) || dt > 0.35) {
      this.addRipple(x, y, 0.55);
    }
  }

  addRipple(x, y, strength) {
    this.lastRipple = { x, y, t: this.time };
    this.ripples.push({ x, y, t: this.time, s: strength });
    if (this.ripples.length > 5) this.ripples.shift();
  }

  // A press: strong ripple and a volley into the units under the electrode.
  pulseAt(x, y) {
    if (this.reduced) return;
    this.addRipple(x, y, 1);
    this.nodes.forEach((n, i) => {
      if (n.lit && n.ref <= 0 && Math.hypot(n.x - x, n.y - y) < 120) this.fire(i);
    });
  }

  // Ignition: every unit is dark until the wavefront from (ox, oy) reaches it, then fires.
  ignite(ox, oy) {
    let maxD = 1;
    for (const n of this.nodes) maxD = Math.max(maxD, Math.hypot(n.x - ox, n.y - oy));
    const dur = 0.85; // seconds for the front to cross the field
    const speed = maxD / dur;
    for (const n of this.nodes) {
      n.lit = 0;
      n.igniteT = this.time + 0.05 + Math.hypot(n.x - ox, n.y - oy) / speed;
    }
    this.wave = { x: ox, y: oy, t0: this.time, speed, maxD };
  }

  fire(i) {
    const n = this.nodes[i];
    n.v = 0; n.ref = 0.06; n.flash = 1;
    for (const ei of n.out) this.pulses.push({ e: ei, t: 0 });
    if (this.rowOf.has(i)) this.spikeLog.push({ row: this.rowOf.get(i), t: this.time, type: n.type });
  }

  step(dt) {
    this.time += dt;
    const nodes = this.nodes;
    const p = this.pointer;
    let dark = 0;
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i];
      if (!n.lit) {
        if (this.time >= n.igniteT) { n.lit = 1; n.igniteT = undefined; this.fire(i); }
        else dark++;
        continue;
      }
      n.flash = Math.max(0, n.flash - dt * 2.6);
      if (n.ref > 0) { n.ref -= dt; continue; }
      n.v -= n.v * dt / 0.12; // leak, tau = 120 ms of page time
      let rate = n.type === "sensory" ? 4.5 : 0.25;
      if (p) {
        const d = Math.hypot(n.x - p.x, n.y - p.y);
        if (d < 140) rate += 26 * (1 - d / 140);
      }
      if (Math.random() < rate * dt) n.v += 0.65;
      if (n.v >= 1) this.fire(i);
    }
    const speed = 260; // px per second
    const keep = [];
    for (const pu of this.pulses) {
      const e = this.edges[pu.e];
      pu.t += (speed * dt) / Math.max(20, e.len);
      if (pu.t >= 1) {
        const tgt = nodes[e.to];
        if (tgt.lit && tgt.ref <= 0) {
          tgt.v += e.weight * 0.62;
          if (tgt.v < 0) tgt.v = 0;
          if (tgt.v >= 1) this.fire(e.to);
        }
      } else keep.push(pu);
    }
    const cap = this.w <= 768 ? 360 : 900;
    this.pulses = keep.length > cap ? keep.slice(-cap) : keep;
    if (this.wave && !dark && this.time - this.wave.t0 > 1.6) this.wave = null;
    if (this.ripples.length && this.time - this.ripples[0].t > 1) this.ripples = this.ripples.filter((r) => this.time - r.t <= 1);
    const horizon = this.time - 12;
    if (this.spikeLog.length && this.spikeLog[0].t < horizon) {
      this.spikeLog = this.spikeLog.filter((s) => s.t >= horizon);
    }
  }

  draw() {
    const { ctx, w, h } = sizeCanvas(this.canvas);
    ctx.clearRect(0, 0, w, h);
    const wave = this.wave;
    const waveAge = wave ? this.time - wave.t0 : 0;
    ctx.lineWidth = 0.6;
    // Neurites fade up behind the ignition front.
    const edgeA = wave ? Math.min(1, waveAge / 1.1) : 1;
    ctx.strokeStyle = `rgba(139,234,247,${(0.11 * edgeA).toFixed(3)})`;
    ctx.stroke(this.edgePath);
    if (wave) {
      const r = waveAge * wave.speed;
      const fade = Math.max(0, 1 - r / (wave.maxD * 1.05));
      if (fade > 0 && r > 2) {
        ctx.save();
        ctx.globalCompositeOperation = "lighter";
        // a soft band of light (no hard rim): rises behind the front, peaks just inside it, fades out
        const inner = Math.max(0, r - 90), outer = r + 24;
        const g = ctx.createRadialGradient(wave.x, wave.y, inner, wave.x, wave.y, outer);
        g.addColorStop(0, rgba(COLORS.green, 0));
        g.addColorStop(0.62, rgba(COLORS.green, 0.06 * fade));
        g.addColorStop(0.8, rgba(COLORS.cyan, 0.2 * fade));
        g.addColorStop(1, rgba(COLORS.cyan, 0));
        ctx.fillStyle = g;
        ctx.beginPath();
        ctx.arc(wave.x, wave.y, outer, 0, Math.PI * 2);
        ctx.fill();
        ctx.restore();
      }
    }

    ctx.globalCompositeOperation = "lighter";
    for (const pu of this.pulses) {
      const e = this.edges[pu.e];
      const a = this.nodes[e.from], b = this.nodes[e.to];
      const q = (t) => {
        const u = 1 - t;
        return [u * u * a.x + 2 * u * t * e.mx + t * t * b.x, u * u * a.y + 2 * u * t * e.my + t * t * b.y];
      };
      const [x1, y1] = q(Math.max(0, pu.t - 0.14));
      const [x2, y2] = q(pu.t);
      const col = a.type === "motor" ? COLORS.magenta : a.type === "sensory" ? COLORS.green : COLORS.cyan;
      const grad = ctx.createLinearGradient(x1, y1, x2, y2);
      grad.addColorStop(0, rgba(col, 0));
      grad.addColorStop(1, rgba(col, e.weight < 0 ? 0.35 : 0.85));
      ctx.strokeStyle = grad;
      ctx.lineWidth = 1.6;
      ctx.beginPath();
      ctx.moveTo(x1, y1);
      ctx.lineTo(x2, y2);
      ctx.stroke();
    }
    for (const n of this.nodes) {
      if (!n.lit) continue;
      const sprite = this.sprites[n.type];
      const base = n.type === "inter" ? 0.3 : 0.55;
      const s = n.r * 7 + n.flash * 40;
      ctx.globalAlpha = Math.min(1, base + n.flash * 0.8 + n.v * 0.25);
      ctx.drawImage(sprite, n.x - s / 2, n.y - s / 2, s, s);
    }
    ctx.globalAlpha = 1;
    // Ripples: one thin stroke each; only a press (strength 1, rare) also gets the wide halo.
    for (const rp of this.ripples) {
      const age = this.time - rp.t;
      const life = 0.55 + rp.s * 0.4;
      if (age > life) continue;
      const k = age / life;
      const a = (1 - k) * (1 - k) * rp.s;
      const rad = 6 + (1 - Math.pow(1 - k, 3)) * (60 + rp.s * 80);
      ctx.beginPath();
      ctx.arc(rp.x, rp.y, rad, 0, Math.PI * 2);
      if (rp.s >= 1) {
        ctx.strokeStyle = rgba(COLORS.green, a * 0.35); // halo
        ctx.lineWidth = 10;
        ctx.stroke();
      }
      ctx.strokeStyle = rgba([210, 255, 236], a * 0.85); // bright core
      ctx.lineWidth = 1.2 + rp.s;
      ctx.stroke();
    }
    ctx.globalCompositeOperation = "source-over";
    if (this.ribbon) this.drawRibbon();
  }

  drawRibbon() {
    const { ctx, w, h } = sizeCanvas(this.ribbon);
    ctx.clearRect(0, 0, w, h);
    const rows = Math.max(1, this.rows);
    const rh = h / rows;
    const pxPerSec = Math.max(60, w / 9);
    for (const s of this.spikeLog) {
      const x = w - (this.time - s.t) * pxPerSec;
      if (x < -2) continue;
      const col = s.type === "motor" ? COLORS.magenta : s.type === "sensory" ? COLORS.green : COLORS.cyan;
      const age = (this.time - s.t);
      ctx.fillStyle = rgba(col, age < 0.25 ? 1 : 0.75);
      ctx.fillRect(x, s.row * rh + rh * 0.15, 1.5, Math.max(1, rh * 0.7));
    }
  }

  _frame(ts) {
    if (!this.running) return;
    const last = this._last || ts;
    const dt = Math.min(0.05, (ts - last) / 1000);
    this._last = ts;
    this.step(dt);
    this.draw();
    this._raf = requestAnimationFrame(this._frame);
  }

  start() {
    if (this.reduced) {
      // Reduced motion: simulate a few seconds offline and show one still frame.
      for (let i = 0; i < 360; i++) this.step(1 / 60);
      this.draw();
      return;
    }
    if (this.running) return;
    if (this.igniteWanted) {
      this.igniteWanted = false;
      if (this.ribbon && !this.spikeLog.length) this.prewarm(2.5);
      const o = this.igniteOrigin || { x: 0.7, y: 0.45 };
      this.ignite(o.x * this.w, o.y * this.h);
    }
    this.running = true;
    this._last = 0;
    this._raf = requestAnimationFrame(this._frame);
  }

  stop() {
    this.running = false;
    if (this._raf) cancelAnimationFrame(this._raf);
  }

  // Live prefers-reduced-motion switch: on = stop the loop and leave one still frame (every unit
  // lit, no ripples, no wavefront); off = the caller may start() the loop again.
  setReduced(on) {
    this.reduced = on;
    if (!on) return;
    this.stop();
    this.igniteWanted = false;
    this.wave = null;
    this.ripples = [];
    this.pointer = null;
    for (const n of this.nodes) if (!n.lit) { n.lit = 1; n.igniteT = undefined; }
    this.start();
  }

  // Simulate some history offline so the raster ribbon is already populated on arrival. Pulses
  // and flashes are cleared afterwards: the visible field still starts dark for the ignition.
  prewarm(seconds) {
    const steps = Math.round(seconds * 60);
    for (let i = 0; i < steps; i++) this.step(1 / 60);
    this.pulses = [];
    for (const n of this.nodes) { n.flash = 0; n.v *= 0.5; }
  }

  resize() {
    this.build();
    this.spikeLog = [];
    this.wave = null;
    this.ripples = [];
    if (!this.running) this.draw();
  }
}

/* ------------------------------------------------------------------ */
/* SpikeTrace: a small live membrane trace (leaky integrate-and-fire), */
/* one row per unit, scrolling right-to-left. Used as the "thinking"   */
/* and "running" indicator in the app instead of a spinner.            */
/* setActivity(0..1) raises the drive, so the firing rate ramps up.    */
/* One rAF loop per visible trace; idles offscreen, in hidden tabs,    */
/* and stops for good once the canvas leaves the document.             */
/* ------------------------------------------------------------------ */
export class SpikeTrace {
  constructor(canvas, { rows = 2, activity = 0.35, reduced = false, seed = 7, speed = 52, colors = null, noise = 0.9 } = {}) {
    this.canvas = canvas;
    this.rowsN = rows;
    this.reduced = reduced;
    this.speed = speed; // px of trace per second
    this.activity = activity;
    this.target = activity;
    this.noise = noise;
    this.R = rng(seed);
    this.colors = colors || [COLORS.green, COLORS.cyan, COLORS.magenta];
    this.rows = Array.from({ length: rows }, (_, i) => ({ v: this.R() * 0.6, ref: 0, buf: [], bias: (i - (rows - 1) / 2) * 0.06 }));
    this.acc = 0;
    this.onscreen = true;
    this.running = false;
    this.dead = false;
    this._frame = this._frame.bind(this);
    this.resize();
    // Pre-roll so the trace is full (never an empty box) on its first frame.
    this._advance(this.width);
    this.draw();
    if (reduced) { this.dead = true; return; } // a still frame; nothing to schedule or observe
    this._vis = () => this._sync();
    document.addEventListener("visibilitychange", this._vis);
    if ("IntersectionObserver" in window) {
      this.io = new IntersectionObserver((es) => { this.onscreen = es[es.length - 1].isIntersecting; this._sync(); });
      this.io.observe(canvas);
    }
    this._sync();
  }

  resize() {
    const { w, h } = sizeCanvas(this.canvas);
    this.width = Math.max(10, Math.round(w));
    this.height = h;
    for (const r of this.rows) if (r.buf.length > this.width) r.buf.splice(0, r.buf.length - this.width);
  }

  setActivity(a) { this.target = Math.max(0, Math.min(1, a)); if (this.reduced) { this.activity = this.target; } }

  // Simulate n new columns (1 column = 1 CSS px of trace).
  _advance(n) {
    const dtc = 1 / this.speed;
    for (let c = 0; c < n; c++) {
      this.activity += (this.target - this.activity) * 0.02;
      for (const r of this.rows) {
        let spike = false;
        if (r.ref > 0) { r.ref -= dtc; r.v = 0; } else {
          const drive = 0.82 + this.activity * 0.95 + r.bias;
          const noise = (this.R() + this.R() + this.R() - 1.5) * this.noise * Math.sqrt(dtc);
          r.v += ((drive - r.v) * dtc) / 0.16 + noise;
          if (r.v < 0) r.v = 0;
          if (r.v >= 1) { spike = true; r.v = 0; r.ref = 0.035; }
        }
        r.buf.push(spike ? -1 : r.v);
        if (r.buf.length > this.width) r.buf.shift();
      }
    }
  }

  draw() {
    const { ctx, w, h } = sizeCanvas(this.canvas);
    ctx.clearRect(0, 0, w, h);
    const bandH = h / this.rowsN;
    this.rows.forEach((r, ri) => {
      const top = ri * bandH + 2;
      const base = top + bandH - 3;
      const span = (bandH - 5) * 0.62;
      const col = this.colors[ri % this.colors.length];
      const x0 = w - r.buf.length;
      ctx.lineWidth = 1;
      ctx.strokeStyle = "rgba(185,195,190,0.42)";
      ctx.beginPath();
      r.buf.forEach((v, i) => {
        const y = base - (v < 0 ? 1 : v) * span;
        if (i === 0) ctx.moveTo(x0 + i, y); else ctx.lineTo(x0 + i, y);
      });
      ctx.stroke();
      ctx.globalCompositeOperation = "lighter";
      r.buf.forEach((v, i) => {
        if (v >= 0) return;
        const x = x0 + i;
        const age = (r.buf.length - i) / Math.max(1, w); // 0 at the head, 1 at the tail
        ctx.fillStyle = rgba(col, 0.95 - age * 0.55);
        ctx.fillRect(x - 0.5, top, 1.5, base - span - top + 1);
        ctx.fillStyle = rgba(col, 0.35 - age * 0.25);
        ctx.fillRect(x - 2, top, 4, 3);
      });
      ctx.globalCompositeOperation = "source-over";
      const last = r.buf[r.buf.length - 1];
      ctx.fillStyle = rgba(col, 1);
      ctx.beginPath();
      ctx.arc(w - 1.5, base - (last < 0 ? 1 : last) * span, 1.8, 0, Math.PI * 2);
      ctx.fill();
    });
  }

  _sync() {
    if (this.dead || this.reduced) return;
    if (!this.canvas.isConnected) { this.destroy(); return; } // its card was replaced
    const want = this.onscreen && !document.hidden && !this.frozen;
    if (want && !this.running) {
      this.running = true;
      this._last = 0;
      this._raf = requestAnimationFrame(this._frame);
    } else if (!want && this.running) {
      this.running = false;
      cancelAnimationFrame(this._raf);
    }
  }

  _frame(ts) {
    if (!this.running) return;
    if (!this.canvas.isConnected) { this.destroy(); return; }
    const last = this._last || ts;
    const dt = Math.min(0.1, (ts - last) / 1000);
    this._last = ts;
    this.acc += dt * this.speed;
    const n = Math.floor(this.acc);
    if (n > 0) { this.acc -= n; this._advance(n); this.draw(); }
    this._raf = requestAnimationFrame(this._frame);
  }

  // Stop animating and leave the current trace on screen (e.g. the run finished).
  freeze() {
    if (this.frozen) return;
    this.frozen = true;
    if (this.dead) return; // already still (reduced) or gone with its card
    this.draw();
    this.destroy();
  }

  destroy() {
    this.dead = true;
    this.running = false;
    cancelAnimationFrame(this._raf);
    if (this._vis) document.removeEventListener("visibilitychange", this._vis);
    if (this.io) this.io.disconnect();
  }
}

// Soft glow like a confocal exposure: blurred copy added on top. Skipped where canvas filters are unsupported.
function bloom(canvas, ctx, w, h) {
  if (typeof ctx.filter !== "string") return;
  const tmp = document.createElement("canvas");
  tmp.width = canvas.width;
  tmp.height = canvas.height;
  tmp.getContext("2d").drawImage(canvas, 0, 0);
  ctx.save();
  ctx.setTransform(1, 0, 0, 1, 0, 0);
  ctx.globalCompositeOperation = "lighter";
  ctx.filter = `blur(${Math.max(3, Math.round(canvas.width / 90))}px)`;
  ctx.globalAlpha = 0.85;
  ctx.drawImage(tmp, 0, 0);
  ctx.restore();
}

/* ------------------------------------------------------------------ */
/* Procedural raster thumbnail, deterministic from a seed string.      */
/* ------------------------------------------------------------------ */
export function drawRasterThumb(canvas, { seed = "x", a = null, b = null, status = "succeeded", rate = null } = {}) {
  const { ctx, w, h } = sizeCanvas(canvas, 2);
  const R = rng(hashString(String(seed)));
  ctx.fillStyle = "#07090a";
  ctx.fillRect(0, 0, w, h);

  const failed = status === "failed" || status === "cancelled";
  const pending = status === "queued" || status === "running" || status === "cancelling";
  const hasB = b !== null && b !== undefined;
  const rows = 16 + Math.floor(R() * 14);
  const total = (a || 0) + (hasB ? b : 0);
  const density = pending ? 0.12 : Math.min(1, 0.18 + Math.log10(total + 1) / 4.2);
  const stimFrac = rate ? Math.min(1, rate / 200) : 0.3 + R() * 0.5;
  const top = h * 0.08, bottom = h * 0.84;
  const rh = (bottom - top) / rows;

  // stimulus train along the bottom (cyan)
  ctx.fillStyle = rgba(COLORS.cyan, failed ? 0.15 : 0.45);
  const stimCount = Math.round(8 + stimFrac * 60);
  for (let i = 0; i < stimCount; i++) {
    const x = (i / stimCount) * w + R() * (w / stimCount) * 0.6;
    ctx.fillRect(x, h * 0.9, 1, h * 0.05);
  }

  const splitAt = hasB ? Math.floor(rows / 2) : rows;
  // bursts: some rows share a burst window, which reads as a propagating wave
  const waves = [];
  const nWaves = 1 + Math.floor(R() * 3);
  for (let i = 0; i < nWaves; i++) waves.push({ x: 0.15 + R() * 0.7, slope: (R() - 0.5) * 0.5, width: 0.04 + R() * 0.08 });

  ctx.globalCompositeOperation = "lighter";
  for (let r = 0; r < rows; r++) {
    const inB = r >= splitAt;
    const col = failed ? COLORS.grey : pending ? COLORS.cyan : inB ? COLORS.green : (r % 5 === 0 ? COLORS.cyan : COLORS.magenta);
    const y = top + r * rh + (inB ? rh * 0.6 : 0);
    const rowRate = density * (0.25 + R() * 1.1) * (r % 7 === 3 ? 2 : 1);
    const n = Math.round(rowRate * 26);
    for (let k = 0; k < n; k++) {
      let x;
      if (R() < 0.55 && waves.length) {
        const wv = waves[Math.floor(R() * waves.length)];
        x = (wv.x + wv.slope * (r / rows) + (R() - 0.5) * wv.width) * w;
      } else {
        x = R() * w;
      }
      const alpha = failed ? 0.35 : 0.5 + R() * 0.5;
      ctx.fillStyle = rgba(col, alpha);
      ctx.fillRect(x, y + rh * 0.15, 1.4, Math.max(1, rh * 0.62));
    }
  }
  ctx.globalCompositeOperation = "source-over";
  bloom(canvas, ctx, w, h);

  if (hasB) {
    ctx.fillStyle = "rgba(226,238,232,0.08)";
    ctx.fillRect(0, top + splitAt * rh + rh * 0.3, w, 1);
  }
  // vignette
  const vg = ctx.createRadialGradient(w / 2, h / 2, Math.min(w, h) * 0.2, w / 2, h / 2, Math.max(w, h) * 0.75);
  vg.addColorStop(0, "rgba(0,0,0,0)");
  vg.addColorStop(1, "rgba(0,0,0,0.55)");
  ctx.fillStyle = vg;
  ctx.fillRect(0, 0, w, h);
  if (failed) {
    ctx.fillStyle = rgba(COLORS.red, 0.85);
    ctx.fillRect(0, 0, 3, h);
  }
}

/* ------------------------------------------------------------------ */
/* Results raster from /spikes rows: one line per neuron, condition A  */
/* ticks in the upper half (magenta), B in the lower half (green).     */
/* Only counts are known here, so tick positions are deterministic     */
/* placements; exact spike times live in spikes.parquet in the export. */
/* ------------------------------------------------------------------ */
// compare=true means every B row was supplied, so a neuron without B rows had 0 spikes in B
// (rates.csv lists only active neurons) and is shown as "B 0" rather than as single-condition.
export function drawResultRaster(canvas, rows, { durationMs = 100, maxNeurons = 60, compare = false } = {}) {
  const byNeuron = new Map();
  for (const r of rows) {
    if (!byNeuron.has(r.root_id)) byNeuron.set(r.root_id, { root_id: r.root_id, readout: false, A: 0, B: 0, hasB: compare, trials: new Set() });
    const n = byNeuron.get(r.root_id);
    n.readout = n.readout || r.is_readout;
    n.trials.add(r.trial);
    if (r.condition === "B") { n.B += r.spike_count; n.hasB = true; } else n.A += r.spike_count;
  }
  const neurons = [...byNeuron.values()]
    .sort((p, q) => (q.readout - p.readout) || (q.A + q.B - p.A - p.B))
    .slice(0, maxNeurons);

  const rowH = 9;
  const labelW = 0;
  canvas.style.height = `${Math.max(120, neurons.length * rowH + 34)}px`;
  const { ctx, w, h } = sizeCanvas(canvas, 2);
  ctx.fillStyle = "#07090a";
  ctx.fillRect(0, 0, w, h);
  const plotW = w - labelW - 16;
  const x0 = labelW + 8;
  // time grid
  ctx.fillStyle = "rgba(226,238,232,0.06)";
  ctx.font = "10px 'Martian Mono', ui-monospace, monospace";
  const steps = 5;
  for (let i = 0; i <= steps; i++) {
    const x = x0 + (plotW * i) / steps;
    ctx.fillStyle = "rgba(226,238,232,0.06)";
    ctx.fillRect(x, 0, 1, h - 22);
    ctx.fillStyle = "rgba(138,149,144,0.9)";
    const label = `${Math.round((durationMs * i) / steps)} ms`;
    const tw = ctx.measureText(label).width;
    ctx.fillText(label, Math.min(Math.max(x - tw / 2, 2), w - tw - 2), h - 7);
  }
  ctx.globalCompositeOperation = "lighter";
  neurons.forEach((n, i) => {
    const y = 6 + i * rowH;
    if (n.readout) {
      ctx.fillStyle = "rgba(255,63,210,0.10)";
      ctx.fillRect(0, y - 1, w, rowH);
    }
    const trials = Math.max(1, n.trials.size);
    const draw = (count, col, yy, key) => {
      const R = rng(hashString(n.root_id + key));
      const c = Math.round(count / trials);
      for (let k = 0; k < c; k++) {
        const x = x0 + R() * plotW;
        ctx.fillStyle = rgba(col, 0.9);
        ctx.fillRect(x, yy, 1.4, 3);
      }
    };
    draw(n.A, COLORS.magenta, y, "A");
    if (n.hasB) draw(n.B, COLORS.green, y + 4, "B");
  });
  ctx.globalCompositeOperation = "source-over";
  return { shown: neurons.length, total: byNeuron.size, neurons, rowH, top: 6 };
}
