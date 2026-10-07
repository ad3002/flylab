// FlyLab motion helpers shared by the landing and the app.
// The vocabulary mirrors the tokens in style.css (--dur-*, --ease-*, --stagger).
// Every helper is a no-op (final state, instantly) under prefers-reduced-motion. The preference is
// read live (isReduced()), so turning it on mid-session stops motion from that moment on.

const rmQuery = window.matchMedia("(prefers-reduced-motion: reduce)");
export const isReduced = () => rmQuery.matches;
export const finePointer = window.matchMedia("(hover: hover) and (pointer: fine)").matches;
// Phones and small tablets get lighter effects: no tilt, no magnetism, no drift.
export const isLite = () => window.innerWidth <= 768;

/* Live reduced-motion switch. The <html> flags (.mo = motion allowed, .intro = hero entrance)
   follow the OS setting; pages subscribe to stop their own loops (canvas, typewriter, parallax). */
const reducedSubs = new Set();
export function onReducedChange(cb) { reducedSubs.add(cb); return () => reducedSubs.delete(cb); }
function applyReducedChange() {
  const on = rmQuery.matches;
  const root = document.documentElement;
  root.classList.toggle("mo", !on);
  if (on) root.classList.remove("intro");
  for (const cb of reducedSubs) cb(on);
}
if (rmQuery.addEventListener) rmQuery.addEventListener("change", applyReducedChange);
else if (rmQuery.addListener) rmQuery.addListener(applyReducedChange);

export const EASE = {
  out: "cubic-bezier(0.16, 1, 0.3, 1)",
  ui: "cubic-bezier(0.34, 1.3, 0.64, 1)",
  inout: "cubic-bezier(0.65, 0, 0.35, 1)",
  in: "cubic-bezier(0.4, 0, 1, 1)",
  linear: "linear",
};
export const DUR = { micro: 120, ui: 220, view: 420, enter: 720, long: 1100 };
export const STAGGER = 55;

const nf = new Intl.NumberFormat("en-US");
export const fmtCount = (n) => nf.format(Math.round(n));

/* Fade + rise in. Returns the Animation (or null when reduced). Only opacity/transform. */
export function enter(el, { delay = 0, y = 12, duration = DUR.enter, easing = EASE.out, scale = null } = {}) {
  if (isReduced() || !el || !el.animate) return null;
  const from = `translate3d(0, ${y}px, 0)${scale ? ` scale(${scale})` : ""}`;
  return el.animate([{ opacity: 0, transform: from }, { opacity: 1, transform: "none" }], { duration, delay, easing, fill: "backwards" });
}

/* Staggered entrance of a list of elements. Delay is capped so long lists never feel slow. */
export function stagger(els, { start = 0, step = STAGGER, max = 12, ...opts } = {}) {
  [...els].forEach((el, i) => enter(el, { ...opts, delay: start + Math.min(i, max) * step }));
}

/* Count a number up to its final text. The final string is written verbatim at the end, so the
   resting value is always exactly what the caller formatted (locale separators included).
   Width is reserved up front so surrounding text never reflows while digits grow. */
export function countUp(el, { to = null, duration = 1000, delay = 0, format = fmtCount } = {}) {
  if (!el) return;
  const finalText = el.textContent;
  let target = to;
  let suffix = "";
  if (target === null) {
    const m = finalText.match(/^\s*([\d,]+(?:\.\d+)?)([\s\S]*)$/);
    if (!m) return;
    target = Number(m[1].replace(/,/g, ""));
    suffix = m[2];
  }
  if (isReduced() || !Number.isFinite(target) || target < 10) return;
  const w = el.getBoundingClientRect().width;
  if (getComputedStyle(el).display === "inline") el.style.display = "inline-block";
  el.style.minWidth = `${Math.ceil(w)}px`;
  el.style.fontVariantNumeric = "tabular-nums";
  // Assistive tech reads the final value throughout; the rolling digits are presentation only.
  const sr = document.createElement("span");
  sr.className = "sr-only";
  sr.textContent = finalText;
  el.setAttribute("aria-hidden", "true");
  el.after(sr);
  const settle = () => { el.textContent = finalText; el.removeAttribute("aria-hidden"); sr.remove(); };
  el.textContent = `${format(0)}${suffix}`;
  const t0 = performance.now() + delay;
  const tick = (now) => {
    if (!el.isConnected) { sr.remove(); return; }
    const t = Math.min(1, Math.max(0, (now - t0) / duration));
    const k = t === 1 ? 1 : 1 - Math.pow(2, -10 * t); // expo-out: fast start, long settle
    if (t >= 1 || isReduced()) { settle(); return; }
    el.textContent = `${format(target * k)}${suffix}`;
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

/* One-shot visibility trigger. */
export function onceVisible(el, cb, { rootMargin = "0px 0px -10% 0px", threshold = 0 } = {}) {
  if (!("IntersectionObserver" in window)) { cb(el); return null; }
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { io.disconnect(); cb(el); return; }
  }, { rootMargin, threshold });
  io.observe(el);
  return io;
}

