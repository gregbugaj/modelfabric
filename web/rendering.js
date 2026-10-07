import { preferCell } from "./actions.js";
import { $ } from "./core.js";
import { platformTag } from "./discover.js";
import { formatTokens, kvMeter, shareBar } from "./mesh.js";
import { capBadges, kv } from "./my-models.js";
import { buildFront, buildView, gib, platformBadge, relativeTime, waitedText } from "./ui-model.js";
import { nodesServing } from "./workload-presets.js";

/* ---------- rendering ---------- */

export function el(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  // textContent throughout: model ids and node names come off the network and
  // are never interpolated as HTML.
  if (text !== undefined) n.textContent = text;
  return n;
}

// Which address a node's engines listen on. Shown for every node, not only
// the misconfigured ones: the absence of a warning is not the same as a
// visible confirmation, and the tailnet is how this mesh connects at all.
//
// Loopback is not broken — the node still serves through its own front door,
// and ModelFabric forwards to it. What it costs is anything that dials an engine
// directly rather than going through that node, which today means llm-d's
// scheduler. That is a consequence of the binding, not the reason for it.
function engineScopeCell(n) {
  const td = el("td");
  if (!n.engineScope) {
    const none = el("span", "muted", "—");
    none.title = `${n.name} runs no engine right now; it routes for the others.`;
    td.append(none);
    return td;
  }
  const tailnet = n.engineScope === "tailnet";
  const wrap = el("span", "engine-scope" + (tailnet ? "" : " off-mesh"));
  wrap.append(el("span", "dot-scope scope-" + n.engineScope));
  wrap.append(el("span", null, tailnet ? "tailnet" : "loopback"));
  wrap.title = tailnet
    ? `${n.name}'s engines listen on its tailnet address, so any machine on the tailnet can reach `
      + `them directly.`
    : `${n.name}'s engines listen on 127.0.0.1, so only ${n.name} itself can reach them. It still `
      + `serves through its own front door, but anything that dials an engine directly — llm-d's `
      + `scheduler — cannot use it. Set engine_bind to its tailnet address.`;
  td.append(wrap);
  return td;
}

function statusCell(alive) {
  const span = el("span", `status ${alive ? "up" : "down"}`);
  span.append(el("span", "dot"), alive ? "up" : "down");
  return span;
}

export function chips(values, emptyText = "none") {
  const frag = document.createDocumentFragment();
  if (!values.length) {
    frag.append(el("span", "muted", emptyText));
    return frag;
  }
  for (const v of values) frag.append(el("span", "chip", v));
  return frag;
}

export function fillTable(bodyId, emptyId, rows, emptyMessage, buildRow) {
  const body = $(bodyId);
  const empty = $(emptyId);
  body.replaceChildren();
  if (!rows.length) {
    empty.replaceChildren(...emptyMessage());
    empty.hidden = false;
    body.closest("table").hidden = true;
    return;
  }
  empty.hidden = true;
  body.closest("table").hidden = false;
  for (const r of rows) body.append(buildRow(r));
}

export let lastView = buildView({});
let lastLocal = { supervised: false, models: [] };

export let meshView = null; // the mesh as last polled; read outside the poll too

// This node's front door: the address apps use and what it asks of them. Read
// from /api/v1/front, which is the only place that reports it.
export let frontView = buildFront(null);

// Each engine's recent in-flight samples, for the load average on Serving.
//
// The instant is what an engine is doing; the average is how work was shared,
// and on a mixed fleet only the second answers the question worth asking.
// Three engines reading 1/2 tells you nothing about whether the slowest of
// them has been carrying an even share of a two-hour run.
const LOAD_WINDOW_MS = 120000;
const loadHistory = new Map(); // engine id -> [{t, inflight}]

export function recordLoad(engines) {
  const now = Date.now();
  const live = new Set();
  for (const e of engines ?? []) {
    live.add(e.id);
    const h = loadHistory.get(e.id) ?? [];
    h.push({ t: now, inflight: e.inflight ?? 0 });
    while (h.length && now - h[0].t > LOAD_WINDOW_MS) h.shift();
    loadHistory.set(e.id, h);
  }
  // An engine that went away keeps no history: its average would otherwise
  // reappear against the next instance to take its id.
  for (const id of [...loadHistory.keys()]) if (!live.has(id)) loadHistory.delete(id);
}

// meanInflight is the average over the window, or null before there is enough
// to mean anything — two samples of a two-minute window is not an average.
function meanInflight(id) {
  const h = loadHistory.get(id);
  if (!h || h.length < 3) return null;
  return h.reduce((sum, x) => sum + x.inflight, 0) / h.length;
}

