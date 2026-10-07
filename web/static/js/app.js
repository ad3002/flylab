// FlyLab application: auth gate, composer, plan review, live runs, library, account.
// Vanilla ES module. Every failure is shown on screen; nothing is only logged.

import { SpikingNet, SpikeTrace, drawRasterThumb, drawResultRaster, hashString, COLORS } from "/static/js/neural.js?v=v3en";
import { isReduced, onReducedChange, finePointer, EASE, DUR, STAGGER, enter, stagger, countUp, splitLines, segIndicator, toast, onceVisible } from "/static/js/motion.js?v=v3en";

/* ================================================================== */
/* helpers                                                             */
/* ================================================================== */

const $ = (sel, root = document) => root.querySelector(sel);

function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "text") el.textContent = v;
    else if (k === "html") el.innerHTML = v; // static markup only, never user data
    else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "dataset") Object.assign(el.dataset, v);
    else if (k === "style") el.style.cssText = v;
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const fmtInt = (n) => (n === null || n === undefined || Number.isNaN(Number(n)) ? "—" : Number(n).toLocaleString("en-US"));
const fmtHz = (n) => (n === null || n === undefined || Number.isNaN(Number(n)) ? "—" : Number(n).toFixed(1));
const fmtSigned = (n) => (n === null || n === undefined ? "—" : `${n > 0 ? "+" : n < 0 ? "−" : "±"}${Math.abs(n).toFixed(1)}`);

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });
function relTime(iso) {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return String(iso);
  const s = (t - Date.now()) / 1000;
  const abs = Math.abs(s);
  if (abs < 45) return "just now";
  if (abs < 3600) return rtf.format(Math.round(s / 60), "minute");
  if (abs < 86400) return rtf.format(Math.round(s / 3600), "hour");
  if (abs < 86400 * 30) return rtf.format(Math.round(s / 86400), "day");
  return new Date(t).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" });
}
function absTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString("en-GB", { dateStyle: "medium", timeStyle: "short" });
}
function secondsBetween(a, b) {
  if (!a) return null;
  const s = ((b ? new Date(b) : new Date()).getTime() - new Date(a).getTime()) / 1000;
  return Number.isFinite(s) ? Math.max(0, s) : null;
}
const fmtDur = (s) => (s === null ? "—" : s < 60 ? `${s.toFixed(s < 10 ? 1 : 0)} s` : `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`);