/* Wrap each word of el in a mask (.lm > .li) and tag it with its visual line (--ln), so lines
   can rise out of their own masks one after another. Inline child elements are kept whole.
   Punctuation glued to the previous word stays inside that word's mask.
   The heading keeps its accessible name as one string (aria-label); the per-word masks are
   hidden from assistive tech so screen readers do not read the heading word by word. */
export function splitLines(el) {
  if (!el || el.dataset.split) return;
  el.dataset.split = "1";
  el.setAttribute("aria-label", el.textContent.replace(/\s+/g, " ").trim());
  const mask = () => {
    const m = document.createElement("span");
    m.className = "lm";
    m.setAttribute("aria-hidden", "true");
    const i = document.createElement("span");
    i.className = "li";
    m.append(i);
    return m;
  };
  const nodes = [...el.childNodes];
  el.replaceChildren();
  let open = null; // mask still accepting glued content (no whitespace since)
  for (const node of nodes) {
    if (node.nodeType === Node.TEXT_NODE) {
      // split on ordinary whitespace only: no-break spaces keep their words together
      for (const part of node.textContent.split(/([ \t\n\r]+)/)) {
        if (!part) continue;
        if (/^[ \t\n\r]+$/.test(part)) { el.append(" "); open = null; continue; }
        if (!open) { open = mask(); el.append(open); }
        open.firstChild.append(part);
      }
    } else {
      if (!open) { open = mask(); el.append(open); }
      open.firstChild.append(node);
    }
  }
  assignLines(el);
}

export function assignLines(el) {
  let line = -1;
  let lastTop = null;
  el.querySelectorAll(":scope > .lm").forEach((m) => {
    const top = m.offsetTop;
    if (lastTop === null || Math.abs(top - lastTop) > 4) { line++; lastTop = top; }
    m.style.setProperty("--ln", String(line));
  });
}

/* Sliding indicator for segmented controls and tab bars. It follows whichever child carries
   aria-selected / aria-checked / aria-current, by watching those attributes, so callers do not
   need to notify it. Movement is a clip-path inset (paint only, no layout). */
export function segIndicator(seg) {
  if (!seg || seg.querySelector(":scope > .seg-ind")) return;
  const ind = document.createElement("span");
  ind.className = "seg-ind instant";
  ind.setAttribute("aria-hidden", "true");
  seg.prepend(ind);
  seg.classList.add("has-ind");
  const update = () => {
    const on = [...seg.children].find((c) => c !== ind && !c.hidden &&
      (c.getAttribute("aria-selected") === "true" || c.getAttribute("aria-checked") === "true" || c.getAttribute("aria-current") === "page"));
    if (!on || !seg.clientWidth) { ind.style.opacity = "0"; return; }
    // offset* and client* are both measured against the seg's padding box, like the indicator.
    const left = on.offsetLeft;
    const right = seg.clientWidth - (on.offsetLeft + on.offsetWidth);
    const top = on.offsetTop;
    const bottom = seg.clientHeight - (on.offsetTop + on.offsetHeight);
    ind.style.clipPath = `inset(${top}px ${Math.max(0, right)}px ${Math.max(0, bottom)}px ${Math.max(0, left)}px round 999px)`;
    ind.style.opacity = "1";
  };
  update();
  requestAnimationFrame(() => requestAnimationFrame(() => ind.classList.remove("instant")));
  new MutationObserver(update).observe(seg, { subtree: true, attributes: true, attributeFilter: ["aria-selected", "aria-checked", "aria-current", "hidden"] });
  if ("ResizeObserver" in window) new ResizeObserver(update).observe(seg);
  return update;
}

