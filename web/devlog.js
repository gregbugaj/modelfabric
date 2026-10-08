import { setCapture } from "./activity.js";
import { $ } from "./core.js";
import { fetchJSON, mm, nodeAPI, readSidePref, selfNode, writeSidePref } from "./my-models.js";
import { el, meshView } from "./rendering.js";
import { aliveNodes, devlogAppend, devlogBody, devlogClock, devlogFields, devlogKey, devlogMatches } from "./ui-model.js";

// The developer log: what is happening to each request as it happens. Every
// node keeps its own, so the page follows one node's stream and asks the
// others for what is new, merging by time. The node holds the history; this
// page keeps only what it has been sent, bounded, and starts over on reload.
//
// It lives in a dock under every page. It began as a tab on Activity, which
// meant leaving whatever page you were on to see what your request was
// doing, and coming back to see the result. Closed, the dock is a bar and
// follows nothing; open, it is a panel as tall as it was last dragged.
// How many lines the page holds, chosen in the dock. With engine lines on, a
// busy node writes dozens a second, and a page that kept them all would grow
// until the tab ran out of memory. Past the limit the oldest go; the node
// still has its own history.
const KEEP_CHOICES = [500, 1000, 2000, 5000];
let keep = KEEP_CHOICES.includes(Number(readSidePref("mfsh.dev.keep"))) ? Number(readSidePref("mfsh.dev.keep")) : 1000;
const ALL = "*";

let entries = [];
// A browser allows six connections to one address, shared by every tab open
// on it, and a stream holds one for as long as it is open. This page first
// opened a stream per node for "every node": with the page's other streams
// that was all six, and every other request the dashboard made, a health
// check among them, waited for ever. So one node is streamed at most, and
// when several are followed the others are asked for what is new every
// second and a half.
const POLL_MS = 1500;
// node -> EventSource. A node appears here only while its stream is open.
const sources = new Map();
// node -> { timer, after }: the nodes being polled, and the last entry had.
const polls = new Map();
const failed = new Set();
let node = readSidePref("mfsh.dev.node") || "";  // "" = this node, "*" = every node
let level = readSidePref("mfsh.dev.level") === "debug" ? "debug" : "info";
let filter = "";
let follow = true;
const open = new Set();  // keys of rows whose detail is expanded
let pending = false;
// Entries that arrived at the end since the last paint, and whether anything
// else changed. A log gains a line many times a second with engine lines on,
// and rebuilding three thousand rows for each would make the page crawl.
let fresh = [];
let redraw = true;
let capture = null;  // whether the followed node records bodies; null = unknown
// The dock: open or closed, how tall when open, and whether it fills the window.
const DOCK_MIN = 140;
const DOCK_BAR = 34;
const dock = {
  open: readSidePref("mfsh.dev.open") === "1",
  height: Number(readSidePref("mfsh.dev.height")) || 300,
  max: false,
};

function followed() {
  if (node === ALL) return aliveNodes(meshView);
  return [node || selfNode];
}

export function startDevlog() {
  stopDevlog();
  renderNodes();
  const nodes = followed();
  // Stream the node asked for, or this one when it is every node.
  const streamed = nodes.includes(selfNode) ? selfNode : nodes[0];
  for (const n of nodes) {
    if (n !== streamed) { startPoll(n); continue; }
    const src = new EventSource(nodeAPI(n, `/api/v1/devlog/stream?backlog=1&level=${level}`));
    sources.set(n, src);
    src.onopen = () => { failed.delete(n); renderStatus(); };
    src.onmessage = (ev) => {
      let e;
      try { e = JSON.parse(ev.data); } catch { return; }
      // Out of order (another node's clock, a replayed backlog) or a repeat
      // draws the list again; otherwise only the new line is added.
      take(e, n);
      schedule();
    };
    // EventSource retries by itself; this only says so. A node on a build
    // from before the developer log answers 404 and never opens.
    src.onerror = () => { failed.add(n); renderStatus(); };
  }
  loadCaptureState();
  renderStatus();
}

function take(e, n) {
  e.node ||= n;
  devlogAppend(entries, e, keep);
  if (entries[entries.length - 1] === e) fresh.push(e); else redraw = true;
}

function startPoll(n) {
  const p = { timer: 0, after: 0, busy: false };
  const ask = async () => {
    if (p.busy) return;
    p.busy = true;
    try {
      const r = await fetchJSON(nodeAPI(n, `/api/v1/devlog?level=${level}&limit=500&after=${p.after}`));
      failed.delete(n);
      for (const e of r.entries ?? []) {
        // A node on a build that does not know ?after sends everything again.
        if ((e.seq ?? 0) <= p.after) continue;
        p.after = e.seq;
        take(e, n);
      }
      schedule();
    } catch {
      failed.add(n);
    } finally {
      p.busy = false;
      renderStatus();
    }
  };
  p.timer = setInterval(ask, POLL_MS);
  polls.set(n, p);
  void ask();
}