function uuid() {
  if (window.crypto && typeof window.crypto.randomUUID === "function") return window.crypto.randomUUID();
  // randomUUID is only exposed in secure contexts; build a v4 UUID from getRandomValues instead.
  const b = window.crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const x = [...b].map((v) => v.toString(16).padStart(2, "0")).join("");
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20)}`;
}

/* ---------- banners ---------- */
function banner(kind, title, body, { meta = null, actions = [] } = {}) {
  return h("div", { class: `banner banner-${kind}`, role: kind === "error" ? "alert" : "status" },
    h("span", { class: "b-mark", "aria-hidden": "true" }),
    h("div", null,
      h("div", { class: "b-title", text: title }),
      body ? h("div", { class: "b-body", text: body }) : null,
      meta ? h("div", { class: "b-meta", text: meta }) : null),
    actions.length ? h("div", { class: "b-actions" }, actions) : h("span"));
}
function errorMeta(err) {
  const bits = [];
  if (err && err.code) bits.push(err.code);
  if (err && err.status) bits.push(`HTTP ${err.status}`);
  if (err && err.requestId) bits.push(`request ${err.requestId}`);
  return bits.join(" · ") || null;
}
function errorBanner(title, err, actions = []) {
  let msg;
  let meta = errorMeta(err);
  if (isNetworkError(err)) {
    msg = `Could not reach the server (${err.message}). Check your connection and try again.`;
  } else if (err instanceof Error && !(err instanceof ApiError)) {
    // A bug or an unexpected response shape inside the page, not a connectivity problem.
    msg = `Something went wrong in the page itself: ${err.name}: ${err.message}. Reload and try again; if it repeats, please report it.`;
    meta = "client error";
  } else {
    msg = (err && err.message) || String(err);
  }
  return banner("error", title, msg, { meta, actions });
}

/* ---------- motion: skeletons, plan assembly, shared-element flight ---------- */
const skelCard = (aspect) => h("div", { class: "skel-card", "aria-hidden": "true" },
  h("div", { class: "skel skel-thumb", style: `aspect-ratio:${aspect}` }),
  h("div", { class: "skel skel-line" }),
  h("div", { class: "skel skel-line short" }));
const skelGrid = (n) => h("div", { class: "skel-grid", "aria-hidden": "true" },
  Array.from({ length: n }, (_, i) => skelCard([4 / 3, 1, 4 / 5, 16 / 10][i % 4])));

// A plan card arrives field by field: frame first, then each section and each spec cell.
function assemble(card) {
  if (isReduced()) return;
  enter(card, { y: 0, scale: 0.985, duration: DUR.view });
  const parts = [];
  for (const child of card.children) {
    if (child.classList.contains("spec-grid")) parts.push(...child.children);
    else parts.push(child);
  }
  stagger(parts, { start: 60, step: 35, max: 18, y: 10, duration: 520 });
}

/* Shared-element flight from a library card to the run page. The card's thumbnail pixels are
   copied into a fixed canvas that flies (transform + clip-path only) onto the run page's art
   box, then dissolves into the real art once that is drawn. Any route that is not the run page,
   or a run page that fails to load, simply fades the ghost away. */
let flip = null;
function flipCapture(canvas) {
  flipDrop();
  if (isReduced() || !canvas.width || !canvas.isConnected) return;
  const r = canvas.getBoundingClientRect();
  if (r.width < 4 || r.bottom < 0 || r.top > window.innerHeight) return;
  const g = document.createElement("canvas");
  g.width = canvas.width;
  g.height = canvas.height;
  g.getContext("2d").drawImage(canvas, 0, 0);
  g.className = "flip-ghost";
  g.setAttribute("aria-hidden", "true");
  Object.assign(g.style, { left: `${r.left}px`, top: `${r.top}px`, width: `${r.width}px`, height: `${r.height}px` });
  document.body.append(g);
  flip = { ghost: g, rect: r, flying: false, landed: false, settle: false, timer: setTimeout(() => flipDrop(), 3000) };
}
function flipFly(target) {
  if (!flip || flip.flying) return;
  // The run may load before this frame; then the skeleton is gone and the real art is the target.
  if (!target || !target.isConnected) target = view.querySelector(".job-art");
  if (!target) { flipDrop(); return; }
  const f = flip;
  const t = target.getBoundingClientRect();
  if (!t.width) { flipDrop(); return; }
  f.flying = true;
  const r = f.rect;
  const s = t.width / r.width;
  const extra = r.height * s - t.height; // >0: source is taller than the target, crop its bottom
  const dy = t.top - r.top + (extra < 0 ? -extra / 2 : 0);
  const radEnd = 22 / s;
  const anim = f.ghost.animate([
    { transform: "translate(0px, 0px) scale(1)", clipPath: "inset(0px 0px 0px 0px round 14px)" },
    { transform: `translate(${t.left - r.left}px, ${dy}px) scale(${s})`, clipPath: `inset(0px 0px ${Math.max(0, extra / s)}px 0px round ${radEnd}px)` },
  ], { duration: 640, easing: EASE.out, fill: "forwards" });
  anim.onfinish = () => { f.landed = true; if (f.settle) flipSettle(); };
}
function flipSettle() {
  if (!flip) return;
  if (!flip.landed) { flip.settle = true; return; }
  flipDrop(280);
}
function flipDrop(duration = 200) {
  if (!flip) return;
  const { ghost, timer } = flip;
  flip = null;
  clearTimeout(timer);
  const a = ghost.animate([{ opacity: 1 }, { opacity: 0 }], { duration, easing: EASE.inout, fill: "forwards" });
  a.onfinish = () => ghost.remove();
  setTimeout(() => ghost.remove(), duration + 200);
}

/* Route crossfade. The outgoing view becomes a fixed, inert ghost that fades up and out while
   the incoming view rises in underneath, so there is never an empty frame and the old view
   never takes input. Ids are stripped from the ghost so lookups find only the new view. */
function swapOut() {
  if (isReduced() || shellEl.hidden || !view.childElementCount) { view.replaceChildren(); return; }
  const r = view.getBoundingClientRect();
  const ghost = h("div", { class: "view view-ghost", "aria-hidden": "true" });
  ghost.inert = true;
  Object.assign(ghost.style, { top: `${r.top}px`, left: `${r.left}px`, width: `${r.width}px` });
  ghost.append(...view.childNodes);
  ghost.querySelectorAll("[id]").forEach((el) => el.removeAttribute("id"));
  shellEl.append(ghost);
  // Sequential, not a double exposure: the old view is gone (140 ms, opacity only) before the
  // new one starts to rise (110 ms in), so two large headlines never overprint each other.
  const a = ghost.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 140, easing: EASE.in, fill: "forwards" });
  a.onfinish = () => ghost.remove();
  setTimeout(() => ghost.remove(), 600);
}
function swapIn() {
  // During a shared-element flight the view only fades, so the flight's target does not move.
  if (flip) enter(view, { y: 0, duration: DUR.view, delay: 110 });
  else enter(view, { y: 14, duration: 520, delay: 110 });
}

/* ================================================================== */
/* API                                                                 */
/* ================================================================== */

class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

const NETWORK_ERROR = "NETWORK_ERROR";
// Only failures of the transport itself are marked as network errors; everything else that
// throws (rendering bugs, unexpected shapes) is reported as what it is.
function networkError(method, path, err) {
  return new ApiError(0, NETWORK_ERROR, `${method} ${path} did not complete: ${(err && err.message) || err}`);
}
function isNetworkError(err) { return err instanceof ApiError && err.code === NETWORK_ERROR; }

async function api(path, { method = "GET", body, headers = {}, gate = true } = {}) {
  const init = {
    method,
    credentials: "same-origin",
    headers: { Accept: "application/json", ...headers },
  };
  // The server's CSRF guard requires JSON on every state-changing request, bodiless ones too.
  if (method !== "GET" && method !== "HEAD") init.headers["Content-Type"] = "application/json";
  if (body !== undefined) init.body = JSON.stringify(body);
  let res, text;
  try {
    res = await fetch(path, init);
    text = await res.text();
  } catch (err) {
    throw networkError(method, path, err);
  }
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (e) {
      throw new ApiError(res.status, "BAD_RESPONSE", `The server answered ${method} ${path} with something that is not JSON (HTTP ${res.status}): ${text.slice(0, 160)}`);
    }
  }
  if (!res.ok) {
    const e = data && data.error ? data.error : {};
    const err = new ApiError(res.status, e.code || `HTTP_${res.status}`, e.message || `${method} ${path} failed with HTTP ${res.status}`, e.request_id || res.headers.get("X-Request-ID"));
    err.details = e.details && typeof e.details === "object" ? e.details : null;
    const ra = Number(res.headers.get("Retry-After"));
    err.retryAfter = Number.isFinite(ra) && ra > 0 ? ra : null;
    if (res.status === 401 && gate) onSessionLost();
    throw err;
  }
  return { data, status: res.status };
}

async function download(url, fallbackName, statusEl, label) {
  const retry = () => [h("button", { class: "btn btn-sm", type: "button", text: "Retry", onclick: () => download(url, fallbackName, statusEl, label) })];
  const fail = (err) => statusEl.replaceChildren(errorBanner(`Download of ${label} failed`, err, retry()));
  statusEl.replaceChildren(h("span", { class: "mono dl-progress", text: `Preparing ${label}…` }));
  let res;
  try {
    res = await fetch(url, { credentials: "same-origin" });
  } catch (err) {
    fail(networkError("GET", url, err));
    return;
  }
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    let code = null;
    let text;
    try {
      text = await res.text();
    } catch (err) {
      fail(networkError("GET", url, err));
      return;
    }
    try {
      const j = JSON.parse(text);
      if (j.error) { msg = j.error.message; code = j.error.code; }
    } catch (e) {
      msg = `HTTP ${res.status}: ${text.slice(0, 160)}`;
    }
    if (res.status === 401) onSessionLost();
    fail(new ApiError(res.status, code, msg));
    return;
  }
  let blob;
  try {
    blob = await res.blob(); // a dropped connection mid-stream rejects here
  } catch (err) {
    fail(networkError("GET", url, err));
    return;
  }
  const cd = res.headers.get("Content-Disposition") || "";
  const m = cd.match(/filename="?([^";]+)"?/);
  const a = h("a", { href: URL.createObjectURL(blob), download: m ? m[1] : fallbackName });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
  const done = `${label} downloaded · ${(blob.size / 1024 / 1024).toFixed(2)} MB`;
  statusEl.replaceChildren(h("span", { class: "mono dl-progress ok", text: done }));
  toast(done);
}

/* ================================================================== */
/* state                                                               */
/* ================================================================== */

const state = {
  me: null,
  caps: null,
  capsError: null,
  groups: null,
  groupsError: null,
  draft: "",
  lang: "auto",
  lastPlan: null,
  lastParseNotice: null,
  aiLang: null, // ru/en picked in an AI panel this session; overrides the per-run default
};
const timers = new Set();
let routeGen = 0;
function later(fn, ms) {
  const id = setTimeout(() => { timers.delete(id); fn(); }, ms);
  timers.add(id);
  return id;
}
function clearTimers() { for (const id of timers) clearTimeout(id); timers.clear(); }

/* Route lifetime. Everything a page starts that outlives its DOM by itself (live traces with
   their observers and listeners, ResizeObservers, document-level listeners) is registered here
   and torn down when the route changes or the account signs out. A trace whose canvas is
   offscreen never sees another frame, so it cannot be relied on to notice it was removed. */
const routeCleanups = new Set();
const liveTraces = new Set();
function onLeave(fn) { routeCleanups.add(fn); return fn; }
function leaveRoute() {
  for (const t of liveTraces) t.destroy();
  liveTraces.clear();
  for (const fn of routeCleanups) fn();
  routeCleanups.clear();
}
function trace(canvas, opts) {
  const t = new SpikeTrace(canvas, { ...opts, reduced: Boolean(opts.reduced) || isReduced() });
  liveTraces.add(t);
  return t;
}

/* ================================================================== */
/* boot, auth gate, shell                                              */
/* ================================================================== */

const bootEl = $("#boot");
const authEl = $("#auth");
const shellEl = $("#shell");
const view = $("#view");
let authNet = null;
let authMode = "login";

async function boot() {
  $("#boot-error").hidden = true;
  $("#boot-text").textContent = "Connecting to the lab…";
  bootEl.hidden = false;
  try {
    const { data } = await api("/api/v1/me", { gate: false });
    state.me = data;
    showShell();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      showAuth();
      return;
    }
    $("#boot-text").textContent = "The lab is not answering.";
    const box = $("#boot-error");
    box.hidden = false;
    box.replaceChildren(errorBanner("Could not load your session", err, [
      h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: boot }),
    ]));
  }
}

// Everything one account typed or planned; must not carry over to the next sign-in in this tab.
function resetPrivateState() {
  state.draft = "";
  state.lastPlan = null;
  state.lang = "auto";
  state.lastParseNotice = null;
  state.aiLang = null;
  aiReset();
}

function onSessionLost() {
  if (authEl.hidden === false) return;
  state.me = null;
  resetPrivateState();
  clearTimers();
  routeGen++;
  showAuth("Your session has ended. Sign in again to continue where you were.");
}

function showAuth(notice) {
  bootEl.hidden = true;
  shellEl.hidden = true;
  leaveRoute();
  view.replaceChildren(); // nothing of the previous account may linger (or crossfade) into the next
  flipDrop(0);
  authEl.hidden = false;
  document.title = "Sign in — FlyLab";
  $("#auth-notice").replaceChildren(notice ? banner("info", notice) : "");
  $("#auth-error").replaceChildren();
  setAuthMode(authMode);
  applyRegistrationPolicy();
  if (!authNet) {
    authNet = new SpikingNet($("#auth-net"), { reduced: isReduced(), seed: 94, ignite: true, igniteOrigin: { x: 0.62, y: 0.42 } });
  } else {
    authNet.resize();
    authNet.igniteWanted = !isReduced(); // every arrival at the sign-in screen re-ignites the field
  }
  authNet.start();
  segIndicator($(".auth-tabs"));
  // The form itself does not fade in: the username field is focused at once, and a focused
  // field (caret, typed characters, focus ring) must never sit inside something at opacity 0.
  const form = $("#auth-form");
  stagger([...$(".auth-box").children].filter((c) => c !== form), { start: 60, step: 50, y: 14, duration: DUR.enter });
  $("#auth-username").focus();
}

// The sign-in art is a live network: the pointer stimulates it, a press fires a volley.
// It idles when the tab is hidden.
(() => {
  const art = $(".auth-art");
  const at = (e) => { const r = $("#auth-net").getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; };
  if (finePointer) {
    art.addEventListener("pointermove", (e) => { if (authNet && !isReduced()) authNet.setPointer(...at(e)); });
    art.addEventListener("pointerleave", () => { if (authNet) authNet.setPointer(null); });
  }
  art.addEventListener("pointerdown", (e) => { if (authNet) authNet.pulseAt(...at(e)); });
  document.addEventListener("visibilitychange", () => {
    if (!authNet || authEl.hidden || isReduced()) return;
    if (document.hidden) authNet.stop(); else authNet.start();
  });
})();

// prefers-reduced-motion switched on mid-session: live traces freeze on their current frame,
// the sign-in field stops on a still frame, a flight in progress is dropped. Switched off: the
// sign-in field resumes (traces stay still until their page is rendered again).
onReducedChange((on) => {
  if (on) {
    for (const t of liveTraces) t.freeze();
    flipDrop(0);
  }
  if (authNet) {
    authNet.setReduced(on);
    if (!on && !authEl.hidden && !document.hidden) authNet.start();
  }
});

// Hide "Create account" when the server says registration is closed (capabilities).
async function applyRegistrationPolicy() {
  let note = $("#auth-reg-note");
  if (!note) {
    note = h("div", { id: "auth-reg-note" });
    $("#auth-notice").after(note);
  }
  note.replaceChildren();
  try {
    const { data } = await api("/capabilities", { gate: false });
    if (!data || typeof data.registration_open !== "boolean") {
      throw new ApiError(200, "BAD_SHAPE", "GET /capabilities did not report registration_open");
    }
    $("#tab-register").hidden = !data.registration_open;
    if (!data.registration_open) {
      if (authMode === "register") setAuthMode("login");
      note.replaceChildren(banner("info", "Accounts are created by the administrator",
        "Registration is closed on this server. Ask the lab administrator for an account, then sign in here."));
    }
  } catch (err) {
    $("#tab-register").hidden = false;
    note.replaceChildren(errorBanner("Could not check whether registration is open", err, [
      h("button", { class: "btn btn-sm", type: "button", text: "Check again", onclick: applyRegistrationPolicy }),
    ]));
  }
}

function setAuthMode(mode) {
  const changed = mode !== authMode;
  authMode = mode;
  const reg = mode === "register";
  $("#tab-login").setAttribute("aria-selected", String(!reg));
  $("#tab-register").setAttribute("aria-selected", String(reg));
  $("#field-display").hidden = !reg;
  $("#auth-title").textContent = reg ? "Create your lab account" : "Sign in to the lab";
  $("#auth-sub").textContent = reg
    ? "Pick a username. Your runs and exports stay private to it."
    : "Your runs, plans and exports live in your account.";
  $("#auth-submit").textContent = reg ? "Create account" : "Sign in";
  $("#auth-password").setAttribute("autocomplete", reg ? "new-password" : "current-password");
  $("#auth-error").replaceChildren();
  if (changed) {
    enter($("#auth-title"), { y: 8, duration: DUR.view });
    enter($("#auth-sub"), { y: 6, duration: DUR.view, delay: 40 });
    if (reg) enter($("#field-display"), { y: -6, duration: DUR.view, delay: 60 });
  }
}
$("#tab-login").addEventListener("click", () => setAuthMode("login"));
$("#tab-register").addEventListener("click", () => setAuthMode("register"));
$(".auth-tabs").addEventListener("keydown", (e) => {
  if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
    if ($("#tab-register").hidden) return;
    setAuthMode(authMode === "login" ? "register" : "login");
    $(authMode === "login" ? "#tab-login" : "#tab-register").focus();
  }
});

$("#auth-username").addEventListener("input", (e) => {
  const v = e.target.value;
  if (v !== v.toLowerCase()) e.target.value = v.toLowerCase();
});

$("#auth-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const errBox = $("#auth-error");
  errBox.replaceChildren();
  const username = $("#auth-username").value.trim().toLowerCase();
  const password = $("#auth-password").value;
  const display = $("#auth-display").value.trim();
  const problems = [];
  if (!/^[a-z0-9_.-]{3,32}$/.test(username)) problems.push("Username must be 3–32 characters of a–z, 0–9, dot, dash or underscore.");
  if (password.length < 8 || password.length > 128) problems.push("Password must be 8–128 characters.");
  if (problems.length) {
    errBox.replaceChildren(banner("error", "Check the form", problems.join(" ")));
    return;
  }
  const btn = $("#auth-submit");
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = authMode === "register" ? "Creating account…" : "Signing in…";
  try {
    const body = authMode === "register" ? { username, password, ...(display ? { display_name: display } : {}) } : { username, password };
    await api(authMode === "register" ? "/api/v1/auth/register" : "/api/v1/auth/login", { method: "POST", body, gate: false });
    const { data } = await api("/api/v1/me", { gate: false });
    state.me = data;
    $("#auth-password").value = "";
    showShell();
  } catch (err) {
    errBox.replaceChildren(errorBanner(authMode === "register" ? "Could not create the account" : "Could not sign in", err));
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
});

function showShell() {
  bootEl.hidden = true;
  authEl.hidden = true;
  if (authNet) authNet.stop();
  shellEl.hidden = false;
  paintIdentity();
  segIndicator($(".tabs"));
  enter($(".topbar"), { y: -8, duration: DUR.enter });
  route(); // loads capabilities too
}

function paintIdentity() {
  const u = (state.me && state.me.user) || {};
  const name = u.display_name || u.username || "?";
  $("#avatar-initial").textContent = name.trim().charAt(0).toUpperCase() || "?";
  $("#menu-name").textContent = name;
  $("#menu-user").textContent = u.username ? `@${u.username}` : "";
}

/* ---------- account menu ---------- */
const avatarBtn = $("#avatar-btn");
const menu = $("#account-menu");
function setMenu(open) {
  menu.hidden = !open;
  avatarBtn.setAttribute("aria-expanded", String(open));
  if (open) {
    if (!isReduced()) menu.animate([{ opacity: 0, transform: "translate3d(0, -6px, 0) scale(0.97)" }, { opacity: 1, transform: "none" }], { duration: DUR.ui, easing: EASE.out });
    menu.querySelector("[role=menuitem]").focus();
  }
}
avatarBtn.addEventListener("click", () => setMenu(menu.hidden));
document.addEventListener("click", (e) => {
  if (!menu.hidden && !e.target.closest(".menu")) setMenu(false);
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !menu.hidden) { setMenu(false); avatarBtn.focus(); }
});
menu.addEventListener("click", (e) => { if (e.target.closest("a")) setMenu(false); });
$("#signout-btn").addEventListener("click", async () => {
  setMenu(false);
  try {
    await api("/api/v1/auth/logout", { method: "POST", gate: false });
    state.me = null;
    resetPrivateState();
    clearTimers();
    routeGen++;
    showAuth("You are signed out.");
  } catch (err) {
    $("#global-banner").replaceChildren(errorBanner("Sign-out failed — you are still signed in", err));
  }
});

/* ---------- capabilities ---------- */
async function loadCaps() {
  const pill = $("#status-pill");
  const text = pill.querySelector(".pill-text");
  const set = (s, label, title) => { pill.dataset.state = s; text.textContent = label; pill.title = title || label; };
  try {
    const { data } = await api("/capabilities", { gate: false });
    state.caps = data;
    state.capsError = null;
    const engine = data.datasets_ready === true && data.worker_ready === true;
    if (data.worker_error) set("bad", "Worker degraded", data.worker_error);
    else if (!engine) set("bad", data.datasets_ready !== true ? "Engine offline · data missing" : "Engine offline · simulator missing");
    else if (data.llm_ready !== true) set("warn", "Planner offline", "The simulator works. Plans come from the heuristic parser or the Advanced form.");
    else set("ok", `Ready · ${data.llm_model || "planner"}`, `Simulator ready. Planner: ${data.llm_provider || "claude-cli"} / ${data.llm_model || ""}`);
  } catch (err) {
    state.capsError = err;
    set("bad", "Status unavailable", err.message);
  }
  const slot = $("#caps-notice");
  if (slot) paintCapsNotice(slot);
  paintWorkerBanner();
}
// Page-level: a degraded worker (e.g. a finished run whose status could not be saved) affects
// every page, so it is shown above the content on all routes until the server recovers.
function paintWorkerBanner() {
  let wb = $("#worker-banner");
  if (!wb) {
    wb = h("div", { id: "worker-banner" });
    $("#global-banner").before(wb);
  }
  const werr = state.caps && state.caps.worker_error;
  wb.replaceChildren(werr ? banner("error", "The simulation worker is in trouble", werr,
    { meta: "Runs may show a stale status until the server is restarted" }) : "");
}
function paintCapsNotice(slot) {
  const c = state.caps;
  if (state.capsError) {
    slot.replaceChildren(errorBanner("Engine status could not be loaded", state.capsError));
  } else if (c && (c.datasets_ready !== true || c.worker_ready !== true)) {
    slot.replaceChildren(banner("error", "The simulation engine is offline", c.datasets_ready !== true
      ? "The connectome data is missing on the server. Plans can be drafted, but runs will fail until it is installed."
      : "The simulator binary is missing on the server. Plans can be drafted, but runs will fail until it is installed."));
  } else if (c && c.llm_ready !== true) {
    slot.replaceChildren(banner("warn", "The language planner is offline", "Generate plan will fall back to a simple keyword parser — review its plan carefully, or use Advanced to set every parameter yourself."));
  } else {
    slot.replaceChildren();
  }
}

/* ---------- groups (registry) ---------- */
let groupsPromise = null;
function loadGroups(force = false) {
  if (groupsPromise && !force) return groupsPromise;
  groupsPromise = api("/groups", { gate: false })
    .then(({ data }) => {
      if (!data || !Array.isArray(data.groups)) throw new ApiError(200, "BAD_SHAPE", "GET /groups did not return a groups array");
      state.groups = data.groups;
      state.groupsError = null;
      return data.groups;
    })
    .catch((err) => {
      state.groupsError = err;
      groupsPromise = null;
      throw err;
    });
  return groupsPromise;
}
const groupById = (id) => (state.groups || []).find((g) => g.group_id === id);

/* ================================================================== */
/* router                                                              */
/* ================================================================== */

function parseHash() {
  const raw = location.hash.replace(/^#/, "") || "/new";
  const [path, qs] = raw.split("?");
  const parts = path.split("/").filter(Boolean);
  return { name: parts[0] || "new", id: parts[1] ? decodeURIComponent(parts[1]) : null, params: new URLSearchParams(qs || "") };
}

function route() {
  if (!state.me) return;
  clearTimers();
  leaveRoute();
  routeGen++;
  $("#global-banner").replaceChildren();
  loadCaps();
  const r = parseHash();
  document.querySelectorAll(".tabs a").forEach((a) => {
    const on = a.dataset.route === r.name || (r.name === "job" && a.dataset.route === "history");
    if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  if (r.name !== "job") flipDrop();
  swapOut();
  window.scrollTo(0, 0);
  if (r.name === "new") renderNew(r.params);
  else if (r.name === "history") renderHistory();
  else if (r.name === "job" && r.id) renderJobPage(r.id, r.params);
  else if (r.name === "account") renderAccount();
  else renderMissing();
  swapIn();
}
window.addEventListener("hashchange", route);

function renderMissing() {
  document.title = "Not found — FlyLab";
  view.append(h("section", { class: "empty-state" },
    h("p", { class: "eyebrow", text: "404" }),
    h("h1", { class: "display empty-title", text: "There is nothing at this address." }),
    h("p", { class: "muted", text: `“${location.hash}” is not a page in the lab.` }),
    h("a", { class: "btn btn-primary", href: "#/new", text: "Go to Create" })));
}

/* ================================================================== */
/* plan helpers                                                        */
/* ================================================================== */

const sel = (entry) => (entry && (entry.selector || entry)) || {};
function selLabel(s) {
  if (s.group_id) {
    const g = groupById(s.group_id);
    return g ? g.name_en : s.group_id;
  }
  if (Array.isArray(s.neuron_ids) && s.neuron_ids.length) return `${s.neuron_ids.length} listed neuron${s.neuron_ids.length === 1 ? "" : "s"}`;
  return "unspecified";
}
const plural = (n, w) => `${fmtInt(n)} ${w}${n === 1 ? "" : "s"}`;

function planShort(plan) {
  if (!plan) return "";
  const act = (plan.activation || []).map((a) => `${sel(a).group_id || "custom"} ${a.rate_hz} Hz`).join(" + ");
  const ro = (plan.readout || []).map((r) => sel(r).group_id || "custom").join(", ");
  return `${act || "no stimulus"} → ${ro || "all"}${plan.experiment_type === "compare_silencing" ? " · A/B" : ""}`;
}

function planSentence(plan, resolved) {
  const frag = document.createDocumentFragment();
  const b = (t) => h("b", { text: t });
  const acts = plan.activation || [];
  frag.append("Stimulate ");
  acts.forEach((a, i) => {
    const n = resolved && resolved.activation && resolved.activation[i] ? resolved.activation[i].neuron_ids.length : null;
    if (i > 0) frag.append(" and ");
    frag.append(b(selLabel(sel(a))), n !== null ? ` (${plural(n, "neuron")})` : "", " at ", b(`${a.rate_hz} Hz`));
  });
  if (!acts.length) frag.append(b("nothing"));
  const sil = plan.silencing || [];
  if (plan.experiment_type === "compare_silencing" && sil.length) {
    const n = resolved ? (resolved.silencing_neuron_ids || []).length : null;
    frag.append(", compare against a run where ", b(sil.map((s) => selLabel(sel(s))).join(" and ")), n !== null ? ` (${plural(n, "neuron")})` : "", " is silenced");
  }
  const ro = plan.readout || [];
  const nro = resolved ? (resolved.readout_neuron_ids || []).length : null;
  frag.append(", and read out ", b(ro.length ? ro.map((r) => selLabel(sel(r))).join(" and ") : "every neuron"), nro !== null ? ` (${plural(nro, "neuron")})` : "",
    " for ", b(`${plan.duration_ms} ms`), ".");
  return frag;
}

function autoTitle(prompt, plan) {
  const src = (prompt || "").trim() || planShort(plan);
  return src.length > 80 ? `${src.slice(0, 77)}…` : src;
}

/* ================================================================== */
/* job submission with one network retry, same Idempotency-Key         */
/* ================================================================== */

async function submitJob(body, key, statusSlot) {
  const call = () => api("/api/v1/jobs", { method: "POST", body, headers: { "Idempotency-Key": key } });
  try {
    return (await call()).data;
  } catch (err) {
    if (!isNetworkError(err)) throw err;
    statusSlot.replaceChildren(banner("info", "Connection dropped — retrying…", "Sending the same request again with the same idempotency key, so it cannot start twice."));
    const data = (await call()).data; // a second failure propagates to the caller
    statusSlot.replaceChildren();
    return data;
  }
}

/* run a plan: creates the job, then hands over to the live job panel. Resolves true once a job
   exists, false when the attempt failed (the error and a Retry with the same key are shown). */
function startRun({ planId, prompt, title, slot, onCreated, key = uuid() }) {
  const status = h("div", { class: "run-status" });
  slot.replaceChildren(status);
  const body = { plan_id: planId };
  if (prompt) body.prompt = prompt.slice(0, 4000);
  if (title) body.title = title.slice(0, 120);
  const attempt = async () => {
    status.replaceChildren(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: "Starting the run…" })));
    let data;
    try {
      data = await submitJob(body, key, status);
      if (!data || !data.job || !data.job.job_id) throw new ApiError(200, "BAD_SHAPE", "POST /api/v1/jobs answered without a job id");
      status.replaceChildren();
    } catch (err) {
      status.replaceChildren(errorBanner("The run did not start", err, [
        h("button", { class: "btn btn-sm", type: "button", text: "Retry", onclick: attempt }),
      ]));
      return false;
    }
    onCreated(data.job); // outside the request try: a rendering bug is not "did not start"
    return true;
  };
  return attempt();
}

// Disables btn while a launch is in flight and reuses one Idempotency-Key for the same
// plan + title until a job has been created, so double clicks and slow retries cannot start
// a second job.
function launchGuard(btn) {
  let pending = null; // { sig, key }
  return async (sig, launch) => {
    if (btn.disabled) return;
    if (!pending || pending.sig !== sig) pending = { sig, key: uuid() };
    btn.disabled = true;
    try {
      if (await launch(pending.key)) pending = null;
    } finally {
      btn.disabled = false;
    }
  };
}

/* ================================================================== */
/* #/new — composer                                                    */
/* ================================================================== */

const CHIPS = [
  "Activate sugar GRNs at 100 Hz and read out MN9",
  "Compare bitter GRN activation with and without silencing the top sugar neuron, read out MN9",
  "Ir94e neurons at 80 Hz for 500 ms, three repeats",
  "Sugar GRNs at 50 Hz, silence one sugar neuron and compare MN9",
];

function detectLang(text) { return /[а-яё]/i.test(text) ? "ru" : "en"; }

function renderNew(params) {
  document.title = "Create — FlyLab";
  const gen = routeGen;
  const incoming = params.get("p");
  if (incoming) {
    state.draft = incoming.slice(0, 4000);
    history.replaceState(null, "", "#/new");
  }

  const textarea = h("textarea", {
    id: "prompt", rows: "2", maxlength: "4000", placeholder: "Describe an experiment…",
    "aria-label": "Describe an experiment", spellcheck: "true",
  });
  textarea.value = state.draft;
  const counter = h("span", { class: "count mono", "aria-live": "off" });
  const langBtns = ["auto", "en", "ru"].map((l) => h("button", {
    type: "button", role: "radio", "aria-checked": String(state.lang === l), "data-lang": l,
    text: l === "auto" ? "Auto" : l.toUpperCase(),
    title: l === "auto" ? "Report language follows the prompt" : `Report in ${l === "en" ? "English" : "Russian"}`,
  }));
  const langSeg = h("div", { class: "seg seg-sm", role: "radiogroup", "aria-label": "Report language" }, langBtns);
  langSeg.addEventListener("click", (e) => {
    const b = e.target.closest("[data-lang]");
    if (!b) return;
    state.lang = b.dataset.lang;
    langBtns.forEach((x) => x.setAttribute("aria-checked", String(x === b)));
  });
  const advToggle = h("button", { type: "button", class: "btn btn-ghost btn-sm", "aria-expanded": "false", "aria-controls": "advanced", text: "Advanced" });
  const genBtn = h("button", { type: "submit", class: "btn btn-primary gen-btn" }, h("span", { text: "Generate plan" }), h("span", { class: "arrow", "aria-hidden": "true", text: "→" }));

  const composer = h("form", { class: "composer", id: "composer", novalidate: true },
    textarea,
    h("div", { class: "composer-bar" },
      h("div", { class: "composer-left" }, langSeg, advToggle),
      h("div", { class: "composer-right" }, counter, genBtn)));

  const chips = h("div", { class: "chips", role: "group", "aria-label": "Example prompts" },
    CHIPS.map((c) => h("button", {
      type: "button", class: "chip", text: c,
      onclick: () => {
        textarea.value = c; state.draft = c; sync(); textarea.focus();
        composer.classList.remove("inject");
        void composer.offsetWidth; // restart the glow pulse on repeated picks
        composer.classList.add("inject");
      },
    })));

  const advanced = h("section", { class: "advanced", id: "advanced", hidden: true, "aria-label": "Manual plan" });
  const capsSlot = h("div", { id: "caps-notice" });
  const parseSlot = h("div", { class: "parse-slot", "aria-live": "polite" });
  const planSlot = h("div", { class: "plan-slot" });
  const runSlot = h("div", { class: "run-slot" });
  const recent = h("section", { class: "recent", "aria-labelledby": "recent-title" });

  view.append(
    h("section", { class: "create" },
      h("div", { class: "create-head" },
        h("p", { class: "eyebrow", text: "New experiment · FlyWire v630" }),
        h("h1", { class: "display create-title", text: "What should the fly brain do?" })),
      composer, chips, capsSlot, advanced, parseSlot, planSlot, runSlot),
    recent);
  paintCapsNotice(capsSlot);
  segIndicator(langSeg);
  const createTitle = view.querySelector(".create-title");
  if (!isReduced()) {
    splitLines(createTitle);
    createTitle.classList.add("split-reveal");
    requestAnimationFrame(() => createTitle.classList.add("in"));
  }
  composer.addEventListener("animationend", () => composer.classList.remove("inject"));

  function sync() {
    state.draft = textarea.value;
    counter.textContent = `${textarea.value.length} / 4000`;
    textarea.style.height = "auto";
    textarea.style.height = `${Math.min(320, textarea.scrollHeight)}px`;
  }
  textarea.addEventListener("input", sync);
  textarea.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); composer.requestSubmit(); }
  });
  requestAnimationFrame(sync);

  advToggle.addEventListener("click", () => {
    const open = advanced.hidden;
    advanced.hidden = !open;
    advToggle.setAttribute("aria-expanded", String(open));
    advToggle.classList.toggle("on", open);
    if (open && !advanced.childElementCount) buildAdvanced(advanced, showPlan);
    if (open) enter(advanced, { y: 10, duration: DUR.enter });
  });

  composer.addEventListener("submit", async (e) => {
    e.preventDefault();
    const prompt = textarea.value.trim();
    if (!prompt) {
      parseSlot.replaceChildren(banner("info", "Describe the experiment first", "Say what to stimulate, at what rate, and which neurons to read out — or pick an example below the box."));
      textarea.focus();
      return;
    }
    const lang = state.lang === "auto" ? detectLang(prompt) : state.lang;
    genBtn.disabled = true;
    genBtn.setAttribute("aria-busy", "true");
    runSlot.replaceChildren();
    const started = performance.now();
    const elapsed = h("span", { class: "mono", text: "0 s" });
    const traceCanvas = h("canvas", { class: "drafting-trace", "aria-hidden": "true" });
    // The draft is a plan-card-shaped plate in the plan's own slot, so the real card replaces it
    // in the same place (the page does not jump) and the user is already looking there.
    const skelBar = (cls) => h("div", { class: `skel ${cls}` });
    const drafting = h("article", { class: "drafting plan-skel", "aria-hidden": "true" },
      h("header", { class: "drafting-head" },
        traceCanvas,
        h("div", null,
          h("p", { class: "drafting-title", text: "Drafting a plan from your description" }),
          h("p", { class: "drafting-sub" }, "Matching neuron groups and checking limits · ", elapsed))),
      skelBar("skel-quote"),
      h("div", { class: "skel-sentence" }, skelBar("skel-sent"), skelBar("skel-sent short")),
      h("div", { class: "spec-grid" }, Array.from({ length: 8 }, () => h("div", { class: "spec" }, skelBar("skel-dt"), skelBar("skel-dd")))),
      h("div", { class: "skel-actions" }, skelBar("skel-input"), skelBar("skel-btn")));
    planSlot.replaceChildren(drafting);
    // the persistent live region announces the wait (the plate itself is decorative)
    parseSlot.replaceChildren(h("p", { class: "sr-only", text: "Drafting a plan from your description…" }));
    // three live membrane traces: the planner "thinking" (stops itself once this card is gone)
    trace(traceCanvas, { rows: 3, activity: 0.4, noise: 1.3, seed: hashString(prompt) });
    enter(drafting, { y: 8, duration: DUR.view });
    // Give the result the stage: if the plate (and the card that will replace it) would not fit
    // below the composer, bring it up under the top bar.
    const dr = drafting.getBoundingClientRect();
    if (dr.top < 72 || dr.top + 540 > window.innerHeight) drafting.scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" });
    const tick = setInterval(() => { elapsed.textContent = `${Math.round((performance.now() - started) / 1000)} s`; }, 500);
    let parsed;
    try {
      ({ data: parsed } = await api("/api/v1/plans/parse", { method: "POST", body: { prompt, dataset_id: "flywire_630", report_language: lang } }));
    } catch (err) {
      clearInterval(tick);
      if (gen !== routeGen) return;
      planSlot.replaceChildren();
      const titles = { 502: "The planner failed on this request", 503: "The planner is busy", 429: "Planner limit reached", 401: "You are signed out" };
      parseSlot.replaceChildren(errorBanner(titles[err.status] || "Could not generate a plan", err, [
        h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: () => composer.requestSubmit() }),
        h("button", { class: "btn btn-sm btn-ghost", type: "button", text: "Use Advanced", onclick: () => { if (advanced.hidden) advToggle.click(); advanced.scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" }); } }),
      ]));
      return;
    } finally {
      clearInterval(tick);
      genBtn.disabled = false;
      genBtn.removeAttribute("aria-busy");
    }
    if (gen !== routeGen) { storeParse(parsed, prompt); return; }
    try {
      handleParse(parsed, prompt);
    } catch (err) {
      planSlot.replaceChildren();
      parseSlot.replaceChildren(errorBanner("The plan could not be displayed", err));
    }
  });

  function storeParse(data, prompt) {
    if (data && data.status === "ready" && data.resolved_plan) {
      state.lastPlan = normalizePlan(data, "parse", prompt);
    }
  }

  function handleParse(data, prompt) {
    parseSlot.replaceChildren();
    planSlot.replaceChildren(); // the drafting plate; a ready plan paints its card here below
    if (!data || typeof data.status !== "string") {
      parseSlot.replaceChildren(banner("error", "The planner answered in an unexpected shape", "No status field in the response from /api/v1/plans/parse."));
      return;
    }
    if (data.status === "needs_input") {
      parseSlot.replaceChildren(h("div", { class: "notice-card" },
        h("p", { class: "eyebrow", text: "Needs more detail" }),
        h("p", { class: "notice-msg", text: data.message || "The planner needs more information." }),
        (data.unresolved_fields || []).length ? h("div", { class: "tags" }, data.unresolved_fields.map((f) => h("span", { class: "tag mono", text: f }))) : null,
        h("p", { class: "muted small", text: "Add the missing details to your description and generate again." })));
      enter(parseSlot.firstChild, { y: 10 });
      textarea.focus();
      return;
    }
    if (data.status === "unsupported") {
      parseSlot.replaceChildren(h("div", { class: "notice-card" },
        h("p", { class: "eyebrow", text: "Outside what the simulator does" }),
        h("p", { class: "notice-msg", text: data.message || "This request cannot be expressed as a stimulation experiment." }),
        h("p", { class: "muted small", text: "FlyLab stimulates and silences neuron groups and reads out spike rates. Whole-animal behaviour such as walking or flight is out of scope." })));
      enter(parseSlot.firstChild, { y: 10 });
      return;
    }
    if (data.status !== "ready") {
      parseSlot.replaceChildren(banner("error", "Unknown planner status", `The planner returned status “${data.status}”.`));
      return;
    }
    if (!data.resolved_plan || !data.resolved_plan.plan_id || !data.plan) {
      parseSlot.replaceChildren(banner("error", "The plan came back incomplete", "Status was ready but the response has no resolved plan id, so it cannot be run. Try again or use Advanced."));
      return;
    }
    showPlan(normalizePlan(data, "parse", prompt));
  }

  function showPlan(p) {
    state.lastPlan = p;
    paintPlan(true);
  }

  // fresh=true: a newly drafted plan assembles itself; a repaint (group names arrived) does not.
  function paintPlan(fresh = false) {
    const p = state.lastPlan;
    if (!p) return;
    if (!state.groups && !state.groupsError) loadGroups().then(() => { if (gen === routeGen && state.lastPlan === p) paintPlan(); }).catch(() => { if (gen === routeGen && state.lastPlan === p) paintPlan(); });
    planSlot.replaceChildren(planCard(p, {
      onRun: (title, key) => startRun({
        planId: p.planId, prompt: p.prompt, title, slot: runSlot, key,
        onCreated: (job) => {
          runSlot.replaceChildren();
          mountJob(runSlot, job, {
            compact: true,
            onStatus: (j, prev) => { if (j.status !== prev) refreshRecentCard(recent, j); },
            // the run is done: offer to ask Claude what it might mean, right under its results
            onSucceeded: (j) => { const slot = h("div", { class: "ai-slot" }); runSlot.append(slot); aiCta(slot, j); },
          });
          loadRecent(recent, gen);
          runSlot.scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" });
        },
      }),
    }));
    if (!fresh || isReduced()) return;
    // The card assembles field by field where the user can see it: at once when it is in view,
    // otherwise (held invisible) the moment a third of it scrolls into view.
    const card = planSlot.firstChild;
    const r = card.getBoundingClientRect();
    if (r.top < window.innerHeight * 0.75 && r.bottom > 0) { assemble(card); return; }
    card.classList.add("pending-assemble");
    onLeave(() => io && io.disconnect());
    const io = onceVisible(card, () => { card.classList.remove("pending-assemble"); assemble(card); }, { rootMargin: "0px", threshold: 0.35 });
  }

  if (state.lastPlan) paintPlan();
  loadRecent(recent, gen);
  if (!incoming && !state.lastPlan) textarea.focus({ preventScroll: true });
}

function normalizePlan(data, source, prompt) {
  return {
    planId: (data.resolved_plan && data.resolved_plan.plan_id) || data.plan_id,
    planHash: (data.resolved_plan && data.resolved_plan.plan_hash) || data.plan_hash,
    plan: data.plan,
    resolved: data.resolved_plan,
    defaults: data.defaults_applied || null,
    budget: data.budget || null,
    meta: data.llm_metadata || null,
    llmError: data.llm_error || null,
    message: data.message || "",
    source,
    prompt: prompt || "",
  };
}

function sourceLabel(p) {
  if (p.source === "manual") return "Manual plan";
  const m = p.meta || {};
  if (m.source === "claude") {
    const bits = [`Drafted by Claude${m.model ? ` · ${m.model}` : ""}`];
    if (typeof m.duration_ms === "number") bits.push(`${(m.duration_ms / 1000).toFixed(1)} s`);
    if (typeof m.cost_usd === "number") bits.push(`$${m.cost_usd.toFixed(3)}`);
    return bits.join(" · ");
  }
  if (m.source === "heuristic_fallback") return "Heuristic parser (planner unavailable)";
  return m.source ? `Planner: ${m.source}` : "Planner";
}

function planCard(p, { onRun }) {
  const { plan, resolved } = p;
  const rows = [];
  const dl = (k, v) => rows.push(h("div", { class: "spec" }, h("dt", { text: k }), h("dd", null, v)));
  dl("Experiment", plan.experiment_type === "compare_silencing" ? "A vs B — baseline and silenced" : "Single condition");
  (plan.activation || []).forEach((a, i) => {
    const n = resolved && resolved.activation && resolved.activation[i] ? resolved.activation[i].neuron_ids.length : null;
    dl(i === 0 ? "Stimulated" : "Also stimulated", h("span", null, selLabel(sel(a)), h("span", { class: "dim", text: ` · ${n !== null ? plural(n, "neuron") : "?"} · ${a.rate_hz} Hz` })));
  });
  if (plan.experiment_type === "compare_silencing") {
    const n = resolved ? (resolved.silencing_neuron_ids || []).length : null;
    dl("Silenced in B", h("span", null, (plan.silencing || []).map((s) => selLabel(sel(s))).join(", ") || "—", h("span", { class: "dim", text: ` · ${n !== null ? plural(n, "neuron") : "?"} · outgoing synapses` })));
  }
  const nro = resolved ? (resolved.readout_neuron_ids || []).length : null;
  dl("Read out", h("span", null, (plan.readout || []).map((r) => selLabel(sel(r))).join(", ") || "—", h("span", { class: "dim", text: ` · ${nro !== null ? plural(nro, "neuron") : "?"}` })));
  dl("Duration", `${plan.duration_ms} ms${resolved && resolved.dt_ms ? ` · ${Math.round(plan.duration_ms / resolved.dt_ms).toLocaleString("en-US")} steps` : ""}`);
  dl("Repeats", String(plan.repeats));
  dl("Seed", String(plan.base_seed));
  if (p.budget && typeof p.budget.estimated_wall_seconds === "number") dl("Estimated time", `≈ ${p.budget.estimated_wall_seconds.toFixed(1)} s`);

  const defaults = p.defaults && Object.keys(p.defaults).length
    ? h("div", { class: "defaults" }, h("span", { class: "eyebrow", text: "Filled with defaults" }),
      Object.entries(p.defaults).map(([k, v]) => h("span", { class: "tag mono", text: `${k} = ${typeof v === "object" ? JSON.stringify(v) : v}` })))
    : null;

  const titleInput = h("input", { class: "title-input", maxlength: "120", placeholder: autoTitle(p.prompt, plan) || "Title this run", "aria-label": "Run title" });
  const runBtn = h("button", { class: "btn btn-primary run-btn", type: "button" }, h("span", { text: "Run simulation" }), h("span", { class: "arrow", "aria-hidden": "true", text: "→" }));
  const guard = launchGuard(runBtn);
  runBtn.addEventListener("click", () => {
    const title = titleInput.value.trim() || autoTitle(p.prompt, plan);
    guard(`${p.planId}\u0000${title}`, (key) => onRun(title, key));
  });

  return h("article", { class: "plan-card", "aria-label": "Experiment plan" },
    p.llmError ? banner("warn", "Planner unavailable — heuristic plan, review carefully", p.llmError) : null,
    state.groupsError ? banner("warn", "Neuron group names could not be loaded; showing group ids", state.groupsError.message) : null,
    h("header", { class: "plan-head" },
      h("p", { class: "eyebrow" }, "Plan ", h("span", { class: "mono plan-id", text: p.planId })),
      h("span", { class: `source-tag${p.meta && p.meta.source === "heuristic_fallback" ? " warn" : ""}`, text: sourceLabel(p) })),
    p.prompt ? h("blockquote", { class: "plan-prompt", text: p.prompt }) : null,
    h("p", { class: "plan-sentence" }, planSentence(plan, resolved)),
    p.message && p.source !== "manual" ? h("p", { class: "plan-message", text: p.message }) : null,
    h("dl", { class: "spec-grid" }, rows),
    defaults,
    h("p", { class: "plan-note muted small", text: "Silencing follows the upstream code: a silenced neuron still receives input but its outgoing synapses are zeroed." }),
    h("div", { class: "plan-actions" }, titleInput, runBtn),
    h("details", { class: "json" }, h("summary", { text: "Plan JSON" }), h("pre", { class: "mono", text: JSON.stringify(resolved || plan, null, 2) })));
}

/* ---------- advanced manual form ---------- */
function buildAdvanced(root, onPlan) {
  const status = h("div");
  root.replaceChildren(h("p", { class: "muted small", text: "Loading neuron groups…" }));
  loadGroups().then((groups) => paint(groups)).catch((err) => {
    root.replaceChildren(errorBanner("Neuron groups could not be loaded", err, [
      h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: () => buildAdvanced(root, onPlan) }),
    ]));
  });

  function paint(groups) {
    const opts = (def) => groups.map((g) => h("option", { value: g.group_id, selected: g.group_id === def ? true : null, text: `${g.name_en} (${g.neuron_ids.length})` }));
    const field = (label, control, hint) => h("label", { class: "field" }, h("span", { class: "field-label", text: label }), control, hint ? h("span", { class: "field-hint", text: hint }) : null);
    const type = h("select", { id: "f-type" }, h("option", { value: "single", text: "Single condition" }), h("option", { value: "compare_silencing", text: "Compare: baseline vs silenced" }));
    const act = h("select", { id: "f-act" }, opts("sugar_grn"));
    const rate = h("input", { id: "f-rate", type: "number", min: "0", max: "200", step: "1", value: "100", inputmode: "decimal" });
    const rateRange = h("input", { type: "range", min: "0", max: "200", step: "1", value: "100", "aria-label": "Stimulation rate slider" });
    rateRange.addEventListener("input", () => { rate.value = rateRange.value; });
    rate.addEventListener("input", () => { rateRange.value = rate.value; });
    const sil = h("select", { id: "f-sil" }, opts("demo_silencing"));
    const ro = h("select", { id: "f-ro" }, opts("mn9"));
    const dur = h("input", { id: "f-dur", type: "number", min: "10", max: "1000", step: "10", value: "1000" });
    const rep = h("input", { id: "f-rep", type: "number", min: "1", max: "3", step: "1", value: "1" });
    const seed = h("input", { id: "f-seed", type: "number", min: "0", max: "2147483645", step: "1", value: "42" });
    const dice = h("button", { type: "button", class: "btn btn-ghost btn-sm", text: "Random", onclick: () => { seed.value = String(Math.floor(Math.random() * 2147483645)); } });
    const silField = field("Silence in condition B", sil, "Outgoing synapses of these neurons are zeroed.");
    const syncType = () => { silField.hidden = type.value !== "compare_silencing"; };
    type.addEventListener("change", syncType);
    syncType();
    const submit = h("button", { type: "submit", class: "btn btn-primary", text: "Validate plan" });
    const form = h("form", { class: "adv-form", novalidate: true },
      h("div", { class: "adv-grid" },
        field("Experiment", type),
        field("Stimulate", act),
        h("div", { class: "field" }, h("span", { class: "field-label", text: "Rate, Hz (0–200)" }), h("div", { class: "rate-row" }, rateRange, rate)),
        silField,
        field("Read out", ro),
        field("Duration, ms (10–1000)", dur),
        field("Repeats (1–3)", rep),
        h("div", { class: "field" }, h("span", { class: "field-label", text: "Seed" }), h("div", { class: "seed-row" }, seed, dice))),
      status,
      h("div", { class: "adv-actions" }, submit));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const problems = [];
      const r = Number(rate.value), d = Number(dur.value), n = Number(rep.value), s = Number(seed.value);
      if (!(r >= 0 && r <= 200)) problems.push("Rate must be between 0 and 200 Hz.");
      if (!(d >= 10 && d <= 1000)) problems.push("Duration must be between 10 and 1000 ms.");
      if (!(Number.isInteger(n) && n >= 1 && n <= 3)) problems.push("Repeats must be 1, 2 or 3.");
      if (!(Number.isInteger(s) && s >= 0 && s <= 2147483645)) problems.push("Seed must be a whole number from 0 to 2147483645.");
      if (problems.length) { status.replaceChildren(banner("error", "Check the parameters", problems.join(" "))); return; }
      const plan = {
        schema_version: "1.0",
        dataset_id: "flywire_630",
        model_id: "shiu_lif_rust",
        experiment_type: type.value,
        activation: [{ selector: { group_id: act.value }, rate_hz: r }],
        silencing: type.value === "compare_silencing" ? [{ selector: { group_id: sil.value } }] : [],
        readout: [{ selector: { group_id: ro.value } }],
        duration_ms: d,
        repeats: n,
        base_seed: s,
        report_language: state.lang === "ru" ? "ru" : "en",
      };
      submit.disabled = true;
      submit.textContent = "Validating…";
      status.replaceChildren();
      try {
        const { data } = await api("/api/v1/plans/validate", { method: "POST", body: plan, gate: false });
        if (!data || !data.plan_id) throw new ApiError(200, "BAD_SHAPE", "POST /api/v1/plans/validate answered without a plan_id");
        onPlan(normalizePlan(data, "manual", ""));
        $(".plan-slot").scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" });
      } catch (err) {
        status.replaceChildren(errorBanner("The plan did not pass validation", err));
      } finally {
        submit.disabled = false;
        submit.textContent = "Validate plan";
      }
    });
    root.replaceChildren(h("header", { class: "adv-head" },
      h("p", { class: "eyebrow", text: "Advanced" }),
      h("p", { class: "muted small", text: "Set every parameter yourself. The plan is checked by the same validator the planner uses." })), form);
  }
}

/* ---------- recent strip on #/new ---------- */
async function loadRecent(root, gen) {
  root.replaceChildren(h("div", { class: "recent-head" }, h("h2", { class: "section-title", id: "recent-title", text: "Recent runs" }), h("a", { href: "#/history", class: "link", text: "Open library →" })));
  const grid = h("div", { class: "recent-grid" }, Array.from({ length: 3 }, () => skelCard(4 / 3)));
  root.append(grid);
  try {
    const { data } = await api("/api/v1/jobs?limit=6&offset=0");
    if (gen !== routeGen) return;
    const jobs = (data && data.jobs) || [];
    if (!jobs.length) {
      grid.replaceWith(h("p", { class: "muted small recent-empty", text: "Your runs will collect here — each one gets its own raster fingerprint." }));
      return;
    }
    grid.replaceChildren(...jobs.map((j) => jobCard(j, { aspect: 4 / 3 })));
    stagger(grid.children, { step: STAGGER, y: 16, duration: 640 });
    requestAnimationFrame(() => grid.querySelectorAll("canvas").forEach((c) => c._draw && c._draw()));
  } catch (err) {
    if (gen !== routeGen) return;
    grid.replaceWith(errorBanner("Recent runs could not be loaded", err));
  }
}

// The in-flight run's entry in "Recent runs" follows its status (queued -> running -> done), so
// the strip never shows a stale "Queued" next to a run that is visibly running.
function refreshRecentCard(root, job) {
  const old = [...root.querySelectorAll(".job-card")].find((c) => c.dataset.jobId === job.job_id);
  if (!old) return;
  const card = jobCard(job, { aspect: 4 / 3 });
  old.replaceWith(card);
  requestAnimationFrame(() => { const c = card.querySelector("canvas"); if (c && c._draw) c._draw(); });
}

/* ================================================================== */
/* job cards + library                                                 */
/* ================================================================== */

const STATUS_LABEL = { queued: "Queued", running: "Running", cancelling: "Cancelling", succeeded: "Done", failed: "Failed", cancelled: "Cancelled" };
const statusChip = (s) => h("span", { class: `status status-${s}`, text: STATUS_LABEL[s] || s });

function thumbFor(job) {
  const s = job.summary || null;
  const a = s ? s.total_spikes_A : null;
  const b = s && s.total_spikes_B !== undefined ? s.total_spikes_B : null;
  const rate = job.plan && job.plan.activation && job.plan.activation[0] ? job.plan.activation[0].rate_hz : null;
  return { seed: `${job.job_id}|${a}|${b}`, a, b, status: job.status, rate };
}

function jobCard(job, { aspect }) {
  const c = h("canvas", { "aria-hidden": "true", style: `aspect-ratio:${aspect}` });
  c._draw = () => { drawRasterThumb(c, thumbFor(job)); c.classList.add("drawn"); };
  const s = job.summary;
  const title = job.title || job.prompt || planShort(job.plan) || job.job_id;
  const counts = s
    ? h("span", { class: "mono card-counts" }, h("i", { class: "sw sw-a", "aria-hidden": "true" }), `A ${fmtInt(s.total_spikes_A)}`,
      s.total_spikes_B !== null && s.total_spikes_B !== undefined ? [h("i", { class: "sw sw-b", "aria-hidden": "true" }), ` B ${fmtInt(s.total_spikes_B)}`] : null)
    : null;
  const errs = [job.plan_error ? `Plan unreadable: ${job.plan_error}` : null, job.summary_error ? `Summary unreadable: ${job.summary_error}` : null].filter(Boolean);
  const marker = job.plan_error ? "Plan unreadable" : "Summary unreadable";
  // a plain click flies the thumbnail into the run page (modified clicks open normally)
  const onclick = (e) => { if (e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey) flipCapture(c); };
  const thumbTop = h("div", { class: "thumb-top" }, statusChip(job.status));
  // The card is a frame holding the link (thumbnail + text) and, for finished runs, the AI action
  // row: a button may not live inside a link, and the action must not open the run.
  const link = h("a", { class: "job-link", href: `#/job/${encodeURIComponent(job.job_id)}`, onclick, "aria-label": `${title} — ${STATUS_LABEL[job.status] || job.status}${errs.length ? ` — ${marker}` : ""}` },
    h("div", { class: "job-thumb" }, c,
      thumbTop,
      errs.length ? h("span", { class: "thumb-error", title: errs.join("\n"), text: errs.length > 1 ? "Plan + summary unreadable" : marker }) : null),
    h("div", { class: "job-body" },
      h("p", { class: "job-title", text: title }),
      errs.map((e) => h("p", { class: "job-err mono", text: e })),
      h("p", { class: "job-meta mono" }, counts, h("span", { text: relTime(job.created_at), title: absTime(job.created_at) }))));
  return h("div", { class: `job-card${errs.length ? " has-error" : ""}`, dataset: { jobId: job.job_id } }, link, cardAi(job, thumbTop, title));
}

