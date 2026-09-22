import { $ } from "./core.js";
import { PLATFORM_ICONS, ensurePlatformDefs, icon, platformTag } from "./discover.js";
import { fetchJSON, kv, nodeAPI, selfNode } from "./my-models.js";
import { el } from "./rendering.js";
import { meta } from "./routing.js";
import { buildEdges, constellation, constellationAll, formatBytes, kvBadge, platformBadge } from "./ui-model.js";

/* ---------- Mesh: the architecture, live ---------- */

const topo = { nodes: new Map(), from: "", view: "arch", model: "" }; // node -> topology or { error }

export async function refreshTopology(peers) {
  const nodes = [selfNode, ...peers];
  await Promise.all(nodes.map(async (node) => {
    // `node` is forced on rather than trusted from the body. renderMesh sorts
    // on it, so a peer that answers 200 without one threw inside tick() — and
    // because the whole poll shares that try, one odd peer response took the
    // entire dashboard to "disconnected" instead of spoiling its own card.
    try {
      const t = await fetchJSON(nodeAPI(node, "/api/v1/topology"));
      topo.nodes.set(node, { ...t, node: t?.node || node });
    } catch (err) { topo.nodes.set(node, { node, error: err.message }); }
  }));
  for (const n of [...topo.nodes.keys()]) if (!nodes.includes(n)) topo.nodes.delete(n);
  renderMesh();
}

const LISTENER_LABEL = { front: "Front door", mesh: "Tailnet listener", public: "Public listener", llmd: "llm-d · Envoy" };
function scopeClass(scope = "") {
  if (scope.startsWith("public")) return "scope-public";
  if (scope.startsWith("tailnet")) return "scope-tailnet";
  if (scope.includes("internal")) return "scope-internal";
  return "scope-loopback";
}
const boxId = (node, kind, id) => `mesh|${node}|${kind}|${id}`;

function renderMesh() {
  const topos = [...topo.nodes.values()].filter((t) => !t.error);
  const star = topo.view === "star";
  $("mesh-star").hidden = !star;
  $("mesh-canvas").hidden = star;
  $("mesh-from-wrap").hidden = star;
  $("mesh-model-wrap").hidden = !star;
  if (star) { renderConstellation(topos); return; }
  // Any node that answered can be the entry point: ModelFabric's own router *is* the
  // front door, so a request enters wherever it is sent.
  if (!topo.from || !topos.find((t) => t.node === topo.from)) topo.from = (topos.find((t) => t.node === selfNode) ?? topos[0])?.node ?? "";

  const seg = $("mesh-from");
  seg.replaceChildren();
  for (const t of topos) {
    const b = el("button", "seg-btn" + (t.node === topo.from ? " active" : ""));
    b.type = "button";
    // What kind of node this is, at a glance: requests entering at an
    // entrypoint come from outside; at a GPU node, from apps on that machine.
    const pub = t.listeners?.some((l) => l.name === "public");
    if (t.role === "entrypoint") {
      b.append(el("i", "dot-scope scope-public"), document.createTextNode(t.node), el("span", "seg-role role-entrypoint", pub ? "entrypoint · public" : "entrypoint"));
      b.title = pub ? "Entrypoint: runs no models; the internet reaches the mesh here through its public listener" : "Entrypoint: runs no models; routes into the mesh";
    } else {
      const chip = icon("chip", 12);
      chip.classList.add("seg-ico");
      b.append(chip, document.createTextNode(t.node));
      if (pub) b.append(el("span", "seg-role role-entrypoint", "public"));
      b.title = t.role === "gpu" ? "GPU node: serves models; apps on it enter here" : t.node;
    }
    b.addEventListener("click", () => { topo.from = t.node; renderMesh(); });
    seg.append(b);
  }
  if (!topos.length) seg.append(el("span", "muted small", "no node answered"));

  // Where requests come from, for the chosen node.
  const entries = $("mesh-entries");
  entries.replaceChildren();
  const src = topos.find((t) => t.node === topo.from);
  const entry = (id, title, sub, ico) => {
    const e = el("div", "mesh-entry");
    e.dataset.box = boxId("_", "entry", id);
    e.append(icon(ico, 16));
    const txt = el("div");
    txt.append(el("div", "mesh-entry-title", title), el("div", "muted small", sub));
    e.append(txt);
    entries.append(e);
  };
  if (src) {
    entry("apps", `Apps on ${src.node}`, "OpenAI-compatible clients, Claude Code, editors", "server");
    if (src.listeners.some((l) => l.name === "public")) entry("internet", "Internet", "through your TLS proxy (nginx, Funnel)", "disk");
  }

  const box = $("mesh-nodes");
  box.replaceChildren();
  const order = [...topo.nodes.values()].sort((a, b) => (a.node === topo.from ? -1 : b.node === topo.from ? 1 : a.node.localeCompare(b.node)));
  for (const t of order) box.append(meshCard(t));

  requestAnimationFrame(drawMeshEdges);
}

