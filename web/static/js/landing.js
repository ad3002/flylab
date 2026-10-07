import { SpikingNet, drawRasterThumb } from "/static/js/neural.js?v=v4g";
import { isReduced, onReducedChange, finePointer, isLite, countUp, splitLines, assignLines, magnetic } from "/static/js/motion.js?v=v4g";

document.documentElement.classList.add("js");
const root = document.documentElement;

/* ---------- example gallery markup (DOM only; thumbnails are drawn later) ---------- */
const EXAMPLES = [
  { p: "Activate sugar GRNs at 100 Hz and read out MN9", type: "single", hz: 100, ms: 1000, a: 2400, b: null },
  { p: "Compare bitter GRN activation at 150 Hz with and without silencing the top sugar neuron", type: "compare", hz: 150, ms: 500, a: 1800, b: 1650 },
  { p: "Sugar GRNs at 50 Hz, silence one sugar neuron and compare MN9", type: "compare", hz: 50, ms: 1000, a: 900, b: 870 },
  { p: "Ir94e neurons at 80 Hz for 500 ms, three repeats", type: "single", hz: 80, ms: 500, a: 640, b: null },
  { p: "Sugar GRNs at 20 Hz — does MN9 respond at all?", type: "single", hz: 20, ms: 1000, a: 120, b: null },
  { p: "Bitter GRNs at 200 Hz for 250 ms, seed 7", type: "single", hz: 200, ms: 250, a: 3100, b: null },
  { p: "Sugar GRNs at 200 Hz for 1000 ms, three repeats, read out MN9", type: "single", hz: 200, ms: 1000, a: 5200, b: null },
  { p: "Sugar and Ir94e together at 60 Hz; silence the demo neuron in condition B", type: "compare", hz: 60, ms: 800, a: 1500, b: 1320 },
];
const strip = document.getElementById("strip");
const thumbs = [];
EXAMPLES.forEach((ex, i) => {
  const a = document.createElement("a");
  a.className = "ex-card reveal";
  a.href = `/app#/new?p=${encodeURIComponent(ex.p)}`;
  const c = document.createElement("canvas");
  c.setAttribute("aria-hidden", "true");
  const body = document.createElement("div");
  body.className = "ex-body";
  const pr = document.createElement("p");
  pr.className = "ex-prompt";
  pr.textContent = ex.p;
  const meta = document.createElement("p");
  meta.className = "ex-meta mono";
  const mk = (t) => { const b = document.createElement("b"); b.textContent = t; return b; };
  meta.append(mk(ex.type === "compare" ? "A vs B" : "single"), mk(`${ex.hz} Hz`), mk(`${ex.ms} ms`));
  const go = document.createElement("span");
  go.className = "ex-try";
  go.textContent = "Start from this →";
  meta.append(go);
  body.append(pr, meta);
  a.append(c, body);
  strip.append(a);
  thumbs.push({ c, ex, i });
});

/* ---------- reveal on scroll: armed first, before anything that could throw ----------
   The hidden "before" states in landing.css only apply under html.reveal-armed, which is set
   here, right after the observer exists. If this module fails to load or throws earlier, the
   page simply stays fully visible. Things that arrive together cascade (capped). */
