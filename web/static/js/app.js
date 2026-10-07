// FlyLab application: auth gate, composer, plan review, live runs, library, account.
// Vanilla ES module. Every failure is shown on screen; nothing is only logged.

import { SpikingNet, drawRasterThumb, drawResultRaster, hashString } from "/static/js/neural.js";

const reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

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
  statusEl.replaceChildren(h("span", { class: "mono dl-progress ok", text: `${label} downloaded · ${(blob.size / 1024 / 1024).toFixed(2)} MB` }));
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
};
const timers = new Set();
let routeGen = 0;
function later(fn, ms) {
  const id = setTimeout(() => { timers.delete(id); fn(); }, ms);
  timers.add(id);
  return id;
}
function clearTimers() { for (const id of timers) clearTimeout(id); timers.clear(); }

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
  authEl.hidden = false;
  document.title = "Sign in — FlyLab";
  $("#auth-notice").replaceChildren(notice ? banner("info", notice) : "");
  $("#auth-error").replaceChildren();
  setAuthMode(authMode);
  applyRegistrationPolicy();
  if (!authNet) {
    authNet = new SpikingNet($("#auth-net"), { reduced, seed: 94 });
  } else {
    authNet.resize();
  }
  authNet.start();
  setTimeout(() => $("#auth-username").focus(), 30);
}

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
  if (open) menu.querySelector("[role=menuitem]").focus();
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
  routeGen++;
  $("#global-banner").replaceChildren();
  loadCaps();
  const r = parseHash();
  document.querySelectorAll(".tabs a").forEach((a) => {
    const on = a.dataset.route === r.name || (r.name === "job" && a.dataset.route === "history");
    if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  view.replaceChildren();
  window.scrollTo(0, 0);
  if (r.name === "new") renderNew(r.params);
  else if (r.name === "history") renderHistory();
  else if (r.name === "job" && r.id) renderJobPage(r.id);
  else if (r.name === "account") renderAccount();
  else renderMissing();
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
  "Стимулируй сахарные рецепторы на 50 Гц, замолчи один нейрон и сравни MN9",
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
      onclick: () => { textarea.value = c; state.draft = c; sync(); textarea.focus(); },
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
    planSlot.replaceChildren();
    runSlot.replaceChildren();
    const started = performance.now();
    const elapsed = h("span", { class: "mono", text: "0 s" });
    const drafting = h("div", { class: "drafting" },
      h("div", { class: "drafting-raster", "aria-hidden": "true" }, Array.from({ length: 7 }, () => h("span"))),
      h("div", null,
        h("p", { class: "drafting-title", text: "Drafting a plan from your description" }),
        h("p", { class: "drafting-sub" }, "Matching neuron groups and checking limits · ", elapsed)));
    parseSlot.replaceChildren(drafting);
    const tick = setInterval(() => { elapsed.textContent = `${Math.round((performance.now() - started) / 1000)} s`; }, 500);
    let parsed;
    try {
      ({ data: parsed } = await api("/api/v1/plans/parse", { method: "POST", body: { prompt, dataset_id: "flywire_630", report_language: lang } }));
    } catch (err) {
      clearInterval(tick);
      if (gen !== routeGen) return;
      const titles = { 502: "The planner failed on this request", 503: "The planner is busy", 429: "Planner limit reached", 401: "You are signed out" };
      parseSlot.replaceChildren(errorBanner(titles[err.status] || "Could not generate a plan", err, [
        h("button", { class: "btn btn-sm", type: "button", text: "Try again", onclick: () => composer.requestSubmit() }),
        h("button", { class: "btn btn-sm btn-ghost", type: "button", text: "Use Advanced", onclick: () => { if (advanced.hidden) advToggle.click(); advanced.scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "start" }); } }),
      ]));
      return;
    } finally {
      clearInterval(tick);
      genBtn.disabled = false;
    }
    if (gen !== routeGen) { storeParse(parsed, prompt); return; }
    try {
      handleParse(parsed, prompt);
    } catch (err) {
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
      textarea.focus();
      return;
    }
    if (data.status === "unsupported") {
      parseSlot.replaceChildren(h("div", { class: "notice-card" },
        h("p", { class: "eyebrow", text: "Outside what the simulator does" }),
        h("p", { class: "notice-msg", text: data.message || "This request cannot be expressed as a stimulation experiment." }),
        h("p", { class: "muted small", text: "FlyLab stimulates and silences neuron groups and reads out spike rates. Whole-animal behaviour such as walking or flight is out of scope." })));
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
    paintPlan();
  }

  function paintPlan() {
    const p = state.lastPlan;
    if (!p) return;
    if (!state.groups && !state.groupsError) loadGroups().then(() => { if (gen === routeGen && state.lastPlan === p) paintPlan(); }).catch(() => { if (gen === routeGen && state.lastPlan === p) paintPlan(); });
    planSlot.replaceChildren(planCard(p, {
      onRun: (title, key) => startRun({
        planId: p.planId, prompt: p.prompt, title, slot: runSlot, key,
        onCreated: (job) => {
          runSlot.replaceChildren();
          mountJob(runSlot, job, { compact: true });
          loadRecent(recent, gen);
          runSlot.scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "start" });
        },
      }),
    }));
    if (!reduced) planSlot.firstChild.animate([{ opacity: 0, transform: "translateY(10px)" }, { opacity: 1, transform: "none" }], { duration: 420, easing: "cubic-bezier(.2,.7,.1,1)" });
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
        $(".plan-slot").scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "start" });
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
  const grid = h("div", { class: "recent-grid" });
  root.append(grid);
  try {
    const { data } = await api("/api/v1/jobs?limit=6&offset=0");
    if (gen !== routeGen) return;
    const jobs = (data && data.jobs) || [];
    if (!jobs.length) {
      grid.replaceWith(h("p", { class: "muted small recent-empty", text: "Your runs will collect here — each one gets its own raster fingerprint." }));
      return;
    }
    jobs.forEach((j) => grid.append(jobCard(j, { aspect: 4 / 3 })));
    requestAnimationFrame(() => grid.querySelectorAll("canvas").forEach((c) => c._draw && c._draw()));
  } catch (err) {
    if (gen !== routeGen) return;
    grid.replaceWith(errorBanner("Recent runs could not be loaded", err));
  }
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
  c._draw = () => drawRasterThumb(c, thumbFor(job));
  const s = job.summary;
  const title = job.title || job.prompt || planShort(job.plan) || job.job_id;
  const counts = s
    ? h("span", { class: "mono card-counts" }, h("i", { class: "sw sw-a", "aria-hidden": "true" }), `A ${fmtInt(s.total_spikes_A)}`,
      s.total_spikes_B !== null && s.total_spikes_B !== undefined ? [h("i", { class: "sw sw-b", "aria-hidden": "true" }), ` B ${fmtInt(s.total_spikes_B)}`] : null)
    : null;
  const errs = [job.plan_error ? `Plan unreadable: ${job.plan_error}` : null, job.summary_error ? `Summary unreadable: ${job.summary_error}` : null].filter(Boolean);
  const marker = job.plan_error ? "Plan unreadable" : "Summary unreadable";
  return h("a", { class: `job-card${errs.length ? " has-error" : ""}`, href: `#/job/${encodeURIComponent(job.job_id)}`, "aria-label": `${title} — ${STATUS_LABEL[job.status] || job.status}${errs.length ? ` — ${marker}` : ""}` },
    h("div", { class: "job-thumb" }, c,
      h("div", { class: "thumb-top" }, statusChip(job.status)),
      errs.length ? h("span", { class: "thumb-error", title: errs.join("\n"), text: errs.length > 1 ? "Plan + summary unreadable" : marker }) : null),
    h("div", { class: "job-body" },
      h("p", { class: "job-title", text: title }),
      errs.map((e) => h("p", { class: "job-err mono", text: e })),
      h("p", { class: "job-meta mono" }, counts, h("span", { text: relTime(job.created_at), title: absTime(job.created_at) }))));
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
    foot.replaceChildren(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: offset ? "Loading more runs…" : "Loading your runs…" })));
    const myFilter = filter;
    try {
      const q = `/api/v1/jobs?limit=24&offset=${offset}${filter ? `&status=${encodeURIComponent(filter)}` : ""}`;
      const { data } = await api(q);
      if (gen !== routeGen || inflight !== mine || myFilter !== filter) return;
      if (!data || !Array.isArray(data.jobs)) throw new ApiError(200, "BAD_SHAPE", "GET /api/v1/jobs did not return a jobs array");
      total = typeof data.total === "number" ? data.total : offset + data.jobs.length + (data.jobs.length === 24 ? 1 : 0);
      for (const j of data.jobs) {
        const aspect = [4 / 3, 1, 4 / 5, 16 / 10][hashString(j.job_id) % 4];
        const card = jobCard(j, { aspect });
        const c = card.querySelector("canvas");
        masonry.add(card, aspect, () => c._draw());
      }
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

function mountJob(root, job, { compact = false, onStatus = null } = {}) {
  const gen = routeGen;
  const stageList = h("ol", { class: "stages", "aria-label": "Run stages" }, STAGES.map((s) => h("li", { "data-stage": s, text: s })));
  const bar = h("div", { class: "bar" });
  const pct = h("span", { class: "mono pct" });
  const elapsed = h("span", { class: "mono" });
  const chipSlot = h("span");
  const cancelBtn = h("button", { class: "btn btn-ghost btn-sm", type: "button", text: "Cancel run" });
  const msg = h("div", { class: "run-msg", "aria-live": "polite" });
  // Errors of user actions live in their own slot: poll() only manages msg.
  const actionMsg = h("div", { class: "run-msg", "aria-live": "assertive" });
  const results = h("div", { class: "results-slot" });
  const live = h("div", { class: "live" },
    h("div", { class: "progress", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": "Run progress" }, bar),
    h("div", { class: "live-row" }, stageList, h("span", { class: "live-nums" }, pct, elapsed)));
  const head = h("header", { class: "run-head" },
    h("div", null,
      h("p", { class: "eyebrow" }, compact ? "Run " : "", h("a", { class: "mono", href: `#/job/${encodeURIComponent(job.job_id)}`, text: job.job_id })),
      compact ? h("p", { class: "run-title", text: job.title || job.prompt || planShort(job.plan) || "Untitled run" }) : null),
    h("div", { class: "run-head-end" }, chipSlot, cancelBtn));
  const panel = h("article", { class: "run-card" }, head, live, actionMsg, msg, results);
  root.append(panel);
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

  function paint(j) {
    current = { ...current, ...j };
    chipSlot.replaceChildren(statusChip(current.status));
    const p = Math.max(0, Math.min(100, Number(current.progress_pct) || 0));
    bar.style.width = `${current.status === "succeeded" ? 100 : p}%`;
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
    if (onStatus) onStatus(current);
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
        renderResults(results, current);
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
    if (job.status === "succeeded") renderResults(results, job);
    else if (job.status === "failed") msg.replaceChildren(banner("error", "The simulation failed", job.error_message || "The worker reported a failure without a message.", { meta: job.error_code || null }));
    else msg.replaceChildren(banner("info", "Run cancelled", "Nothing was kept from this run."));
  } else {
    poll();
  }
  return panel;
}

async function renderResults(root, job) {
  const gen = routeGen;
  const id = encodeURIComponent(job.job_id);
  root.replaceChildren(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: "Loading results…" })));
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
            h("span", { class: "dbar", "aria-hidden": "true" }, h("i", { style: `${r.delta_hz < 0 ? "right:50%" : "left:50%"};width:${(Math.abs(r.delta_hz || 0) / maxAbs) * 50}%` })),
            h("span", { text: fmtSigned(r.delta_hz) })) : null);
      }))))
    : h("p", { class: "muted small", text: "This plan had no readout neurons, so there is no readout table." });

  // raster
  const rasterCanvas = h("canvas", { class: "raster", role: "img", "aria-label": "Spike raster: one row per neuron, condition A in magenta on top, condition B in green below" });
  const tip = h("div", { class: "tip", hidden: true, role: "tooltip" });
  const rasterNote = h("p", { class: "muted small raster-note" });
  const rasterBody = h("div", { class: "raster-wrap" }, rasterCanvas, tip);
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

  root.append(h("section", { class: "results" },
    tiles,
    h("div", { class: "results-grid" },
      h("section", { class: "panel" }, h("div", { class: "panel-head" }, h("h3", { class: "panel-title", text: "Readout neurons" })), table),
      rasterPanel),
    downloads));

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
    const partial = conds.filter((c) => counts[c].got < counts[c].total).map((c) => `condition ${c}: first ${fmtInt(counts[c].got)} of ${fmtInt(counts[c].total)} rows`);
    rasterNote.textContent = `Showing the ${geo.shown} most active of ${fmtInt(geo.total)} neurons (${conds.map((c) => `${c}: ${fmtInt(counts[c].total)} neuron × trial rows`).join(", ")}). Each row shows that neuron’s spike count; tick positions inside the window are illustrative — exact spike times are in spikes.parquet in the ZIP.${partial.length ? ` Only part of the data is drawn (${partial.join("; ")}).` : ""}`;
    if (missingB) {
      rasterPanel.insertBefore(banner("error", "Condition B is missing from the raster",
        `The summary reports ${fmtInt(summary.total_spikes_B)} spikes in B, but rates.csv returned no B rows. Only condition A is drawn.`), rasterBody);
    }
    new ResizeObserver(() => { geo = draw(); }).observe(rasterBody);
    rasterCanvas.addEventListener("pointermove", (e) => {
      const r = rasterCanvas.getBoundingClientRect();
      const i = Math.floor((e.clientY - r.top - geo.top + 1) / geo.rowH);
      const n = geo.neurons[i];
      if (!n) { tip.hidden = true; return; }
      tip.hidden = false;
      tip.replaceChildren(
        h("b", { class: "mono", text: n.root_id }),
        h("span", { text: `${n.readout ? "readout · " : ""}A ${fmtInt(n.A)} spikes${n.hasB ? ` · B ${fmtInt(n.B)}` : ""}` }));
      const x = Math.min(e.clientX - r.left + 14, r.width - 220);
      tip.style.transform = `translate(${Math.max(0, x)}px, ${e.clientY - r.top + 12}px)`;
    });
    rasterCanvas.addEventListener("pointerleave", () => { tip.hidden = true; });
  } catch (err) {
    if (gen !== routeGen) return;
    rasterBody.replaceChildren(errorBanner("Spike data could not be loaded", err));
  }
}

