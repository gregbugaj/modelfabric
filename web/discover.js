import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { capBadges, fetchJSON, mm, nodeAPI, selfNode } from "./my-models.js";
import { tick } from "./polling.js";
import { el } from "./rendering.js";
import { operations, progressBar } from "./runtime.js";
import { compactCount, downloadTargets, formatBytes, platformBadge, quantRows, relativeTime, renderMarkdown } from "./ui-model.js";

/* ---------- Discover: the hub, LM Studio's model browser ---------- */

// A small icon set, drawn inline so the dashboard fetches nothing.
const ICONS = {
  download: '<path d="M12 3v12m0 0-5-5m5 5 5-5M4 19h16"/>',
  heart: '<path d="M12 20s-7-4.4-7-10a4 4 0 0 1 7-2.6A4 4 0 0 1 19 10c0 5.6-7 10-7 10Z"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/>',
  server: '<rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/>',
  disk: '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="2.5"/>',
  check: '<path d="m5 12 5 5 9-10"/>',
  x: '<path d="M6 6l12 12M18 6 6 18"/>',
  star: '<path d="m12 3 2.7 5.6 6.1.9-4.4 4.3 1 6.1L12 17l-5.4 2.9 1-6.1-4.4-4.3 6.1-.9Z"/>',
  chip: '<rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4"/>',
};
// Platform glyphs, in the same stroked style as ICONS. A node's platform is
// worth seeing at a glance now that the fleet is mixed: only a Mac runs Metal
// and MLX, only Linux runs llm-d.
export const PLATFORM_ICONS = {
  // Each platform in its own colours, as people know them: Tux's orange beak
  // and feet, the Apple stripes, Microsoft's four panes. The body of the
  // penguin is currentColor so it stays visible in both themes; the cut-outs
  // are the surface behind the icon, which is the table row or a node circle.
  mac: '<path fill="url(#mfsh-apple)" d="M16.8 13.1c0 3.6-2.2 6.7-3.7 6.7-.85 0-1.25-.42-2.1-.42s-1.28.42-2.12.42c-1.5 0-3.68-3.1-3.68-6.7 0-2.7 1.85-4.3 3.6-4.3.92 0 1.55.5 2.2.5s1.22-.5 2.18-.5c1.75 0 3.62 1.6 3.62 4.3Z"/><path fill="#61bb46" d="M13.05 6.35c.62-.75 1.68-1.25 2.55-1.25.12.95-.28 1.9-.9 2.6-.62.72-1.6 1.2-2.5 1.15-.13-.9.23-1.8.85-2.5Z"/>',
  linux: '<path fill="currentColor" d="M12 2.6c2 0 3.3 1.5 3.3 3.6 0 1 .35 1.7 1 2.5 1.2 1.5 2.1 3.4 2.1 5.4 0 3.6-2.6 5.8-6.4 5.8S5.6 17.7 5.6 14.1c0-2 .9-3.9 2.1-5.4.65-.8 1-1.5 1-2.5 0-2.1 1.3-3.6 3.3-3.6Z"/><ellipse cx="12" cy="15.4" rx="3" ry="3.6" fill="var(--surface)"/><circle cx="10.5" cy="6.2" r="1.05" fill="var(--surface)"/><circle cx="13.5" cy="6.2" r="1.05" fill="var(--surface)"/><circle cx="10.6" cy="6.35" r=".45" fill="currentColor"/><circle cx="13.4" cy="6.35" r=".45" fill="currentColor"/><path fill="#f7a41d" d="M12 7.5c.75 0 1.35.5 1.35 1s-.6 1-1.35 1-1.35-.5-1.35-1 .6-1 1.35-1Z"/><path fill="#f7a41d" d="M8.7 19.6c-.7.8-1.6 1.3-2.5 1.5.2-1 .8-1.9 1.6-2.4Zm6.6 0 .9-.9c.8.5 1.4 1.4 1.6 2.4-.9-.2-1.8-.7-2.5-1.5Z"/>',
  windows: '<path fill="#f25022" d="M3.8 6.1 11 5.1v6.4H3.8Z"/><path fill="#7fba00" d="m12.2 4.9 8-1.1v7.7h-8Z"/><path fill="#00a4ef" d="M3.8 12.7H11v6.3l-7.2-1Z"/><path fill="#ffb900" d="M12.2 12.7h8v7.6l-8-1.1Z"/>',
};

