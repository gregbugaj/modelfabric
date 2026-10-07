import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { caps, icon, platformTag } from "./discover.js";
import { tick } from "./polling.js";
import { el, lastView } from "./rendering.js";
import { meta } from "./routing.js";
import { operations } from "./runtime.js";
import { fieldInput, openPresetEditor, openPresetManager, presets, refreshPresets, setPresets } from "./settings-presets.js";
import { PRESET_FIELDS, SETTINGS_SCHEMA, buildCatalog, filterCatalog, formatBytes, groupByModel, inheritedValue, parseSettingsForm, presetDrift, relativeTime, settingsToForm } from "./ui-model.js";
import { profileTitle, routingView, workloadFor } from "./workload-presets.js";

// Peer API calls use /api/v1/nodes/<node>/... and require the same owner.
// The path argument is the local /api/v1/... path.
export function nodeAPI(node, path) {
  return node === selfNode ? path : `/api/v1/nodes/${encodeURIComponent(node)}${path.replace(/^\/api\/v1/, "")}`;
}

export let selfNode = "";
export const mm = {
  node: "",            // "" = all nodes
  group: "model",      // "model" = where each model lives; "node" = per machine
  text: "",
  selected: "",        // row id
  tab: "info",
  nodes: new Map(),    // node -> { api, operations, error }
  catalog: { rows: [], unreachable: [], perNode: [] },
  shownFor: "",        // "<row id>|<tab>" the side panel was built for
  pinned: readSidePref("mfsh.side.pinned") === "1",
  width: Number(readSidePref("mfsh.side.width")) || 420,
  presets: new Map(),  // node -> [presets]
};
const mmPending = new Set(); // row ids with an action in flight

// Unavailable browser storage must not prevent the panel from opening.
export function readSidePref(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}
export function writeSidePref(key, value) {
  try { localStorage.setItem(key, value); } catch { /* Browser storage is optional. */ }
}

export async function fetchJSON(url, opts) {
  const resp = await fetch(url, { cache: "no-store", ...opts });
  const text = await resp.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { /* Preserve the response text if it is not JSON. */ }
  if (!resp.ok) throw new Error(data?.error?.message || text || `HTTP ${resp.status}`);
  return data;
}

// Subscribe to peer events only while My Models is open. The local node
// proxies each stream, avoiding repeated peer requests on every tick.
const peerFeeds = new Map();

function openPeerFeed(node) {
  const f = { es: new EventSource(nodeAPI(node, "/api/v1/events?only=models,operations")), data: new Map() };
  for (const name of ["models", "operations"]) {
    f.es.addEventListener(name, (e) => {
      f.data.set(name, JSON.parse(e.data));
      if (f.data.size === 2) void tick();
    });
  }
  f.es.onerror = () => {
    f.data.clear();
    // A CLOSED stream means an old or refusing peer. Keep its entry so
    // polling takes over; retry the stream only after it rejoins the mesh.
  };
  peerFeeds.set(node, f);
}

export function closePeerFeeds(keep = []) {
  for (const [node, f] of peerFeeds) {
    if (keep.includes(node)) continue;
    f.es.close();
    peerFeeds.delete(node);
  }
}

export async function refreshPeerModels(peers) {
  closePeerFeeds(peers);
  await Promise.all(peers.map(async (node) => {
    if (!document.hidden && !peerFeeds.has(node)) openPeerFeed(node);
    const f = peerFeeds.get(node);
    if (f?.data.size === 2) {
      // The feed uses null for a GET that did not return 200.
      const api = f.data.get("models");
      mm.nodes.set(node, api
        ? { api, operations: f.data.get("operations")?.operations ?? [] }
        : { error: "this node does not manage models" });
      return;
    }
    try {
      const [api, ops] = await Promise.all([
        fetchJSON(nodeAPI(node, "/api/v1/models")),
        fetchJSON(nodeAPI(node, "/api/v1/operations")).catch(() => ({ operations: [] })),
      ]);
      mm.nodes.set(node, { api, operations: ops.operations ?? [] });
    } catch (err) {
      mm.nodes.set(node, { error: err.message });
    }
  }));
}