/* ================================================================== */
/* #/job/<id>                                                          */
/* ================================================================== */

async function renderJobPage(jobId) {
  document.title = "Run — FlyLab";
  const gen = routeGen;
  const root = h("section", { class: "job-page" });
  view.append(h("a", { class: "back link", href: "#/history", text: "← Library" }), root);
  root.append(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: "Loading run…" })));
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
      copyStatus.textContent = "Link copied";
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
  if (gen !== routeGen) return;
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
        h("div", { class: "job-actions" }, againBtn, copyBtn, copyStatus),
        againSlot)),
    job.summary_error ? banner("error", "This run’s summary file is corrupted", job.summary_error) : null,
    h("div", { class: "job-live" }),
    plan ? h("details", { class: "json" }, h("summary", { text: "Plan JSON" }), h("pre", { class: "mono", text: JSON.stringify(plan, null, 2) })) : null,
  ].filter(Boolean));
  requestAnimationFrame(() => drawRasterThumb(art, thumbFor(job)));
  mountJob(root.querySelector(".job-live"), job, { compact: false, onStatus });
}

/* ================================================================== */
/* #/account                                                           */
/* ================================================================== */

async function renderAccount() {
  document.title = "Account — FlyLab";
  const gen = routeGen;
  const root = h("section", { class: "account" });
  view.append(root);
  root.append(h("div", { class: "launching" }, h("span", { class: "spinner", "aria-hidden": "true" }), h("span", { text: "Loading account…" })));
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
}

/* ================================================================== */
boot();