export function stopDevlog() {
  for (const src of sources.values()) src.close();
  sources.clear();
  for (const p of polls.values()) clearInterval(p.timer);
  polls.clear();
  failed.clear();
  // Nothing is followed now, and the light must not say otherwise.
  renderStatus();
}

async function loadCaptureState() {
  capture = null;
  if (node === ALL) { renderCapture(); return; }
  try {
    const r = await fetchJSON(nodeAPI(node || selfNode, "/api/v1/devlog?limit=1"));
    capture = Boolean(r.capture);
  } catch { /* an older node: leave it unknown */ }
  renderCapture();
}

function renderCapture() {
  const box = $("dev-bodies");
  const note = $("dev-bodies-note");
  if (!box || !note) return;
  box.disabled = node === ALL || capture === null;
  box.checked = capture === true;
  note.textContent = node === ALL
    ? "Each node records bodies or not by its own switch; choose one node to change it."
    : capture === true
      ? "On: prompts and replies are held in memory on that node and shown here."
      : capture === false
        ? "Off: sizes and counts only. Nothing a request says is recorded."
        : "";
  $("dev-capture")?.classList.toggle("on", capture === true);
}

function renderNodes() {
  const sel = $("dev-node");
  if (!sel) return;
  const names = aliveNodes(meshView);
  const want = ["", ...names.filter((n) => n !== selfNode), ALL];
  if ([...sel.options].map((o) => o.value).join("|") !== want.join("|")) {
    sel.replaceChildren(...want.map((v) => {
      const o = el("option", null, v === "" ? `This node${selfNode ? ` (${selfNode})` : ""}` : v === ALL ? "Every node, merged" : v);
      o.value = v;
      return o;
    }));
  }
  // A remembered node that has left the mesh falls back to this one.
  if (!want.includes(node)) node = "";
  sel.value = node;
}

function renderStatus() {
  const s = $("dev-status");
  if (!s) return;
  const bad = [...failed];
  s.classList.toggle("bad", bad.length > 0);
  s.textContent = bad.length
    ? `no stream from ${bad.join(", ")} (unreachable, or on a build without the developer log)`
    : "";
  const following = sources.size + polls.size;
  $("dev-live")?.classList.toggle("on", following > 0 && bad.length < following);
}

function schedule() {
  if (pending) return;
  pending = true;
  requestAnimationFrame(() => { pending = false; paint(); });
}

// paint adds what is new, or draws everything when more than that changed.
function paint() {
  const box = $("dev-log");
  if (!box) return;
  if (redraw) { render(); return; }
  const add = fresh.filter((e) => devlogMatches(e, filter));
  fresh = [];
  box.append(...add.map(row));
  while (box.childElementCount > keep) box.firstElementChild.remove();
  counts(box.childElementCount);
  if (follow) box.scrollTop = box.scrollHeight;
}

function counts(shown) {
  const empty = $("dev-empty");
  if (empty) {
    empty.hidden = shown > 0;
    empty.textContent = entries.length
      ? "Nothing matches the filter."
      : "Waiting for a request. Send one to this mesh and it appears here as it arrives.";
  }
  $("dev-count").textContent = shown === entries.length ? `${entries.length} lines` : `${shown} of ${entries.length} lines`;
}

function row(e) {
  const key = devlogKey(e);
  const r = el("div", `dev-row lvl-${e.level} src-${e.source}`);
  r.dataset.key = key;
  const head = el("div", "dev-line");
  head.append(el("span", "dev-time", devlogClock(e.time)));
  head.append(el("span", `dev-level lvl-${e.level}`, e.level));
  if (node === ALL) head.append(el("span", "dev-node", e.node));
  if (e.model) head.append(el("span", "dev-model", e.model));
  if (e.trace) {
    const t = el("button", "dev-trace", e.trace.slice(0, 6));
    t.type = "button";
    t.title = `Show only request ${e.trace}`;
    t.addEventListener("click", (ev) => { ev.stopPropagation(); setFilter(e.trace); });
    head.append(t);
  } else if (e.source === "engine") {
    head.append(el("span", "dev-from", "engine"));
  }
  head.append(el("span", "dev-msg", e.msg));
  r.append(head);

  const more = devlogFields(e.fields);
  if (more || e.body || e.engine) {
    r.classList.add("has-more");
    head.title = "Click for the figures behind this line";
    head.addEventListener("click", () => {
      if (open.has(key)) open.delete(key); else open.add(key);
      render();
    });
    if (open.has(key)) {
      const d = el("div", "dev-more");
      if (e.engine) d.append(el("div", "dev-fields", `engine=${e.engine}${e.trace ? `  trace=${e.trace}` : ""}`));
      if (more) d.append(el("div", "dev-fields", more));
      if (e.body) d.append(el("pre", "act-body", devlogBody(e.body, e.truncated)));
      r.append(d);
    }
  }
  return r;
}

function render() {
  const box = $("dev-log");
  if (!box) return;
  redraw = false;
  fresh = [];
  const shown = entries.filter((e) => devlogMatches(e, filter));
  box.replaceChildren(...shown.map(row));
  counts(shown.length);
  if (follow) box.scrollTop = box.scrollHeight;
}