// The Apple stripes live in one hidden <defs> the icons point at, so the
// gradient is defined once however many nodes are on screen.
export function ensurePlatformDefs() {
  if (document.getElementById("mfsh-plat-defs")) return;
  const defs = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  defs.id = "mfsh-plat-defs";
  defs.setAttribute("aria-hidden", "true");
  defs.setAttribute("width", "0");
  defs.setAttribute("height", "0");
  defs.style.position = "absolute";
  defs.innerHTML = `<defs><linearGradient id="mfsh-apple" x1="0" y1="0" x2="0" y2="1">
    <stop offset="0" stop-color="#61bb46"/><stop offset="0.001" stop-color="#61bb46"/>
    <stop offset="0.2" stop-color="#61bb46"/><stop offset="0.2" stop-color="#fdb827"/>
    <stop offset="0.4" stop-color="#fdb827"/><stop offset="0.4" stop-color="#f5821f"/>
    <stop offset="0.6" stop-color="#f5821f"/><stop offset="0.6" stop-color="#e03a3e"/>
    <stop offset="0.75" stop-color="#e03a3e"/><stop offset="0.75" stop-color="#963d97"/>
    <stop offset="0.88" stop-color="#963d97"/><stop offset="0.88" stop-color="#009ddc"/>
    <stop offset="1" stop-color="#009ddc"/></linearGradient></defs>`;
  document.body.append(defs);
}

// platformTag renders a node's platform as a glyph with the detail on hover.
// A node that does not report one shows nothing rather than a guess.
export function platformTag(platform, osVersion, size = 19) {
  const b = platformBadge(platform, osVersion);
  const glyph = PLATFORM_ICONS[b.key];
  if (!glyph) return null;
  ensurePlatformDefs();
  const s = el("span", "plat plat-" + b.key);
  s.title = b.title;
  s.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" role="img" aria-label="${b.label}">${glyph}</svg>`;
  return s;
}

