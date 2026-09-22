import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { icon } from "./discover.js";
import { fetchJSON, kv, mm, nodeAPI, readSidePref, selfNode, startSideResize, writeSidePref } from "./my-models.js";
import { el, render } from "./rendering.js";
import { meta } from "./routing.js";
import { available, operations } from "./runtime.js";
import { formatBytes, relativeTime } from "./ui-model.js";

/* ---------- activity: what the node did, and what it is doing ---------- */

// Kept in memory rather than re-fetched: the stream is the source, and a
// reload is allowed to start the list over. Capped so a long-lived tab does
// not grow a table with ten thousand rows in it.
const TRAFFIC_MAX = 250;
let trafficRows = [];
let trafficSource = null;

let captureCfg = { bodies: false, keep: 200, max_bytes: 8192, to_file: false, keep_max: 2000 };
// Requests live in each node's own memory, so "the mesh" is a fan-out rather
// than a subscription: this node streams live over SSE, peers are polled and
// merged. The choice is remembered because it is a way of working, not a mood.
export let actScope = readSidePref("mfsh.act.scope") === "mesh" ? "mesh" : "node";
const peerTraffic = new Map();  // node -> events, newest first
const peerCapture = new Map();  // node -> whether that node is capturing
// node -> "ok" | "old" | "unreachable". Nodes in this mesh run different
// builds on purpose, so a peer that does not know this route is a fact to
// report, not a failure to hide.
const peerStatus = new Map();
// Bodies fetched for a request someone opened, by row key. The mesh poll
// refreshes peer events every couple of seconds with metadata only, which used
// to wipe the bodies out from under the open flyout; they live here instead,
// outside what the poll replaces. Bounded: only clicked rows land in it.
const bodyCache = new Map();
const BODY_CACHE_MAX = 20;
// The request the flyout is showing, by key. A key rather than an index: new
// requests arrive at the top of the list while the panel is open.
export let actSelected = "";
// Wider than the model panel by default: JSON bodies read badly at 420px.
const actSide = { width: Number(readSidePref("mfsh.act.width")) || 560 };

// Every request the chosen scope covers, newest first. Each row carries the
// node whose front door it came through, which is not the node that served it.
function activityRows() {
  const rows = trafficRows.map((e) => ({ ...e, front: selfNode }));
  if (actScope === "mesh") {
    for (const [node, events] of peerTraffic) {
      for (const e of events) rows.push({ ...e, front: node, ...(bodyCache.get(trafficKey(e)) ?? {}) });
    }
  }
  rows.sort((a, b) => new Date(b.time) - new Date(a.time));
  return rows.slice(0, TRAFFIC_MAX);
}

// Peers are asked only while Activity is open and the scope includes them.
// Metadata only: the table shows no bodies, and a poll that shipped every
// prompt across the tailnet to render it would be careless. The one request
// someone opens fetches its own bodies.
export async function refreshPeerTraffic() {
  if (actScope !== "mesh") return;
  const peers = [...mm.nodes.keys()].filter((n) => n && n !== selfNode);
  await Promise.all(peers.map(async (node) => {
    try {
      const r = await fetchJSON(nodeAPI(node, `/api/v1/traffic/recent?limit=${TRAFFIC_MAX}`));
      peerTraffic.set(node, r.events ?? []);
      peerCapture.set(node, Boolean(r.capture));
      peerStatus.set(node, "ok");
    } catch (err) {
      // A node that will not answer is left out rather than faked. "No such
      // endpoint" means it is up and answering, just older than this route.
      peerTraffic.delete(node);
      peerCapture.delete(node);
      peerStatus.set(node, /no such endpoint/i.test(err.message) ? "old" : "unreachable");
    }
  }));
  renderTraffic();
  renderCapture();
}

export function setActScope(scope) {
  actScope = scope;
  bodyCache.clear();
  writeSidePref("mfsh.act.scope", scope);
  actSelected = "";
  for (const b of document.querySelectorAll("#act-scope .seg-btn")) {
    b.classList.toggle("active", b.dataset.scope === scope);
  }
  for (const th of document.querySelectorAll("th.act-front")) th.hidden = scope !== "mesh";
  $("act-scope-hint").textContent = scope === "mesh"
    ? "every request any node in the mesh answered, merged from each one's own log."
    : "every request this node answered.";
  // The page's own subtitle follows the scope: it was claiming "this node"
  // while the table showed the whole mesh.
  const sub = $("page-sub");
  if (sub && !document.querySelector('section[data-view="activity"]').hidden) {
    sub.textContent = scope === "mesh"
      ? "Every request the mesh answered, merged from each node, and what every node has loaded and unloaded."
      : "Every request this node answered, and what every node has loaded and unloaded.";
  }
  renderCapture();
  renderTraffic();
  if (scope === "mesh") refreshPeerTraffic();
}