class Masonry {
  constructor(root) {
    this.root = root;
    this.items = [];
    this.n = 0;
    this.ro = new ResizeObserver(() => this.layout());
    this.ro.observe(root);
  }
  count() {
    const w = this.root.clientWidth || window.innerWidth;
    return Math.max(1, Math.min(5, Math.floor((w + 16) / (250 + 16))));
  }
  layout(force = false) {
    const n = this.count();
    if (n === this.n && !force) return;
    this.n = n;
    this.cols = Array.from({ length: n }, () => h("div", { class: "mcol" }));
    this.heights = new Array(n).fill(0);
    this.root.replaceChildren(...this.cols);
    for (const it of this.items) this.place(it);
    requestAnimationFrame(() => this.items.forEach((it) => it.draw()));
  }
  place(it) {
    let i = 0;
    for (let k = 1; k < this.n; k++) if (this.heights[k] < this.heights[i] - 0.01) i = k;
    this.cols[i].append(it.el);
    this.heights[i] += 1 / it.aspect + 0.5;
  }
  add(el, aspect, draw) {
    if (!this.n) this.layout();
    const it = { el, aspect, draw };
    this.items.push(it);
    this.place(it);
    requestAnimationFrame(draw);
  }
  disconnect() { this.ro.disconnect(); }
}