export function icon(name, size = 14) {
  const s = el("span", "ico");
  s.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name]}</svg>`;
  return s;
}
function withIcon(name, text, cls = "") {
  const s = el("span", "with-ico " + cls);
  s.append(icon(name), document.createTextNode(text));
  return s;
}

// The creator's picture, through the node (cached); initials until it loads
// or when there is none.
function avatar(org, size = 32) {
  const box = el("span", "avatar");
  box.style.width = box.style.height = `${size}px`;
  box.textContent = (org || "?").slice(0, 2);
  if (org) {
    const img = new Image(size, size);
    img.alt = "";
    img.onload = () => box.replaceChildren(img);
    img.src = `/api/v1/hub/avatar?org=${encodeURIComponent(org)}`;
  }
  return box;
}

export function caps(m) {
  return [m.vision && "vision", m.tools && "tool_use", m.reasoning && "reasoning"].filter(Boolean);
}
function contextLabel(n) { return n ? (n >= 1024 ? `${Math.round(n / 1024)}K` : String(n)) : ""; }

export const dv = { author: "", q: "", results: [], searched: false, repo: "", details: null, quant: "", chosen: new Set(),
  storage: {}, storageAt: 0, more: [], timer: 0,
  pickerOpen: false }; // the quantization list stays collapsed until asked for

async function dvSearch() {
  dv.searched = true;
  const list = $("dv-results");
  list.replaceChildren(el("div", "empty", "Searching…"));
  try {
    const params = new URLSearchParams({ q: dv.q, author: dv.author });
    dv.results = (await fetchJSON(`/api/v1/hub/search?${params}`)).results ?? [];
  } catch (err) {
    list.replaceChildren(el("div", "empty err-text", `Search failed: ${err.message}`));
    return;
  }
  renderResults();
  if (!dv.repo && dv.results.length) dvOpen(dv.results[0].repo);
}

function resultMeta(m) {
  return [m.params, m.architecture, m.context_length ? `${contextLabel(m.context_length)} context` : ""].filter(Boolean).join(" · ");
}

function renderResults() {
  const list = $("dv-results");
  list.replaceChildren();
  if (!dv.results.length) { list.append(el("div", "empty", "No GGUF models match.")); return; }
  for (const m of dv.results) {
    const item = el("button", "dv-item" + (m.repo === dv.repo ? " active" : ""));
    item.type = "button";
    const body = el("div", "dv-item-body");
    const top = el("div", "dv-item-top");
    top.append(el("span", "dv-name", m.name));
    if (m.pick) { const b = icon("star", 12); b.classList.add("pick"); b.title = "LM Studio community pick"; top.append(b); }
    body.append(top, el("div", "dv-sub", resultMeta(m) || m.author));
    const side = el("div", "dv-item-side");
    side.append(capBadges(caps(m)), el("span", "dv-date", relativeTime(m.last_modified)));
    item.append(avatar(m.creator || m.author, 30), body, side);
    item.addEventListener("click", () => dvOpen(m.repo));
    list.append(item);
  }
}

async function refreshStorage() {
  if (Date.now() - dv.storageAt < 30e3) return;
  dv.storageAt = Date.now();
  await Promise.all(mm.catalog.perNode.map(async (n) => {
    try { dv.storage[n.node] = await fetchJSON(nodeAPI(n.node, "/api/v1/storage")); } catch { /* unknown */ }
  }));
}

async function dvOpen(repo) {
  dv.repo = repo;
  dv.details = null;
  dv.chosen = new Set();
  renderResults();
  const page = $("dv-page");
  page.replaceChildren(el("div", "empty", `Loading ${repo}…`));
  try {
    const [d] = await Promise.all([fetchJSON(`/api/v1/hub/model?repo=${encodeURIComponent(repo)}`), refreshStorage()]);
    dv.details = d;
  } catch (err) {
    page.replaceChildren(el("div", "empty err-text", err.message));
    return;
  }
  if (dv.repo !== repo) return;
  const rec = dv.details.options.find((o) => o.recommended) ?? dv.details.options[0];
  dv.quant = rec?.quant ?? "";
  renderPage();
  loadMore(dv.details);
}

async function loadMore(d) {
  dv.more = [];
  const who = d.creator || d.repo.split("/")[0];
  try {
    const r = await fetchJSON(`/api/v1/hub/search?${new URLSearchParams({ q: who, author: "" })}`);
    dv.more = (r.results ?? []).filter((m) => m.repo !== d.repo).slice(0, 6);
  } catch { /* optional */ }
  if (dv.details === d) renderMore();
}

function renderPage() {
  const d = dv.details;
  const page = $("dv-page");
  page.replaceChildren();
  const name = d.repo.split("/")[1].replace(/-GGUF$/i, "");

  // Header: who made it, what it is called, where it lives.
  const head = el("div", "dv-head");
  const title = el("div", "dv-title");
  const titleText = el("div", "dv-title-text");
  titleText.append(el("div", "dv-h", name));
  const repoLine = el("div", "dv-repo");
  const link = el("a", "mono", d.repo);
  link.href = `https://huggingface.co/${d.repo}`;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  const copy = el("button", "btn icon ghost");
  copy.title = "Copy repository id";
  copy.append(icon("copy", 13));
  copy.addEventListener("click", () => navigator.clipboard?.writeText(d.repo).then(() => showNotice(`Copied ${d.repo}`, "success")));
  repoLine.append(link, copy);
  titleText.append(repoLine);
  title.append(avatar(d.creator || d.repo.split("/")[0], 40), titleText);
  if (d.pick) { const p = el("span", "pick-badge"); p.append(icon("star", 12), "LM Studio pick"); title.append(p); }
  const stats = el("div", "dv-stats");
  stats.append(withIcon("download", compactCount(d.downloads), "chip"), withIcon("heart", compactCount(d.likes), "chip"),
    withIcon("clock", `updated ${relativeTime(d.last_modified)}`, "muted"));
  head.append(title, stats);
  page.append(head);

  // What it is, at a glance.
  const facts = el("div", "dv-facts");
  const fact = (label, value, cls = "") => {
    if (!value) return;
    const f = el("div", "fact");
    f.append(el("span", "fact-label", label), value instanceof Node ? value : el("span", "fact-value " + cls, value));
    facts.append(f);
  };
  fact("Params", d.params);
  fact("Arch", d.architecture, "mono");
  fact("Context", d.context_length ? `${contextLabel(d.context_length)} tokens` : "");
  fact("Format", Object.assign(el("span", "format-badge"), { textContent: "GGUF" }));
  fact("License", d.license);
  if (d.base_model) fact("Base", d.base_model, "mono");
  const c = caps(d);
  if (c.length) fact("Capabilities", capBadges(c, true));
  page.append(facts);

  page.append(renderDownloadCard(d));

  const readme = el("div", "dv-section");
  readme.append(el("div", "dv-section-title", "Model card"));
  const md = el("div", "md");
  md.innerHTML = d.readme ? renderMarkdown(d.readme) : "<p class=\"muted\">No model card.</p>";
  readme.append(md);
  page.append(readme);

  const more = el("div", "dv-section");
  more.id = "dv-more";
  page.append(more);
  renderMore();
}