export async function loadCapture() {
  try {
    const r = await fetch("/api/v1/traffic", { cache: "no-store" });
    if (r.ok) { captureCfg = await r.json(); renderCapture(); }
  } catch { /* the panel still works without it */ }
}

export async function setCapture(patch, node = selfNode) {
  try {
    // A peer's switch goes through this node, which forwards it over the
    // tailnet; that node accepts it only from a device of the same owner.
    const r = await fetch(nodeAPI(node, "/api/v1/traffic"), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(patch),
    });
    if (!r.ok) throw new Error(await r.text());
    if (node !== selfNode) {
      const cfg = await r.json();
      peerCapture.set(node, Boolean(cfg.bodies));
      // Its ring cleared with the switch, so drop what we hold of it rather
      // than showing bodies the node no longer has.
      if (!cfg.bodies) {
        peerTraffic.delete(node);
        bodyCache.clear();
      }
      renderCapture();
      refreshPeerTraffic();
      return;
    }
    {
      captureCfg = await r.json();
      // Turning capture off clears what was kept, so drop it here too rather
      // than leaving stale bodies expanded on screen.
      if (!captureCfg.bodies) {
        for (const e of trafficRows) { e.req_body = ""; e.resp_body = ""; }
      }
      renderCapture();
      renderTraffic();
    }
  } catch { /* leave the control where it was */ }
}

function renderCapture() {
  const box = $("act-capture");
  if (!box) return;
  $("act-bodies").checked = Boolean(captureCfg.bodies);
  $("act-keep").value = String(captureCfg.keep || 200);
  box.classList.toggle("on", Boolean(captureCfg.bodies));
  const kb = Math.round((captureCfg.max_bytes || 8192) / 1024);
  $("act-bodies-note").textContent = captureCfg.bodies
    ? `On — prompts and replies are held in memory, first ${kb}KB of each${captureCfg.to_file ? ", and appended to the log file" : ""}.`
    : "Off — only metadata is recorded.";
  renderPeerCapture();
}

// Capture is each node's own switch — its ring, its memory — so the mesh view
// offers one per node rather than a single control that would only ever have
// flipped the machine you happen to be looking at.
function renderPeerCapture() {
  const strip = $("act-peers");
  if (!strip) return;
  strip.hidden = actScope !== "mesh";
  strip.replaceChildren();
  if (actScope !== "mesh") return;
  const peers = [...mm.nodes.keys()].filter((n) => n && n !== selfNode).sort();
  if (!peers.length) return;
  strip.append(el("span", "hint", "Capture on other nodes"));
  for (const node of peers) {
    const status = peerStatus.get(node) ?? "unreachable";
    const label = el("label", "act-toggle" + (status === "ok" ? "" : " off"));
    const cb = el("input");
    cb.type = "checkbox";
    cb.checked = Boolean(peerCapture.get(node));
    cb.disabled = status !== "ok";
    cb.title = {
      ok: `Capture prompts and replies on ${node}`,
      old: `${node} runs a build without request sharing, so its requests stay on that machine — open its own dashboard to see them`,
      unreachable: `${node} is not answering; its requests are left out rather than guessed at`,
    }[status];
    cb.addEventListener("change", (e) => setCapture({ bodies: e.target.checked }, node));
    label.append(cb, el("span", null, node));
    if (status === "old") label.append(el("span", "chip", "older build"));
    strip.append(label);
  }
}

export function startTrafficStream() {
  if (trafficSource) return;
  // backlog=1 asks for what the node already has, so the table is not empty
  // the first time it is opened.
  trafficSource = new EventSource("/z/log/stream?backlog=1");
  trafficSource.addEventListener("message", (ev) => {
    let e;
    try { e = JSON.parse(ev.data); } catch { return; }
    trafficRows.unshift(e);
    if (trafficRows.length > TRAFFIC_MAX) trafficRows.length = TRAFFIC_MAX;
    renderTraffic();
  });
  trafficSource.addEventListener("open", () => { $("act-live").classList.add("on"); });
  trafficSource.addEventListener("error", () => { $("act-live").classList.remove("on"); });
}

// JSON bodies read far better indented; anything else is shown as it came.
function pretty(text) {
  try { return JSON.stringify(JSON.parse(text), null, 2); } catch { return text; }
}

function clockTime(iso) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleTimeString([], { hour12: false });
}

