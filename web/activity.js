import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { icon } from "./discover.js";
import { fetchJSON, kv, mm, nodeAPI, readSidePref, selfNode, startSideResize, writeSidePref } from "./my-models.js";
import { el, render } from "./rendering.js";
import { meta } from "./routing.js";
import { available, operations } from "./runtime.js";
import { formatBytes, relativeTime } from "./ui-model.js";

// The stream owns this in-memory history; a reload starts it over.
// Cap it to bound memory in long-lived tabs.
const TRAFFIC_MAX = 250;
let trafficRows = [];
let trafficSource = null;

let captureCfg = { bodies: false, keep: 200, max_bytes: 8192, to_file: false, keep_max: 2000 };
// Request histories are node-local: stream this node over SSE, then poll
// and merge peer histories.
export let actScope = readSidePref("mfsh.act.scope") === "mesh" ? "mesh" : "node";
const peerTraffic = new Map();  // node -> events, newest first
const peerCapture = new Map();  // node -> whether that node is capturing
// node -> "ok" | "old" | "unreachable"; older peers may lack this endpoint.
const peerStatus = new Map();
// Cache opened bodies separately: metadata-only peer polls replace events
// and would otherwise clear the open flyout.
const bodyCache = new Map();
const BODY_CACHE_MAX = 20;
// Use a key, not an index: new requests are inserted while the flyout is open.
export let actSelected = "";
// JSON bodies need more width than the model panel.
const actSide = { width: Number(readSidePref("mfsh.act.width")) || 560 };

// Rows name the entry node, which may differ from the serving node.
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

// Poll peer metadata only while the mesh Activity view is open.
// Fetch prompt bodies only when their request is opened.
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
      // A missing endpoint identifies an older peer; other failures are unreachable.
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
  } catch { /* Keep metadata available if capture status cannot be fetched. */ }
}

export async function setCapture(patch, node = selfNode) {
  try {
    // The proxy forwards over the tailnet; the peer requires the same owner.
    const r = await fetch(nodeAPI(node, "/api/v1/traffic"), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(patch),
    });
    if (!r.ok) throw new Error(await r.text());
    if (node !== selfNode) {
      const cfg = await r.json();
      peerCapture.set(node, Boolean(cfg.bodies));
      // Capture changes clear the node ring, so discard its cached bodies too.
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
      // Turning capture off clears the node ring and cached bodies.
      if (!captureCfg.bodies) {
        for (const e of trafficRows) { e.req_body = ""; e.resp_body = ""; }
      }
      renderCapture();
      renderTraffic();
    }
  } catch { /* Preserve the previous control state on failure. */ }
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

// Capture and its in-memory ring are configured separately on each node.
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
  const selectedTrace = rows.find((x) => trafficKey(x) === actSelected)?.trace || "";
  for (const e of rows) {
    const tr = el("tr");
    tr.append(el("td", "mono", clockTime(e.time)));
    if (actScope === "mesh") {
      // The entry node can differ from the serving node.
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
      // Resolve "local" relative to the request's entry node, not this dashboard.
      if (!e.local) where.append(el("span", "chip", "forwarded"));
      if (e.affine) where.append(el("span", "chip", "affinity"));
      if (e.via) where.append(el("span", "chip", `via ${e.via}`));
    } else if (e.via) {
      // llm-d chose the engine; ModelFabric only proxied the request.
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

    // Routing metadata is available even when body capture is off.
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
    // With llm-d, EPP chooses engines behind Envoy; this router may log less detail.
    $("act-log-empty").textContent = actScope === "mesh"
      ? "Nothing yet on any node — send a request to any front door and it appears here."
      : "Nothing yet — send a request and it appears here.";
  }
  renderActSide(rows.find((e) => trafficKey(e) === actSelected), rows);
}

// Nodes provide no request ID, and timestamps can repeat within a millisecond.
function trafficKey(e) {
  return `${e.time}|${e.path}|${e.ms}|${e.bytes_out ?? ""}`;
}

function openActSide(key) {
  actSelected = key;
  renderTraffic();
  $("act-side").focus({ preventScroll: true });
  fetchBodiesFor(key);
}

// Peer polls omit bodies. Fetch them on open, then verify the same row
// is still selected before displaying them.
async function fetchBodiesFor(key) {
  const row = activityRows().find((e) => trafficKey(e) === key);
  if (!row || row.front === selfNode || row.req_body || row.resp_body) return;
  if (!peerCapture.get(row.front)) return; // No bodies exist when capture is off.
  try {
    const r = await fetchJSON(nodeAPI(row.front, `/api/v1/traffic/recent?limit=${TRAFFIC_MAX}&bodies=1`));
    peerCapture.set(row.front, Boolean(r.capture));
    const got = (r.events ?? []).find((e) => trafficKey(e) === key);
    if (!got) return;
    if (bodyCache.size >= BODY_CACHE_MAX) bodyCache.delete(bodyCache.keys().next().value);
    bodyCache.set(key, { req_body: got.req_body, resp_body: got.resp_body, truncated: got.truncated });
    if (actSelected === key) renderTraffic();
  } catch { /* Keep metadata available if bodies cannot be fetched. */ }
}

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
  // The dialled address identifies the serving machine even when
  // ModelFabric did not choose it.
  if (e.node) {
    kv(box, "Served by", e.node + (e.node === selfNode ? " (this node)" : ""), false);
    kv(box, "Engine", e.engine || "not identified — no engine of that node listens on this port", !!e.engine);
  } else if (e.via === "llm-d") {
    // Envoy does not report the engine EPP selected; leave the serving node unknown.
    kv(box, "Served by", "not reported — llm-d's scheduler picked the engine behind Envoy", false);
  } else {
    kv(box, "Served by", e.upstream ? "not in the mesh" : "not recorded", false);
  }
  if (e.upstream) kv(box, "Upstream", e.upstream);
  if (e.trace) kv(box, "Trace", e.trace);
  // The entry node owns the router decision, including in peer logs.
  const chooser = !e.front || e.front === selfNode ? "this node" : `${e.front}'s router`;
  kv(box, "Chosen by", e.via
    ? `${e.via} — ModelFabric carried the request; ${e.via} picked where it went`
    : `${chooser} — ${e.affine ? "prefix affinity" : "least loaded"}`, false);
  kv(box, "Latency", e.ms != null ? `${e.ms} ms` : "—");
  kv(box, "Response size", e.bytes_out != null ? formatBytes(e.bytes_out) : "—");
  if (e.error) kv(box, "Error", e.error, false);
  body.append(box);

  // A forwarded request appears in both nodes' logs; the trace joins those records.
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
    // Use textContent via el(): prompt text is untrusted.
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

  // Reuse peer operations already fetched by My Models; each node owns its journal.
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