function renderDownloadCard(d) {
  const card = el("div", "dv-dl");
  card.id = "dv-dl";
  const head = el("div", "dv-dl-head");
  head.append(withIcon("download", "Download"));
  card.append(head);
  if (!d.options.length) {
    card.append(el("div", "muted", "This repository has no GGUF files."));
    return card;
  }

  // 1. Which file: LM Studio's download options.
  // 1. Which file. A popular repo ships a dozen quantizations, so the list
  // collapses to the chosen one — as LM Studio's download options do — and
  // opens to a scrollable list rather than pushing the page down.
  card.append(el("div", "dv-step", "Quantization"));
  const chosenOpt = d.options.find((o) => o.quant === dv.quant) ?? d.options[0];
  const { rows: qrows, common } = quantRows(d.options, chosenOpt?.quant);

  // The size column: the number, and under it a bar against the largest file.
  // The choice between quantizations is quality against bytes on a disk, so
  // the bytes are drawn, not only printed.
  const sizeCell = (r, bar) => {
    const cell = el("span", "dv-size");
    cell.append(el("span", "dv-bytes", r.sizeLabel));
    if (bar) {
      const track = el("span", "dv-bar");
      const fill = el("span", "dv-bar-fill");
      fill.style.width = Math.max(r.frac * 100, 4) + "%";
      track.append(fill);
      cell.append(track);
    }
    return cell;
  };

  // The quantization is the row's identity, so it leads; a badge earns its
  // place only by telling this row from the others.
  const optionLabel = (r, withCheck) => {
    const label = el("span", "dv-option-main");
    if (withCheck) {
      const tick = el("span", "dv-tick");
      if (r.active) tick.append(icon("check", 13));
      label.append(tick);
    }
    label.append(el("span", "dv-quant", r.quant));
    if (r.recommended) label.append(el("span", "rec", "recommended"));
    if (r.fileCount > 1) label.append(el("span", "muted small", `${r.fileCount} files`));
    if (r.projector) { const v = capBadges(["vision"]); v.title = `includes ${r.projector}`; label.append(v); }
    return label;
  };

  const picker = el("details", "dv-picker");
  picker.open = dv.pickerOpen === true;
  const summary = el("summary", "dv-option dv-summary");
  const chosenRow = qrows.find((r) => r.active) ?? qrows[0];
  if (chosenRow) {
    summary.append(optionLabel(chosenRow, false), sizeCell(chosenRow, false));
    const hint = el("span", "dv-change", qrows.length > 1 ? "Change" : "");
    summary.append(hint);
  } else {
    summary.append(el("span", "muted", "No GGUF files in this repository"));
    summary.append(el("span"), el("span"));
  }
  summary.append(el("span", "dv-caret", "⌄"));
  picker.append(summary);
  picker.addEventListener("toggle", () => { dv.pickerOpen = picker.open; });

  const list = el("div", "dv-options");
  if (qrows.length > 1) {
    // One line for everything the options agree on, instead of the same badge
    // on every row: format, and a projector they all carry.
    const facts = [`${common.count} builds`];
    if (common.format) facts.push(common.format);
    if (common.projector) facts.push("vision projector included");
    // Outside the scroll area: it describes the list, so it should not scroll
    // away as you look down it.
    picker.append(el("div", "dv-picker-head", facts.join(" \u00b7 ")));
  }
  for (const r of qrows) {
    const o = d.options.find((x) => x.quant === r.quant);
    // A label with a real radio, not a button: a <button> is an awkward grid
    // container (its children do not become grid items, so the size column
    // collapsed) and it carries the UA's own background, which read as grey
    // blocks in dark mode.
    const row = el("label", "dv-option" + (r.active ? " active" : ""));
    const radio = el("input", "dv-radio");
    radio.type = "radio";
    radio.name = "dv-quant";
    radio.value = r.quant;
    radio.checked = r.active;
    radio.addEventListener("change", () => {
      dv.quant = r.quant;
      dv.chosen = new Set();
      dv.pickerOpen = false;
      renderDownloadSection();
    });
    row.title = o?.projector ? `includes ${o.projector}` : "";
    row.append(radio, optionLabel(r, true), sizeCell(r, true));
    list.append(row);
  }
  picker.append(list);
  card.append(picker);

  // 2. Where: the nodes, as cards to tick.
  card.append(el("div", "dv-step", "Download to"));
  const nodes = el("div", "dv-nodes");
  nodes.id = "dv-nodes";
  card.append(nodes);
  const action = el("div", "dv-action");
  action.id = "dv-action";
  card.append(action);
  renderDownloadSection(card);
  return card;
}