export function renderTraffic() {
  const body = $("act-log");
  if (!body) return;
  body.replaceChildren();
  const rows = activityRows();
  // The open request's trace, so its other records stand out in the table
  // rather than having to be hunted for by eye.
  const selectedTrace = rows.find((x) => trafficKey(x) === actSelected)?.trace || "";
  for (const e of rows) {
    const tr = el("tr");
    tr.append(el("td", "mono", clockTime(e.time)));
    if (actScope === "mesh") {
      // Where the request came in, which is a different question from where
      // it ran: an app talking to one node can be served by another.
      const front = el("td");
      front.append(el("span", null, e.front || "—"));
      if (e.front === selfNode) front.append(el("span", "chip", "this node"));
      tr.append(front);
    }
    tr.append(el("td", "mono", e.path || "—"));
    tr.append(el("td", null, e.model || "—"));

    const where = el("td");
    if (e.node) {
      where.append(el("span", null, e.node));
      if (e.engine) where.append(el("span", "hint", ` ${e.engine}`));
      // A request that did not stay on the node it arrived at crossed the
      // tailnet, which is the whole point of the mesh. "local" is recorded by
      // that node about itself, so it is read against the front door, never
      // against whichever node this dashboard happens to be running on.
      if (!e.local) where.append(el("span", "chip", "forwarded"));
      if (e.affine) where.append(el("span", "chip", "affinity"));
      // Who picked it is a separate fact from who ran it, and both fit.
      if (e.via) where.append(el("span", "chip", `via ${e.via}`));
    } else if (e.via) {
      // ModelFabric proxied it but did not choose the engine. Say who did rather
      // than leaving the row looking like a gap in the record. The chip alone:
      // what "via llm-d" means is said once in the panel hint, not repeated on
      // every row.
      where.append(el("span", "chip", `via ${e.via}`));
    } else {
      where.append(el("span", "hint", "—"));
    }
    tr.append(where);

    const status = el("td", "num", String(e.status || "—"));
    if (e.status && e.status >= 400) status.classList.add("bad");
    tr.append(status);
    tr.append(el("td", "num", e.ms != null ? `${e.ms} ms` : "—"));
    tr.append(el("td", "num", e.bytes_out != null ? formatBytes(e.bytes_out) : "—"));
    if (e.error) tr.title = e.error;

    // Every request opens, not just a captured one: how it was routed is the
    // part you usually came for, and it is recorded either way.
    const key = trafficKey(e);
    tr.dataset.row = key;
    tr.tabIndex = 0;
    tr.classList.add("act-row");
    if (key === actSelected) tr.classList.add("selected");
    else if (selectedTrace && e.trace === selectedTrace) tr.classList.add("act-linked");
    tr.title = "Click for the full request";
    tr.addEventListener("click", () => openActSide(key));
    tr.addEventListener("keydown", (ev) => {
      if (ev.target !== tr) return;
      if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); openActSide(key); }
    });
    body.append(tr);
  }
  $("act-log-empty").hidden = rows.length > 0;
  if (!rows.length) {
    // Under llm-d the EPP behind Envoy dials engines itself, so this node's
    // router never chose one and the log can be thin. Saying that beats an
    // empty table that looks like a broken stream.
    $("act-log-empty").textContent = actScope === "mesh"
      ? "Nothing yet on any node — send a request to any front door and it appears here."
      : "Nothing yet — send a request and it appears here.";
  }
  renderActSide(rows.find((e) => trafficKey(e) === actSelected), rows);
}

/* One request, in the flyout — the same panel My Models uses for a model. */

// Identity for a row: the node stamps no id, and time alone repeats when two
// requests land in the same millisecond.
function trafficKey(e) {
  return `${e.time}|${e.path}|${e.ms}|${e.bytes_out ?? ""}`;
}

function openActSide(key) {
  actSelected = key;
  renderTraffic();
  $("act-side").focus({ preventScroll: true });
  fetchBodiesFor(key);
}

// The mesh poll asks peers for metadata only, so a peer's request arrives
// without its prompt. One click is a different matter from a timer: fetch that
// node's ring with bodies, and if the row is still the one open, show them.
async function fetchBodiesFor(key) {
  const row = activityRows().find((e) => trafficKey(e) === key);
  if (!row || row.front === selfNode || row.req_body || row.resp_body) return;
  if (!peerCapture.get(row.front)) return; // that node is not capturing; nothing to fetch
  try {
    const r = await fetchJSON(nodeAPI(row.front, `/api/v1/traffic/recent?limit=${TRAFFIC_MAX}&bodies=1`));
    peerCapture.set(row.front, Boolean(r.capture));
    const got = (r.events ?? []).find((e) => trafficKey(e) === key);
    if (!got) return;
    if (bodyCache.size >= BODY_CACHE_MAX) bodyCache.delete(bodyCache.keys().next().value);
    bodyCache.set(key, { req_body: got.req_body, resp_body: got.resp_body, truncated: got.truncated });
    if (actSelected === key) renderTraffic();
  } catch { /* the panel keeps its metadata */ }
}