function setFilter(text) {
  filter = text;
  const input = $("dev-filter");
  if (input && input.value !== text) input.value = text;
  render();
}

export function initDevlog() {
  $("dev-node")?.addEventListener("change", (e) => {
    node = e.target.value;
    writeSidePref("mfsh.dev.node", node);
    entries = [];
    open.clear();
    render();
    startDevlog();
  });
  $("dev-level")?.addEventListener("change", (e) => {
    level = e.target.value === "debug" ? "debug" : "info";
    writeSidePref("mfsh.dev.level", level);
    // The node filters by level, so less detail means asking again.
    entries = [];
    render();
    startDevlog();
  });
  $("dev-filter")?.addEventListener("input", (e) => setFilter(e.target.value));
  $("dev-follow")?.addEventListener("change", (e) => { follow = e.target.checked; if (follow) render(); });
  $("dev-clear")?.addEventListener("click", () => { entries = []; open.clear(); render(); });
  $("dev-bodies")?.addEventListener("change", async (e) => {
    await setCapture({ bodies: e.target.checked }, node || selfNode);
    loadCaptureState();
  });
  // Scrolling up to read stops the page pulling the view back down.
  $("dev-log")?.addEventListener("scroll", (e) => {
    const b = e.target;
    const atEnd = b.scrollHeight - b.scrollTop - b.clientHeight < 24;
    if (!atEnd && follow) { follow = false; $("dev-follow").checked = false; }
  });
  const lv = $("dev-level");
  if (lv) lv.value = level;
  const kp = $("dev-keep");
  if (kp) {
    kp.replaceChildren(...KEEP_CHOICES.map((n) => Object.assign(el("option", null, n.toLocaleString()), { value: String(n) })));
    kp.value = String(keep);
    kp.addEventListener("change", (e) => {
      keep = Number(e.target.value);
      writeSidePref("mfsh.dev.keep", String(keep));
      // Fewer: the oldest go now, not when the next line arrives.
      if (entries.length > keep) entries.splice(0, entries.length - keep);
      render();
    });
  }

  $("dev-dock-toggle")?.addEventListener("click", () => setDock(!dock.open));
  $("dev-dock-max")?.addEventListener("click", () => { dock.max = !dock.max; layoutDock(); });
  $("dev-dock-grip")?.addEventListener("pointerdown", startDockResize);
  // Ctrl+` is where a terminal or a console is in most tools people use.
  document.addEventListener("keydown", (e) => {
    if (e.ctrlKey && !e.altKey && !e.metaKey && e.key === "`") { e.preventDefault(); setDock(!dock.open); }
  });
  // A tab nobody is looking at holds no stream (see the note on POLL_MS).
  document.addEventListener("visibilitychange", syncDock);
  window.addEventListener("resize", layoutDock);
  layoutDock();
  syncDock();
}

function setDock(open) {
  dock.open = open;
  if (!open) dock.max = false;
  writeSidePref("mfsh.dev.open", open ? "1" : "0");
  layoutDock();
  syncDock();
}

// syncDock follows the log while the dock is open and on screen, and stops
// when it is not.
function syncDock() {
  const want = dock.open && !document.hidden;
  const following = sources.size + polls.size > 0;
  if (want && !following) startDevlog();
  if (!want && following) stopDevlog();
}

// layoutDock sizes the dock and leaves the page room above it, so the last
// row of whatever is on the page can still be scrolled into view.
function layoutDock() {
  const el = $("dev-dock");
  if (!el) return;
  const most = Math.max(window.innerHeight - 56, DOCK_MIN);
  const h = !dock.open ? DOCK_BAR : dock.max ? most : Math.min(Math.max(dock.height, DOCK_MIN), most);
  el.classList.toggle("open", dock.open);
  el.classList.toggle("max", dock.open && dock.max);
  el.style.height = `${h}px`;
  document.documentElement.style.setProperty("--dev-dock-h", `${h}px`);
  $("dev-dock-toggle")?.setAttribute("aria-expanded", String(dock.open));
  if (dock.open && follow) { const box = $("dev-log"); if (box) box.scrollTop = box.scrollHeight; }
}

function startDockResize(e) {
  if (!dock.open || e.button !== 0) return;
  e.preventDefault();
  dock.max = false;
  const startY = e.clientY;
  const startH = $("dev-dock").getBoundingClientRect().height;
  const move = (ev) => { dock.height = Math.round(startH + (startY - ev.clientY)); layoutDock(); };
  const up = () => {
    document.removeEventListener("pointermove", move);
    document.removeEventListener("pointerup", up);
    document.body.classList.remove("dock-resizing");
    writeSidePref("mfsh.dev.height", String(Math.min(Math.max(dock.height, DOCK_MIN), window.innerHeight - 56)));
  };
  document.body.classList.add("dock-resizing");
  document.addEventListener("pointermove", move);
  document.addEventListener("pointerup", up);
}