function renderHistory() {
  document.title = "Library — FlyLab";
  const gen = routeGen;
  let offset = 0, total = null, filter = "";
  // The load in flight for the current filter (null = none). A filter change abandons it and
  // starts a new load at once; a stale response sees it is no longer current and drops itself.
  let inflight = null;
  const countEl = h("p", { class: "muted lib-count", "aria-live": "polite" });
  const filters = [["", "All"], ["succeeded", "Done"], ["running", "Running"], ["queued", "Queued"], ["failed", "Failed"]];
  const filterBtns = filters.map(([v, l]) => h("button", { type: "button", role: "radio", "aria-checked": String(v === filter), text: l, "data-v": v }));
  const seg = h("div", { class: "seg seg-sm", role: "radiogroup", "aria-label": "Filter by status" }, filterBtns);
  const grid = h("div", { class: "masonry" });
  const foot = h("div", { class: "lib-foot" });
  const sentinel = h("div", { class: "sentinel", "aria-hidden": "true" });
  view.append(h("section", { class: "library" },
    h("header", { class: "lib-head" },
      h("div", null, h("p", { class: "eyebrow", text: "Library" }), h("h1", { class: "display page-title", text: "Your runs" }), countEl),
      h("div", { class: "lib-tools" }, seg, h("a", { class: "btn btn-primary btn-sm", href: "#/new", text: "New experiment" }))),
    grid, foot, sentinel));
  let masonry = new Masonry(grid);
  onLeave(() => masonry.disconnect());
  segIndicator(seg);
  if (!isReduced()) {
    const t = view.querySelector(".page-title");
    splitLines(t);
    t.classList.add("split-reveal");
    requestAnimationFrame(() => t.classList.add("in"));
  }

  seg.addEventListener("click", (e) => {
    const b = e.target.closest("[data-v]");
    if (!b || b.dataset.v === filter) return;
    filter = b.dataset.v;
    filterBtns.forEach((x) => x.setAttribute("aria-checked", String(x === b)));
    masonry.disconnect();
    grid.replaceChildren();
    masonry = new Masonry(grid);
    offset = 0; total = null;
    countEl.textContent = "";
    inflight = null;
    load();
  });

  async function load() {
    if (inflight || (total !== null && offset >= total)) return;
    const mine = { filter };
    inflight = mine;
    // skeleton cards hold the space the incoming page will take; the text stays for screen readers
    foot.replaceChildren(h("div", { class: "lib-loading" },
      skelGrid(offset ? 4 : 8),
      h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: offset ? "Loading more runs…" : "Loading your runs…" }))));
    const myFilter = filter;
    try {
      const q = `/api/v1/jobs?limit=24&offset=${offset}${filter ? `&status=${encodeURIComponent(filter)}` : ""}`;
      const { data } = await api(q);
      if (gen !== routeGen || inflight !== mine || myFilter !== filter) return;
      if (!data || !Array.isArray(data.jobs)) throw new ApiError(200, "BAD_SHAPE", "GET /api/v1/jobs did not return a jobs array");
      total = typeof data.total === "number" ? data.total : offset + data.jobs.length + (data.jobs.length === 24 ? 1 : 0);
      data.jobs.forEach((j, k) => {
        const aspect = [4 / 3, 1, 4 / 5, 16 / 10][hashString(j.job_id) % 4];
        const card = jobCard(j, { aspect });
        const c = card.querySelector("canvas");
        masonry.add(card, aspect, () => c._draw());
        // every page (first or infinite-scroll) arrives as a short cascade
        enter(card, { delay: Math.min(k, 12) * 45, y: 18, duration: 640 });
      });
      offset += data.jobs.length;
      if (data.jobs.length === 0) total = offset;
      countEl.textContent = total === 0 ? "" : `${fmtInt(offset)} of ${fmtInt(total)} run${total === 1 ? "" : "s"}${filter ? ` · ${STATUS_LABEL[filter] || filter}` : ""}`;
      if (total === 0) {
        foot.replaceChildren(emptyLibrary(filter));
      } else if (offset < total) {
        foot.replaceChildren(h("button", { class: "btn", type: "button", text: "Load more", onclick: load }));
      } else {
        foot.replaceChildren(h("p", { class: "muted small end-note", text: "That’s every run." }));
      }
    } catch (err) {
      if (gen !== routeGen || inflight !== mine) return;
      foot.replaceChildren(errorBanner("Your runs could not be loaded", err, [h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: load })]));
    } finally {
      if (inflight === mine) inflight = null;
    }
  }
  const io = new IntersectionObserver((entries) => {
    if (gen !== routeGen) { io.disconnect(); return; }
    if (entries[0].isIntersecting && total !== null && offset < total) load();
  }, { rootMargin: "600px 0px" });
  io.observe(sentinel);
  onLeave(() => io.disconnect());
  load();
}

function emptyLibrary(filter) {
  const c = h("canvas", { class: "empty-art", "aria-hidden": "true" });
  requestAnimationFrame(() => drawRasterThumb(c, { seed: "empty-library", a: 20, b: null, status: "queued" }));
  return h("div", { class: "empty-state" }, c,
    h("h2", { class: "display empty-title", text: filter ? "No runs with this status." : "No runs yet." }),
    h("p", { class: "muted", text: filter ? "Try another filter, or start a new experiment." : "Describe an experiment and run it — every run lands here with its own raster fingerprint." }),
    h("a", { class: "btn btn-primary", href: "#/new", text: "Describe an experiment" }));
}

/* ================================================================== */
/* live job panel (polling) + results                                  */
/* ================================================================== */

const STAGES = ["queued", "loading", "building", "simulating", "aggregating", "exporting"];
const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

function mountJob(root, job, { compact = false, onStatus = null, onSucceeded = null } = {}) {
  const gen = routeGen;
  // A run that is already over when mounted assembles its stage dots in sequence.
  const stageList = h("ol", { class: "stages", "aria-label": "Run stages" }, STAGES.map((s, i) => h("li", { "data-stage": s, text: s, style: TERMINAL.has(job.status) ? `--si:${i}` : null })));
  const bar = h("div", { class: "bar" });
  const pct = h("span", { class: "mono pct" });
  const elapsed = h("span", { class: "mono" });
  const pulseCanvas = h("canvas", { class: "run-pulse", "aria-hidden": "true" });
  const chipSlot = h("span");
  const cancelBtn = h("button", { class: "btn btn-ghost btn-sm", type: "button", text: "Cancel run" });
  const msg = h("div", { class: "run-msg", "aria-live": "polite" });
  // Errors of user actions live in their own slot: poll() only manages msg.
  const actionMsg = h("div", { class: "run-msg", "aria-live": "assertive" });
  const results = h("div", { class: "results-slot" });
  const live = h("div", { class: "live" },
    h("div", { class: "progress", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": "Run progress" }, bar),
    h("div", { class: "live-row" }, stageList, h("span", { class: "live-nums" }, pulseCanvas, pct, elapsed)));
  const head = h("header", { class: "run-head" },
    h("div", null,
      h("p", { class: "eyebrow" }, compact ? "Run " : "", h("a", { class: "mono", href: `#/job/${encodeURIComponent(job.job_id)}`, text: job.job_id })),
      compact ? h("p", { class: "run-title", text: job.title || job.prompt || planShort(job.plan) || "Untitled run" }) : null),
    h("div", { class: "run-head-end" }, chipSlot, cancelBtn));
  // A run started from Create shows its own fingerprint developing while it runs (blur, exposure
  // and a reveal edge that follow progress; CSS transitions only). On success the final pixels,
  // the same ones its library card and run page show, settle into focus before the results.
  const art = compact ? h("canvas", { "aria-hidden": "true" }) : null;
  const artBox = compact ? h("div", { class: "run-art" }, art) : null;
  const panel = compact
    ? h("article", { class: "run-card compact" }, head, h("div", { class: "run-body" }, artBox, h("div", { class: "run-side" }, live, actionMsg, msg)), results)
    : h("article", { class: "run-card" }, head, live, actionMsg, msg, results);
  root.append(panel);
  // Live activity: a two-unit spiking trace whose drive follows progress; a finished run shows
  // a still frame (no loop at all).
  const over = TERMINAL.has(job.status);
  const pulse = trace(pulseCanvas, { rows: 2, activity: over ? 0.6 : 0.1, reduced: over, seed: hashString(job.job_id) });
  let artFinal = false;
  function develop(j, p) {
    if (!art) return;
    if (!TERMINAL.has(j.status)) {
      if (!art._provisional) {
        const plan = j.plan || {};
        const rate = plan.activation && plan.activation[0] ? plan.activation[0].rate_hz : null;
        // a provisional exposure (the run's own seed, a typical spike count) that the final
        // fingerprint replaces when the run is done
        drawRasterThumb(art, { seed: `${j.job_id}|develop`, a: 1500, b: plan.experiment_type === "compare_silencing" ? 1500 : null, status: "succeeded", rate });
        art._provisional = true;
      }
      artBox.classList.add("developing");
      artBox.style.setProperty("--dev", (j.status === "queued" ? 0 : p / 100).toFixed(3));
      return;
    }
    if (artFinal) return;
    artFinal = true;
    drawRasterThumb(art, thumbFor(j));
    artBox.classList.remove("developing");
    artBox.style.removeProperty("--dev");
    if (j.status === "succeeded" && !isReduced()) {
      art.animate([{ transform: "scale(1.02)" }, { transform: "none" }], { duration: 520, delay: 420, easing: EASE.out });
    }
  }
  // A panel replaced by a newer run (or a re-render) stops polling: nobody can see it.
  const alive = () => gen === routeGen && panel.isConnected;

  let failures = 0;
  let current = job;
  cancelBtn.addEventListener("click", async () => {
    cancelBtn.disabled = true;
    actionMsg.replaceChildren();
    let data;
    try {
      ({ data } = await api(`/api/v1/jobs/${encodeURIComponent(job.job_id)}/cancel`, { method: "POST" }));
    } catch (err) {
      actionMsg.replaceChildren(errorBanner("Cancel failed — the run is still going", err, [
        h("button", { class: "btn btn-sm btn-ghost", type: "button", text: "Dismiss", onclick: () => actionMsg.replaceChildren() }),
      ]));
      cancelBtn.disabled = false;
      return;
    }
    paint(data);
  });

  let lastStatus;
  function paint(j) {
    current = { ...current, ...j };
    const prevStatus = lastStatus;
    lastStatus = current.status;
    chipSlot.replaceChildren(statusChip(current.status));
    const p = Math.max(0, Math.min(100, Number(current.progress_pct) || 0));
    bar.style.transform = `scaleX(${(current.status === "succeeded" ? 100 : p) / 100})`;
    develop(current, p);
    if (TERMINAL.has(current.status)) pulse.freeze();
    else pulse.setActivity(current.status === "queued" ? 0.05 : 0.2 + 0.8 * (p / 100));
    live.querySelector(".progress").setAttribute("aria-valuenow", String(Math.round(p)));
    pct.textContent = `${Math.round(current.status === "succeeded" ? 100 : p)}%`;
    const idx = STAGES.indexOf(current.stage);
    stageList.querySelectorAll("li").forEach((li, i) => {
      li.classList.toggle("done", current.status === "succeeded" || (idx >= 0 && i < idx));
      li.classList.toggle("now", !TERMINAL.has(current.status) && i === idx);
    });
    if (idx < 0 && current.stage && !TERMINAL.has(current.status)) pct.textContent += ` · ${current.stage}`;
    const s = secondsBetween(current.started_at || current.created_at, current.finished_at);
    elapsed.textContent = s === null ? "" : fmtDur(s);
    cancelBtn.hidden = TERMINAL.has(current.status) || current.status === "cancelling";
    if (TERMINAL.has(current.status)) actionMsg.replaceChildren();
    panel.dataset.status = current.status;
    if (onStatus) onStatus(current, prevStatus);
  }

  async function poll() {
    if (!alive()) return;
    try {
      const { data } = await api(`/api/v1/jobs/${encodeURIComponent(job.job_id)}`);
      if (!alive()) return;
      failures = 0;
      msg.replaceChildren();
      paint(data);
      if (data.status === "succeeded") {
        live.classList.add("finished");
        renderResults(results, current, { delay: art && !isReduced() ? 540 : 0 });
        if (onSucceeded) onSucceeded(current);
      } else if (data.status === "failed") {
        live.classList.add("finished");
        msg.replaceChildren(banner("error", "The simulation failed", current.error_message || "The worker reported a failure without a message.", { meta: current.error_code || null }));
      } else if (data.status === "cancelled") {
        live.classList.add("finished");
        msg.replaceChildren(banner("info", "Run cancelled", "Nothing was kept from this run."));
      } else {
        later(poll, secondsBetween(current.created_at) > 30 ? 2000 : 1000);
      }
    } catch (err) {
      if (!alive()) return;
      if (isNetworkError(err)) {
        failures += 1;
        if (failures <= 8) {
          msg.replaceChildren(banner("info", "Lost connection to the server — retrying", `Attempt ${failures} of 8. The run keeps going on the server either way.`));
          later(poll, Math.min(8000, 1000 * 2 ** (failures - 1)));
          return;
        }
      }
      msg.replaceChildren(errorBanner("Progress updates stopped", err, [
        h("button", { class: "btn btn-sm", type: "button", text: "Resume", onclick: () => { failures = 0; msg.replaceChildren(); poll(); } }),
      ]));
    }
  }
  paint(job);
  if (TERMINAL.has(job.status)) {
    live.classList.add("finished");
    if (job.status === "succeeded") {
      renderResults(results, job);
      if (onSucceeded) onSucceeded(job);
    } else if (job.status === "failed") msg.replaceChildren(banner("error", "The simulation failed", job.error_message || "The worker reported a failure without a message.", { meta: job.error_code || null }));
    else msg.replaceChildren(banner("info", "Run cancelled", "Nothing was kept from this run."));
  } else {
    poll();
  }
  return panel;
}

async function renderResults(root, job, { delay = 0 } = {}) {
  const gen = routeGen;
  const id = encodeURIComponent(job.job_id);
  root.replaceChildren(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: "Loading results…" })));
  enter(root.firstChild, { y: 6, duration: DUR.view });
  if (job.summary_error) {
    root.replaceChildren(banner("error", "This run’s summary file is corrupted", job.summary_error, { meta: "summary.json could not be read — totals below may be missing" }));
  }
  let summary;
  try {
    const { data } = await api(`/api/v1/jobs/${id}/results`);
    if (gen !== routeGen) return;
    summary = data && data.summary;
    if (!summary || typeof summary !== "object") throw new ApiError(200, "BAD_SHAPE", "Results response has no summary object");
  } catch (err) {
    if (gen !== routeGen) return;
    root.append(errorBanner("Results could not be loaded", err, [h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: () => renderResults(root, job) })]));
    root.querySelector(".launching") && root.querySelector(".launching").remove();
    return;
  }
  const launching = root.querySelector(".launching");
  if (launching) launching.remove();

  const hasB = summary.total_spikes_B !== null && summary.total_spikes_B !== undefined;
  const tile = (label, value, sw, note) => h("div", { class: "tile" },
    h("p", { class: "tile-label" }, sw ? h("i", { class: `sw ${sw}`, "aria-hidden": "true" }) : null, label),
    h("p", { class: "tile-value", text: value }),
    note ? h("p", { class: "tile-note", text: note }) : null);
  const deltaPct = (a, b) => (a ? `${b - a >= 0 ? "+" : "−"}${Math.abs(((b - a) / a) * 100).toFixed(1)}% vs A` : null);
  const tiles = h("div", { class: "tiles" },
    tile("Spikes · A", fmtInt(summary.total_spikes_A), "sw-a", "baseline"),
    tile("Spikes · B", hasB ? fmtInt(summary.total_spikes_B) : "—", "sw-b", hasB ? deltaPct(summary.total_spikes_A, summary.total_spikes_B) : "single condition"),
    tile("Active neurons · A", fmtInt(summary.active_neurons_count_A), "sw-a", "fired at least once"),
    tile("Active neurons · B", hasB ? fmtInt(summary.active_neurons_count_B) : "—", "sw-b", hasB ? deltaPct(summary.active_neurons_count_A, summary.active_neurons_count_B) : "single condition"));

  // readout table
  const ro = Array.isArray(summary.readout_summary) ? summary.readout_summary : [];
  const maxAbs = Math.max(1e-9, ...ro.map((r) => Math.abs(r.delta_hz || 0)));
  const table = ro.length
    ? h("div", { class: "table-wrap" }, h("table", { class: "ro-table" },
      h("caption", { class: "sr-only", text: "Readout neuron firing rates" }),
      h("thead", null, h("tr", null,
        h("th", { scope: "col", text: "Neuron (root id)" }),
        h("th", { scope: "col", class: "num" }, h("i", { class: "sw sw-a", "aria-hidden": "true" }), "A, Hz"),
        hasB ? h("th", { scope: "col", class: "num" }, h("i", { class: "sw sw-b", "aria-hidden": "true" }), "B, Hz") : null,
        hasB ? h("th", { scope: "col", class: "num", text: "Δ Hz (B − A)" }) : null)),
      h("tbody", null, ro.map((r) => {
        const g = groupById && (state.groups || []).find((gg) => gg.neuron_ids.includes(String(r.root_id)));
        return h("tr", null,
          h("td", null, h("span", { class: "mono", text: r.root_id }), g ? h("span", { class: "dim small", text: ` ${g.name_en}` }) : null),
          h("td", { class: "num mono", text: fmtHz(r.rate_A_hz) }),
          hasB ? h("td", { class: "num mono", text: fmtHz(r.rate_B_hz) }) : null,
          hasB ? h("td", { class: "num mono delta" },
            h("span", { class: "dbar", "aria-hidden": "true" }, h("i", { style: `${r.delta_hz < 0 ? "right:50%;transform-origin:right center" : "left:50%;transform-origin:left center"};width:${(Math.abs(r.delta_hz || 0) / maxAbs) * 50}%;--ri:${ro.indexOf(r)}` })),
            h("span", { text: fmtSigned(r.delta_hz) })) : null);
      }))))
    : h("p", { class: "muted small", text: "This plan had no readout neurons, so there is no readout table." });

  // raster
  // The raster is readable without a mouse: tap a row, or focus it and step with the arrow keys.
  const rasterCanvas = h("canvas", { class: "raster", role: "img", tabindex: "0", "aria-describedby": "raster-row",
    "aria-label": "Spike raster: one row per neuron, condition A in magenta on top, condition B in green below. Tap a row, or use the up and down arrow keys, to read a neuron's spike counts." });
  const tip = h("div", { class: "tip", hidden: true, role: "tooltip" });
  const rowLive = h("p", { class: "sr-only", id: "raster-row", "aria-live": "polite" });
  const rasterNote = h("p", { class: "muted small raster-note" });
  const rasterBody = h("div", { class: "raster-wrap" }, rasterCanvas, tip, rowLive);
  const rasterPanel = h("section", { class: "panel" },
    h("div", { class: "panel-head" },
      h("h3", { class: "panel-title", text: "Raster" }),
      h("p", { class: "legend mono" }, h("span", null, h("i", { class: "sw sw-a" }), "A baseline"), hasB ? h("span", null, h("i", { class: "sw sw-b" }), "B silenced") : null, h("span", null, h("i", { class: "sw sw-ro" }), "readout"))),
    rasterBody, rasterNote);

  // downloads
  const dlStatus = h("div", { class: "dl-status", "aria-live": "polite" });
  const dlBtn = (label, url, name, primary) => h("button", { class: `btn ${primary ? "btn-primary" : "btn-sm"}`, type: "button", onclick: () => download(url, name, dlStatus, label) },
    primary ? [h("span", { text: label }), h("span", { class: "arrow", "aria-hidden": "true", text: "↓" })] : label);
  const downloads = h("section", { class: "downloads" },
    h("div", null,
      h("h3", { class: "panel-title", text: "Take it with you" }),
      h("p", { class: "muted small", text: "The ZIP holds the resolved plan, input events, every spike (Parquet), rates, checksums and replay.sh, which re-runs this exact simulation offline." })),
    h("div", { class: "dl-row" },
      dlBtn("Download ZIP", `/api/v1/jobs/${id}/export`, `flylab_${job.job_id}_export.zip`, true),
      dlBtn("rates.csv", `/api/v1/jobs/${id}/artifacts/rates.csv`, "rates.csv"),
      dlBtn("report.md", `/api/v1/jobs/${id}/artifacts/report.md`, "report.md")),
    dlStatus);

  const readoutPanel = h("section", { class: "panel" }, h("div", { class: "panel-head" }, h("h3", { class: "panel-title", text: "Readout neurons" })), table);
  root.append(h("section", { class: "results" },
    tiles,
    h("div", { class: "results-grid" }, readoutPanel, rasterPanel),
    downloads));
  // Results reveal in reading order: tiles (numbers roll up to the totals), panels, downloads.
  stagger(tiles.children, { start: delay, step: 70, y: 14, duration: DUR.enter });
  tiles.querySelectorAll(".tile-value").forEach((v, i) => countUp(v, { duration: 1100, delay: delay + 120 + i * 70 }));
  stagger([readoutPanel, rasterPanel, downloads], { start: delay + 220, step: 90, y: 18, duration: DUR.enter });
  if (!isReduced()) stagger(readoutPanel.querySelectorAll("tbody tr"), { start: delay + 360, step: 40, y: 0, duration: 500 });

  // spikes for the raster: each condition is fetched on its own. rates.csv lists every A row
  // before any B row, so one unfiltered page would hold no B rows at all for a busy run.
  const RASTER_LIMIT = 10000; // the endpoint maximum; a page covers all rows of typical runs
  try {
    const conds = hasB ? ["A", "B"] : ["A"];
    const pages = await Promise.all(conds.map((c) => api(`/api/v1/jobs/${id}/spikes?format=json&condition=${c}&limit=${RASTER_LIMIT}`)));
    if (gen !== routeGen) return;
    const rows = [];
    const counts = {};
    pages.forEach(({ data }, i) => {
      if (!data || !Array.isArray(data.spikes) || typeof data.total !== "number") throw new ApiError(200, "BAD_SHAPE", `GET /spikes for condition ${conds[i]} did not return spikes and total`);
      rows.push(...data.spikes);
      counts[conds[i]] = { got: data.spikes.length, total: data.total };
    });
    if (!rows.length) {
      rasterBody.replaceChildren(h("p", { class: "muted small empty-raster", text: "No neuron fired in this run, so the raster is empty." }));
      return;
    }
    const missingB = hasB && counts.B.total === 0 && Number(summary.total_spikes_B) > 0;
    const duration = Number(summary.duration_ms || (job.plan && job.plan.duration_ms) || 100);
    const draw = () => drawResultRaster(rasterCanvas, rows, { durationMs: duration, maxNeurons: rasterBody.clientWidth < 520 ? 32 : 60, compare: hasB && !missingB });
    let geo = draw();
    drawIn(rasterCanvas, rasterBody, delay);
    const partial = conds.filter((c) => counts[c].got < counts[c].total).map((c) => `condition ${c}: first ${fmtInt(counts[c].got)} of ${fmtInt(counts[c].total)} rows`);
    rasterNote.textContent = `Showing the ${geo.shown} most active of ${fmtInt(geo.total)} neurons (${conds.map((c) => `${c}: ${fmtInt(counts[c].total)} neuron × trial rows`).join(", ")}). Each row shows that neuron’s spike count; tick positions inside the window are illustrative — exact spike times are in spikes.parquet in the ZIP.${partial.length ? ` Only part of the data is drawn (${partial.join("; ")}).` : ""}`;
    if (missingB) {
      rasterPanel.insertBefore(banner("error", "Condition B is missing from the raster",
        `The summary reports ${fmtInt(summary.total_spikes_B)} spikes in B, but rates.csv returned no B rows. Only condition A is drawn.`), rasterBody);
    }
    // The observer's first callback arrives right after observe(): redraw only for a real width
    // change, not a second full raster pass in the job page's first frames.
    let drawnW = rasterBody.clientWidth;
    const ro = new ResizeObserver(() => {
      const w = rasterBody.clientWidth;
      if (w === drawnW) return;
      drawnW = w;
      geo = draw();
      if (sel >= geo.neurons.length) sel = geo.neurons.length - 1;
      if (!tip.hidden && sel >= 0) showRow(sel, null, null);
    });
    ro.observe(rasterBody);
    onLeave(() => ro.disconnect());
    let sel = -1; // row shown by a tap or the keyboard (-1: none)
    const rowText = (n) => `${n.readout ? "readout · " : ""}A ${fmtInt(n.A)} spikes${n.hasB ? ` · B ${fmtInt(n.B)}` : ""}`;
    // x, y: where to put the tip, relative to the canvas; null = beside the row
    function showRow(i, x, y) {
      const n = geo.neurons[i];
      if (!n) { tip.hidden = true; return; }
      tip.hidden = false;
      tip.replaceChildren(h("b", { class: "mono", text: n.root_id }), h("span", { text: rowText(n) }));
      const w = rasterCanvas.clientWidth;
      const rowY = geo.top + i * geo.rowH;
      const tx = Math.max(0, Math.min((x === null ? 8 : x + 14), w - 220));
      const ty = y === null ? rowY + geo.rowH + 6 : y + 12;
      tip.style.transform = `translate(${tx}px, ${ty}px)`;
    }
    const rowAt = (clientY) => Math.floor((clientY - rasterCanvas.getBoundingClientRect().top - geo.top + 1) / geo.rowH);
    rasterCanvas.addEventListener("pointermove", (e) => {
      if (e.pointerType !== "mouse") return;
      const r = rasterCanvas.getBoundingClientRect();
      showRow(rowAt(e.clientY), e.clientX - r.left, e.clientY - r.top);
    });
    rasterCanvas.addEventListener("pointerleave", (e) => { if (e.pointerType === "mouse" && sel < 0) tip.hidden = true; });
    // touch / pen: a tap shows that row's counts until the next tap elsewhere
    rasterCanvas.addEventListener("pointerdown", (e) => {
      if (e.pointerType === "mouse") return;
      const r = rasterCanvas.getBoundingClientRect();
      const i = rowAt(e.clientY);
      if (!geo.neurons[i]) { tip.hidden = true; sel = -1; return; }
      sel = i;
      showRow(i, e.clientX - r.left, e.clientY - r.top);
    });
    const outside = (e) => { if (sel >= 0 && !rasterBody.contains(e.target)) { sel = -1; tip.hidden = true; } };
    document.addEventListener("pointerdown", outside);
    onLeave(() => document.removeEventListener("pointerdown", outside));
    rasterCanvas.addEventListener("keydown", (e) => {
      const last = geo.neurons.length - 1;
      let i = sel;
      if (e.key === "ArrowDown") i = Math.min(last, sel + 1);
      else if (e.key === "ArrowUp") i = Math.max(0, sel < 0 ? 0 : sel - 1);
      else if (e.key === "Home") i = 0;
      else if (e.key === "End") i = last;
      else if (e.key === "Escape") { sel = -1; tip.hidden = true; return; }
      else return;
      e.preventDefault();
      sel = i;
      showRow(i, null, null);
      const n = geo.neurons[i];
      rowLive.textContent = `Row ${i + 1} of ${last + 1}: neuron ${n.root_id}, ${rowText(n)}`;
    });
    rasterCanvas.addEventListener("blur", () => { sel = -1; tip.hidden = true; });
  } catch (err) {
    if (gen !== routeGen) return;
    rasterBody.replaceChildren(errorBanner("Spike data could not be loaded", err));
  }
}