// Closing hands focus back to the row, so keyboard use does not restart at the
// top of a table that has grown while the panel was open.
export function closeActSide() {
  const key = actSelected;
  actSelected = "";
  renderTraffic();
  const row = document.querySelector(`#act-log [data-row="${CSS.escape(key)}"]`);
  if (row) row.focus({ preventScroll: true });
}

async function copyToClipboard(text, what) {
  try {
    await navigator.clipboard.writeText(text);
    showNotice(`${what} copied`, "success");
  } catch {
    showNotice("Clipboard is not available here", "error");
  }
}

function renderActSide(e, rows = []) {
  const side = $("act-side");
  if (!side) return;
  side.hidden = !e;
  if (!e) return;

  side.replaceChildren();
  side.tabIndex = -1;
  side.style.setProperty("--side-w", `${actSide.width}px`);
  const grip = el("div", "side-grip");
  grip.title = "Drag to resize";
  grip.addEventListener("pointerdown", (ev) => startSideResize(ev, "act-side", actSide, "mfsh.act.width"));
  side.append(grip);

  const head = el("div", "side-head");
  const title = el("div", "side-title");
  title.append(el("span", "mono", e.path || "request"));
  const actions = el("div", "side-actions");
  const close = el("button", "btn icon", "×");
  close.title = "Close (Esc)";
  close.addEventListener("click", closeActSide);
  actions.append(close);
  title.append(actions);

  const sub = el("div", "side-sub");
  const status = el("span", e.status >= 400 ? "pill fit-no" : "pill fit-yes", String(e.status || "—"));
  sub.append(status);
  sub.append(el("span", "hint", new Date(e.time).toLocaleString()));
  head.append(title, sub);
  side.append(head);

  const body = el("div", "side-body");
  const box = el("div", "kv-list");
  kv(box, "Model", e.model || "—");
  if (actScope === "mesh") {
    kv(box, "Front door", e.front + (e.front === selfNode ? " (this node)" : ""), false);
  }
  // Two separate facts, kept apart: which machine ran it, and who decided.
  // The machine is resolved from the address the request was dialled at, so
  // "served by" is answerable even when ModelFabric did not choose.
  if (e.node) {
    kv(box, "Served by", e.node + (e.node === selfNode ? " (this node)" : ""), false);
    kv(box, "Engine", e.engine || "not identified — no engine of that node listens on this port", !!e.engine);
  } else if (e.via === "llm-d") {
    // Envoy is the address; the EPP behind it chose an engine somewhere in the
    // mesh and does not report which. Naming this machine would be a guess.
    kv(box, "Served by", "not reported — llm-d's scheduler picked the engine behind Envoy", false);
  } else {
    kv(box, "Served by", e.upstream ? "not in the mesh" : "not recorded", false);
  }
  if (e.upstream) kv(box, "Upstream", e.upstream);
  if (e.trace) kv(box, "Trace", e.trace);
  // The router that chose is the one at the front door the request came
  // through — which is this node only when you are looking at its own log.
  const chooser = !e.front || e.front === selfNode ? "this node" : `${e.front}'s router`;
  kv(box, "Chosen by", e.via
    ? `${e.via} — ModelFabric carried the request; ${e.via} picked where it went`
    : `${chooser} — ${e.affine ? "prefix affinity" : "least loaded"}`, false);
  kv(box, "Latency", e.ms != null ? `${e.ms} ms` : "—");
  kv(box, "Response size", e.bytes_out != null ? formatBytes(e.bytes_out) : "—");
  if (e.error) kv(box, "Error", e.error, false);
  body.append(box);

  // The point of the trace: a request forwarded from one front door to another
  // node is recorded by both, and those two rows are one request. Shown only
  // when there is more than one, so a purely local request stays uncluttered.
  const hops = e.trace ? rows.filter((x) => x.trace === e.trace) : [];
  if (hops.length > 1) {
    hops.sort((a, b) => new Date(a.time) - new Date(b.time));
    const group = el("div", "side-group");
    group.append(el("div", "side-group-title", `Path — ${hops.length} nodes recorded this request`));
    const list = el("ol", "act-path");
    for (const h of hops) {
      const li = el("li");
      li.append(el("span", "act-path-front", h.front || "—"));
      const arrow = el("span", "act-path-to");
      arrow.textContent = h.node ? `→ ${h.node}${h.engine ? ` ${h.engine}` : ""}` : (h.via ? `→ via ${h.via}` : "→ —");
      li.append(arrow);
      const meta = el("span", "act-path-meta");
      meta.textContent = `${h.status ?? "—"} · ${h.ms != null ? `${h.ms} ms` : "—"}`;
      li.append(meta);
      if (trafficKey(h) === actSelected) li.classList.add("here");
      li.addEventListener("click", () => openActSide(trafficKey(h)));
      list.append(li);
    }
    group.append(list);
    body.append(group);
  }

  const part = (label, text) => {
    if (!text) return;
    const group = el("div", "side-group");
    const gh = el("div", "act-body-head");
    gh.append(el("span", "side-group-title", label));
    const copy = el("button", "btn icon", "⧉");
    copy.title = `Copy the ${label.toLowerCase()}`;
    copy.addEventListener("click", () => copyToClipboard(text, label));
    gh.append(copy);
    group.append(gh);
    // textContent, via el(): this is somebody's prompt coming back off the wire.
    group.append(el("pre", "act-body", pretty(text)));
    body.append(group);
  };
  part("Request", e.req_body);
  part("Response", e.resp_body);

  if (e.truncated) {
    body.append(el("p", "side-note hint",
      `Kept the first ${Math.round((captureCfg.max_bytes || 8192) / 1024)}KB of each body; the rest was dropped.`));
  }
  if (!e.req_body && !e.resp_body) {
    const remote = e.front && e.front !== selfNode;
    const capturing = remote ? peerCapture.get(e.front) : captureCfg.bodies;
    body.append(el("p", "side-note hint", capturing
      ? "No bodies for this request — capture was off when it ran."
      : remote
        ? `Capture is off on ${e.front}, so only metadata was recorded there. Its switch is in the strip above the table.`
        : "Capture is off, so only metadata was recorded. Turn on “Capture request & response” and send it again to see what was on the wire."));
  }
  side.append(body);
}