const nodePlatforms = new Map();

export function renderMyModels() {
  for (const n of lastView?.nodes ?? []) {
    if (n.platform) nodePlatforms.set(n.name, { platform: n.platform, osVersion: n.osVersion });
  }
  const inputs = [...mm.nodes.entries()].map(([node, d]) => ({ node, self: node === selfNode, ...d }));
  mm.catalog = buildCatalog(inputs);
  const { rows, unreachable, perNode } = mm.catalog;

  const seg = $("mm-nodes");
  seg.replaceChildren();
  const tab = (value, label, count) => {
    const b = el("button", "seg-btn" + (mm.node === value ? " active" : ""));
    b.type = "button";
    b.append(label, el("span", "seg-count", String(count)));
    b.addEventListener("click", () => { mm.node = value; renderMyModels(); });
    seg.append(b);
  };
  tab("", "All nodes", rows.length);
  for (const n of perNode) tab(n.node, n.node, n.models);

  const warn = $("mm-unreachable");
  warn.hidden = unreachable.length === 0;
  warn.replaceChildren(...unreachable.map((u) => el("div", null, `${u.node}: ${u.error}`)));

  const groupSeg = $("mm-group");
  groupSeg.replaceChildren();
  for (const [value, label] of [["model", "By model"], ["node", "By node"]]) {
    const b = el("button", "seg-btn" + (mm.group === value ? " active" : ""), label);
    b.type = "button";
    b.addEventListener("click", () => { mm.group = value; renderMyModels(); });
    groupSeg.append(b);
  }

  const shown = filterCatalog(rows, mm.node, mm.text);
  const head = $("mm-head");
  head.replaceChildren();
  const headRow = el("tr");
  const cols = mm.group === "model"
    ? ["Node", "Quant", { t: "Size", c: "num" }, { t: "Modified", c: "opt" }, "State", ""]
    : ["Node", "Arch", "Params", "Model", "Quant", { t: "Size", c: "num" }, { t: "Modified", c: "opt" }, "State", ""];
  for (const c of cols) headRow.append(el("th", typeof c === "string" ? "" : c.c, typeof c === "string" ? c : c.t));
  head.append(headRow);

  const body = $("mm-body");
  body.replaceChildren();
  if (mm.group === "model") {
    for (const g of groupByModel(shown)) {
      body.append(modelGroupRow(g, cols.length));
      for (const r of g.nodes) body.append(modelRow(r, true));
    }
  } else {
    for (const r of shown) body.append(modelRow(r));
  }
  const empty = $("mm-empty");
  empty.hidden = shown.length > 0;
  empty.textContent = rows.length ? "No models match." : "No models on any node you can manage yet.";

  const total = perNode.reduce((n, x) => n + x.bytes, 0);
  $("mm-foot").textContent = `${rows.length} model${rows.length === 1 ? "" : "s"} on ${perNode.length} node${perNode.length === 1 ? "" : "s"}, ${formatBytes(total)} on disk`;

  const sel = rows.find((r) => r.id === mm.selected);
  if (!sel) { mm.selected = ""; }
  const side = $("mm-side");
  const mmBox = side.closest(".mm");
  mmBox.classList.toggle("has-pinned", Boolean(sel) && mm.pinned);
  mmBox.style.setProperty("--side-w", `${mm.width}px`);
  side.classList.toggle("pinned", mm.pinned);
  side.style.setProperty("--side-w", `${mm.width}px`);
  renderSide(sel);
}