// The raster records itself: a playhead sweeps left to right at constant speed (it is a time
// axis, so the easing is linear) and the canvas is uncovered behind it.
function drawIn(canvas, wrap, delay = 0) {
  if (isReduced()) return;
  const dur = 820;
  canvas.animate([{ clipPath: "inset(0 100% 0 0)" }, { clipPath: "inset(0 0% 0 0)" }], { duration: dur, delay, easing: EASE.linear, fill: "backwards" });
  const head = h("span", { class: "playhead", "aria-hidden": "true" });
  wrap.append(head);
  const w = canvas.clientWidth;
  const fadeMs = 180; // the head reaches the right edge exactly when the reveal does, then fades
  const a = head.animate([
    { transform: "translateX(0px)", opacity: 1 },
    { transform: `translateX(${w - 2}px)`, opacity: 1, offset: dur / (dur + fadeMs) },
    { transform: `translateX(${w - 2}px)`, opacity: 0 },
  ], { duration: dur + fadeMs, delay, easing: EASE.linear, fill: "both" });
  a.onfinish = () => head.remove();
}

/* ================================================================== */
/* #/job/<id>                                                          */
/* ================================================================== */

async function renderJobPage(jobId, params = new URLSearchParams()) {
  document.title = "Run — FlyLab";
  const gen = routeGen;
  const root = h("section", { class: "job-page" });
  view.append(h("a", { class: "back link", href: "#/history", text: "← Library" }), root);
  // Skeleton in the page's real geometry, so a thumbnail flying in from the library has a target.
  const skelArt = h("div", { class: "job-art skel skel-art" });
  root.append(h("div", { class: "launching sr-only" }, h("span", { text: "Loading run…" })),
    h("div", { class: "job-hero", "aria-hidden": "true" },
      skelArt,
      h("div", { class: "job-info" },
        h("div", { class: "skel skel-line short", style: "margin:0;width:30%" }),
        h("div", { class: "skel skel-title" }),
        h("div", { class: "skel skel-title", style: "width:55%" }),
        h("div", { class: "skel skel-block" }))));
  requestAnimationFrame(() => flipFly(skelArt));
  let job;
  try {
    const { data } = await api(`/api/v1/jobs/${encodeURIComponent(jobId)}`);
    job = data;
    if (!job || !job.job_id) throw new ApiError(200, "BAD_SHAPE", "Job response has no job_id");
  } catch (err) {
    if (gen !== routeGen) return;
    if (err instanceof ApiError && err.status === 404) {
      root.replaceChildren(h("div", { class: "empty-state" },
        h("p", { class: "eyebrow", text: "Not found" }),
        h("h1", { class: "display empty-title", text: "This run isn’t in your library." }),
        h("p", { class: "muted", text: "It may belong to another account, or the link is wrong." }),
        h("a", { class: "btn btn-primary", href: "#/history", text: "Back to your runs" })));
    } else {
      root.replaceChildren(errorBanner("The run could not be loaded", err, [h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: route })]));
    }
    flipDrop();
    return;
  }
  if (gen !== routeGen) return;
  const title = job.title || job.prompt || planShort(job.plan) || job.job_id;
  document.title = `${title.slice(0, 60)} — FlyLab`;

  const art = h("canvas", { class: "hero-thumb", "aria-hidden": "true" });
  const againSlot = h("div");
  const againBtn = h("button", { class: "btn btn-primary", type: "button" }, h("span", { text: "Run again" }), h("span", { class: "arrow", "aria-hidden": "true", text: "→" }));
  const againGuard = launchGuard(againBtn);
  againBtn.addEventListener("click", () => {
    const t = job.title || autoTitle(job.prompt, job.plan);
    againGuard(`${job.plan_id}\u0000${t}`, (key) => startRun({
      planId: job.plan_id, prompt: job.prompt || "", title: t, slot: againSlot, key,
      onCreated: (nj) => { location.hash = `#/job/${encodeURIComponent(nj.job_id)}`; },
    }));
  });
  const copyStatus = h("span", { class: "mono small copy-status", "aria-live": "polite" });
  const copyBtn = h("button", { class: "btn btn-sm", type: "button", text: "Copy link" });
  copyBtn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(location.href);
      // The inline live region (always in the DOM) carries the confirmation for screen readers;
      // the toast is the visual echo.
      copyStatus.textContent = "Link copied";
      later(() => { if (copyStatus.textContent === "Link copied") copyStatus.textContent = ""; }, 4000);
      toast("Link copied");
    } catch (err) {
      copyStatus.textContent = `Copy failed: ${err.message}`;
    }
  });

  const metaRow = (k, v) => h("div", { class: "spec" }, h("dt", { text: k }), h("dd", null, v));
  const plan = job.plan;
  // Hero parts that follow the live panel's polling (status chip, thumbnail, run time).
  const heroChip = h("span", null, statusChip(job.status));
  const runTimeDd = h("dd", { text: job.finished_at ? fmtDur(secondsBetween(job.started_at || job.created_at, job.finished_at)) : "" });
  const runTimeRow = h("div", { class: "spec", hidden: !job.finished_at }, h("dt", { text: "Run time" }), runTimeDd);
  let heroStatus = job.status;
  // AI hypotheses: a panel below the results, and a prominent action in the hero that leads to it
  // (and starts an interpretation when the run has none yet). Both appear once the run succeeded.
  const aiSlot = h("div", { class: "ai-slot" });
  const aiBtn = h("button", { class: "btn ai-hero-btn", type: "button", hidden: true });
  let aiPanel = null;
  // Labelled in the language of the stored hypotheses once there are some (the panel switches
  // to it too), else in the default language.
  const paintAiBtn = (has, lang) => {
    const L = aiT(lang || (has && job.interpretation_language) || aiDefaultLang(job));
    aiBtn.dataset.has = String(has);
    aiBtn.replaceChildren(h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { lang: L.code, text: has ? L.viewHyp : L.interpret }));
  };
  paintAiBtn(job.has_interpretation === true);
  const ensureAi = (opts = {}) => {
    aiBtn.hidden = false;
    if (!aiPanel) aiPanel = mountAiPanel(aiSlot, job, { ...opts, onHas: paintAiBtn });
    return aiPanel;
  };
  aiBtn.addEventListener("click", () => {
    const p = ensureAi();
    if (aiBtn.dataset.has !== "true") p.start();
    p.reveal();
  });
  const onSucceeded = () => ensureAi({ scroll: params.get("ai") === "1" });
  const onStatus = (j) => {
    if (j.finished_at) {
      runTimeRow.hidden = false;
      runTimeDd.textContent = fmtDur(secondsBetween(j.started_at || j.created_at, j.finished_at));
    }
    if (j.status === heroStatus) return;
    heroStatus = j.status;
    heroChip.replaceChildren(statusChip(j.status));
    drawRasterThumb(art, thumbFor({ ...job, ...j }));
  };
  const planMissing = job.plan_error
    ? banner("error", "This run’s stored plan is unreadable", job.plan_error, { meta: "The plan exists on the server but could not be loaded" })
    : banner("warn", "Plan details are missing for this run", "The server returned no plan and no plan_error for this run.");
  // A registry failure is shown as a banner on this page; group ids are displayed instead of names.
  const groupsErr = state.groups ? null : await loadGroups().then(() => null, (err) => err);
  if (gen !== routeGen) { flipDrop(); return; }
  root.replaceChildren(...[
    groupsErr ? banner("warn", "Neuron group names could not be loaded; showing group ids", groupsErr.message) : null,
    h("div", { class: "job-hero" },
      h("div", { class: "job-art" }, art),
      h("div", { class: "job-info" },
        h("div", { class: "job-info-top" }, heroChip, h("span", { class: "mono dim", text: job.job_id })),
        h("h1", { class: "display job-h1", text: title }),
        job.prompt ? h("blockquote", { class: "plan-prompt", text: job.prompt }) : null,
        plan ? h("p", { class: "plan-sentence" }, planSentence(plan, null)) : planMissing,
        h("dl", { class: "spec-grid compact" },
          metaRow("Created", h("span", { title: absTime(job.created_at), text: `${relTime(job.created_at)} · ${absTime(job.created_at)}` })),
          runTimeRow,
          plan ? metaRow("Repeats · seed", `${plan.repeats} · ${plan.base_seed}`) : null,
          metaRow("Plan", h("span", { class: "mono", text: job.plan_id }))),
        h("div", { class: "job-actions" }, againBtn, aiBtn, copyBtn, copyStatus),
        againSlot)),
    job.summary_error ? banner("error", "This run’s summary file is corrupted", job.summary_error) : null,
    h("div", { class: "job-live" }),
    aiSlot,
    plan ? h("details", { class: "json" }, h("summary", { text: "Plan JSON" }), h("pre", { class: "mono", text: JSON.stringify(plan, null, 2) })) : null,
  ].filter(Boolean));
  // the page settles in after the skeleton: info column and the rest in a short cascade. During a
  // flight from the library the art lands first (640 ms) and the text rises beside it.
  const flying = Boolean(flip && !flip.landed);
  requestAnimationFrame(() => { drawRasterThumb(art, thumbFor(job)); flipSettle(); });
  stagger(root.querySelector(".job-info").children, { start: flying ? 480 : 0, step: 50, y: 10, duration: DUR.enter });
  mountJob(root.querySelector(".job-live"), job, { compact: false, onStatus, onSucceeded });
}

/* ================================================================== */
/* AI hypotheses (v3): run digest → Claude → hypotheses for review      */
/* ================================================================== */

/* Visual grammar of this panel: solid rules and plain chips are things FlyLab computed (or that
   Claude restated from them); dashed cyan outlines are things Claude proposed. The disclaimer
   stamp sits above the content in every state and is never collapsible. */

const AI_DISCLAIMER = {
  en: "AI-generated hypotheses about a computational model. They are not established biological findings and must be evaluated by an expert.",
  ru: "Гипотезы, сгенерированные ИИ, о вычислительной модели. Это не установленные биологические факты; их должен оценить эксперт.",
};

const fmtWait = (s, ru) => (s < 60 ? `${Math.ceil(s)} ${ru ? "с" : "s"}` : `${Math.ceil(s / 60)} ${ru ? "мин" : "min"}`);