export function renderFrontDoor() {
  const view = lastView;
  if (!view) return;
  // The page is served by the front door, so its own origin stands in when the
  // node could not be asked.
  const url = frontView.url || `${location.origin}/v1`;
  $("fd-url").textContent = url;

  const auth = $("fd-auth");
  auth.replaceChildren();
  if (frontView.requireKey) {
    auth.append(el("span", "locked", "API key required"), document.createTextNode(" — apps send it as a bearer token"));
  } else {
    auth.append(document.createTextNode("No key needed on loopback"));
  }
  if (frontView.publicListen) {
    auth.append(el("span", "sep", " · "), el("span", "locked", `public on ${frontView.publicListen}`),
      document.createTextNode(" (key required)"));
  }

  // Each stop is something actually in the path right now. Two of them, since
  // ModelFabric's own router is the front door: nothing sits between them.
  const engines = view.meshEngines ?? [];
  const nodesServing = new Set(engines.map((e) => e.node)).size;
  const stops = [
    { scope: "loopback", name: "Front door", where: url.replace(/^https?:\/\//, "").replace(/\/v1$/, ""), on: true },
    {
      scope: engines.length ? "tailnet" : "off",
      name: engines.length ? plural(engines.length, "engine") : "No engines",
      where: engines.length ? `on ${plural(nodesServing, "node")}` : "load a model on any node",
      on: engines.length > 0,
    },
  ];

  const path = $("fd-path");
  path.replaceChildren();
  stops.forEach((stop, i) => {
    // The arrow belongs to the stop it points at, so a wrap never leaves one
    // dangling at the end of a line.
    const li = el("li", "fd-stop" + (stop.on ? "" : " off"));
    if (i) li.append(el("span", "fd-arrow", "→"));
    li.append(el("i", `dot-scope scope-${stop.scope}`), el("b", null, stop.name));
    if (stop.where) li.append(el("span", "fd-where", stop.where));
    path.append(li);
  });

  // One line where four tiles used to be: on an idle mesh three of them read
  // zero, which is a lot of furniture to say nothing.
  const p = $("pulse");
  p.replaceChildren();
  const { nodesOnline, nodesTotal, models, inflight } = view.stats;
  const nodesText = nodesTotal > nodesOnline ? `${nodesOnline} of ${nodesTotal} nodes up` : `${plural(nodesOnline, "node")} up`;
  p.append(el("b", nodesTotal > nodesOnline ? "warn" : "", nodesText));
  p.append(el("span", "sep", "·"), el("span", null, models ? plural(models, "model") : "no models loaded"));
  p.append(el("span", "sep", "·"),
    inflight ? el("span", "busy", `${plural(inflight, "request")} in flight`) : el("span", null, "idle"));
}

function plural(n, word) { return `${n} ${word}${n === 1 ? "" : "s"}`; }

export function render(view, local) {
  lastView = view;
  lastLocal = local;
  $("node-name").textContent = view.self || "—";

  renderFrontDoor();

  fillTable("nodes-body", "nodes-empty", view.nodes,
    () => [el("span", null, "No nodes yet.")],
    (n) => {
      const tr = el("tr");
      const name = el("td");
      const plat = platformTag(n.platform, n.osVersion);
      if (plat) name.append(plat);
      name.append(el("span", null, n.name));
      if (n.isSelf) name.append(el("span", "self-tag", "this node"));
      const ver = platformBadge(n.platform, n.osVersion).version;
      if (ver) name.append(el("span", "plat-ver", ver));
      const addr = el("td", "mono muted", n.addr);
      const status = el("td");
      status.append(statusCell(n.alive));
      tr.append(name, addr, engineScopeCell(n), status,
        el("td", "num", String(n.inflight)),
        el("td", "num", String(n.modelCount)),
        // This node used to show a dash here, on the reasoning that "last seen"
        // is about peers and a node does not probe itself. But it does refresh
        // its own state on the same clock, and that timestamp is already
        // carried — so the column read as missing data next to three rows
        // saying "1s ago". The row is already labelled "this node"; the
        // distinction does not need making twice.
        el("td", "muted", relativeTime(n.lastSeen)),
        preferCell(n));
      return tr;
    });
  const note = $("pref-note");
  note.hidden = !view.preferred;
  if (view.preferred) {
    note.textContent = view.preferredOnline
      ? `Requests for models on ${view.preferred} go there first; other nodes (or llm-d, when it is on) serve what it cannot.`
      : `${view.preferred} is preferred but offline, so every node is used until it returns.`;
  }

  fillTable("models-body", "models-empty", view.models,
    () => [
      el("span", null, "No models in the mesh. Start an engine and add it to "),
      el("code", null, "config.json"),
      el("span", null, "."),
    ],
    (m) => {
      const tr = el("tr");
      const served = el("td");
      served.append(chips(m.nodes));
      tr.append(el("td", "mono", m.id), el("td", "num", String(m.replicas)), served);
      return tr;
    });

  // Work done: what each engine was actually given. The share column is the
  // point — three engines at healthy rates can still mean one carried the run,
  // and only a total shows it.
  const totalPrefilled = (view.meshEngines ?? []).reduce((sum, e) => sum + (e.promptTokens || 0), 0);
  fillTable("work-body", "work-empty", view.meshEngines,
    () => [el("span", null, "No engines are running anywhere in the mesh.")],
    (e) => {
      const tr = el("tr");
      const node = el("td");
      node.append(el("span", null, e.node));
      if (e.isSelf) node.append(el("span", "self-tag", "this node"));
      const share = totalPrefilled ? (e.promptTokens || 0) / totalPrefilled : 0;
      const shareCell = el("td");
      shareCell.append(shareBar(share));
      const seen = (e.promptTokens || 0) + (e.cachedTokens || 0);
      const hit = seen ? (e.cachedTokens || 0) / seen : 0;
      const hitCell = el("td", "num", seen ? `${(hit * 100).toFixed(1)}%` : "—");
      hitCell.title = "Prompt tokens served from this engine's cache rather than prefilled again. Routing that keeps a conversation on one engine raises it.";
      tr.append(node,
        el("td", "num", formatTokens(e.promptTokens)),
        shareCell,
        el("td", "num", formatTokens(e.cachedTokens)),
        hitCell,
        el("td", "num", formatTokens(e.outputTokens)),
        movedCell(view.movedIn?.[e.node]));
      return tr;
    });

  // One line for the whole mesh, so "is anything waiting, and is anything
  // free" is answered without adding up a column.
  const cap = $("engines-capacity");
  if (cap) {
    const c = view.capacity;
    cap.replaceChildren();
    if (c && c.slots) {
      cap.append(
        el("span", null, `${c.running} running`),
        el("span", c.waiting ? "waiting" : null, `${c.waiting} waiting on engines`),
        ...(c.held ? [el("span", "waiting", `${c.held} held at the router`)] : []),
        el("span", null, `${c.free} of ${plural(c.slots, "slot")} free`));
      cap.title = c.waitingBesideFree
        ? "Requests are queued on one engine while another has a slot free. A request already queued inside an engine cannot be moved."
        : "";
    }
  }

  // Each held request on its own line: how long, where, and what for.
  const heldBox = $("engines-held");
  if (heldBox) {
    const held = view.heldRequests ?? [];
    heldBox.hidden = held.length === 0;
    heldBox.replaceChildren(...held.map((h) => {
      const row = el("div", "held-row");
      row.append(
        el("span", "held-wait", waitedText(h.waitedMs)),
        el("span", null, `held at ${h.at}`),
        el("span", "held-why", h.home ? `conversation on ${h.home}: ${h.why}` : h.why));
      return row;
    }));
  }

  fillTable("engines-body", "engines-empty", view.meshEngines,
    () => [
      el("span", null, "No engines are running anywhere in the mesh. Load a model on any node."),
    ],
    (e) => {
      const tr = el("tr");
      const node = el("td");
      node.append(el("span", null, e.node));
      if (e.isSelf) node.append(el("span", "self-tag", "this node"));
      const status = el("td");
      status.append(statusCell(e.healthy));
      if (!e.healthy && e.state) status.append(el("div", "err-text", e.state));
      const kv = el("td");
      kv.append(kvMeter(e.kvUsage));
      const running = el("td", "num", e.slots ? `${e.running} / ${e.slots}` : String(e.running));
      running.title = e.slots ? `${e.running} of ${plural(e.slots, "slot")} busy` : "This engine does not report how many slots it has.";
      const waiting = el("td", "num" + (e.waiting ? " waiting" : ""), e.waiting === null ? "—" : String(e.waiting));
      waiting.title = e.waiting === null
        ? "Unknown: this engine does not report how many slots it has."
        : e.waiting ? `${plural(e.waiting, "request")} queued for a slot on this engine.` : "";
      const free = el("td", "num", e.free === null ? "—" : String(e.free));
      // The average beside the instant: on a mixed fleet an even share of
      // requests is not an even share of work, and a single sample cannot
      // show which engine has been carrying a run.
      // The node's own average first: it covers the whole run, where the
      // dashboard's only covers since the page was opened. A peer too old to
      // publish one falls back to what this page has seen.
      const served = typeof e.loadAvg === "number" && e.loadAvg >= 0 ? e.loadAvg : null;
      const avg = served ?? meanInflight(e.id);
      const load = el("td", "num", avg === null ? "—" : avg.toFixed(2));
      load.title = avg === null
        ? "Mean requests in flight over the last two minutes, once there are enough samples."
        : `Mean requests in flight over the last two minutes${served === null ? ", since this page was opened" : ""}. Full is ${e.slots}.`;
      // "~" while the rate is still rough: it is shown early so a busy engine
      // does not read as a dash, but it is not what llm-d is scheduling by.
      const roughMark = e.prefillTokS && !e.prefillTrusted ? "~" : "";
      const prefill = el("td", "num" + (roughMark ? " rough" : ""),
        e.prefillTokS ? roughMark + Math.round(e.prefillTokS).toLocaleString() : "—");
      prefill.title = !e.prefillTokS
        ? "Not yet measured: this engine has not served enough prompt tokens."
        : roughMark
          ? "Measured prompt tokens per second, but still rough: under 20,000 prompt tokens a few short requests dominate the average. Shown so a busy engine is not a dash; routing and llm-d wait for it to settle."
          : "Measured prompt tokens per second: reading the conversation. Settled enough that routing and llm-d schedule by it.";
      // Decode is the other half of a turn, and the half speculative decoding
      // moves — prefill alone showed nothing when it was switched on.
      const decode = el("td", "num", e.decodeTokS ? Math.round(e.decodeTokS).toLocaleString() : "—");
      decode.title = e.decodeTokS
        ? "Measured generated tokens per second: writing the answer. This is what speculative decoding changes."
        : "Not yet measured: this engine has not generated enough tokens.";
      const drafts = el("td", "num", e.specAccepted >= 0 ? `${Math.round(e.specAccepted * 100)}%` : "—");
      drafts.title = e.specAccepted >= 0
        ? "Share of speculatively drafted tokens the model kept. Rejected drafts cost time, so this is whether speculation is paying."
        : "This engine is not speculating, or has not drafted yet.";
      const model = el("td", "mono");
      model.append(el("span", null, e.model || "—"));
      // Vision is a property of the launched process, not of the model file:
      // the same model runs with images on one node and not another, and the
      // slot count beside it is what an image request actually gets.
      if (e.vision) {
        const v = capBadges(["vision"]);
        v.title = e.slots === 1
          ? "Serving images with one slot — what llama.cpp needs to process an image reliably."
          : `Serving images with ${e.slots} slots. One is the default: llama.cpp can fail an image request with "failed to process mtmd chunk" when slots are shared.`;
        model.append(v);
      }
      // The host-RAM cache and what it has cost: a count that keeps rising
      // means conversations are being read again for want of room.
      const ram = el("td", "num");
      if (e.cacheRamMib === null) {
        ram.textContent = "—";
        ram.title = "This engine did not report a host-RAM prompt cache.";
      } else {
        ram.append(el("span", null, gib(e.cacheRamMib)));
        if (e.cacheDropped) ram.append(el("span", "dropped", ` · ${e.cacheDropped} dropped`));
        ram.title = e.cacheDropped
          ? `${e.cacheDropped} conversation${e.cacheDropped === 1 ? "" : "s"} dropped from a ${gib(e.cacheRamMib)} cache since this engine started. Each is read again in full when it returns.`
          : `Nothing dropped from a ${gib(e.cacheRamMib)} cache since this engine started.`;
      }
      const mem = el("td", "num", gib(e.memoryMb));
      mem.title = e.hostMemTotalMb === null
        ? "This node did not report its memory."
        : `${gib(e.hostMemAvailableMb)} free of ${gib(e.hostMemTotalMb)} on ${e.node}.`;
      tr.append(node, model, status, running, waiting, free, load, prefill, decode, drafts, kv, ram, mem,
        el("td", "mono small muted", e.address || "—"), el("td", "small muted", e.runtime || "—"));
      return tr;
    });
}


// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and meshView is written by the poll
// loop and read here.
export function setMeshView(v) { meshView = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and frontView is written by the poll
// loop and read here.
export function setFrontView(v) { frontView = v; }

// movedCell is "moved in / sent here" for one node, or a dash when no router
// in the mesh has reported sending it anything: not known is not zero.
function movedCell(t) {
  if (!t || !t.calls) {
    const td = el("td", "num", "—");
    td.title = "No node has reported routing requests here yet.";
    return td;
  }
  const td = el("td", "num", `${t.moved} / ${t.calls}`);
  td.title = `${t.moved} of ${t.calls} requests sent here arrived from another engine (${((100 * t.moved) / t.calls).toFixed(1)}%).`;
  return td;
}