export function renderOperations() {
  const body = $("act-ops");
  if (!body) return;
  body.replaceChildren();

  // Every node keeps its own journal, and a load that happened on another
  // machine is exactly the thing you came here to find. mm.nodes already holds
  // each peer's operations for My Models, so reuse it rather than fetching twice.
  const all = [];
  for (const [node, entry] of mm.nodes) {
    for (const o of entry?.operations ?? []) all.push({ ...o, node });
  }
  if (!all.length) for (const o of operations) all.push({ ...o, node: selfNode });

  all.sort((a, b) => String(b.started_at || "").localeCompare(String(a.started_at || "")));
  const rows = all.slice(0, 150);

  for (const o of rows) {
    const tr = el("tr");
    tr.append(el("td", "mono", o.id || "—"));

    const node = el("td");
    node.append(el("span", null, o.node || "—"));
    if (o.node && o.node === selfNode) node.append(el("span", "chip", "this node"));
    tr.append(node);

    tr.append(el("td", null, o.kind || "—"));
    tr.append(el("td", null, o.model || "—"));

    const state = el("td");
    const chip = el("span", "chip", o.state || "—");
    if (o.state === "failed") chip.classList.add("bad");
    if (o.state === "running") chip.classList.add("on");
    state.append(chip);
    tr.append(state);

    tr.append(stampCell(o.started_at));
    tr.append(stampCell(o.ended_at));
    tr.append(el("td", "num", o.elapsed_ms != null ? formatDuration(o.elapsed_ms) : "—"));
    tr.append(el("td", "hint", o.message || ""));
    if (o.instance_id) tr.title = `instance ${o.instance_id}`;
    body.append(tr);
  }

  $("act-ops-empty").hidden = rows.length > 0;
  if (!rows.length) $("act-ops-empty").textContent = "No operations recorded yet.";
}

// Clock time in the cell, the full timestamp on hover: a journal spanning days
// needs the date, but showing it on every row would drown the time you want.
function stampCell(iso) {
  const td = el("td", "mono");
  if (!iso) { td.textContent = "—"; td.classList.add("hint"); return td; }
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) { td.textContent = "—"; return td; }
  td.textContent = d.toLocaleTimeString([], { hour12: false });
  td.title = `${d.toLocaleString()} (${relativeTime(iso)})`;
  return td;
}

function formatDuration(ms) {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)} s`;
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}