const AI_T = {
  en: {
    code: "en",
    eyebrow: "Claude · interpretation",
    title: "AI hypotheses",
    stampTitle: "AI-generated assumptions for expert review",
    disclaimerMissing: "The server sent no disclaimer with this result; the standard wording is shown.",
    legendSolid: "Solid: restated from the run’s numbers",
    legendDashed: "Dashed: proposed by Claude",
    langLabel: "Language of the hypotheses",
    intro: "Claude reads a digest of this run — the numbers FlyLab computed, FlyWire cell annotations and the model’s limits — and proposes hypotheses about what the activity might mean. Each one cites its evidence and comes with an experiment you can run to test it.",
    generate: "Generate hypotheses",
    interpret: "Interpret",
    viewHyp: "View hypotheses",
    regenerate: "Regenerate",
    regenerateIn: "Regenerate in English",
    checking: "Checking for saved hypotheses…",
    loadTitle: "Claude is reading this run",
    loadSub: (m) => `Building the digest, then asking ${m || "Claude"}`,
    loadNote: "Usually 30–120 s. You can keep browsing: the result is saved with this run and shows up here and in the Library.",
    loadSlow: "Still working. If Claude does not answer in time, the server stops waiting and the reason appears here.",
    loadSr: "Generating hypotheses. This can take a couple of minutes.",
    regenerating: (l) => `Regenerating in ${l === "ru" ? "Russian" : "English"}`,
    regenNote: "The current hypotheses stay until the new ones arrive.",
    headline: "In one sentence",
    observations: "Observations",
    obsSub: "Numbers from the digest, restated without interpretation",
    hypotheses: "Hypotheses",
    hypSub: "For you to evaluate — ordered as Claude gave them",
    noHyp: "Claude returned no hypotheses for this run.",
    conf: { low: "Low confidence", medium: "Medium confidence", high: "High confidence" },
    confUnknown: (v) => `Confidence “${v}” (not low, medium or high)`,
    why: "Why",
    evidence: "Evidence",
    evidenceFilter: "Show this in “What the AI saw”",
    notInDigest: "not in digest",
    caveats: "Caveats",
    test: "Proposed test",
    expected: "If the hypothesis holds",
    runTest: "Run this test",
    testPrefix: "Test: ",
    planInvalid: "This test plan did not pass validation",
    noPlanId: "This test cannot be run",
    noPlanIdBody: "The server returned neither a plan id nor a validation error for it.",
    noTest: "No test was proposed for this hypothesis.",
    testJson: "Test plan JSON",
    evWarnTitle: "Some evidence names neurons the digest does not contain",
    evWarnBody: "Claude cited ids that FlyLab computed nothing for. Treat those points as unsupported; they are marked “not in digest” below.",
    coverageTitle: "Low annotation coverage",
    digestErrTitle: "The digest of this run could not be built",
    digestWarnTitle: "The digest carries a warning",
    statementWord: "statement",
    testWord: "test plan",
    observationWord: "Observation",
    noDigest: "The response carried no digest, so “What the AI saw” is unavailable for this result.",
    limitations: "Limitations",
    reading: "Suggested reading",
    saw: "What the AI saw",
    willSee: "What the AI will see",
    sawSub: "The digest sent to Claude, as tables",
    digestLoading: "Building the digest…",
    filter: "Filter rows",
    filterPh: "neuron id, cell type, class…",
    clear: "Clear",
    rowsMatch: (n, t) => `${n} of ${t} rows match`,
    noRows: "No row matches this filter.",
    summary: "Summary",
    empty: "empty",
    yes: "yes", no: "no",
    foot: { cached: "saved result", fresh: "generated now", lang: "language" },
    retry: "Try again",
    generateAnyway: "Generate hypotheses",
    replaceCorrupt: "Replace with new hypotheses",
    rebuild: "Rebuild and generate",
    retryIn: (s) => `You can try again in ${fmtWait(s, false)}.`,
    err: {
      default: "Interpretation failed",
      check: "Saved hypotheses could not be loaded",
      inFlight: "Another interpretation is running",
      INTERPRETATION_CORRUPT: "The saved hypotheses of this run are unreadable",
      DIGEST_ERROR: "The digest of this run could not be built",
      DIGEST_BUSY: "Another run’s digest is being computed",
      401: "You are signed out",
      404: "This run was not found",
      409: "This run has not finished yet",
      429: "Interpretation limit reached",
      502: "Claude could not produce hypotheses",
      503: "Claude is busy right now",
    },
    lastFailed: "The last attempt failed",
    toastReady: (t) => `Hypotheses ready · ${t}`,
    failedTitle: (t) => `Interpretation failed · ${t}`,
    openRun: "Open the run",
    dismiss: "Dismiss",
    waiting: "Waiting",
    waitingFor: (t) => `One interpretation runs at a time: this one can start when “${t}” is done.`,
    testWarnTitle: "This test may not discriminate as written",
    calibTitle: "Confidence check",
    interpreting: "Interpreting",
    markTitle: "This run has AI hypotheses",
    cta: "Ask AI what this might mean",
    ctaSub: "Claude proposes hypotheses from this run’s numbers, each with a test you can run. Hypotheses, not findings.",
    untitled: "Untitled hypothesis",
  },
  ru: {
    code: "ru",
    eyebrow: "Claude · интерпретация",
    title: "Гипотезы ИИ",
    stampTitle: "Предположения, сгенерированные ИИ, для проверки экспертом",
    disclaimerMissing: "Сервер не прислал предупреждение к этому результату; показана стандартная формулировка.",
    legendSolid: "Сплошная линия: пересказ чисел запуска",
    legendDashed: "Пунктир: предложено Claude",
    langLabel: "Язык гипотез",
    intro: "Claude читает сводку этого запуска — числа, посчитанные FlyLab, аннотации клеток FlyWire и ограничения модели — и предлагает гипотезы о том, что может означать эта активность. К каждой приложены доказательства и эксперимент, которым её можно проверить.",
    generate: "Сгенерировать гипотезы",
    interpret: "Интерпретировать",
    viewHyp: "Смотреть гипотезы",
    regenerate: "Сгенерировать заново",
    regenerateIn: "Сгенерировать заново на русском",
    checking: "Проверяем сохранённые гипотезы…",
    loadTitle: "Claude читает этот запуск",
    loadSub: (m) => `Собираем сводку и спрашиваем ${m || "Claude"}`,
    loadNote: "Обычно 30–120 с. Можно продолжать работу: результат сохранится в этом запуске и появится здесь и в библиотеке.",
    loadSlow: "Всё ещё работаем. Если Claude не ответит вовремя, сервер прекратит ожидание, и причина появится здесь.",
    loadSr: "Генерируем гипотезы. Это может занять пару минут.",
    regenerating: (l) => `Генерируем заново на ${l === "ru" ? "русском" : "английском"}`,
    regenNote: "Текущие гипотезы останутся, пока не придут новые.",
    headline: "В одном предложении",
    observations: "Наблюдения",
    obsSub: "Числа из сводки, пересказанные без интерпретации",
    hypotheses: "Гипотезы",
    hypSub: "Для вашей оценки — в порядке, в котором их дал Claude",
    noHyp: "Claude не предложил гипотез для этого запуска.",
    conf: { low: "Низкая уверенность", medium: "Средняя уверенность", high: "Высокая уверенность" },
    confUnknown: (v) => `Уверенность «${v}» (не low, medium или high)`,
    why: "Почему",
    evidence: "Доказательства",
    evidenceFilter: "Показать в «Что видел ИИ»",
    notInDigest: "нет в сводке",
    caveats: "Оговорки",
    test: "Предлагаемая проверка",
    expected: "Если гипотеза верна",
    runTest: "Запустить проверку",
    testPrefix: "Проверка: ",
    planInvalid: "План этой проверки не прошёл валидацию",
    noPlanId: "Эту проверку нельзя запустить",
    noPlanIdBody: "Сервер не вернул для неё ни id плана, ни ошибку валидации.",
    noTest: "Для этой гипотезы проверка не предложена.",
    testJson: "JSON плана проверки",
    evWarnTitle: "Часть доказательств ссылается на нейроны, которых нет в сводке",
    evWarnBody: "Claude сослался на id, для которых FlyLab ничего не считал. Считайте эти пункты неподтверждёнными; ниже они помечены «нет в сводке».",
    coverageTitle: "Низкое покрытие аннотациями",
    digestErrTitle: "Не удалось собрать сводку этого запуска",
    digestWarnTitle: "В сводке есть предупреждение",
    statementWord: "утверждение",
    testWord: "план проверки",
    observationWord: "Наблюдение",
    noDigest: "В ответе нет сводки, поэтому «Что видел ИИ» для этого результата недоступно.",
    limitations: "Ограничения",
    reading: "Что почитать",
    saw: "Что видел ИИ",
    willSee: "Что увидит ИИ",
    sawSub: "Сводка, отправленная Claude, в виде таблиц",
    digestLoading: "Собираем сводку…",
    filter: "Фильтр строк",
    filterPh: "id нейрона, тип клетки, класс…",
    clear: "Сбросить",
    rowsMatch: (n, t) => `Совпало строк: ${n} из ${t}`,
    noRows: "Ни одна строка не подходит под фильтр.",
    summary: "Сводка",
    empty: "пусто",
    yes: "да", no: "нет",
    foot: { cached: "сохранённый результат", fresh: "только что", lang: "язык" },
    retry: "Повторить",
    generateAnyway: "Сгенерировать гипотезы",
    replaceCorrupt: "Заменить новыми гипотезами",
    rebuild: "Пересобрать и сгенерировать",
    retryIn: (s) => `Повторить можно через ${fmtWait(s, true)}.`,
    err: {
      default: "Интерпретация не удалась",
      check: "Не удалось загрузить сохранённые гипотезы",
      inFlight: "Уже идёт другая интерпретация",
      INTERPRETATION_CORRUPT: "Сохранённые гипотезы этого запуска повреждены",
      DIGEST_ERROR: "Не удалось собрать сводку этого запуска",
      DIGEST_BUSY: "Сейчас считается сводка другого запуска",
      401: "Вы вышли из аккаунта",
      404: "Запуск не найден",
      409: "Запуск ещё не завершён",
      429: "Лимит интерпретаций исчерпан",
      502: "Claude не смог сформулировать гипотезы",
      503: "Claude сейчас занят",
    },
    lastFailed: "Последняя попытка не удалась",
    toastReady: (t) => `Гипотезы готовы · ${t}`,
    failedTitle: (t) => `Интерпретация не удалась · ${t}`,
    openRun: "Открыть запуск",
    dismiss: "Закрыть",
    waiting: "Ожидание",
    waitingFor: (t) => `Интерпретации идут по одной: эта начнётся, когда закончится «${t}».`,
    testWarnTitle: "В таком виде проверка может ничего не различить",
    calibTitle: "Проверка уверенности",
    interpreting: "Интерпретируем",
    markTitle: "У этого запуска есть гипотезы ИИ",
    cta: "Спросить ИИ, что это может значить",
    ctaSub: "Claude предложит гипотезы по числам этого запуска, к каждой — проверку, которую можно запустить. Гипотезы, а не выводы.",
    untitled: "Гипотеза без названия",
  },
};
const aiT = (lang) => AI_T[lang] || AI_T.en;

// Default language of a run's panel: English (the site is English), unless the user explicitly
// picked Russian in a panel this session or set the report-language toggle to RU.
function aiDefaultLang(job) {
  if (state.aiLang === "ru" || state.aiLang === "en") return state.aiLang;
  if (state.lang === "ru") return "ru";
  return "en"; // the site is English; Russian hypotheses only when picked explicitly
}
// The interpretation model, when the server reports it; otherwise the UI just says "Claude".
const aiModelName = () => (state.caps && (state.caps.interpret_model || state.caps.llm_interpret_model)) || null;

/* Requests outlive the view that started them: a Library card or a run page can be left while
   Claude works. The in-flight record, the latest result and the latest failure are kept per job,
   so whichever view of that job is on screen next shows the progress, the result or the error. */
const aiStore = { epoch: 0, inflight: new Map(), cache: new Map(), errors: new Map(), digests: new Map() };
function aiReset() {
  aiStore.epoch++;
  aiStore.inflight.clear();
  aiStore.cache.clear();
  aiStore.errors.clear();
  aiStore.digests.clear();
}

function checkInterpretation(data, where) {
  const it = data && data.interpretation;
  if (!it || typeof it !== "object" || !Array.isArray(it.hypotheses)) {
    throw new ApiError(200, "BAD_SHAPE", `${where} answered without interpretation.hypotheses`);
  }
  return data;
}

function aiRequest(jobId, lang, regenerate, label) {
  const cur = aiStore.inflight.get(jobId);
  if (cur) return cur;
  const epoch = aiStore.epoch;
  const rec = { jobId, label: label || jobId, started: performance.now(), lang, regenerate: Boolean(regenerate), promise: null };
  aiStore.errors.delete(jobId);
  rec.promise = api(`/api/v1/jobs/${encodeURIComponent(jobId)}/interpretation`, { method: "POST", body: { language: lang, regenerate: Boolean(regenerate) } })
    .then(({ data }) => {
      checkInterpretation(data, "POST /interpretation");
      if (epoch === aiStore.epoch) {
        aiStore.cache.set(jobId, data);
        aiStore.errors.delete(jobId);
        // a confirmation wherever the user is now; failures are shown inline by the job's views
        toast(aiT(lang).toastReady(label || jobId));
      }
      return data;
    }, (err) => {
      if (epoch === aiStore.epoch) {
        aiStore.errors.set(jobId, err);
        // The request may have run for minutes while the user went elsewhere: a failure is
        // announced wherever they are (like a success), unless this run's page is open, where
        // the panel shows it inline. It stays until dismissed.
        if (!location.hash.startsWith(`#/job/${encodeURIComponent(jobId)}`)) aiFailNotice(lang, label || jobId, jobId, err);
      }
      throw err;
    })
    .finally(() => { if (aiStore.inflight.get(jobId) === rec) aiStore.inflight.delete(jobId); });
  // Every view of this job attaches its own handlers; this one only keeps a failure that happens
  // while no view is attached from becoming an unhandled rejection. The failure itself is kept in
  // aiStore.errors and shown by the next card or panel of this job.
  rec.promise.catch(() => {});
  aiStore.inflight.set(jobId, rec);
  return rec;
}

async function fetchDigest(jobId) {
  if (aiStore.digests.has(jobId)) return aiStore.digests.get(jobId);
  const { data } = await api(`/api/v1/jobs/${encodeURIComponent(jobId)}/digest`);
  if (!data || typeof data !== "object") throw new ApiError(200, "BAD_SHAPE", "GET /digest did not return an object");
  if (data.digest_error) throw new ApiError(200, "DIGEST_ERROR", String(data.digest_error));
  const dg = data.digest && typeof data.digest === "object" ? data.digest : data;
  aiStore.digests.set(jobId, dg);
  return dg;
}

/* ---------- errors, localized titles, server message verbatim ---------- */
// Title by error code first (a corrupt result, a digest problem or a busy digest slot are not
// "Claude" problems), then the 429 scope, then the HTTP status.
function aiErrTitle(L, err) {
  if (!err) return L.err.default;
  if (err.code && L.err[err.code]) return L.err[err.code];
  if (err.status === 429 && err.details && err.details.scope === "user_in_flight") return L.err.inFlight;
  return (err.status && L.err[err.status]) || L.err.default;
}
// Errors the server recovers from only with regenerate:true (a stored result it cannot read,
// a digest it could not build): the retry actions then send it.
const aiNeedsRegen = (err) => Boolean(err && (err.code === "INTERPRETATION_CORRUPT" || err.code === "DIGEST_ERROR"));
// "You can try again in N s" for 429/503, unless the server message already says when.
function aiRetryNote(L, err) {
  if (!err || (err.status !== 429 && err.status !== 503)) return null;
  const s = (err.details && Number(err.details.retry_after_seconds)) || err.retryAfter;
  if (!(s > 0) || /retry in \d+\s*s\b|try again shortly/i.test(err.message || "")) return null;
  return L.retryIn(s);
}
function aiErrorBanner(L, err, actions = [], titleOverride = null) {
  const b = errorBanner(titleOverride || aiErrTitle(L, err), err, actions);
  const note = aiRetryNote(L, err);
  if (note) b.querySelector(".b-body").append(" ", note);
  return b;
}
// A failure notice that stays on screen (bottom corner) until dismissed, with a link to the run.
function aiFailNotice(lang, label, jobId, err) {
  const L = aiT(lang);
  let host = document.querySelector(".ai-notices");
  if (!host) {
    host = h("div", { class: "ai-notices" });
    document.body.append(host);
  }
  const note = aiRetryNote(L, err);
  const close = h("button", { class: "btn btn-ghost btn-sm", type: "button", text: L.dismiss });
  const card = h("div", { class: "ai-notice", role: "alert", lang: L.code },
    h("p", { class: "ai-notice-title", text: L.failedTitle(label) }),
    h("p", { class: "ai-notice-body" }, h("b", { text: `${aiErrTitle(L, err)}: ` }), (err && err.message) || String(err), note ? ` ${note}` : ""),
    errorMeta(err) ? h("p", { class: "ai-notice-meta mono", text: errorMeta(err) }) : null,
    h("div", { class: "ai-notice-actions" },
      h("a", { class: "btn btn-sm", href: `#/job/${encodeURIComponent(jobId)}?ai=1`, text: L.openRun, onclick: () => card.remove() }),
      close));
  close.addEventListener("click", () => card.remove());
  host.append(card);
  while (host.childElementCount > 3) host.firstElementChild.remove();
  enter(card, { y: 10, duration: DUR.view });
}

/* ---------- the permanent label + disclaimer ---------- */
function aiStamp(L, disclaimer) {
  const text = typeof disclaimer === "string" && disclaimer.trim() ? disclaimer : null;
  return h("div", { class: "ai-stamp", role: "note", "aria-label": L.stampTitle },
    h("span", { class: "ai-stamp-mark", "aria-hidden": "true" }),
    h("div", null,
      h("p", { class: "ai-stamp-title", text: L.stampTitle }),
      h("p", { class: "ai-stamp-body", text: text || AI_DISCLAIMER[L.code] }),
      disclaimer !== undefined && !text ? h("p", { class: "ai-stamp-meta mono", text: L.disclaimerMissing }) : null));
}
const aiLegend = (L) => h("p", { class: "ai-legend mono" },
  h("span", null, h("i", { class: "lg-solid", "aria-hidden": "true" }), L.legendSolid),
  h("span", null, h("i", { class: "lg-dash", "aria-hidden": "true" }), L.legendDashed));

/* ---------- evidence chips ---------- */
// evidence_warnings: {location, kind, neuron_id?, reference?, text, message}. Each one is shown
// where it applies (chip, statement, test plan, reading item) and listed in a banner.
function normWarnings(ws) {
  if (!Array.isArray(ws)) return [];
  return ws.map((w) => {
    if (w === null || w === undefined) return null;
    if (typeof w !== "object") return { message: String(w), text: String(w), location: null, id: null };
    return {
      message: String(w.message || w.warning || w.text || JSON.stringify(w)),
      text: typeof w.text === "string" ? w.text : "",
      location: typeof w.location === "string" ? w.location : null,
      id: w.neuron_id || w.reference || null,
    };
  }).filter(Boolean);
}
const warnsAt = (ctx, loc) => ctx.warnings.filter((w) => w.location === loc);
function warnLoc(loc, L) {
  const m = String(loc || "").match(/^(observations|hypotheses|suggested_reading)\[(\d+)\](?:\.(evidence|statement|test\.plan)(?:\[(\d+)\])?)?$/);
  if (!m) return loc || "";
  const n = Number(m[2]) + 1;
  const part = m[3] === "evidence" ? ` · ${L.evidence.toLowerCase()} ${Number(m[4]) + 1}` : m[3] === "statement" ? ` · ${L.statementWord}` : m[3] ? ` · ${L.testWord}` : "";
  if (m[1] === "hypotheses") return `H${n}${part}`;
  if (m[1] === "observations") return `${L.observationWord} ${n}${part}`;
  return `${L.reading} ${n}`;
}
function warnLine(ws) {
  if (!ws.length) return null;
  return h("p", { class: "ai-inline-warn" }, ws.map((w, i) => [i ? "; " : "", w.message]));
}
const ROOT_ID = /\b\d{15,20}\b/;
function evChips(items, L, ctx, locBase) {
  if (!Array.isArray(items)) return null;
  const list = items.map((x, j) => [x, j]).filter(([x]) => x !== null && x !== undefined && String(x).trim());
  if (!list.length) return null;
  const located = ctx.warnings.some((w) => w.location);
  return h("div", { class: "ev-row" }, list.map(([ev, j]) => {
    const text = String(ev);
    const id = (text.match(ROOT_ID) || [])[0] || null;
    // structured warnings name the exact chip; plain-string warnings fall back to text matching
    const here = located ? warnsAt(ctx, `${locBase}[${j}]`) : ctx.warnings.filter((w) => w.message.includes(text) || (id && w.message.includes(id)));
    const flagged = here.length > 0;
    return h("button", {
      type: "button", class: `ev mono${flagged ? " ev-warn" : ""}`,
      title: [flagged ? here.map((w) => w.message).join("; ") : null, ctx.canPick ? L.evidenceFilter : null].filter(Boolean).join(" — ") || null,
      disabled: ctx.canPick ? null : true,
      onclick: () => ctx.pick(id || text),
    }, h("span", { text }), flagged ? h("span", { class: "ev-flag", text: L.notInDigest }) : null);
  }));
}