const CAP_ICONS = {
  vision: ["Vision", '<path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>'],
  tool_use: ["Tool use", '<path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.5-.5-.5-2.5 2.5-2.5Z"/>'],
  reasoning: ["Reasoning", '<path d="M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9c.6.5 1 1.2 1 2.1h5c0-.9.4-1.6 1-2.1A6 6 0 0 0 12 3Z"/>'],
};

export function capBadges(caps, withText = false) {
  const wrap = el("span", "caps");
  for (const c of caps) {
    // Newer peers can report unknown capabilities; guard lookup failures
    // so one missing icon cannot break the table.
    const known = CAP_ICONS[c];
    if (!known) continue;
    const [label, path] = known;
    const b = el("span", `cap cap-${c}`);
    b.title = label;
    b.innerHTML = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">${path}</svg>`;
    if (withText) b.append(" " + label);
    wrap.append(b);
  }
  return wrap;
}

function modelGroupRow(g, span) {
  const tr = el("tr", "mm-group-row");
  const td = el("td");
  td.colSpan = span;
  const line = el("div", "mm-group-line");
  const name = el("span", "mm-group-name mono");
  name.append(el("span", null, g.key), capBadges(g.caps));
  line.append(name);
  const tags = el("span", "mm-group-tags");
  if (g.arch) tags.append(el("span", "tag", g.arch));
  if (g.mtp) tags.append(el("span", "tag", "MTP"));
  if (g.params) tags.append(el("span", "tag", g.params));
  line.append(tags);
  const where = g.loaded
    ? `loaded on ${g.loaded} of ${g.nodes.length} node${g.nodes.length === 1 ? "" : "s"}`
    : `on ${g.nodes.length} node${g.nodes.length === 1 ? "" : "s"}, none loaded`;
  line.append(el("span", "mm-group-where" + (g.loaded ? " on" : ""), where));
  td.append(line);
  tr.append(td);
  return tr;
}

function modelRow(r, grouped) {
  const tr = el("tr", r.id === mm.selected ? "selected" : "");
  tr.dataset.row = r.id;
  tr.tabIndex = 0;
  const open = () => { mm.selected = r.id; renderMyModels(); $("mm-side").focus({ preventScroll: true }); };
  tr.addEventListener("click", (e) => {
    if (e.target.closest("button")) return;
    open();
  });
  tr.addEventListener("keydown", (e) => {
    if (e.target !== tr) return;
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); }
  });
  const arch = el("td");
  arch.append(el("span", "tag", r.arch || "—"));
  if (r.mtp) arch.append(el("span", "tag", "MTP"));
  const name = el("td", "mono");
  const inner = el("div", "mm-name");
  inner.append(el("span", null, r.key), capBadges(r.caps));
  name.append(inner);
  const state = el("td");
  const busy = r.busy || mmPending.has(r.id);
  if (busy) {
    const s = el("span", "muted");
    s.append(el("span", "spinner"), r.busyKind === "unload" ? "unloading" : "loading");
    state.append(s);
  } else if (r.loaded) {
    state.append(el("span", "pill fit-yes", "loaded"));
    // Place instance settings beside "loaded": they differ by node, and
    // the model-name cell is absent when rows are grouped by model.
    const think = thinkingState(r);
    if (think) {
      const chip = el("span", "chip think" + (think.on ? "" : " off"), think.text);
      chip.title = think.on
        ? "Thinking effort this instance is running with — the model's own recommendation unless something overrode it. Change it on the Inference tab."
        : "This instance answers without thinking first.";
      state.append(chip);
    }
  } else {
    state.append(el("span", "muted", "—"));
  }
  const actions = el("td", "actions");
  const btn = r.loaded ? el("button", "btn sm", "Unload") : el("button", "btn sm accent", "Load");
  btn.disabled = busy;
  btn.addEventListener("click", () => (r.loaded ? unloadRow(r) : loadRow(r)));
  actions.append(btn);
  const quant = el("td");
  if (r.quant) quant.append(el("span", "tag", r.quant)); else quant.textContent = "—";
  const nodeCell = el("td", (r.self ? "node self" : "node") + (grouped ? " mm-sub" : ""));
  const p = nodePlatforms.get(r.node);
  const plat = grouped && p ? platformTag(p.platform, p.osVersion, 14) : null;
  if (plat) nodeCell.append(plat);
  nodeCell.append(document.createTextNode(r.node));
  // Model groups already show identity; child rows show only node differences.
  if (grouped) {
    tr.classList.add("mm-sub-row");
    tr.append(nodeCell, quant, el("td", "num", formatBytes(r.sizeBytes)),
      el("td", "muted opt", r.modified ? relativeTime(r.modified) : "—"), state, actions);
    return tr;
  }
  tr.append(nodeCell, arch, el("td", null, r.params || "—"), name, quant,
    el("td", "num", formatBytes(r.sizeBytes)),
    el("td", "muted opt", r.modified ? relativeTime(r.modified) : "—"),
    state, actions);
  return tr;
}