function currentOption() {
  return dv.details?.options.find((o) => o.quant === dv.quant);
}

function renderDownloadSection(card = document) {
  const d = dv.details;
  const nodesBox = card.querySelector ? card.querySelector("#dv-nodes") : null;
  if (!d || !nodesBox) return;
  for (const r of document.querySelectorAll(".dv-option")) r.classList.toggle("active", r.querySelector("input")?.value === dv.quant);
  const ops = Object.fromEntries([...mm.nodes].map(([n, data]) => [n, data.operations ?? []]));
  const option = currentOption();
  const targets = downloadTargets({ repo: d.repo, option, nodes: mm.catalog.perNode.map((n) => n.node),
    self: selfNode, storage: dv.storage, rows: mm.catalog.rows, ops });
  for (const n of [...dv.chosen]) if (!targets.find((t) => t.node === n && t.selectable)) dv.chosen.delete(n);
  if (dv.chosen.size === 0 && !dv.touched) {
    const first = targets.find((t) => t.selectable && t.self) ?? targets.find((t) => t.selectable);
    if (first) dv.chosen.add(first.node);
  }

  nodesBox.replaceChildren();
  for (const t of targets) {
    const tile = el("label", `dv-node state-${t.state}` + (dv.chosen.has(t.node) ? " chosen" : ""));
    const box = el("input");
    box.type = "checkbox";
    box.checked = dv.chosen.has(t.node);
    box.disabled = !t.selectable;
    box.addEventListener("change", () => {
      dv.touched = true;
      if (box.checked) dv.chosen.add(t.node); else dv.chosen.delete(t.node);
      renderDownloadSection();
    });
    const main = el("div", "dv-node-main");
    const name = el("div", "dv-node-name");
    name.append(icon("server", 14), document.createTextNode(t.node));
    if (t.self) name.append(el("span", "self-tag", "this node"));
    main.append(name);
    const specs = el("div", "dv-node-specs");
    if (t.gpu) specs.append(withIcon("chip", `${t.gpu.replace(/^(NVIDIA|AMD|Intel)\s+(GeForce\s+)?/i, "")} · ${formatBytes(t.vramBytes)}`));
    if (t.freeBytes !== null) specs.append(withIcon("disk", `${formatBytes(t.freeBytes)} free`));
    main.append(specs);
    const status = el("div", "dv-node-status");
    if (t.state === "have") status.append(withIcon("check", t.note, "ok"));
    else if (t.state === "downloading") {
      status.append(el("div", "small", t.note), progressBar(t.op?.fraction ?? 0));
      const cancel = el("button", "btn sm danger");
      cancel.type = "button";
      cancel.append(icon("x", 12), " Cancel");
      cancel.addEventListener("click", (e) => { e.preventDefault(); cancelDownload(t.node, t.op); });
      status.append(cancel);
    } else if (t.note) status.append(el("div", t.state === "nospace" ? "err-text small" : "muted small", t.note));
    if (t.warn) status.append(el("div", "warn-text small", t.warn));
    tile.append(box, main, status);
    nodesBox.append(tile);
  }

  const action = card.querySelector("#dv-action");
  action.replaceChildren();
  const n = dv.chosen.size;
  const go = el("button", "btn primary lg");
  go.type = "button";
  go.disabled = n === 0 || !option;
  go.append(icon("download", 15), n ? ` Download ${option?.quant} to ${n === 1 ? [...dv.chosen][0] : `${n} nodes`}` : " Choose a node");
  go.addEventListener("click", dvDownload);
  const total = option ? formatBytes(option.bytes * Math.max(n, 1)) : "";
  action.append(go, el("span", "muted small", n ? `${total} in total · runs on each node; this tab can close` : ""));
  // Recent failures and cancellations, so a stopped download is not a mystery.
  for (const [node, list] of Object.entries(ops)) {
    for (const op of list) {
      if (op.kind !== "download" || !op.model.startsWith(d.repo) || op.state === "running" || op.state === "succeeded") continue;
      if (Date.now() - Date.parse(op.started_at) > 15 * 60e3) continue;
      action.append(el("div", op.state === "cancelled" ? "muted small" : "err-text small",
        `${node}: ${op.state === "cancelled" ? "download cancelled; downloading again resumes it" : "download failed: " + (op.error || "")}`));
    }
  }
}