function confBadge(L, level) {
  const known = ["low", "medium", "high"].indexOf(level);
  const n = known + 1;
  return h("span", { class: `conf${known < 0 ? " conf-bad" : ""}`, "data-level": known < 0 ? "unknown" : level },
    h("span", { class: "conf-ticks", "aria-hidden": "true" }, [1, 2, 3].map((i) => h("i", { class: i <= n ? "on" : null, style: `--ti:${i - 1}` }))),
    h("span", { text: known < 0 ? L.confUnknown(String(level)) : L.conf[level] }));
}

/* ---------- one hypothesis ---------- */
function hypCard(hy, i, L, ctx) {
  const tid = `hyp-${ctx.uid}-${i}`;
  const caveats = Array.isArray(hy.caveats) ? hy.caveats.filter(Boolean) : [];
  return h("article", { class: "hyp", "aria-labelledby": tid, style: `--hi:${i}` },
    h("header", { class: "hyp-head" },
      h("span", { class: "hyp-n mono", text: `H${i + 1}` }),
      h("h4", { class: "hyp-title", id: tid, text: hy.title || L.untitled }),
      confBadge(L, hy.confidence)),
    hy.confidence_reason ? h("p", { class: "hyp-reason" }, h("span", { class: "hyp-label", text: `${L.why}: ` }), hy.confidence_reason) : null,
    hy.calibration_warning ? h("p", { class: "ai-inline-warn hyp-calib" }, h("b", { text: `${L.calibTitle}: ` }), String(hy.calibration_warning)) : null,
    hy.statement ? h("p", { class: "hyp-statement", text: hy.statement }) : null,
    warnLine(warnsAt(ctx, `hypotheses[${i}].statement`)),
    Array.isArray(hy.evidence) && hy.evidence.length ? h("div", { class: "hyp-block" }, h("p", { class: "hyp-label", text: L.evidence }), evChips(hy.evidence, L, ctx, `hypotheses[${i}].evidence`)) : null,
    caveats.length ? h("div", { class: "hyp-block" }, h("p", { class: "hyp-label", text: L.caveats }), h("ul", { class: "hyp-caveats" }, caveats.map((c) => h("li", { text: c })))) : null,
    testBlock(hy, L, ctx, i));
}

function testBlock(hy, L, ctx, i) {
  const test = hy.test;
  if (!test || typeof test !== "object") return h("p", { class: "muted small hyp-notest", text: L.noTest });
  const slot = h("div", { class: "hyp-run-slot" });
  let action;
  if (test.plan_error) {
    action = banner("error", L.planInvalid, String(test.plan_error), { meta: "plan_error" });
  } else if (test.plan_id) {
    const btn = h("button", { class: "btn btn-sm run-test", type: "button" }, h("span", { text: L.runTest }), h("span", { class: "arrow", "aria-hidden": "true", text: "→" }));
    const guard = launchGuard(btn);
    const title = `${L.testPrefix}${hy.title || L.untitled}`.slice(0, 120);
    btn.addEventListener("click", () => guard(`${test.plan_id}\u0000${title}`, (key) => startRun({
      planId: test.plan_id, prompt: test.description || hy.statement || "", title, slot, key,
      onCreated: (nj) => { location.hash = `#/job/${encodeURIComponent(nj.job_id)}`; },
    })));
    action = h("div", { class: "hyp-test-actions" }, btn, h("span", { class: "mono dim", text: test.plan_id }));
  } else {
    action = banner("warn", L.noPlanId, L.noPlanIdBody);
  }
  const tws = Array.isArray(test.warnings) ? test.warnings.filter(Boolean) : [];
  let twBanner = null;
  if (tws.length) {
    twBanner = banner("warn", L.testWarnTitle, null, { meta: "test.warnings" });
    twBanner.children[1].append(h("ul", { class: "ai-warn-list" }, tws.map((w) => h("li", { text: String(w) }))));
  }
  return h("div", { class: "hyp-test" },
    h("p", { class: "hyp-label", text: L.test }),
    test.description ? h("p", { class: "hyp-test-desc", text: test.description }) : null,
    test.expected_if_true ? h("p", { class: "hyp-expect" }, h("span", { class: "hyp-label", text: `${L.expected}: ` }), test.expected_if_true) : null,
    test.plan && typeof test.plan === "object" ? h("p", { class: "hyp-test-plan mono", text: planShort(test.plan) }) : null,
    warnLine(warnsAt(ctx, `hypotheses[${i}].test.plan`)),
    twBanner,
    action,
    slot,
    test.plan ? h("details", { class: "json" }, h("summary", { text: L.testJson }), h("pre", { class: "mono", text: JSON.stringify(test.plan, null, 2) })) : null);
}

/* ---------- the digest as readable tables ---------- */
const isPlainObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const isScalar = (v) => v === null || typeof v !== "object";
const DIGEST_FIRST = ["coverage", "experiment", "totals"];
const DIGEST_RU = {
  coverage: "Покрытие аннотациями", experiment: "Эксперимент", totals: "Итоги по условиям",
  activity_by_super_class: "Активность по super_class", activity_by_cell_class: "Активность по cell_class",
  activity_by_top_nt: "Активность по медиатору", top_neurons: "Самые активные нейроны",
  top_delta_neurons: "Наибольшие изменения", readout: "Нейроны считывания", readout_neurons: "Нейроны считывания",
  proxies: "Поведенческие прокси", behavioural_proxies: "Поведенческие прокси", model_facts: "Факты о модели",
  stimulated: "Стимулированные", silenced: "Заглушённые", warning: "Предупреждение", warnings: "Предупреждения",
  top_neurons_by_rate: "Самые активные нейроны (A)", top_neurons_by_delta: "Наибольшие изменения |Δ|",
  readouts: "Нейроны считывания", references: "Литература", graph_units: "Единицы графа",
  per_condition: "По условиям", unannotated_top_neurons: "Неаннотированные среди топа", rows: "Строки",
  activity_by_cell_type: "Активность по cell_type", stimulated_neurons: "Стимулированные нейроны (каждый)",
  silenced_neurons: "Заглушённые нейроны (каждый)", readout_inputs: "Активные входы нейронов считывания",
  model_parameters: "Параметры модели", per_field: "Заполненность полей аннотации", field_warnings: "Предупреждения по полям",
};
function humanKey(k, lang) {
  if (lang === "ru" && DIGEST_RU[k]) return DIGEST_RU[k];
  const s = String(k).replace(/_/g, " ").trim();
  return s.charAt(0).toUpperCase() + s.slice(1);
}
function fmtDigest(k, v, L) {
  if (v === null || v === undefined) return "—";
  if (typeof v === "boolean") return v ? L.yes : L.no;
  if (typeof v === "number") {
    if (!Number.isFinite(v)) return String(v);
    // ids above 2^53 cannot round-trip through JSON numbers; they are shown exactly as parsed
    if (Number.isInteger(v)) return Math.abs(v) >= 1e12 ? String(v) : v.toLocaleString("en-US");
    const s = Math.abs(v) >= 100 ? v.toFixed(1) : Math.abs(v) >= 1 ? v.toFixed(2) : v.toPrecision(3);
    return /share|fraction|frac|coverage|ratio/i.test(k) && v >= 0 && v <= 1 ? `${s} (${(v * 100).toFixed(1)}%)` : s;
  }
  if (Array.isArray(v)) return v.map((x) => fmtDigest(k, x, L)).join(v.every(isScalar) ? ", " : " ; ");
  if (isPlainObj(v)) return Object.entries(v).map(([kk, vv]) => `${kk}: ${fmtDigest(kk, vv, L)}`).join(" · ");
  return String(v);
}
// columns x_en / x_ru: only the one in the panel's language is shown
function pickCols(cols, lang) {
  return cols.filter((k) => {
    const m = String(k).match(/^(.*)_(en|ru)$/);
    if (!m) return true;
    const other = `${m[1]}_${m[2] === "en" ? "ru" : "en"}`;
    return !cols.includes(other) || m[2] === lang;
  });
}
// A nested object of scalars becomes columns: annotation fields keep their own names (so a cell
// type is a column you can filter by), others are prefixed with their parent (A_…, B_…).
function flattenRow(r) {
  const out = {};
  for (const [k, v] of Object.entries(r)) {
    if (isPlainObj(v) && Object.values(v).every(isScalar)) {
      for (const [kk, vv] of Object.entries(v)) out[k === "annotation" && !(kk in r) ? kk : `${k}_${kk}`] = vv;
    } else if (!(v === null && k === "annotation")) out[k] = v; // unannotated: its columns read "—"
  }
  return out;
}
function digestTable(rawRows, L) {
  const rows = rawRows.map(flattenRow);
  const all = [];
  for (const r of rows.slice(0, 60)) for (const k of Object.keys(r)) if (!all.includes(k)) all.push(k);
  const cols = pickCols(all, L.code);
  const numeric = new Set(cols.filter((k) => rows.every((r) => r[k] === null || r[k] === undefined || typeof r[k] === "number") && rows.some((r) => typeof r[k] === "number" && Math.abs(r[k]) < 1e12)));
  const idish = (k, v) => /(^|_)ids?$/.test(k) || (typeof v === "string" && /^\d{12,}$/.test(v)) || (typeof v === "number" && Math.abs(v) >= 1e12);
  return h("div", { class: "table-wrap" }, h("table", { class: "ai-table" },
    h("thead", null, h("tr", null, cols.map((k) => h("th", { scope: "col", class: numeric.has(k) ? "num" : null, text: humanKey(k, L.code) })))),
    h("tbody", null, rows.map((r) => h("tr", null, cols.map((k) => h("td", {
      class: [numeric.has(k) ? "num" : "", idish(k, r[k]) ? "mono" : ""].filter(Boolean).join(" ") || null,
      text: fmtDigest(k, r[k], L),
    })))))));
}
function digestBlock(key, val, L, depth) {
  const head = h(depth ? "h5" : "h4", { class: "ai-d-h", text: humanKey(key, L.code) });
  const sec = (...kids) => h("section", { class: "ai-d-sec", "data-depth": String(depth), "data-key": key }, head, ...kids);
  if (Array.isArray(val)) {
    if (!val.length) return sec(h("p", { class: "muted small", text: L.empty }));
    if (val.every(isPlainObj)) return sec(digestTable(val, L));
    if (val.every(isScalar)) {
      return val.some((x) => String(x).length > 48)
        ? sec(h("ul", { class: "ai-d-list" }, val.map((x) => h("li", { text: fmtDigest(key, x, L) }))))
        : sec(h("ul", { class: "ai-d-list ai-d-chips" }, val.map((x) => h("li", { class: "tag mono", text: fmtDigest(key, x, L) }))));
    }
    return sec(h("pre", { class: "mono ai-d-pre", text: JSON.stringify(val, null, 2) }));
  }
  if (isPlainObj(val)) {
    if (depth >= 3) return sec(h("pre", { class: "mono ai-d-pre", text: JSON.stringify(val, null, 2) }));
    const entries = Object.entries(val).filter(([k]) => pickCols(Object.keys(val), L.code).includes(k));
    const flat = entries.filter(([, v]) => isScalar(v) || (Array.isArray(v) && v.every(isScalar) && v.length <= 6));
    const nested = entries.filter((e) => !flat.includes(e));
    return sec(
      flat.length ? h("dl", { class: "spec-grid compact ai-d-grid" }, flat.map(([k, v]) => h("div", { class: "spec" }, h("dt", { text: humanKey(k, L.code) }), h("dd", { class: typeof v === "number" ? "mono" : null, text: fmtDigest(k, v, L) })))) : null,
      nested.map(([k, v]) => digestBlock(k, v, L, depth + 1)));
  }
  return sec(h("p", { text: fmtDigest(key, val, L) }));
}
function digestContent(dg, L) {
  const keys = Object.keys(dg).filter((k) => k !== "digest_error");
  const order = [...DIGEST_FIRST.filter((k) => keys.includes(k)), ...keys.filter((k) => !DIGEST_FIRST.includes(k))];
  const top = order.filter((k) => isScalar(dg[k]));
  return [
    top.length ? h("section", { class: "ai-d-sec", "data-depth": "0" }, h("h4", { class: "ai-d-h", text: L.summary }),
      h("dl", { class: "spec-grid compact ai-d-grid" }, top.map((k) => h("div", { class: "spec" }, h("dt", { text: humanKey(k, L.code) }), h("dd", { text: fmtDigest(k, dg[k], L) }))))) : null,
    ...order.filter((k) => !isScalar(dg[k])).map((k) => digestBlock(k, dg[k], L, 0)),
  ];
}
// Coverage warnings, wherever the digest carries them (coverage.warning is the documented place).
function digestWarnings(dg) {
  if (!isPlainObj(dg)) return [];
  const out = [];
  const add = (w, coverage) => {
    if (typeof w === "string" && w.trim()) {
      if (!out.some((o) => o.text === w.trim())) out.push({ text: w.trim(), coverage });
    } else if (Array.isArray(w)) w.forEach((x) => add(x, coverage));
  };
  const cov = dg.coverage;
  if (isPlainObj(cov)) { add(cov.warning, true); add(cov.warnings, true); add(cov.field_warnings, true); }
  add(dg.coverage_warning, true);
  add(dg.warnings, false); // the digest repeats the coverage warning here; it is shown once
  return out;
}

/* "What the AI saw / will see": built when first opened; a filter narrows every table, and an
   evidence chip opens it already filtered to that neuron or class. */
function digestDetails(st, L, load, titleText) {
  const det = h("details", { class: "ai-digest" });
  if (st.digestOpen) det.open = true;
  const body = h("div", { class: "ai-d-body" });
  det.append(h("summary", null, h("span", { class: "ai-d-sum", text: titleText }), h("span", { class: "muted small", text: L.sawSub })), body);
  let built = false, input = null, count = null, content = null;
  const apply = () => {
    if (!content) return;
    const q = (st.filter || "").trim().toLowerCase();
    let shown = 0, total = 0;
    content.querySelectorAll("tbody tr, .ai-d-list li").forEach((row) => {
      total++;
      const on = !q || row.textContent.toLowerCase().includes(q);
      row.hidden = !on;
      row.classList.toggle("hit", Boolean(q) && on);
      if (on) shown++;
    });
    content.querySelectorAll(".table-wrap").forEach((w) => { w.hidden = Boolean(q) && !w.querySelector("tbody tr:not([hidden])"); });
    count.textContent = q ? L.rowsMatch(shown, total) : "";
    content.querySelector(".ai-d-none").hidden = !(q && shown === 0);
  };
  const build = async () => {
    if (built) return;
    built = true;
    body.replaceChildren(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: L.digestLoading })));
    let dg;
    try {
      dg = await load();
      if (!isPlainObj(dg)) throw new ApiError(200, "BAD_SHAPE", "The digest is not an object");
    } catch (err) {
      built = false;
      if (det.isConnected) body.replaceChildren(aiErrorBanner(L, err, [h("button", { class: "btn btn-sm", type: "button", text: L.retry, onclick: build })], L.digestErrTitle));
      return;
    }
    if (!det.isConnected) return;
    input = h("input", { class: "ai-d-input", type: "search", placeholder: L.filterPh, "aria-label": L.filter, value: st.filter || "" });
    input.value = st.filter || "";
    count = h("span", { class: "mono dim", "aria-live": "polite" });
    input.addEventListener("input", () => { st.filter = input.value; apply(); });
    const clear = h("button", { class: "btn btn-ghost btn-sm", type: "button", text: L.clear, onclick: () => { st.filter = ""; input.value = ""; apply(); input.focus(); } });
    content = h("div", { class: "ai-d-content" }, h("p", { class: "muted small ai-d-none", hidden: true, text: L.noRows }), digestContent(dg, L));
    body.replaceChildren(h("div", { class: "ai-d-filter" }, h("label", { class: "ai-d-flabel", text: L.filter }, input), clear, count), content);
    apply();
  };
  det.addEventListener("toggle", () => { st.digestOpen = det.open; if (det.open) build(); });
  if (det.open) build();
  return {
    el: det,
    pick(token) {
      st.filter = token;
      if (input) { input.value = token; apply(); }
      if (!det.open) det.open = true; // the toggle event builds it, with the filter applied
      det.scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" });
    },
  };
}

/* ---------- loading plate (no interpretation yet) ---------- */
function aiLoadingPlate(L, rec, uid) {
  const elapsed = h("span", { class: "mono", "aria-hidden": "true" });
  const note = h("p", { class: "ai-load-note", text: L.loadNote });
  const cv = h("canvas", { class: "drafting-trace", "aria-hidden": "true" });
  const bar = (cls) => h("div", { class: `skel ${cls}` });
  const plate = h("div", { class: "ai-loading" },
    h("p", { class: "sr-only", role: "status", text: L.loadSr }),
    h("div", { class: "drafting-head" }, cv,
      h("div", null,
        h("p", { class: "drafting-title", text: L.loadTitle }),
        h("p", { class: "drafting-sub" }, L.loadSub(aiModelName()), " · ", elapsed))),
    h("div", { class: "ai-skel", "aria-hidden": "true" },
      bar("ai-skel-head"), bar("ai-skel-line"), bar("ai-skel-line short"),
      h("div", { class: "ai-skel-hyp" }, bar("ai-skel-line"), bar("ai-skel-line short")),
      h("div", { class: "ai-skel-hyp" }, bar("ai-skel-line"), bar("ai-skel-line short"))),
    note);
  plate._start = () => {
    trace(cv, { rows: 3, activity: 0.45, noise: 1.3, seed: hashString(`${uid}|ai`), colors: [COLORS.cyan, COLORS.green, COLORS.magenta] });
    const tick = () => {
      if (!plate.isConnected || aiStore.inflight.get(rec.jobId) !== rec) return;
      const s = (performance.now() - rec.started) / 1000;
      elapsed.textContent = `${Math.floor(s)} ${L.code === "ru" ? "с" : "s"}`;
      if (s > 100 && note.textContent !== L.loadSlow) note.textContent = L.loadSlow;
      later(tick, 500);
    };
    tick();
  };
  return plate;
}