export function closeSide() {
  const id = mm.selected;
  mm.selected = "";
  renderMyModels();
  const row = document.querySelector(`#mm-body [data-row="${CSS.escape(id)}"]`);
  if (row) row.focus({ preventScroll: true });
}

// A scrim captures the drag so it cannot activate the underlying table.
export function startSideResize(e, sideId = "mm-side", state = mm, prefKey = "mfsh.side.width") {
  const side = $(sideId);
  if (state.pinned || e.button !== 0) return;
  e.preventDefault();
  const scrim = el("div", "side-scrim");
  document.body.append(scrim);
  side.classList.add("resizing");
  const move = (ev) => {
    const w = Math.round(Math.min(900, Math.max(320, window.innerWidth - ev.clientX)));
    state.width = w;
    side.style.setProperty("--side-w", `${w}px`);
  };
  const up = () => {
    side.classList.remove("resizing");
    scrim.remove();
    writeSidePref(prefKey, String(state.width));
    window.removeEventListener("pointermove", move);
    window.removeEventListener("pointerup", up);
  };
  window.addEventListener("pointermove", move);
  window.addEventListener("pointerup", up);
}

function renderSide(r) {
  const side = $("mm-side");
  side.hidden = !r;
  if (!r) { mm.shownFor = ""; return; }
  const key = `${r.id}|${mm.tab}`;
  if (mm.shownFor === key) { updateSideHead(r); return; } // keep edits in progress
  mm.shownFor = key;

  side.replaceChildren();
  side.tabIndex = -1;
  const grip = el("div", "side-grip");
  grip.title = "Drag to resize";
  grip.addEventListener("pointerdown", startSideResize);
  side.append(grip);
  const head = el("div", "side-head");
  head.id = "side-head";
  side.append(head);
  updateSideHead(r);

  const tabs = el("div", "side-tabs");
  tabs.setAttribute("role", "tablist");
  for (const [id, label] of [["info", "Info"], ["load", "Load"], ["inference", "Inference"]]) {
    const active = mm.tab === id;
    const b = el("button", "side-tab" + (active ? " active" : ""), label);
    b.type = "button";
    b.setAttribute("role", "tab");
    b.setAttribute("aria-selected", String(active));
    b.addEventListener("click", () => { mm.tab = id; renderSide(r); });
    tabs.append(b);
  }
  side.append(tabs);

  const body = el("div", "side-body");
  side.append(body);
  if (mm.tab === "info") renderInfo(body, r);
  else renderSettingsTab(body, r, mm.tab === "inference");
}