const brainSection = document.getElementById("brain");
const ribbonFig = document.querySelector(".ribbon");
const CASCADE_MAX = 6;   // a batch never waits more than 6 steps
const CASCADE_STEP = 60; // ms between items of one batch
const revealIn = (el, k, step = CASCADE_STEP) => {
  el.style.setProperty("--rd", `${Math.min(k, CASCADE_MAX) * step}ms`);
  el.classList.add("in");
  if (el.classList.contains("statement-text")) el.querySelectorAll("[data-count]").forEach((n) => countUp(n, { duration: 1400, delay: 150 }));
};
// The example strip reveals as one gallery: cards in view cascade, cards still to the right of the
// viewport are revealed at the same moment (they finish before anyone can swipe to them).
const revealStrip = () => {
  const vw = window.innerWidth;
  let k = 0;
  for (const card of strip.children) {
    if (!card.classList.contains("reveal")) continue;
    revealIn(card, card.getBoundingClientRect().left < vw ? k++ : 0, 40);
  }
};
// Headings: lines rise out of their own masks.
if (!isReduced()) {
  document.querySelectorAll(".h2, .cta-title").forEach((el) => {
    splitLines(el);
    el.classList.add("split-reveal");
  });
}
const revealEls = [...document.querySelectorAll(".reveal, .split-reveal")].filter((el) => !el.classList.contains("ex-card"));
if (!isReduced() && "IntersectionObserver" in window) {
  const io = new IntersectionObserver((entries) => {
    let k = 0;
    for (const e of entries) {
      if (!e.isIntersecting) continue;
      io.unobserve(e.target);
      if (e.target === strip) revealStrip();
      else if (e.target === ribbonFig) {
        // the raster strip unrolls as part of the hero intro (from 600 ms), or on arrival later
        ribbonFig.style.setProperty("--rd", `${Math.max(0, Math.round(600 - performance.now()))}ms`);
        ribbonFig.classList.add("in");
      } else revealIn(e.target, k++);
    }
  }, { rootMargin: "0px 0px 12% 0px" }); // start just before the fold, so sections are moving when they arrive
  revealEls.forEach((el) => io.observe(el));
  io.observe(strip);
  if (ribbonFig) io.observe(ribbonFig);
  // The connectome plate scales in and its labels draw themselves when it is well in view.
  const bio = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) { brainSection.classList.add("in"); bio.disconnect(); }
  }, { threshold: 0.28 });
  bio.observe(brainSection);
  root.classList.add("reveal-armed");
} else {
  revealEls.forEach((el) => el.classList.add("in"));
  strip.querySelectorAll(".reveal").forEach((el) => el.classList.add("in"));
  if (ribbonFig) ribbonFig.classList.add("in");
  brainSection.classList.add("in");
}
// Keyboard focus never lands on something still invisible: show it at once.
document.addEventListener("focusin", (e) => {
  const r = e.target.closest && e.target.closest(".reveal");
  if (!r) return;
  if (!r.classList.contains("in")) { r.style.setProperty("--rd", "0ms"); r.classList.add("in", "instant"); return; }
  for (const a of r.getAnimations()) if (a.animationName === "revealUp") a.finish();
});

/* ---------- headline: tag each visual line so lines (not words) rise together ---------- */
const heroTitle = document.getElementById("hero-title");
assignLines(heroTitle);
if (document.fonts && document.fonts.ready) document.fonts.ready.then(() => assignLines(heroTitle));

/* ---------- optional images: hide cleanly if an asset is missing ---------- */
document.querySelectorAll("img[data-optional]").forEach((img) => {
  const hide = () => { img.style.display = "none"; };
  if (img.complete && img.naturalWidth === 0) hide();
  img.addEventListener("error", hide, { once: true });
});

/* ---------- nav state ---------- */
const nav = document.getElementById("nav");
const onNav = () => nav.classList.toggle("scrolled", window.scrollY > 40);
onNav();
window.addEventListener("scroll", onNav, { passive: true });

/* ---------- live status pill from /capabilities ---------- */
async function loadStatus() {
  const pill = document.getElementById("status-pill");
  const text = pill.querySelector(".pill-text");
  const set = (state, label, title) => {
    pill.dataset.state = state;
    text.textContent = label;
    pill.title = title || label;
  };
  let res;
  try {
    res = await fetch("/capabilities", { credentials: "same-origin", headers: { Accept: "application/json" } });
  } catch (err) {
    set("bad", "Status unavailable", `Could not reach /capabilities: ${err.message}`);
    return;
  }
  if (!res.ok) {
    set("bad", `Status error · HTTP ${res.status}`, `GET /capabilities returned HTTP ${res.status}`);
    return;
  }
  let cap;
  try {
    cap = await res.json();
  } catch (err) {
    set("bad", "Status unreadable", `GET /capabilities returned invalid JSON: ${err.message}`);
    return;
  }
  const engineReady = cap.datasets_ready === true && cap.worker_ready === true;
  const model = cap.llm_model || "planner";
  if (!engineReady) {
    const why = cap.datasets_ready !== true ? "connectome data missing" : "simulator binary missing";
    set("bad", `Engine offline · ${why}`);
  } else if (cap.llm_ready !== true) {
    set("warn", "Engine ready · planner offline", "Simulation works. The language planner is unavailable, so plans come from the heuristic parser or the manual form.");
  } else {
    set("ok", `Engine ready · ${model}`, `Simulator and connectome loaded. Planner: ${cap.llm_provider || "claude-cli"} / ${model}`);
  }
}
loadStatus();

