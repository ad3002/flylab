import { SpikingNet, drawRasterThumb } from "/static/js/neural.js";

document.documentElement.classList.add("js");
const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
const finePointer = window.matchMedia("(hover: hover) and (pointer: fine)").matches;

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
  "Стимулируй сахарные рецепторы на 50 Гц и посмотри на MN9",
  "Ir94e neurons at 80 Hz for 500 ms, three repeats",
];
teaser.addEventListener("submit", (e) => {
  e.preventDefault();
  const prompt = teaserInput.value.trim() || teaserInput.placeholder;
  window.location.href = `/app#/new?p=${encodeURIComponent(prompt)}`;
});
if (!reduced) {
  let idx = 0;
  let typing = null;
  const cycle = () => {
    if (document.activeElement === teaserInput || teaserInput.value) return;
    idx = (idx + 1) % examplesForTeaser.length;
    const target = examplesForTeaser[idx];
    let n = 0;
    clearInterval(typing);
    typing = setInterval(() => {
      n += 2;
      teaserInput.placeholder = target.slice(0, n);
      if (n >= target.length) clearInterval(typing);
    }, 28);
  };
  setInterval(cycle, 4800);
}

/* ---------- hero network ---------- */
const hero = document.getElementById("hero");
const netCanvas = document.getElementById("net");
const ribbon = document.getElementById("ribbon");
const net = new SpikingNet(netCanvas, { ribbon, reduced });
let heroVisible = true;
const syncRun = () => {
  if (reduced) return;
  if (heroVisible && !document.hidden) net.start(); else net.stop();
};
new IntersectionObserver((entries) => {
  heroVisible = entries[0].isIntersecting;
  syncRun();
}, { threshold: 0 }).observe(hero);
document.addEventListener("visibilitychange", syncRun);
net.start();

let resizeT;
window.addEventListener("resize", () => {
  clearTimeout(resizeT);
  resizeT = setTimeout(() => { net.resize(); if (reduced) net.start(); }, 180);
});

/* ---------- parallax: scroll + pointer, one rAF loop ---------- */
const layers = [...hero.querySelectorAll(".layer")].map((el) => ({
  el,
  scroll: parseFloat(el.dataset.scroll || "0"),
  pointer: parseFloat(el.dataset.pointer || "0"),
}));
const brainStage = document.querySelector(".brain-stage");
const brainImg = document.getElementById("brain-img");
const brainNotes = document.getElementById("brain-notes");

const target = { x: 0, y: 0 };
const cur = { x: 0, y: 0 };
let rafId = 0;

function frame() {
  rafId = 0;
  const sy = window.scrollY;
  cur.x += (target.x - cur.x) * 0.075;
  cur.y += (target.y - cur.y) * 0.075;
  const heroH = hero.offsetHeight;
  if (sy < heroH * 1.1) {
    for (const l of layers) {
      const x = cur.x * l.pointer;
      const y = sy * l.scroll + cur.y * l.pointer;
      l.el.style.transform = `translate3d(${x.toFixed(2)}px, ${y.toFixed(2)}px, 0)`;
    }
  }
  if (brainStage) {
    const r = brainStage.getBoundingClientRect();
    const vh = window.innerHeight;
    if (r.bottom > -100 && r.top < vh + 100) {
      const p = (r.top + r.height / 2 - vh / 2) / vh; // -1..1 around centre
      brainImg.style.transform = `translate3d(0, ${(p * 70).toFixed(2)}px, 0) scale(${(1.06 - Math.abs(p) * 0.04).toFixed(4)})`;
      brainNotes.style.transform = `translate3d(0, ${(p * -14).toFixed(2)}px, 0)`;
    }
  }
  if (Math.abs(target.x - cur.x) > 0.001 || Math.abs(target.y - cur.y) > 0.001) schedule();
}
function schedule() { if (!rafId) rafId = requestAnimationFrame(frame); }

if (!reduced) {
  window.addEventListener("scroll", schedule, { passive: true });
  window.addEventListener("resize", schedule);
  if (finePointer) {
    hero.addEventListener("pointermove", (e) => {
      const r = hero.getBoundingClientRect();
      target.x = (e.clientX - r.left) / r.width - 0.5;
      target.y = (e.clientY - r.top) / r.height - 0.5;
      // the cursor is a stimulation electrode for the network
      const cr = netCanvas.getBoundingClientRect();
      net.setPointer(e.clientX - cr.left, e.clientY - cr.top);
      schedule();
    });
    hero.addEventListener("pointerleave", () => {
      target.x = 0; target.y = 0;
      net.setPointer(null);
      schedule();
    });
  }
  schedule();
}

/* ---------- example gallery ---------- */
const EXAMPLES = [
  { p: "Activate sugar GRNs at 100 Hz and read out MN9", type: "single", hz: 100, ms: 1000, a: 2400, b: null },
  { p: "Compare bitter GRN activation at 150 Hz with and without silencing the top sugar neuron", type: "compare", hz: 150, ms: 500, a: 1800, b: 1650 },
  { p: "Стимулируй сахарные рецепторы на 50 Гц, замолчи один нейрон и сравни MN9", type: "compare", hz: 50, ms: 1000, a: 900, b: 870 },
  { p: "Ir94e neurons at 80 Hz for 500 ms, three repeats", type: "single", hz: 80, ms: 500, a: 640, b: null },
  { p: "Sugar GRNs at 20 Hz — does MN9 respond at all?", type: "single", hz: 20, ms: 1000, a: 120, b: null },
  { p: "Bitter GRNs at 200 Hz for 250 ms, seed 7", type: "single", hz: 200, ms: 250, a: 3100, b: null },
  { p: "Сахарные GRN 200 Гц, 1000 мс, три повтора, читать MN9", type: "single", hz: 200, ms: 1000, a: 5200, b: null },
  { p: "Sugar and Ir94e together at 60 Hz; silence the demo neuron in condition B", type: "compare", hz: 60, ms: 800, a: 1500, b: 1320 },
];
const strip = document.getElementById("strip");
const thumbs = [];
EXAMPLES.forEach((ex, i) => {
  const a = document.createElement("a");
  a.className = "ex-card";
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
const drawThumbs = () => thumbs.forEach(({ c, ex, i }) =>
  drawRasterThumb(c, { seed: `example-${i}-${ex.p}`, a: ex.a, b: ex.b, rate: ex.hz }));
// draw after layout so canvas sizes are known
requestAnimationFrame(drawThumbs);
let thumbT;
window.addEventListener("resize", () => { clearTimeout(thumbT); thumbT = setTimeout(drawThumbs, 200); });

/* ---------- reveal on scroll ---------- */
if (!reduced && "IntersectionObserver" in window) {
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { e.target.classList.add("in"); io.unobserve(e.target); }
  }, { rootMargin: "0px 0px -8% 0px" });
  document.querySelectorAll(".reveal").forEach((el) => io.observe(el));
} else {
  document.querySelectorAll(".reveal").forEach((el) => el.classList.add("in"));
}