function updateSideHead(r) {
  const head = $("side-head");
  if (!head) return;
  head.replaceChildren();
  const title = el("div", "side-title");
  title.append(el("span", "mono", r.key));
  const actions = el("div", "side-actions");
  const pin = el("button", "btn icon", mm.pinned ? "⇥" : "⇤");
  pin.title = mm.pinned ? "Unpin — let it fly out over the table" : "Pin — dock it beside the table";
  pin.setAttribute("aria-pressed", String(mm.pinned));
  pin.addEventListener("click", () => {
    mm.pinned = !mm.pinned;
    writeSidePref("mfsh.side.pinned", mm.pinned ? "1" : "0");
    renderMyModels();
  });
  const close = el("button", "btn icon", "×");
  close.title = "Close (Esc)";
  close.addEventListener("click", closeSide);
  actions.append(pin, close);
  title.append(actions);
  const sub = el("div", "side-sub");
  sub.append(el("span", r.self ? "node self" : "node", r.node));
  if (r.loaded) sub.append(el("span", "pill fit-yes", "loaded"));
  const busy = r.busy || mmPending.has(r.id);
  const btn = r.loaded ? el("button", "btn", "Unload") : el("button", "btn primary", "Load");
  btn.disabled = busy;
  btn.title = r.loaded ? `Unload from ${r.node}` : `Load on ${r.node}`;
  btn.addEventListener("click", () => (r.loaded ? unloadRow(r) : loadRow(r)));
  sub.append(btn);
  head.append(title, sub);
}

export function kv(parent, label, value, mono = true) {
  const row = el("div", "kv");
  row.append(el("span", "kv-label", label));
  const v = el("span", mono ? "kv-value mono" : "kv-value");
  if (value instanceof Node) v.append(value); else v.textContent = value || "—";
  row.append(v);
  parent.append(row);
}

function renderInfo(body, r) {
  const box = el("div", "kv-list");
  kv(box, "Model", r.key);
  kv(box, "File", r.file);
  kv(box, "Format", r.format);
  kv(box, "Quantization", r.quant);
  const arch = el("span");
  arch.append(el("span", "tag", r.arch || "—"));
  if (r.mtp) arch.append(el("span", "tag", "MTP"));
  kv(box, "Architecture", arch);
  kv(box, "Capabilities", r.caps.length ? capBadges(r.caps, true) : "—");
  kv(box, "Domain", r.type);
  kv(box, "Parameters", r.params);
  kv(box, "Layers", r.layers ? String(r.layers) : "");
  kv(box, "Max context", r.maxContext ? r.maxContext.toLocaleString() + " tokens" : "");
  kv(box, "Size on disk", formatBytes(r.sizeBytes));
  if (r.minMemory) kv(box, "Needs about", formatBytes(r.minMemory));
  kv(box, "Node", r.node, false);
  const wl = workloadFor(r.key);
  if (wl) {
    kv(box, "Workload", `${wl.title} — ${profileTitle(wl.profile)}`, false);
  } else if (routingView?.llmd?.running) {
    // llm-d serves one model at a time; name the model occupying it.
    kv(box, "Workload", `none — llm-d is scheduling ${routingView.llmd.model}`, false);
  }
  body.append(box);
  for (const i of r.instances) {
    const c = i.config ?? {};
    const inst = el("div", "kv-list");
    inst.append(el("div", "kv-head", `Running: ${i.id}`));
    kv(inst, "Context", c.context_length ? `${c.context_length.toLocaleString()} × ${c.parallel ?? 1} slots` : "");
    kv(inst, "Runtime", c.runtime || "");
    if (c.spec_type) kv(inst, "Speculative", c.spec_type);
    body.append(inst);
  }
}