/* ---------- teaser composer: hands the prompt to the app ---------- */
const teaser = document.getElementById("teaser");
const teaserInput = document.getElementById("teaser-input");
const examplesForTeaser = [
  "Activate sugar GRNs at 100 Hz and read out MN9",
  "Compare bitter GRN activation with and without silencing the top sugar neuron",
  "Sugar GRNs at 50 Hz — how strongly does MN9 fire?",
  "Ir94e neurons at 80 Hz for 500 ms, three repeats",
];
teaser.addEventListener("submit", (e) => {
  e.preventDefault();
  const prompt = teaserInput.value.trim() || teaserInput.placeholder;
  window.location.href = `/app#/new?p=${encodeURIComponent(prompt)}`;
});
// Placeholder typewriter: cycles through examples; stops (on a complete example) under reduced motion.
const typer = { idx: 0, cycleT: 0, typingT: 0 };
const cycle = () => {
  if (document.hidden || document.activeElement === teaserInput || teaserInput.value) return;
  typer.idx = (typer.idx + 1) % examplesForTeaser.length;
  const target = examplesForTeaser[typer.idx];
  let n = 0;
  clearInterval(typer.typingT);
  typer.typingT = setInterval(() => {
    n += 2;
    teaserInput.placeholder = target.slice(0, n);
    if (n >= target.length) clearInterval(typer.typingT);
  }, 28);
};
const startTyper = () => { if (!typer.cycleT && !isReduced()) typer.cycleT = setInterval(cycle, 4800); };
const stopTyper = () => {
  clearInterval(typer.cycleT); clearInterval(typer.typingT);
  typer.cycleT = typer.typingT = 0;
  teaserInput.placeholder = examplesForTeaser[typer.idx];
};
startTyper();

/* ---------- hero network ---------- */
const hero = document.getElementById("hero");
const netCanvas = document.getElementById("net");
const ribbon = document.getElementById("ribbon");
// A canvas the browser refuses (privacy hardening, no 2D context) must not take the page down:
// the hint under the composer says the live network is unavailable, and why.
let net = null;
try {
  // The ignition wave starts on the brain side of the plate (right on desktop, upper right on phones).
  net = new SpikingNet(netCanvas, {
    ribbon, reduced: isReduced(), ignite: true,
    igniteOrigin: window.innerWidth <= 760 ? { x: 0.72, y: 0.3 } : { x: 0.74, y: 0.48 },
  });
} catch (err) {
  const hint = document.querySelector(".hint");
  hint.classList.add("hint-error");
  hint.replaceChildren(`Live network unavailable in this browser (canvas: ${err.message}).`);
  if (ribbonFig) ribbonFig.hidden = true;
}
let heroVisible = true;
const syncRun = () => {
  if (!net || isReduced()) return;
  if (heroVisible && !document.hidden) net.start(); else net.stop();
};
new IntersectionObserver((entries) => {
  heroVisible = entries[0].isIntersecting;
  syncRun();
}, { threshold: 0 }).observe(hero);
document.addEventListener("visibilitychange", syncRun);
if (net) net.start();

let resizeT;
window.addEventListener("resize", () => {
  clearTimeout(resizeT);
  resizeT = setTimeout(() => { if (!net) return; net.resize(); if (isReduced()) net.start(); }, 180);
});

/* ---------- parallax: scroll + pointer, one rAF loop that idles when settled ----------
   The pointer drives a damped spring (not a plain lerp): layers carry momentum and settle with
   a slight overshoot, so the plate feels like glass on a table rather than a cursor follower.
   Section positions are cached in document coordinates (re-measured only when the page's
   layout changes), so a frame does no layout reads at all: it only writes transforms. */