// kvMeter draws an engine's KV-cache utilization: a bar for how full the pool
// is, and nothing at all when the engine could not be asked — an unmeasured
// engine must not read as an empty one.
// formatTokens keeps a million-token counter readable: the interesting part
// is the magnitude and the ratio between engines, not the last three digits.
export function formatTokens(n) {
  if (!n) return "—";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return Math.round(n / 1e3) + "k";
  return String(n);
}

// shareBar is one engine's share of the fleet's prefill, drawn rather than
// written: the imbalance is the finding, and a row of percentages hides it.
export function shareBar(fraction) {
  const wrap = el("span", "share");
  // A track behind the fill, so a small share reads as "little of the whole"
  // rather than as a stray dash with nothing to measure it against.
  const track = el("span", "share-track");
  const fill = el("span", "share-fill");
  fill.style.width = `${Math.round(Math.min(1, Math.max(0, fraction)) * 100)}%`;
  track.append(fill);
  wrap.append(track, el("span", "share-pct", `${Math.round(fraction * 100)}%`));
  wrap.title = "This engine's share of every prompt token the fleet prefilled.";
  return wrap;
}

export function kvMeter(usage) {
  const b = kvBadge(usage);
  const wrap = el("span", "kvm" + (b.known ? " kv-" + b.level : " kv-unknown"));
  if (!b.known) {
    wrap.title = "KV-cache usage unknown: this engine publishes none and ModelFabric could not read its slots";
    wrap.append(el("span", "kv-none", "—"));
    return wrap;
  }
  wrap.title = `${b.label} — tokens resident in the engine's slots over their capacity, including caches kept for reuse; computed from /slots`;
  const bar = el("span", "kv-bar");
  const fill = el("span", "kv-fill");
  fill.style.width = b.pct + "%";
  bar.append(fill);
  wrap.append(bar, el("span", "kv-pct", b.pct + "%"));
  return wrap;
}

function meshCard(t) {
  const card = el("div", "mesh-node" + (t.error ? " unreadable" : "") + (t.node === topo.from ? " from" : ""));
  const head = el("div", "mesh-node-head");
  const title = el("div", "mesh-node-title");
  // The platform mark stands in for the generic server icon where the node
  // reports one: what a node can run follows from it.
  const plat = platformTag(t.platform, t.os_version, 20);
  title.append(plat ?? icon("server", 16), el("span", null, t.node));
  if (t.node === selfNode) title.append(el("span", "self-tag", "this node"));
  if (t.role) title.append(el("span", `role role-${t.role}`, t.role === "gpu" ? "GPU node" : t.role));
  if (t.preferred && t.preferred === t.node) { const p = icon("star", 13); p.classList.add("pick"); p.title = "preferred node"; title.append(p); }
  head.append(title);
  if (t.error) { card.append(head, el("div", "muted small mesh-note", `Cannot read its details: ${t.error}`)); return card; }
  const sub = el("div", "mesh-node-sub mono");
  sub.append(t.tailnet_ip || "no tailnet address");
  const osv = platformBadge(t.platform, t.os_version).version;
  if (osv) sub.append(el("span", "muted", ` · ${osv}`));
  for (const g of t.gpus ?? []) sub.append(el("span", "muted", ` · ${g.name.replace(/^(NVIDIA|AMD|Intel)\s+(GeForce\s+)?/i, "")} ${formatBytes(g.vram_mb * 1048576)}`));
  head.append(sub);
  card.append(head);

  const ls = el("div", "mesh-block");
  ls.append(el("div", "mesh-block-title", "Listens on"));
  for (const l of t.listeners ?? []) {
    const row = el("div", "mesh-row" + (l.enabled ? "" : " off"));
    row.dataset.box = boxId(t.node, "listener", l.name);
    row.title = `${l.serves}\nwho: ${l.scope}\nauth: ${l.auth}`;
    const name = el("span", "mesh-row-name");
    name.append(el("i", `dot-scope ${scopeClass(l.scope)}`), LISTENER_LABEL[l.name] || l.name);
    row.append(name, el("span", "mono mesh-addr", l.addr || "—"), el("span", "mesh-auth", l.auth));
    ls.append(row);
  }
  card.append(ls);

  const es = el("div", "mesh-block");
  es.append(el("div", "mesh-block-title", t.engines?.length ? "Engines" : "Engines · none"));
  for (const e of t.engines ?? []) {
    const row = el("div", "mesh-row engine" + (e.healthy ? "" : " off"));
    row.dataset.box = boxId(t.node, "engine", e.id);
    row.title = `${e.id}\n${e.runtime || ""}`;
    const loop = e.addr === "127.0.0.1" || e.addr === "localhost";
    const name = el("span", "mesh-row-name");
    name.append(el("i", `dot-scope ${loop ? "scope-loopback" : "scope-tailnet"}`), el("span", "mono", e.model));
    const slots = el("span", "slots");
    const n = Math.max(e.slots || 0, e.inflight || 0);
    for (let i = 0; i < n; i++) slots.append(el("i", i < (e.inflight || 0) ? "busy" : ""));
    slots.title = `${e.inflight}/${e.slots || "?"} slots busy`;
    const meta = el("span", "mesh-auth");
    meta.textContent = [e.context ? `${Math.round(e.context / 1024)}K ctx` : "", e.prefill_tok_s ? `${Math.round(e.prefill_tok_s)} tok/s prefill` : ""].filter(Boolean).join(" · ");
    row.append(name, el("span", "mono mesh-addr", `${e.addr}:${e.port}`), slots, kvMeter(e.kv_usage), meta);
    es.append(row);
  }
  card.append(es);
  return card;
}