async function renderSettingsTab(body, r, inference) {
  body.append(el("div", "muted side-note", "Loading settings…"));
  let saved = { preset: "", settings: {} };
  let presets = [];
  try {
    const [d, p] = await Promise.all([
      fetchJSON(nodeAPI(r.node, `/api/v1/model-defaults?model=${encodeURIComponent(r.key)}`)),
      fetchJSON(nodeAPI(r.node, "/api/v1/presets")).catch(() => ({ presets: [] })),
    ]);
    saved = d.defaults ?? saved;
    setPresets(p.presets ?? []);
  } catch (err) {
    body.replaceChildren(el("div", "err-text side-note", `Cannot read settings on ${r.node}: ${err.message}`));
    return;
  }
  if (mm.shownFor !== `${r.id}|${mm.tab}`) return;
  body.replaceChildren();

  const values = settingsToForm(saved.settings ?? {});
  const form = el("form", "side-form");
  form.addEventListener("submit", (e) => e.preventDefault());
  body.append(el("div", "muted side-note", inference
    ? "Applied to every request unless the request sets its own. An empty field inherits what is shown greyed — the preset's value when one is chosen, else the model's own recommendation."
    : `Saved on ${r.node} and used on every load there. Empty fields use ModelFabric's defaults for this model and GPU.`));

  const nodeModels = mm.catalog.rows.filter((x) => x.node === r.node && x.key !== r.key).map((x) => x.key);
  if (inference) {
    const bar = el("div", "preset-bar");
    bar.append(el("span", "preset-label", "Preset"));
    const sel = el("select", "select preset-select");
    sel.name = "__preset";
    sel.append(Object.assign(el("option", null, "none (this model's own settings)"), { value: "" }));
    for (const p of presets) sel.append(Object.assign(el("option", null, p.name), { value: p.name }));
    sel.value = saved.preset || "";
    sel.title = "Applied to every request for this model unless the request sets its own";
    const mark = el("span", "preset-dirty");
    mark.hidden = true;
    const saveTo = el("button", "btn small", "Save to preset");
    saveTo.type = "button";
    saveTo.hidden = true;
    const saveAs = el("button", "btn small", "Save as new…");
    saveAs.type = "button";
    saveAs.title = "Keep these settings as a preset of their own";
    const manage = el("button", "btn small", "Manage…");
    manage.type = "button";
    manage.title = "Create, edit, import or delete presets";
    manage.addEventListener("click", (e) => { e.preventDefault(); openPresetManager(); });

    const currentValues = () => {
      const v = {};
      for (const input of form.querySelectorAll("[name]")) v[input.name] = input.value;
      return v;
    };
    const refreshDrift = () => {
      const chosen = presets.find((p) => p.name === sel.value);
      const { dirty, changed } = presetDrift(currentValues(), chosen?.settings ?? {});
      const on = Boolean(chosen) && dirty;
      mark.hidden = !on;
      saveTo.hidden = !on;
      if (on) {
        mark.textContent = "unsaved";
        mark.title = `Differs from ${chosen.name}: ${changed.join(", ")}`;
        saveTo.title = `Overwrite ${chosen.name} with these settings`;
      }
      saveAs.hidden = Boolean(chosen) && !dirty;
    };
    // Empty fields inherit the selected preset, then model recommendations.
    const refreshHints = () => {
      const chosen = presets.find((p) => p.name === sel.value);
      for (const f of PRESET_FIELDS) {
        const input = form.querySelector(`[name="${f.key}"]`);
        if (!input) continue;
        const fromPreset = chosen ? settingsToForm(chosen.settings ?? {})[f.key] : "";
        const hint = fromPreset !== "" && fromPreset !== undefined
          ? `${chosen.name}: ${fromPreset}`
          : inheritedValue(r.spec, f.key);
        if (input.tagName === "SELECT") {
          if (input.options[0]) input.options[0].textContent = hint ? `model: ${hint}` : "inherit";
        } else {
          input.placeholder = hint || "inherit";
        }
      }
    };
    sel.addEventListener("change", () => { refreshHints(); refreshDrift(); });
    form.addEventListener("input", refreshDrift);

    saveAs.addEventListener("click", (e) => {
      e.preventDefault();
      const { settings } = parseSettingsForm(currentValues(), PRESET_FIELDS);
      openPresetEditor({ name: "", description: "", settings });
    });
    saveTo.addEventListener("click", async (e) => {
      e.preventDefault();
      const chosen = presets.find((p) => p.name === sel.value);
      if (!chosen) return;
      const { settings, errors } = parseSettingsForm(currentValues(), PRESET_FIELDS);
      if (Object.keys(errors).length) {
        showNotice("Fix: " + Object.entries(errors).map(([k, x]) => `${k} (${x})`).join(", "), "error");
        return;
      }
      try {
        await fetchJSON(`/api/v1/presets/${encodeURIComponent(chosen.name)}`, {
          method: "PUT", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ description: chosen.description ?? "", settings }),
        });
        chosen.settings = settings;
        await refreshPresets();
        refreshDrift();
        showNotice(`Saved these settings into ${chosen.name}`, "success");
      } catch (err) {
        showNotice(`Could not save the preset: ${err.message}`, "error");
      }
    });
    bar.append(sel, mark, saveTo, saveAs, manage);
    form.append(bar);
    queueMicrotask(() => { refreshHints(); refreshDrift(); });
  }
  for (const group of SETTINGS_SCHEMA.filter((gr) => Boolean(gr.inference) === inference)) {
    const g = el("div", "side-group");
    g.append(el("div", "side-group-title", group.group));
    for (const f of group.fields) {
      // Use levels from the model's template; guessed levels can cause
      // Jinja errors. Fall back to text input if the template lists none.
      let field = f;
      if (f.key === "reasoning_effort" && r.reasoningEfforts?.length) {
        field = { ...f, type: "pills", options: r.reasoningEfforts };
      }
      const input = fieldInput(field, values[f.key], nodeModels);
      const inherit = inference ? inheritedValue(r.spec, f.key) : runningValue(r, f.key);
      if (inherit) {
        if (input.tagName === "SELECT") input.options[0].textContent = `${inference ? "model" : "running"}: ${inherit}`;
        else if (input.classList.contains("pills")) {
          input.querySelector(".pill").textContent = `${inference ? "model" : "running"}: ${inherit}`;
        } else input.placeholder = inherit;
      }
      g.append(settingRow(f, input));
    }
    form.append(g);
  }
  if (!inference) {
    const g = el("div", "side-group");
    g.append(el("div", "side-group-title", "Engine"));
    const extra = fieldInput({ key: "extra_args", type: "text" }, values.extra_args, []);
    g.append(settingRow({ label: "Extra arguments", help: "space-separated; flags ModelFabric manages are refused" }, extra));
    form.append(g);
  }
  body.append(form);

  const err = el("div", "err-text side-note");
  const foot = el("div", "side-foot");
  const save = async (reload) => {
    const v = {};
    for (const input of form.querySelectorAll("[name]")) v[input.name] = input.value;
    // This form edits one tab; preserve the other tab's saved values.
    const other = settingsToForm(saved.settings ?? {});
    const merged = { ...other, ...v };
    const { settings, errors } = parseSettingsForm(merged);
    if (Object.keys(errors).length) {
      err.textContent = "Fix: " + Object.entries(errors).map(([k, e]) => `${k} (${e})`).join(", ");
      return;
    }
    const preset = "__preset" in v ? v.__preset : (saved.preset || "");
    try {
      await fetchJSON(nodeAPI(r.node, `/api/v1/model-defaults?model=${encodeURIComponent(r.key)}`), {
        method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ preset, settings }),
      });
      saved = { preset, settings };
      err.textContent = "";
      if (reload && r.loaded) {
        showNotice(`Saved. Reloading ${r.key} on ${r.node}…`);
        await unloadRow(r, true);
        await loadRow(r);
      } else {
        showNotice(`Saved ${r.key}'s settings on ${r.node}${r.loaded ? "; they apply on its next load" : ""}`, "success");
      }
    } catch (e) {
      err.textContent = e.message;
    }
  };
  const reset = el("button", "btn danger", "Reset");
  reset.type = "button";
  reset.title = "Remove every saved setting for this model on this node";
  reset.addEventListener("click", async () => {
    await fetchJSON(nodeAPI(r.node, `/api/v1/model-defaults?model=${encodeURIComponent(r.key)}`), { method: "DELETE" }).catch(() => {});
    mm.shownFor = ""; renderSide(r);
    showNotice(`Cleared ${r.key}'s saved settings on ${r.node}`, "success");
  });
  const saveBtn = el("button", "btn primary", "Save");
  saveBtn.type = "button";
  saveBtn.addEventListener("click", () => save(false));
  foot.append(reset, saveBtn);
  if (r.loaded) {
    const rl = el("button", "btn primary", "Save & reload");
    rl.type = "button";
    rl.addEventListener("click", () => save(true));
    foot.append(rl);
  }
  body.append(err, foot);
}