const layers = [...hero.querySelectorAll(".layer")].map((el) => ({
  el,
  scroll: parseFloat(el.dataset.scroll || "0"),
  pointer: parseFloat(el.dataset.pointer || "0"),
}));
const brainStage = document.querySelector(".brain-stage");
const brainImg = document.getElementById("brain-img");
const brainNotes = document.getElementById("brain-notes");
const examples = document.getElementById("examples");

const target = { x: 0, y: 0 };
const spring = { x: 0, y: 0, vx: 0, vy: 0 };
const K = 0.05;      // stiffness per frame
const DAMP = 0.8;    // velocity kept per frame (~0.5 damping ratio: one soft overshoot)
let rafId = 0;
let lastDrift = null;
let geo = null;
const measureGeo = () => {
  const sy = window.scrollY;
  const b = brainStage.getBoundingClientRect();
  const e = examples.getBoundingClientRect();
  geo = { heroH: hero.offsetHeight, bTop: b.top + sy, bH: b.height, eTop: e.top + sy, eH: e.height };
};
const invalidateGeo = () => { geo = null; schedule(); };

function frame() {
  rafId = 0;
  if (isReduced()) return;
  if (!geo) measureGeo();
  const sy = window.scrollY;
  const vh = window.innerHeight;
  const driftOn = !isLite() && finePointer;
  // integrate
  spring.vx = (spring.vx + (target.x - spring.x) * K) * DAMP;
  spring.vy = (spring.vy + (target.y - spring.y) * K) * DAMP;
  spring.x += spring.vx;
  spring.y += spring.vy;
  // write
  if (sy < geo.heroH * 1.1) {
    for (const l of layers) {
      const x = spring.x * l.pointer;
      const y = sy * l.scroll + spring.y * l.pointer;
      l.el.style.transform = `translate3d(${x.toFixed(2)}px, ${y.toFixed(2)}px, 0)`;
    }
  }
  const bTop = geo.bTop - sy;
  if (bTop + geo.bH > -100 && bTop < vh + 100) {
    const p = (bTop + geo.bH / 2 - vh / 2) / vh; // -1..1 around centre
    // depth: the plate travels further than its labels, which read as nearer to the viewer
    brainImg.style.transform = `translate3d(0, ${(p * 70).toFixed(2)}px, 0) scale(${(1.06 - Math.abs(p) * 0.04).toFixed(4)})`;
    brainNotes.style.transform = `translate3d(0, ${(p * -14).toFixed(2)}px, 0)`;
  }
  // the raster strip (example gallery) drifts sideways as the page scrolls past it: one
  // `translate` on the strip itself, so no card subtree is restyled
  const eTop = geo.eTop - sy;
  if (driftOn && eTop + geo.eH > 0 && eTop < vh) {
    const prog = (vh - eTop) / (vh + geo.eH); // 0 entering .. 1 leaving
    const d = Math.round((0.5 - prog) * 80);
    if (d !== lastDrift) { strip.style.translate = `${d}px 0`; lastDrift = d; }
  } else if (!driftOn && lastDrift !== null) {
    strip.style.translate = "";
    lastDrift = null;
  }
  const moving = Math.abs(target.x - spring.x) > 0.0004 || Math.abs(target.y - spring.y) > 0.0004 ||
    Math.abs(spring.vx) > 0.0004 || Math.abs(spring.vy) > 0.0004;
  if (moving) schedule();
}
function schedule() { if (!rafId && !isReduced()) rafId = requestAnimationFrame(frame); }
const resetParallax = () => {
  if (rafId) { cancelAnimationFrame(rafId); rafId = 0; }
  target.x = target.y = 0;
  Object.assign(spring, { x: 0, y: 0, vx: 0, vy: 0 });
  for (const l of layers) l.el.style.transform = "";
  brainImg.style.transform = "";
  brainNotes.style.transform = "";
  strip.style.translate = "";
  lastDrift = null;
};