/* ---------- the panel ---------- */
function mountAiPanel(root, job, { autostart = false, scroll = false, onHas = null } = {}) {
  const gen = routeGen;
  const jobId = job.job_id;
  const uid = hashString(jobId).toString(36);
  const runLabel = job.title || job.prompt || planShort(job.plan) || jobId;
  const st = {
    lang: aiDefaultLang(job),
    phase: "check", // check | check_error | empty | generating | result
    data: null,
    rec: null,
    actionError: null,
    checkError: null,
    pendingStart: autostart,
    pendingScroll: scroll,
    filter: "",
    digestOpen: false,
    fresh: false,
  };
  const titleId = `ai-title-${uid}`;
  const section = h("section", { class: "ai-panel", "aria-labelledby": titleId });
  root.append(section);
  const alive = () => gen === routeGen && section.isConnected;

  function setData(d, fresh) {
    st.data = d;
    st.phase = "result";
    st.fresh = fresh;
    const ml = d.meta && d.meta.language;
    if (ml === "ru" || ml === "en") st.lang = ml;
    if (onHas) onHas(true, ml === "ru" || ml === "en" ? ml : null);
  }

  function attach(rec) {
    st.rec = rec;
    st.lang = rec.lang;
    st.actionError = null;
    st.phase = st.data ? "result" : "generating";
    paint();
    rec.promise.then((d) => {
      if (!alive()) return;
      st.rec = null;
      setData(d, true);
      paint();
    }, (err) => {
      if (!alive()) return;
      st.rec = null;
      st.actionError = err;
      st.phase = st.data ? "result" : "empty";
      paint();
    });
  }

  function generate(regenerate) {
    if (st.rec) return;
    attach(aiRequest(jobId, st.lang, regenerate, runLabel));
  }

  async function check() {
    st.phase = "check";
    st.checkError = null;
    paint();
    const cached = aiStore.cache.get(jobId);
    const running = aiStore.inflight.get(jobId);
    if (running) { if (cached) setData(cached, false); attach(running); return; }
    if (cached) { setData(cached, false); paint(); return; }
    try {
      const { data } = await api(`/api/v1/jobs/${encodeURIComponent(jobId)}/interpretation`);
      if (!alive()) return;
      checkInterpretation(data, "GET /interpretation");
      aiStore.cache.set(jobId, data);
      setData(data, false);
      paint();
    } catch (err) {
      if (!alive()) return;
      // a request started meanwhile (hero button) owns the panel now
      if (st.rec) return;
      if (err instanceof ApiError && err.status === 404 && err.code === "INTERPRETATION_NOT_FOUND") {
        st.phase = "empty";
        if (onHas) onHas(false);
        if (aiStore.errors.has(jobId)) st.actionError = aiStore.errors.get(jobId);
        if (st.pendingStart) { st.pendingStart = false; generate(false); return; }
        paint();
        return;
      }
      st.phase = "check_error";
      st.checkError = err;
      paint();
    }
  }

  function langSeg(L) {
    const btns = ["ru", "en"].map((l) => h("button", {
      type: "button", role: "radio", "aria-checked": String(st.lang === l), "data-lang": l, lang: l,
      text: l.toUpperCase(), title: l === "ru" ? "Русский" : "English", disabled: st.rec ? true : null,
    }));
    const seg = h("div", { class: "seg seg-sm", role: "radiogroup", "aria-label": L.langLabel }, btns);
    seg.addEventListener("click", (e) => {
      const b = e.target.closest("[data-lang]");
      if (!b || st.rec || b.dataset.lang === st.lang) return;
      st.lang = b.dataset.lang;
      state.aiLang = st.lang;
      paint();
      const again = section.querySelector(`.ai-tools [data-lang="${st.lang}"]`);
      if (again) again.focus();
    });
    requestAnimationFrame(() => segIndicator(seg));
    return seg;
  }

  function result(L) {
    const d = st.data;
    const it = d.interpretation;
    const digest = isPlainObj(d.digest) ? d.digest : null;
    const warnings = normWarnings(d.evidence_warnings);
    let dd = null;
    const ctx = { uid, warnings, canPick: Boolean(digest), pick: (t) => dd && dd.pick(t) };
    const digestErr = d.digest_error || (digest && digest.digest_error) || null;
    const obs = Array.isArray(it.observations) ? it.observations : [];
    const hyps = it.hypotheses;
    const lims = Array.isArray(it.limitations) ? it.limitations.filter(Boolean) : [];
    const reading = Array.isArray(it.suggested_reading) ? it.suggested_reading.filter(Boolean) : [];
    let evBanner = null;
    if (warnings.length) {
      evBanner = banner("warn", L.evWarnTitle, L.evWarnBody, { meta: "evidence_warnings" });
      evBanner.children[1].append(h("ul", { class: "ai-warn-list" }, warnings.map((w) => h("li", null,
        w.location ? h("b", { text: `${warnLoc(w.location, L)}: ` }) : null, w.message))));
    }
    if (digest) dd = digestDetails(st, L, () => Promise.resolve(digest), L.saw);
    const content = h("div", { class: `ai-content${st.rec ? " busy" : ""}` },
      digestErr ? banner("error", L.digestErrTitle, String(digestErr), { meta: "digest_error" }) : null,
      digestWarnings(digest).map((w) => banner("warn", w.coverage ? L.coverageTitle : L.digestWarnTitle, w.text, { meta: w.coverage ? "coverage.warning" : "digest warnings" })),
      evBanner,
      it.headline ? h("div", { class: "ai-headline-wrap" }, h("p", { class: "hyp-label", text: L.headline }), h("blockquote", { class: "ai-headline", text: it.headline })) : null,
      obs.length ? h("section", { class: "ai-sec" },
        h("div", { class: "ai-sec-head" }, h("h3", { class: "ai-sec-title", text: L.observations }), h("p", { class: "ai-sec-sub", text: L.obsSub })),
        h("ul", { class: "ai-obs" }, obs.map((o, i) => h("li", null,
          h("p", { text: typeof o === "string" ? o : (o && o.text) || "" }),
          o && typeof o === "object" ? evChips(o.evidence, L, ctx, `observations[${i}].evidence`) : null)))) : null,
      h("section", { class: "ai-sec" },
        h("div", { class: "ai-sec-head" }, h("h3", { class: "ai-sec-title", text: L.hypotheses }), h("p", { class: "ai-sec-sub", text: L.hypSub })),
        hyps.length ? h("div", { class: "hyps" }, hyps.map((hy, i) => hypCard(hy || {}, i, L, ctx))) : h("p", { class: "muted", text: L.noHyp })),
      lims.length ? h("section", { class: "ai-sec ai-sec-small" }, h("h3", { class: "ai-sec-title", text: L.limitations }), h("ul", { class: "ai-lims" }, lims.map((x) => h("li", { text: x })))) : null,
      reading.length ? h("section", { class: "ai-sec ai-sec-small" }, h("h3", { class: "ai-sec-title", text: L.reading }), h("ul", { class: "ai-reading" }, it.suggested_reading.map((x, i) => {
        if (!x) return null;
        const ws = warnsAt(ctx, `suggested_reading[${i}]`);
        return h("li", { class: ws.length ? "flagged" : null }, h("span", { text: x }), ws.length ? h("span", { class: "ev-flag", text: ws.map((w) => w.message).join("; ") }) : null);
      }))) : null,
      dd ? dd.el : banner("warn", L.saw, L.noDigest));
    if (st.rec) content.inert = true;
    // Claude's text is in the language it was generated in, whatever the labels are set to
    const dl = d.meta && d.meta.language;
    if (dl === "ru" || dl === "en") content.setAttribute("lang", dl);
    return content;
  }

  function footer(L) {
    const m = (st.data && st.data.meta) || {};
    const bits = [];
    if (m.model) bits.push(h("span", null, m.model));
    if (typeof m.cost_usd === "number") bits.push(h("span", null, `$${m.cost_usd.toFixed(3)}`));
    if (typeof m.duration_ms === "number") bits.push(h("span", null, `${(m.duration_ms / 1000).toFixed(1)} ${L.code === "ru" ? "с" : "s"}`));
    if (m.created_at) bits.push(h("span", { title: absTime(m.created_at) }, aiRelTime(m.created_at, L.code)));
    if (m.language) bits.push(h("span", null, `${L.foot.lang} ${String(m.language).toUpperCase()}`));
    if (typeof m.cached === "boolean") bits.push(h("span", null, m.cached ? L.foot.cached : L.foot.fresh));
    return bits.length ? h("footer", { class: "ai-foot mono" }, bits) : null;
  }

  function paint() {
    if (!alive()) return;
    const L = aiT(st.lang);
    const busy = Boolean(st.rec) || st.phase === "check";
    section.setAttribute("lang", L.code);
    section.setAttribute("aria-busy", String(busy));
    section.classList.toggle("arrive", st.fresh && !isReduced());
    const d = st.data;
    const regenBtn = st.phase === "result" ? h("button", {
      class: "btn btn-sm ai-regen", type: "button", disabled: st.rec ? true : null,
      text: d && d.meta && d.meta.language && d.meta.language !== st.lang ? L.regenerateIn : L.regenerate,
      onclick: () => generate(true),
    }) : null;
    const head = h("header", { class: "ai-head" },
      h("div", null,
        h("p", { class: "eyebrow ai-eyebrow" }, h("span", { class: "ai-glyph", "aria-hidden": "true" }), L.eyebrow),
        h("h2", { class: "display ai-title", id: titleId, tabindex: "-1", text: L.title })),
      h("div", { class: "ai-tools" }, langSeg(L), regenBtn));
    const parts = [head, aiStamp(L, d ? d.disclaimer : undefined), aiLegend(L)];
    // After INTERPRETATION_CORRUPT or DIGEST_ERROR the retry sends regenerate:true, the only
    // request the server recovers from.
    const regenAfter = (err) => Boolean(st.data) || aiNeedsRegen(err);
    const retryGen = (err) => h("button", { class: "btn btn-sm", type: "button", text: aiNeedsRegen(err) && !st.data ? L.rebuild : L.retry, onclick: () => generate(regenAfter(err)) });
    if (st.actionError) parts.push(aiErrorBanner(L, st.actionError, [retryGen(st.actionError)]));
    if (st.phase === "check") {
      parts.push(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: L.checking })));
    } else if (st.phase === "check_error") {
      parts.push(aiErrorBanner(L, st.checkError, [
        h("button", { class: "btn btn-sm", type: "button", text: L.retry, onclick: check }),
        h("button", { class: "btn btn-sm btn-ghost", type: "button", text: aiNeedsRegen(st.checkError) ? L.replaceCorrupt : L.generateAnyway, onclick: () => generate(aiNeedsRegen(st.checkError)) }),
      ], st.checkError && st.checkError.code && L.err[st.checkError.code] ? null : L.err.check));
    } else if (st.phase === "empty") {
      const model = aiModelName();
      const gen2 = h("button", { class: "btn btn-primary ai-gen", type: "button", onclick: () => generate(aiNeedsRegen(st.actionError)) },
        h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { text: L.generate }),
        model ? h("span", { class: "ai-model mono", text: model }) : null);
      parts.push(h("div", { class: "ai-empty" }, h("p", { class: "ai-intro", text: L.intro }), h("div", { class: "ai-actions" }, gen2)));
      parts.push(digestDetails(st, L, () => fetchDigest(jobId), L.willSee).el);
    } else if (st.phase === "generating") {
      parts.push(aiLoadingPlate(L, st.rec, uid));
    } else if (st.phase === "result") {
      if (st.rec) {
        const cv = h("canvas", { class: "ai-regen-trace", "aria-hidden": "true" });
        const strip = h("div", { class: "ai-regen-strip", role: "status" }, cv,
          h("div", null, h("p", { class: "drafting-title", text: L.regenerating(st.rec.lang) }), h("p", { class: "drafting-sub", text: L.regenNote })));
        strip._start = () => trace(cv, { rows: 2, activity: 0.5, seed: hashString(`${uid}|regen`), colors: [COLORS.cyan, COLORS.magenta] });
        parts.push(strip);
      }
      parts.push(result(L));
      parts.push(footer(L));
    }
    section.replaceChildren(...parts.filter(Boolean));
    section.querySelectorAll(".ai-loading, .ai-regen-strip").forEach((el) => el._start && el._start());
    if (st.fresh) { st.fresh = false; arrive(); }
    if (st.pendingScroll && st.phase !== "check") {
      st.pendingScroll = false;
      requestAnimationFrame(() => api_.reveal());
    }
  }

  // Results arrive in reading order: headline, observations, then the hypothesis cards; the
  // confidence ticks light up per card (CSS, under .arrive).
  function arrive() {
    if (isReduced()) return;
    const parts = [...section.querySelectorAll(".ai-content > .banner, .ai-headline-wrap, .ai-obs > li, .hyp, .ai-sec-small, .ai-digest, .ai-foot")];
    stagger(parts, { step: 60, max: 14, y: 12, duration: DUR.enter });
  }

  const api_ = {
    start() {
      if (st.rec || st.phase === "result" || st.phase === "generating") return;
      if (st.phase === "check") { st.pendingStart = true; return; }
      generate(false);
    },
    reveal() {
      section.scrollIntoView({ behavior: isReduced() ? "auto" : "smooth", block: "start" });
      const t = section.querySelector(`#${titleId}`);
      if (t) t.focus({ preventScroll: true });
    },
  };
  check();
  return api_;
}

function aiRelTime(iso, lang) {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return String(iso);
  const f = new Intl.RelativeTimeFormat(lang === "ru" ? "ru" : "en", { numeric: "auto" });
  const s = (t - Date.now()) / 1000;
  const a = Math.abs(s);
  if (a < 60) return f.format(0, "minute");
  if (a < 3600) return f.format(Math.round(s / 60), "minute");
  if (a < 86400) return f.format(Math.round(s / 3600), "hour");
  return f.format(Math.round(s / 86400), "day");
}

/* ---------- composer: after a successful run ---------- */
function aiCta(slot, job) {
  const L = aiT(aiDefaultLang(job));
  const btn = h("button", { class: "btn btn-primary ai-cta-btn", type: "button" }, h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { text: L.cta }));
  const cta = h("div", { class: "ai-cta", lang: L.code },
    h("div", null, h("p", { class: "ai-cta-title", text: L.cta }), h("p", { class: "ai-cta-sub", text: L.ctaSub })),
    btn);
  btn.addEventListener("click", () => {
    slot.replaceChildren();
    const p = mountAiPanel(slot, job, { autostart: true });
    enter(slot.firstChild, { y: 14, duration: DUR.enter });
    p.reveal();
  });
  slot.replaceChildren(cta);
  enter(cta, { y: 12, duration: DUR.enter, delay: isReduced() ? 0 : 900 });
}

/* ---------- Library cards: interpret in place, then "View hypotheses" ---------- */
// A run with hypotheses is labelled in their language (this session's result, else the stored
// language from history); a run without uses the default language.
function cardLang(job) {
  const cached = aiStore.cache.get(job.job_id);
  const ml = cached && cached.meta && cached.meta.language;
  if (ml === "ru" || ml === "en") return ml;
  if (job.has_interpretation === true && (job.interpretation_language === "ru" || job.interpretation_language === "en")) return job.interpretation_language;
  return aiDefaultLang(job);
}
function cardAi(job, thumbTop, label) {
  if (job.status !== "succeeded") return null;
  const jobId = job.job_id;
  const box = h("div", { class: "card-ai" });
  const href = `#/job/${encodeURIComponent(jobId)}?ai=1`;
  let marker = null;
  const mark = (on, isNew, L) => {
    if (!on || marker) return;
    marker = h("span", { class: `ai-mark${isNew ? " new" : ""}`, title: L.markTitle, text: "AI" });
    thumbTop.append(marker);
  };
  function paint(isNew = false) {
    const lang = cardLang(job);
    const L = aiT(lang);
    box.setAttribute("lang", L.code);
    const rec = aiStore.inflight.get(jobId);
    const has = job.has_interpretation === true || aiStore.cache.has(jobId);
    mark(has, isNew, L);
    if (rec) {
      const secs = h("span", { class: "mono dim", "aria-hidden": "true" });
      box.replaceChildren(h("div", { class: "card-ai-busy" },
        h("span", { class: "spinner", "aria-hidden": "true" }),
        h("span", { role: "status", text: `${L.interpreting}…` }), secs));
      const tick = () => {
        if (aiStore.inflight.get(jobId) !== rec) return;
        secs.textContent = `${Math.floor((performance.now() - rec.started) / 1000)} ${L.code === "ru" ? "с" : "s"}`;
        if (box.isConnected || !box.parentNode) later(tick, 1000);
      };
      tick();
      rec.promise.then(() => { if (box.isConnected) paint(true); }, () => { if (box.isConnected) paint(); });
      return;
    }
    if (has) {
      box.replaceChildren(h("a", { class: "card-ai-link", href }, h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { text: L.viewHyp }), h("span", { class: "arrow", "aria-hidden": "true", text: "→" })));
      if (isNew) enter(box.firstChild, { y: 6, duration: DUR.view });
      return;
    }
    // The server runs one interpretation per account at a time: while another run's is in
    // flight this card waits (and repaints when that one settles) instead of failing with 429.
    const other = [...aiStore.inflight.values()].find((r) => r.jobId !== jobId);
    if (other) {
      box.replaceChildren(h("div", { class: "card-ai-wait" },
        h("button", { class: "btn btn-sm card-ai-btn", type: "button", disabled: true, "aria-describedby": `wait-${hashString(jobId).toString(36)}` },
          h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { text: L.interpret })),
        h("p", { class: "card-ai-note", id: `wait-${hashString(jobId).toString(36)}`, text: L.waitingFor(other.label) })));
      other.promise.then(() => { if (box.isConnected) paint(); }, () => { if (box.isConnected) paint(); });
      return;
    }
    const err = aiStore.errors.get(jobId);
    const regen = aiNeedsRegen(err);
    const btn = h("button", { class: "btn btn-sm card-ai-btn", type: "button", "aria-label": `${L.interpret}: ${label}` },
      h("span", { class: "ai-glyph", "aria-hidden": "true" }), h("span", { text: regen ? L.rebuild : err ? L.retry : L.interpret }));
    btn.addEventListener("click", () => {
      aiRequest(jobId, lang, regen, label);
      // every card on the page repaints: this one shows progress, the others wait
      document.querySelectorAll(".card-ai").forEach((b) => b !== box && b._paint && b._paint());
      paint();
    });
    const note = err ? aiRetryNote(L, err) : null;
    // replaceChildren() would print a null child as the text "null": only pass real nodes
    box.replaceChildren(...[
      err ? h("p", { class: "card-ai-err", role: "alert" },
        h("b", { text: aiErrTitle(L, err) }), " ",
        err.message || String(err),
        note ? ` ${note}` : "",
        errorMeta(err) ? h("span", { class: "mono card-ai-meta", text: errorMeta(err) }) : null) : null,
      btn].filter(Boolean));
  }
  box._paint = () => paint();
  paint();
  return box;
}

/* ================================================================== */
/* #/account                                                           */
/* ================================================================== */

async function renderAccount() {
  document.title = "Account — FlyLab";
  const gen = routeGen;
  const root = h("section", { class: "account" });
  view.append(root);
  root.append(h("div", { class: "launching sr-only" }, h("span", { text: "Loading account…" })),
    h("div", { class: "acct-head", "aria-hidden": "true" }, h("span", { class: "skel", style: "width:88px;height:88px;border-radius:50%" }), h("div", { class: "skel skel-title", style: "width:240px" })),
    h("div", { class: "tiles", "aria-hidden": "true" }, Array.from({ length: 4 }, () => h("div", { class: "skel skel-tile" }))));
  try {
    const { data } = await api("/api/v1/me");
    if (gen !== routeGen) return;
    state.me = data;
    paintIdentity();
  } catch (err) {
    if (gen !== routeGen) return;
    root.replaceChildren(errorBanner("Account details could not be loaded", err, [h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: route })]));
    return;
  }
  const u = state.me.user || {};
  const st = state.me.stats || {};
  const stat = (k, v, cls) => h("div", { class: `tile${cls ? ` ${cls}` : ""}` }, h("p", { class: "tile-label", text: k }), h("p", { class: "tile-value", text: v }));
  const signout = h("button", { class: "btn", type: "button", text: "Sign out", onclick: () => $("#signout-btn").click() });
  root.replaceChildren(
    h("header", { class: "acct-head" },
      h("span", { class: "avatar avatar-lg", "aria-hidden": "true", text: (u.display_name || u.username || "?").charAt(0).toUpperCase() }),
      h("div", null,
        h("p", { class: "eyebrow", text: "Account" }),
        h("h1", { class: "display page-title", text: u.display_name || u.username }),
        h("p", { class: "mono dim" }, `@${u.username}`, u.created_at ? ` · member since ${absTime(u.created_at)}` : ""))),
    h("div", { class: "tiles" },
      stat("Total runs", fmtInt(st.total_jobs)),
      stat("Done", fmtInt(st.succeeded)),
      stat("Running", fmtInt(st.running)),
      stat("Failed", fmtInt(st.failed), st.failed ? "tile-bad" : "")),
    h("p", { class: "muted" }, "Last run: ", h("span", { text: st.last_job_at ? `${relTime(st.last_job_at)} (${absTime(st.last_job_at)})` : "none yet" })),
    h("div", { class: "acct-actions" }, h("a", { class: "btn btn-primary", href: "#/history", text: "Open your library" }), signout));
  stagger([...root.children].filter((c) => !c.classList.contains("tiles")), { step: 70, y: 12, duration: DUR.enter });
  stagger(root.querySelectorAll(".tile"), { start: 120, step: 60, y: 12, duration: DUR.enter });
  root.querySelectorAll(".tile-value").forEach((v, i) => countUp(v, { duration: 900, delay: 150 + i * 60 }));
}

/* ================================================================== */
boot();