// Called on every poll: progress and states change; the rest stays put.
export function renderDownloads() {
  if (dv.details) renderDownloadSection();
}

function renderMore() {
  const box = $("dv-more");
  if (!box) return;
  box.replaceChildren();
  if (!dv.more.length) return;
  box.append(el("div", "dv-section-title", `More from ${dv.details.creator || dv.details.repo.split("/")[0]}`));
  for (const m of dv.more) {
    const row = el("button", "dv-more-row");
    row.type = "button";
    row.append(el("span", "mono", m.repo), el("span", "muted small", resultMeta(m)),
      withIcon("download", compactCount(m.downloads), "muted small"));
    row.addEventListener("click", () => dvOpen(m.repo));
    box.append(row);
  }
}

async function dvDownload() {
  const d = dv.details;
  const quant = dv.quant;
  const nodes = [...dv.chosen];
  const results = await Promise.allSettled(nodes.map((node) => fetchJSON(nodeAPI(node, "/api/v1/models/download"), {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ repo: d.repo, quant }),
  })));
  const failed = results.map((r, i) => [nodes[i], r]).filter(([, r]) => r.status === "rejected");
  if (failed.length) showNotice(failed.map(([n, r]) => `${n}: ${r.reason.message}`).join("; "), "error");
  else showNotice(`Downloading ${d.repo} (${quant}) to ${nodes.join(", ")}.`, "success");
  dv.chosen = new Set();
  dv.touched = false;
  dv.storageAt = 0;
  tick();
}

async function cancelDownload(node, op) {
  if (!op) return;
  try {
    await fetchJSON(nodeAPI(node, `/api/v1/operations/${encodeURIComponent(op.id)}/cancel`), { method: "POST" });
    showNotice(`Cancelled the download on ${node}; downloading again resumes it.`);
  } catch (err) {
    showNotice(`Could not cancel on ${node}: ${err.message}`, "error");
  }
  tick();
}

// Discover opens over My Models, as LM Studio's does: finding a model is
// part of managing models, not a place of its own.
$("dv-open").addEventListener("click", () => {
  $("dv-dialog").showModal();
  if (!dv.searched) dvSearch();
  $("dv-q").focus();
  tick();
});
$("dv-close").addEventListener("click", () => $("dv-dialog").close());
$("dv-dialog").addEventListener("click", (e) => { if (e.target === $("dv-dialog")) $("dv-dialog").close(); });

$("dv-q").addEventListener("input", (e) => {
  dv.q = e.target.value;
  clearTimeout(dv.timer);
  dv.timer = setTimeout(dvSearch, 350);
});
for (const b of document.querySelectorAll("#dv-source .seg-btn")) {
  b.addEventListener("click", () => {
    dv.author = b.dataset.author;
    for (const o of document.querySelectorAll("#dv-source .seg-btn")) o.classList.toggle("active", o === b);
    dvSearch();
  });
}