function drawMeshEdges() {
  const svg = $("mesh-svg");
  const canvas = $("mesh-canvas");
  if (!svg || canvas.closest("section").hidden) return;
  const cr = canvas.getBoundingClientRect();
  svg.setAttribute("width", cr.width);
  svg.setAttribute("height", cr.height);
  svg.setAttribute("viewBox", `0 0 ${cr.width} ${cr.height}`);
  const find = (b) => canvas.querySelector(`[data-box="${CSS.escape(boxId(b.node, b.kind, b.id))}"]`);
  const topos = [...topo.nodes.values()].filter((t) => !t.error);
  const edges = buildEdges(topos, topo.from);
  const src = topos.find((t) => t.node === topo.from);
  if (src) {
    edges.unshift({ from: { node: "_", kind: "entry", id: "apps" }, to: { node: src.node, kind: "listener", id: "front" }, style: "entry" });
    if (src.listeners.some((l) => l.name === "public")) {
      edges.unshift({ from: { node: "_", kind: "entry", id: "internet" }, to: { node: src.node, kind: "listener", id: "public" }, style: "entry" });
      // The public listener is the same router as the front door, reached from
      // outside: a request joins the paths below at that box, so without this
      // hop the public listener sat unconnected to anything it feeds.
      edges.push({ from: { node: src.node, kind: "listener", id: "public" }, to: { node: src.node, kind: "listener", id: "front" }, style: "entry" });
    }
  }
  let paths = `<defs>${["direct", "forwarded", "llmd", "preferred", "entry", "peer"].map((s) =>
    `<marker id="arr-${s}" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0 8 4 0 8Z" class="arrow-${s}"/></marker>`).join("")}</defs>`;
  const seen = new Set();
  for (const e of edges) {
    const a = find(e.from), b = find(e.to);
    if (!a || !b) continue;
    const key = `${e.from.node}${e.from.id}${e.to.node}${e.to.id}${e.style}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
    const ax = ra.left - cr.left, ay = ra.top - cr.top, bx = rb.left - cr.left, by = rb.top - cr.top;
    let d;
    // A line that would cross another node's card dips below the cards and
    // runs through the tailnet band instead: drawn straight, the line from
    // sites-01 to xpredator ran across minion's Front door and read as a
    // connection to minion.
    const cards = [...canvas.querySelectorAll(".mesh-node")].map((n) => n.getBoundingClientRect());
    const lo = Math.min(ra.left, rb.left), hi = Math.max(ra.right, rb.right);
    const between = cards.some((c) => c.left > lo + 1 && c.right < hi - 1 && !(c.left <= ra.left && c.right >= ra.right) && !(c.left <= rb.left && c.right >= rb.right));
    if (between && Math.abs(ax - bx) >= 8) {
      const right = bx > ax;
      const x1 = right ? ax + ra.width : ax, y1 = ay + ra.height / 2;
      const x2 = right ? bx : bx + rb.width, y2 = by + rb.height / 2;
      const yb = Math.max(...cards.map((c) => c.bottom)) - cr.top + 10 + (seen.size % 3) * 5;
      const k = right ? 1 : -1;
      d = `M${x1},${y1} C${x1 + 28 * k},${y1} ${x1 + 28 * k},${yb} ${x1 + 56 * k},${yb} L${x2 - 56 * k},${yb} C${x2 - 28 * k},${yb} ${x2 - 28 * k},${y2} ${x2 - 2 * k},${y2}`;
    } else if (Math.abs(ax - bx) < 8) { // same card: a loop on its right edge
      const x = ax + ra.width, y1 = ay + ra.height / 2, y2 = by + rb.height / 2, bulge = 26 + Math.min(60, Math.abs(y2 - y1) / 5);
      d = `M${x},${y1} C${x + bulge},${y1} ${x + bulge},${y2} ${x + 2},${y2}`;
    } else {
      const right = bx > ax;
      const x1 = right ? ax + ra.width : ax, y1 = ay + ra.height / 2;
      const x2 = right ? bx : bx + rb.width, y2 = by + rb.height / 2;
      const c = Math.max(40, Math.abs(x2 - x1) / 3);
      d = `M${x1},${y1} C${x1 + (right ? c : -c)},${y1} ${x2 + (right ? -c : c)},${y2} ${x2 + (right ? -2 : 2)},${y2}`;
    }
    const esc = (x) => String(x).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
    const label = e.model ? `<title>${esc(e.model)} · ${esc(e.style)}</title>` : "";
    paths += `<path d="${d}" class="edge edge-${e.style}" marker-end="url(#arr-${e.style})">${label}</path>`;
  }
  svg.innerHTML = paths;
}
window.addEventListener("resize", () => requestAnimationFrame(drawMeshEdges));


/* The Constellation: one model at the centre, drawn for the eye. */

const STAR_STYLE = { direct: "direct", forwarded: "forwarded", llmd: "llmd", preferred: "preferred" };

function renderConstellation(topos) {
  const all = constellationAll(topos);
  if (!topo.model) topo.model = all.models.length > 1 ? "*" : "";
  const seg = $("mesh-model");
  seg.replaceChildren();
  if (all.models.length > 1) {
    const b = el("button", "seg-btn" + (topo.model === "*" ? " active" : ""), "All models");
    b.type = "button";
    b.addEventListener("click", () => { topo.model = "*"; renderMesh(); });
    seg.append(b);
  }
  if (topo.model === "*" && all.models.length > 1) {
    for (const m of all.models) seg.append(modelButton(m.model, false));
    renderConstellationAll(all);
    return;
  }
  const c = constellation(topos, topo.model === "*" ? "" : topo.model);
  topo.model = c.model;
  for (const m of c.models) seg.append(modelButton(m, m === c.model));
  drawConstellation(c);
}

function modelButton(m, active) {
  const b = el("button", "seg-btn" + (active ? " active" : ""), m.split("/").pop());
  b.type = "button";
  b.title = m;
  b.addEventListener("click", () => { topo.model = m; renderMesh(); });
  return b;
}

// The platform mark inside a node's circle. The node is filled with --surface,
// which is exactly what the glyphs cut their holes in, so it needs no variant.
function starPlatform(x, y, platform, size = 16) {
  const glyph = PLATFORM_ICONS[platformBadge(platform).key];
  if (!glyph) return "";
  ensurePlatformDefs();
  const s = size / 24;
  return `<g transform="translate(${x - size / 2},${y - size / 2}) scale(${s})" class="plat-star">${glyph}</g>`;
}

// Colours for models in the all-models view, apart from the route colours.
const MODEL_HUES = [252, 186, 330, 45, 205, 300, 160, 15];
const starEsc = (x) => String(x).replace(/[&<>"]/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[ch]);

// Places items on a circle at the angles they would prefer (next to what
// they connect to), nudging neighbours apart until each has room.
function orbit(items, prefer, r, cx, cy) {
  const pos = new Map();
  if (!items.length) return pos;
  const gap = Math.min((2 * Math.PI) / items.length, 0.9);
  const placed = items.map((it) => ({ it, a: prefer(it) })).sort((x, y) => x.a - y.a);
  for (let pass = 0; pass < 40; pass++) {
    let moved = false;
    for (let i = 0; i < placed.length; i++) {
      const cur = placed[i], next = placed[(i + 1) % placed.length];
      let d = next.a - cur.a;
      if (i === placed.length - 1) d += 2 * Math.PI;
      if (placed.length > 1 && d < gap) {
        const push = (gap - d) / 2 + 0.001;
        cur.a -= push;
        next.a += push;
        moved = true;
      }
    }
    if (!moved) break;
  }
  for (const p of placed) pos.set(p.it, [cx + r * Math.cos(p.a), cy + r * Math.sin(p.a)]);
  return pos;
}

function renderConstellationAll(c) {
  const svg = $("star-svg");
  const W = Math.max(640, $("mesh-star").clientWidth), H = 700;
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("width", W);
  svg.setAttribute("height", H);
  const cx = W / 2, cy = H / 2, esc = starEsc;
  const R0 = c.models.length > 1 ? 44 + 18 * c.models.length : 0;
  const R1 = Math.min(W, H) * 0.37, R2 = Math.min(W * 0.47, H * 0.46);
  const hue = new Map(c.models.map((m, i) => [m.model, MODEL_HUES[i % MODEL_HUES.length]]));

  // Models on a small central ring; nodes and entry points ordered by where
  // what they connect to sits.
  const mpos = new Map();
  const first = c.models.length === 2 ? Math.PI : -Math.PI / 2; // two models side by side
  c.models.forEach((m, i) => {
    const a = first + (2 * Math.PI * i) / c.models.length;
    mpos.set(m.model, [cx + R0 * Math.cos(a), cy + R0 * Math.sin(a)]);
  });
  const angleOf = ([x, y]) => Math.atan2(y - cy, x - cx);
  const meanAngle = (points) => {
    if (!points.length) return Math.PI / 2;
    const sx = points.reduce((k, p) => k + Math.cos(angleOf(p)), 0), sy = points.reduce((k, p) => k + Math.sin(angleOf(p)), 0);
    // Connected to models on opposite sides: sit between them, above.
    if (Math.hypot(sx, sy) < 0.2 * points.length) return -Math.PI / 2;
    return Math.atan2(sy, sx);
  };
  const npos = orbit(c.nodes.map((n) => n.node),
    (n) => meanAngle(c.nodes.find((x) => x.node === n).serves.map((s) => mpos.get(s.model))), R1, cx, cy);
  const outer = [...c.entries.filter((e) => !npos.has(e.node)).map((e) => e.node), ...c.others];
  const opos = orbit(outer, (n) => {
    const e = c.entries.find((x) => x.node === n);
    return e ? meanAngle(e.links.map((l) => npos.get(l.to)).filter(Boolean)) : Math.PI / 2;
  }, R2, cx, cy);
  const pos = (n) => npos.get(n) ?? opos.get(n);

  let out = `<defs>
    <radialGradient id="star-bg" cx="50%" cy="50%" r="60%"><stop offset="0%" class="bg-in"/><stop offset="100%" class="bg-out"/></radialGradient>
    <filter id="star-glow" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="6"/></filter>
    ${c.models.map((m) => { const h = hue.get(m.model); return `<radialGradient id="core-${h}" cx="40%" cy="35%" r="75%"><stop offset="0%" stop-color="hsl(${h} 75% 62%)"/><stop offset="100%" stop-color="hsl(${h} 60% 34%)"/></radialGradient>`; }).join("")}
  </defs>
  <rect width="${W}" height="${H}" fill="url(#star-bg)" rx="14"/>
  <circle cx="${cx}" cy="${cy}" r="${R2 + 34}" class="orbit"/>
  <text x="${cx}" y="${cy - R2 - 42}" class="orbit-label" text-anchor="middle">tailnet</text>
  <circle cx="${cx}" cy="${cy}" r="${R1}" class="orbit inner"/>`;

  // Entry points to the nodes they route to.
  for (const e of c.entries) {
    const [x1, y1] = pos(e.node);
    for (const l of e.links) {
      const p2 = pos(l.to);
      if (!p2) continue;
      const [x2, y2] = p2;
      const mx = (x1 + x2) / 2 + (cx - (x1 + x2) / 2) * 0.3, my = (y1 + y2) / 2 + (cy - (y1 + y2) / 2) * 0.3;
      const st = STAR_STYLE[l.style] || "direct";
      const d = `M${x1},${y1} Q${mx},${my} ${x2},${y2}`;
      out += `<path d="${d}" class="link link-${st}"/><path d="${d}" class="flow flow-${st}"><title>${esc(e.node)} → ${esc(l.to)} · ${esc(l.style)}: ${esc(l.models.join(", "))}</title></path>`;
    }
  }
  // Nodes to the models they serve, each in its model's colour.
  for (const n of c.nodes) {
    const [x, y] = pos(n.node);
    for (const s of n.serves) {
      const [mx, my] = mpos.get(s.model);
      const h = hue.get(s.model), hot = s.inflight > 0;
      out += `<line x1="${x}" y1="${y}" x2="${mx}" y2="${my}" style="stroke:hsl(${h} 65% 55% / ${hot ? 0.6 : 0.3})" class="spoke${hot ? " hot" : ""}"/>`;
      out += `<line x1="${x}" y1="${y}" x2="${mx}" y2="${my}" style="stroke:hsl(${h} 70% 52%)" class="flow${hot ? " hot" : ""}"><title>${esc(n.node)} serves ${esc(s.model)} · ${s.inflight}/${s.slots} busy</title></line>`;
    }
  }
  // The models.
  const coreR = c.models.length > 3 ? 38 : 46;
  for (const m of c.models) {
    const [x, y] = mpos.get(m.model);
    const h = hue.get(m.model);
    const [pub, name] = m.model.includes("/") ? m.model.split("/") : ["", m.model];
    out += `<circle cx="${x}" cy="${y}" r="${coreR + 14}" style="fill:hsl(${h} 70% 55% / 0.14);stroke:hsl(${h} 70% 55% / 0.3)" class="halo"/>
      <circle cx="${x}" cy="${y}" r="${coreR}" fill="url(#core-${h})" style="stroke:hsl(${h} 70% 60% / 0.7)" class="core"/>
      <text x="${x}" y="${y - 9}" text-anchor="middle" class="core-pub">${esc(pub)}</text>
      <text x="${x}" y="${y + 6}" text-anchor="middle" class="core-name small-core">${esc(name)}</text>
      <text x="${x}" y="${y + 21}" text-anchor="middle" class="core-sub">${m.inflight}/${m.slots} slots</text>`;
  }
  // Nodes, sized by GPU memory and ringed by their slots.
  for (const n of c.nodes) {
    const [x, y] = pos(n.node);
    const vram = (n.gpu?.vram_mb ?? 16000) / 1024;
    const r = Math.max(26, Math.min(44, 16 + Math.sqrt(vram) * 4));
    out += `<circle cx="${x}" cy="${y}" r="${r + 10}" class="node-glow${n.inflight ? " hot" : ""}" filter="url(#star-glow)"/>`;
    out += `<circle cx="${x}" cy="${y}" r="${r}" class="node gpu"/>`;
    const slots = Math.max(n.slots, 1), gap = 0.12;
    for (let i = 0; i < slots; i++) {
      const a0 = -Math.PI / 2 + (2 * Math.PI * i) / slots + gap / 2, a1 = -Math.PI / 2 + (2 * Math.PI * (i + 1)) / slots - gap / 2, rr = r + 6;
      out += `<path d="M${x + rr * Math.cos(a0)},${y + rr * Math.sin(a0)} A${rr},${rr} 0 ${a1 - a0 > Math.PI ? 1 : 0} 1 ${x + rr * Math.cos(a1)},${y + rr * Math.sin(a1)}" class="slot${i < n.inflight ? " busy" : ""}"/>`;
    }
    out += starPlatform(x, y - 12, n.platform);
    out += `<text x="${x}" y="${y + 6}" text-anchor="middle" class="node-name">${esc(n.node)}</text>`;
    const d = Math.hypot(x - cx, y - cy) || 1, ux = (x - cx) / d, uy = (y - cy) / d;
    const lx = x + ux * (r + 22), ly = y + uy * (r + 22) + (uy < -0.35 ? -14 : 0);
    const anchor = Math.abs(ux) < 0.35 ? "middle" : ux > 0 ? "start" : "end";
    const gpu = n.gpu ? `${n.gpu.name.replace(/^(NVIDIA|AMD|Intel)\s+(GeForce\s+)?/i, "")} · ${Math.round(n.gpu.vram_mb / 1024)}GB` : "";
    out += `<text x="${lx}" y="${ly}" text-anchor="${anchor}" class="node-sub">${esc(gpu)}</text>`;
    out += `<text x="${lx}" y="${ly + 14}" text-anchor="${anchor}" class="node-sub">${n.inflight}/${n.slots} slots busy · ${n.serves.length} model${n.serves.length === 1 ? "" : "s"}</text>`;
  }
  // Entry points, the internet reaching the public one, and the rest.
  for (const e of c.entries) {
    if (npos.has(e.node)) continue;
    const [x, y] = pos(e.node);
    out += `<rect x="${x - 58}" y="${y - 20}" width="116" height="40" rx="20" class="${e.role === "entrypoint" ? "entry" : "node gpu dim"}"/>
      <text x="${x}" y="${y - 1}" text-anchor="middle" class="entry-name">${esc(e.node)}</text>
      <text x="${x}" y="${y + 13}" text-anchor="middle" class="entry-sub">${e.role === "entrypoint" ? (e.public ? "entrypoint · public" : "entrypoint") : "router"}</text>`;
    if (e.public) {
      const d = Math.hypot(x - cx, y - cy) || 1, ux = (x - cx) / d, uy = (y - cy) / d;
      const ox = cx + ux * (R2 + 112), oy = cy + uy * (R2 + 112), ex = x + ux * 58, ey = y + uy * 20;
      out += `<line x1="${ox - ux * 15}" y1="${oy - uy * 15}" x2="${ex}" y2="${ey}" class="link link-public"/>
        <line x1="${ox - ux * 15}" y1="${oy - uy * 15}" x2="${ex}" y2="${ey}" class="flow flow-public"/>
        <circle cx="${ox}" cy="${oy}" r="15" class="globe"/>
        <path d="M${ox - 15},${oy} h30 M${ox},${oy - 15} q-9,15 0,30 q9,-15 0,-30" class="globe-lines"/>
        <text x="${ox}" y="${oy + 30}" text-anchor="middle" class="entry-sub">internet</text>`;
    }
  }
  for (const n of c.others) {
    const [x, y] = pos(n);
    out += `<circle cx="${x}" cy="${y}" r="16" class="node dim"/><text x="${x}" y="${y + 30}" text-anchor="middle" class="node-sub">${esc(n)}</text>`;
  }
  svg.innerHTML = out;
}

function drawConstellation(c) {

  const svg = $("star-svg");
  const W = Math.max(640, $("mesh-star").clientWidth), H = 660;
  const cx = W / 2, cy = H / 2;
  const R1 = Math.min(W, H) * 0.3, R2 = Math.min(W * 0.46, H * 0.44);
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("width", W);
  svg.setAttribute("height", H);
  const esc = (x) => String(x).replace(/[&<>"]/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[ch]);
  const at = (i, n, r, offset = 0) => {
    const a = -Math.PI / 2 + offset + (2 * Math.PI * i) / Math.max(n, 1);
    return [cx + r * Math.cos(a), cy + r * Math.sin(a)];
  };
  let out = `<defs>
    <radialGradient id="star-bg" cx="50%" cy="50%" r="60%"><stop offset="0%" class="bg-in"/><stop offset="100%" class="bg-out"/></radialGradient>
    <radialGradient id="star-core" cx="40%" cy="35%" r="75%"><stop offset="0%" class="core-in"/><stop offset="100%" class="core-out"/></radialGradient>
    <filter id="star-glow" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="6"/></filter>
  </defs>
  <rect width="${W}" height="${H}" fill="url(#star-bg)" rx="14"/>
  <circle cx="${cx}" cy="${cy}" r="${R2 + 34}" class="orbit"/>
  <text x="${cx}" y="${cy - R2 - 42}" class="orbit-label" text-anchor="middle">tailnet</text>
  <circle cx="${cx}" cy="${cy}" r="${R1}" class="orbit inner"/>`;

  if (!c.model) {
    svg.innerHTML = out + `<text x="${cx}" y="${cy}" text-anchor="middle" class="star-empty">No model is loaded anywhere in the mesh.</text>`;
    return;
  }

  // Positions: serving nodes on the inner orbit, entry points and the rest outside.
  const pos = new Map();
  c.serving.forEach((s, i) => pos.set(s.node, at(i, c.serving.length, R1, Math.PI / 5)));
  const outer = [...c.entries.filter((e) => !pos.has(e.node)).map((e) => e.node), ...c.others];
  outer.forEach((n, i) => pos.set(n, at(i, outer.length, R2, Math.PI / Math.max(outer.length, 1))));

  // Links: entry points to the nodes they route to, then serving nodes to the model.
  for (const e of c.entries) {
    const [x1, y1] = pos.get(e.node);
    for (const l of e.links) {
      if (l.to === e.node) continue;
      const [x2, y2] = pos.get(l.to);
      const mx = (x1 + x2) / 2 + (cx - (x1 + x2) / 2) * 0.35, my = (y1 + y2) / 2 + (cy - (y1 + y2) / 2) * 0.35;
      const st = STAR_STYLE[l.style] || "direct";
      const d = `M${x1},${y1} Q${mx},${my} ${x2},${y2}`;
      out += `<path d="${d}" class="link link-${st}"/><path d="${d}" class="flow flow-${st}"><title>${esc(e.node)} → ${esc(l.to)} · ${esc(l.style)}</title></path>`;
    }
  }
  for (const s of c.serving) {
    const [x, y] = pos.get(s.node);
    const hot = s.inflight > 0;
    out += `<line x1="${x}" y1="${y}" x2="${cx}" y2="${cy}" class="spoke${hot ? " hot" : ""}"/>`;
    out += `<line x1="${x}" y1="${y}" x2="${cx}" y2="${cy}" class="flow flow-spoke${hot ? " hot" : ""}"/>`;
  }

  // The model.
  const [pub, name] = c.model.includes("/") ? c.model.split("/") : ["", c.model];
  const slots = c.serving.reduce((n, s) => n + s.slots, 0), busy = c.serving.reduce((n, s) => n + s.inflight, 0);
  out += `<circle cx="${cx}" cy="${cy}" r="80" class="halo"/>
    <circle cx="${cx}" cy="${cy}" r="62" fill="url(#star-core)" class="core"/>
    <text x="${cx}" y="${cy - 14}" text-anchor="middle" class="core-pub">${esc(pub)}</text>
    <text x="${cx}" y="${cy + 6}" text-anchor="middle" class="core-name">${esc(name)}</text>
    <text x="${cx}" y="${cy + 26}" text-anchor="middle" class="core-sub">${c.serving.length} node${c.serving.length === 1 ? "" : "s"} · ${busy}/${slots} slots</text>`;

  // Serving nodes: sized by GPU memory, ringed by their slots.
  for (const s of c.serving) {
    const [x, y] = pos.get(s.node);
    const vram = (s.gpu?.vram_mb ?? 16000) / 1024;
    const r = Math.max(26, Math.min(44, 16 + Math.sqrt(vram) * 4));
    out += `<circle cx="${x}" cy="${y}" r="${r + 10}" class="node-glow${s.inflight ? " hot" : ""}" filter="url(#star-glow)"/>`;
    out += `<circle cx="${x}" cy="${y}" r="${r}" class="node gpu"/>`;
    const n = Math.max(s.slots, 1), gap = 0.12;
    for (let i = 0; i < n; i++) {
      const a0 = -Math.PI / 2 + (2 * Math.PI * i) / n + gap / 2, a1 = -Math.PI / 2 + (2 * Math.PI * (i + 1)) / n - gap / 2, rr = r + 6;
      const p0 = [x + rr * Math.cos(a0), y + rr * Math.sin(a0)], p1 = [x + rr * Math.cos(a1), y + rr * Math.sin(a1)];
      out += `<path d="M${p0[0]},${p0[1]} A${rr},${rr} 0 ${a1 - a0 > Math.PI ? 1 : 0} 1 ${p1[0]},${p1[1]}" class="slot${i < s.inflight ? " busy" : ""}"/>`;
    }
    const gpu = s.gpu ? s.gpu.name.replace(/^(NVIDIA|AMD|Intel)\s+(GeForce\s+)?/i, "") : "";
    out += starPlatform(x, y - 12, s.platform);
    out += `<text x="${x}" y="${y + 6}" text-anchor="middle" class="node-name">${esc(s.node)}</text>`;
    // Details on the side away from the model, so they never cross it.
    const ux = (x - cx) / (Math.hypot(x - cx, y - cy) || 1), uy = (y - cy) / (Math.hypot(x - cx, y - cy) || 1);
    const lx = x + ux * (r + 22), ly = y + uy * (r + 22);
    const anchor = Math.abs(ux) < 0.35 ? "middle" : ux > 0 ? "start" : "end";
    const dy = uy < -0.35 ? -14 : 0;
    out += `<text x="${lx}" y="${ly + dy}" text-anchor="${anchor}" class="node-sub">${esc(gpu)}${s.gpu ? ` · ${Math.round(s.gpu.vram_mb / 1024)}GB` : ""}</text>`;
    out += `<text x="${lx}" y="${ly + dy + 14}" text-anchor="${anchor}" class="node-sub">${s.inflight}/${s.slots} slots busy${s.prefill ? ` · ${Math.round(s.prefill)} tok/s prefill` : ""}</text>`;
  }

  // Entry points and the rest, on the outer orbit.
  for (const e of c.entries) {
    if (c.serving.find((s) => s.node === e.node)) continue;
    const [x, y] = pos.get(e.node);
    const cls = e.role === "entrypoint" ? "entry" : "node gpu dim";
    out += `<rect x="${x - 58}" y="${y - 20}" width="116" height="40" rx="20" class="${cls}"/>`;
    out += `<text x="${x}" y="${y - 1}" text-anchor="middle" class="entry-name">${esc(e.node)}</text>`;
    out += `<text x="${x}" y="${y + 13}" text-anchor="middle" class="entry-sub">${e.role === "entrypoint" ? (e.public ? "entrypoint · public" : "entrypoint") : "routes " + esc(e.via)}</text>`;
    if (e.public) {
      // The internet, just outside the tailnet orbit, reaching in here.
      const ux = (x - cx) / (Math.hypot(x - cx, y - cy) || 1), uy = (y - cy) / (Math.hypot(x - cx, y - cy) || 1);
      const ox = cx + ux * (R2 + 112), oy = cy + uy * (R2 + 112);
      const ex = x + ux * 58, ey = y + uy * 20; // the pill's outer edge
      out += `<line x1="${ox + ux * -15}" y1="${oy + uy * -15}" x2="${ex}" y2="${ey}" class="link link-public"/>
        <line x1="${ox + ux * -15}" y1="${oy + uy * -15}" x2="${ex}" y2="${ey}" class="flow flow-public"/>
        <circle cx="${ox}" cy="${oy}" r="15" class="globe"/>
        <path d="M${ox - 15},${oy} h30 M${ox},${oy - 15} q-9,15 0,30 q9,-15 0,-30" class="globe-lines"/>
        <text x="${ox}" y="${oy + 30}" text-anchor="middle" class="entry-sub">internet</text>`;
    }
  }
  for (const n of c.others) {
    const [x, y] = pos.get(n);
    out += `<circle cx="${x}" cy="${y}" r="16" class="node dim"/><text x="${x}" y="${y + 30}" text-anchor="middle" class="node-sub">${esc(n)}</text>`;
  }
  svg.innerHTML = out;
}

for (const b of document.querySelectorAll("#mesh-view .seg-btn")) {
  b.addEventListener("click", () => {
    topo.view = b.dataset.view;
    for (const o of document.querySelectorAll("#mesh-view .seg-btn")) o.classList.toggle("active", o === b);
    renderMesh();
  });
}