function runningValue(r, key) {
  const c = r.instances[0]?.config;
  if (!c) return "";
  if (key === "spec_mode") {
    const mode = { "draft-mtp": "mtp", "draft-simple": "draft" }[c.spec_type];
    return mode || (c.speculative === false ? "off" : "");
  }
  const v = c[key === "cache_ram" ? "cache_ram_mib" : key];
  if (v === undefined || v === null || v === "" || v === 0 && key !== "cache_ram") return "";
  if (typeof v === "boolean") return v ? "on" : "off";
  return String(v);
}

function thinkingState(r) {
  const c = r.instances[0]?.config;
  if (!c) return null;
  const kwargs = c.chat_template_kwargs ?? {};
  if (c.reasoning === "off" || kwargs.enable_thinking === false) return { on: false, text: "think off" };
  const effort = c.reasoning_effort || kwargs.reasoning_effort || r.spec?.template_vars?.reasoning_effort || "";
  // A zero budget disables thinking; -1 is the default and needs no badge.
  if (c.reasoning_budget === 0) return { on: false, text: "think off" };
  const budget = typeof c.reasoning_budget === "number" && c.reasoning_budget > 0 ? ` ≤${c.reasoning_budget}` : "";
  if (!effort && !budget) return null;
  return { on: true, text: `think ${effort || "on"}${budget}` };
}