window.addEventListener("scroll", schedule, { passive: true });
window.addEventListener("resize", invalidateGeo);
if ("ResizeObserver" in window) new ResizeObserver(invalidateGeo).observe(document.body);
if (document.fonts && document.fonts.ready) document.fonts.ready.then(invalidateGeo);
if (finePointer) {
  hero.addEventListener("pointermove", (e) => {
    if (isReduced()) return;
    const r = hero.getBoundingClientRect();
    target.x = (e.clientX - r.left) / r.width - 0.5;
    target.y = (e.clientY - r.top) / r.height - 0.5;
    // the cursor is a stimulation electrode for the network; it leaves ripples
    if (net) {
      const cr = netCanvas.getBoundingClientRect();
      net.setPointer(e.clientX - cr.left, e.clientY - cr.top);
    }
    schedule();
  });
  hero.addEventListener("pointerdown", (e) => {
    if (isReduced() || !net || e.target.closest("a, button, input, form")) return;
    const cr = netCanvas.getBoundingClientRect();
    net.pulseAt(e.clientX - cr.left, e.clientY - cr.top);
  });
  hero.addEventListener("pointerleave", () => {
    target.x = 0; target.y = 0;
    if (net) net.setPointer(null);
    schedule();
  });
}
schedule();

/* ---------- example thumbnails ---------- */
const drawThumbs = () => {
  try {
    thumbs.forEach(({ c, ex, i }) => {
      drawRasterThumb(c, { seed: `example-${i}-${ex.p}`, a: ex.a, b: ex.b, rate: ex.hz });
      c.classList.add("drawn"); // thumbnails fade in only once they have pixels
    });
  } catch (err) {
    let note = document.getElementById("strip-error");
    if (!note) {
      note = document.createElement("p");
      note.id = "strip-error";
      note.className = "strip-error mono";
      strip.before(note);
    }
    note.textContent = `Run thumbnails could not be drawn in this browser (canvas: ${err.message}). The examples below still work.`;
    thumbs.forEach(({ c }) => c.classList.add("drawn"));
  }
};
// draw after layout so canvas sizes are known
requestAnimationFrame(drawThumbs);
let thumbT;
window.addEventListener("resize", () => { clearTimeout(thumbT); thumbT = setTimeout(drawThumbs, 200); });

/* ---------- gallery: cards lean toward the pointer (desktop only, max 6 deg) ---------- */
if (finePointer) {
  const MAX = 6;
  thumbs.forEach(({ c }) => {
    const card = c.closest(".ex-card");
    let raf = 0, ev = null;
    const apply = () => {
      raf = 0;
      if (!ev || isLite() || isReduced()) return;
      const r = card.getBoundingClientRect();
      const nx = (ev.clientX - r.left) / r.width;  // 0..1
      const ny = (ev.clientY - r.top) / r.height;
      card.style.transform = `perspective(900px) rotateX(${((0.5 - ny) * 2 * MAX).toFixed(2)}deg) rotateY(${((nx - 0.5) * 2 * MAX).toFixed(2)}deg) translateY(-4px)`;
      card.style.setProperty("--gx", `${(nx * 100).toFixed(1)}%`);
      card.style.setProperty("--gy", `${(ny * 100).toFixed(1)}%`);
    };
    card.addEventListener("pointerenter", () => card.classList.add("tilting"));
    card.addEventListener("pointermove", (e) => {
      if (e.pointerType !== "mouse" || isReduced()) return;
      ev = e;
      if (!raf) raf = requestAnimationFrame(apply);
    });
    card.addEventListener("pointerleave", () => {
      ev = null;
      card.classList.remove("tilting"); // settle back on the soft spring
      card.style.transform = "";
    });
  });
}

/* ---------- magnetic primary CTAs ---------- */
document.querySelectorAll("[data-magnetic]").forEach((el) => magnetic(el, { max: Number(el.dataset.magnetic) || 8 }));

/* ---------- prefers-reduced-motion turned on (or off) mid-session ---------- */
onReducedChange((on) => {
  if (on) {
    stopTyper();
    resetParallax();
    thumbs.forEach(({ c }) => { const card = c.closest(".ex-card"); card.style.transform = ""; card.classList.remove("tilting"); });
    if (net) net.setReduced(true);
  } else {
    startTyper();
    if (net) { net.setReduced(false); syncRun(); }
    schedule();
  }
});
