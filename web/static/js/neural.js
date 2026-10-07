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
export class SpikingNet {
  constructor(canvas, { ribbon = null, reduced = false, seed = 630 } = {}) {
    this.canvas = canvas;
    this.ribbon = ribbon;
    this.reduced = reduced;
    this.seed = seed;
    this.running = false;
    this.visible = true;
    this.pointer = null;
    this.spikeLog = [];
    this.time = 0;
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
    const narrow = w < 720;
    const count = narrow ? 46 : Math.min(120, Math.round((w * h) / 14000));
    const nodes = [];
    for (let i = 0; i < count; i++) {
      // Bias toward the right half, where the plate image is dense; keep text side calm.
      const bx = narrow ? R() : 0.4 + 0.6 * Math.pow(R(), 0.8);
      const x = bx * w;
      const y = (0.06 + 0.88 * R()) * h;
      const roll = R();
      const type = roll < 0.14 ? "sensory" : roll > 0.9 ? "motor" : "inter";
      nodes.push({ x, y, type, v: R() * 0.5, ref: 0, flash: 0, out: [], r: type === "inter" ? 1.3 + R() * 0.9 : 1.8 + R() * 1.1 });
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

  setPointer(x, y) { this.pointer = x === null ? null : { x, y }; }

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
    for (let i = 0; i < nodes.length; i++) {
      const n = nodes[i];
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
        if (tgt.ref <= 0) {
          tgt.v += e.weight * 0.62;
          if (tgt.v < 0) tgt.v = 0;
          if (tgt.v >= 1) this.fire(e.to);
        }
      } else keep.push(pu);
    }
    this.pulses = keep.length > 900 ? keep.slice(-900) : keep;
    const horizon = this.time - 12;
    if (this.spikeLog.length && this.spikeLog[0].t < horizon) {
      this.spikeLog = this.spikeLog.filter((s) => s.t >= horizon);
    }
  }

  draw() {
    const { ctx, w, h } = sizeCanvas(this.canvas);
    ctx.clearRect(0, 0, w, h);
    ctx.lineWidth = 0.6;
    ctx.strokeStyle = "rgba(139,234,247,0.11)";
    ctx.stroke(this.edgePath);

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
      const sprite = this.sprites[n.type];
      const base = n.type === "inter" ? 0.3 : 0.55;
      const s = n.r * 7 + n.flash * 40;
      ctx.globalAlpha = Math.min(1, base + n.flash * 0.8 + n.v * 0.25);
      ctx.drawImage(sprite, n.x - s / 2, n.y - s / 2, s, s);
    }
    ctx.globalAlpha = 1;
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
    this.running = true;
    this._last = 0;
    this._raf = requestAnimationFrame(this._frame);
  }

  stop() {
    this.running = false;
    if (this._raf) cancelAnimationFrame(this._raf);
  }

  resize() {
    this.build();
    this.spikeLog = [];
    if (!this.running) this.draw();
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