function settingRow(f, input) {
  const row = el("label", "setting");
  const label = el("span", "setting-label", f.label);
  if (f.help) label.title = f.help;
  row.append(label, input);
  return row;
}

async function loadRow(r) {
  if (mmPending.has(r.id)) return;
  mmPending.add(r.id);
  renderMyModels();
  showNotice(`Loading ${r.key} on ${r.node}… a large model can take a minute.`);
  try {
    const res = await fetchJSON(nodeAPI(r.node, "/api/v1/models/load"), {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model: r.key, echo_load_config: true }),
    });
    const secs = ((res.elapsed_ms ?? 0) / 1000).toFixed(1);
    showNotice(res.instance?.id ? `Loaded ${r.key} on ${r.node} in ${secs}s` : `Load of ${r.key} is still running on ${r.node}.`, "success");
  } catch (err) {
    showNotice(`Load failed on ${r.node}: ${err.message}`, "error");
  } finally {
    mmPending.delete(r.id);
    tick();
  }
}

async function unloadRow(r, quiet = false) {
  if (!r.instances.length) return;
  if (!quiet && mmPending.has(r.id)) return;
  mmPending.add(r.id);
  renderMyModels();
  try {
    for (const i of r.instances) {
      await fetchJSON(nodeAPI(r.node, "/api/v1/models/unload"), {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ instance_id: i.id }),
      });
    }
    if (!quiet) showNotice(`Unloaded ${r.key} on ${r.node}`, "success");
  } catch (err) {
    showNotice(`Unload failed on ${r.node}: ${err.message}`, "error");
  } finally {
    mmPending.delete(r.id);
    if (!quiet) tick();
  }
}

$("mm-search").addEventListener("input", (e) => { mm.text = e.target.value; renderMyModels(); });

export function setSelfNode(v) { selfNode = v; }