/* Toasts confirm something the user just did. Failures are never toast-only: they stay inline.
   The live region exists (empty) from page load: a region inserted together with its first
   message is often not announced by screen readers. */
let toastHost = null;
function ensureToastHost() {
  if (toastHost && toastHost.isConnected) return toastHost;
  toastHost = document.createElement("div");
  toastHost.className = "toasts";
  toastHost.setAttribute("role", "status");
  toastHost.setAttribute("aria-live", "polite");
  document.body.append(toastHost);
  return toastHost;
}
if (document.body) ensureToastHost();
else document.addEventListener("DOMContentLoaded", ensureToastHost, { once: true });

export function toast(message, { timeout = 2600 } = {}) {
  const host = ensureToastHost();
  const t = document.createElement("div");
  t.className = "toast";
  const s = document.createElement("span");
  s.textContent = message;
  t.append(s);
  host.append(t);
  while (host.childElementCount > 3) host.firstElementChild.remove();
  if (!isReduced()) t.animate([{ opacity: 0, transform: "translate3d(0, 14px, 0) scale(0.96)" }, { opacity: 1, transform: "none" }], { duration: DUR.view, easing: EASE.ui, fill: "backwards" });
  setTimeout(() => {
    if (isReduced()) { t.remove(); return; }
    const a = t.animate([{ opacity: 1 }, { opacity: 0, transform: "translate3d(0, 6px, 0)" }], { duration: DUR.ui, easing: EASE.inout, fill: "forwards" });
    a.onfinish = () => t.remove();
  }, timeout);
  return t;
}

/* Magnetic pull toward the pointer for a primary CTA (desktop, fine pointer only).
   Uses the individual `translate` property so the button's own transforms keep working.
   The button's layout box is cached in document coordinates from the offsetParent chain, which
   ignores transforms: the pull (and its transition) never feeds back into the measurement, and
   a pointer move costs no layout read. The cache is dropped on resize and on any change of the
   page's size; nothing runs while the button is offscreen. */
export function magnetic(el, { strength = 0.28, max = 10, radius = 90 } = {}) {
  if (!el || !finePointer) return;
  let raf = 0;
  let px = 0, py = 0;
  let engaged = false;
  let onscreen = true;
  let box = null;
  const measure = () => {
    let x = 0, y = 0;
    for (let n = el; n; n = n.offsetParent) {
      x += n.offsetLeft; y += n.offsetTop;
      if (n.offsetParent) { x += n.offsetParent.clientLeft; y += n.offsetParent.clientTop; }
    }
    box = { x, y, w: el.offsetWidth, h: el.offsetHeight };
  };
  const release = () => { if (engaged) { engaged = false; el.style.translate = ""; } };
  const apply = () => {
    raf = 0;
    if (isReduced() || isLite() || !onscreen) { release(); return; }
    if (!box) measure();
    const cx = box.x + box.w / 2 - window.scrollX, cy = box.y + box.h / 2 - window.scrollY;
    const dx = px - cx, dy = py - cy;
    const within = Math.abs(dx) < box.w / 2 + radius && Math.abs(dy) < box.h / 2 + radius;
    if (within) {
      engaged = true;
      const tx = Math.max(-max, Math.min(max, dx * strength));
      const ty = Math.max(-max, Math.min(max, dy * strength));
      el.style.translate = `${tx.toFixed(2)}px ${ty.toFixed(2)}px`;
    } else release();
  };
  window.addEventListener("pointermove", (e) => {
    if (e.pointerType !== "mouse" || !onscreen) return;
    px = e.clientX; py = e.clientY;
    if (!raf) raf = requestAnimationFrame(apply);
  }, { passive: true });
  window.addEventListener("resize", () => { box = null; }, { passive: true });
  if ("ResizeObserver" in window) new ResizeObserver(() => { box = null; }).observe(document.body);
  if ("IntersectionObserver" in window) {
    new IntersectionObserver((es) => {
      onscreen = es[es.length - 1].isIntersecting;
      if (!onscreen) release();
    }).observe(el);
  }
  document.addEventListener("pointerleave", release);
}
