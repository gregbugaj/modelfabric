import { post, showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, mm, nodeAPI, selfNode } from "./my-models.js";
import { tick } from "./polling.js";
import { el, meshView } from "./rendering.js";
import { relativeTime } from "./ui-model.js";

// Each node measures its own slot capacity through /api/v1/tune, matching
// `mfsh tune -fleet`. Capacity depends on hardware and loaded context.
// Sweeps restore the original count; applying a recommendation is separate.

// Match the CLI fleet polling interval; each sweep row takes tens of seconds.
const TUNE_POLL_MS = 3000;

// Mirror internal/tuner's sweepMax to infer the unreported in-flight count.
const TUNE_MAX_SLOTS = 32;

// Allow transient poll failures before detaching from an unreachable node.
const TUNE_MISSES = 5;

export const tn = {
  model: "",
  scope: "",         // "" = every node serving the model, otherwise that one
  stage: "confirm",
  // node -> { status, error, misses, unmanaged, unsupported, attached, applying }
  nodes: new Map(),
  poll: 0,
};

// Only loaded models have a node and context available for a sweep.
function tuneModels() {
  return (meshView?.models ?? []).map((m) => m.id);
}

// Reuse the mesh view; match the CLI's fleetNodes selection.
function tuneNodesFor(model) {
  return [...((meshView?.models ?? []).find((m) => m.id === model)?.nodes ?? [])].sort();
}

function tuneTargets() {
  const all = tuneNodesFor(tn.model);
  return tn.scope ? all.filter((n) => n === tn.scope) : all;
}

// Show the loaded context in confirmation: throughput measurements
// at different context sizes are not comparable.
function tuneRunning(node, model) {
  const row = (mm.catalog.rows ?? []).find((r) => r.node === node && r.key === model);
  const c = row?.instances[0]?.config;
  if (c) return { slots: c.parallel ?? 0, context: c.context_length ?? 0 };
  const e = (meshView?.meshEngines ?? []).find((x) => x.node === node && x.model === model);
  return { slots: e?.slots ?? 0, context: 0 };
}

export function openTuneDialog() {
  const models = tuneModels();
  const selected = (mm.catalog.rows ?? []).find((r) => r.id === mm.selected)?.key;
  tn.model = models.includes(selected) ? selected : models[0] ?? "";
  tn.scope = "";
  tn.stage = "confirm";
  tn.nodes = new Map();
  renderTune();
  $("tn-dialog").showModal();
  // Sweeps survive tab closure; check for an existing run before offering Start.
  attachTune();
}

// Attach to active runs; offer completed reports separately because they may be stale.
async function attachTune() {
  const targets = tuneTargets();
  await Promise.all(targets.map((node) => tuneStatus(node)));
  if ([...tn.nodes.values()].some((d) => d.status?.running)) {
    tn.stage = "run";
    pollTune();
  }
  renderTune();
}

// Preserve HTTP status: 409 attaches to an active sweep, 503 means no
// supervisor, and 404 may mean an older build. fetchJSON/post discard it.
async function tuneCall(method, node, body) {
  const resp = await fetch(nodeAPI(node, "/api/v1/tune"), {
    method,
    cache: "no-store",
    ...(body ? { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : {}),
  });
  const text = await resp.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { /* Preserve the response text if it is not JSON. */ }
  return { ok: resp.ok, status: resp.status, message: data?.error?.message || text || `HTTP ${resp.status}`, data };
}

// Distinguish 404 bodies: "no live node named …" comes from the proxy
// for an offline peer; the peer's missing route indicates an older build.
function tuneNotFound(prev, message) {
  if (/no live node/.test(message)) return { error: message, misses: (prev.misses ?? 0) + 1 };
  return { unsupported: "running a build without slot tuning" };
}

async function tuneStatus(node) {
  const prev = tn.nodes.get(node) ?? {};
  try {
    const { ok, status, message, data } = await tuneCall("GET", node);
    if (ok) {
      tn.nodes.set(node, { ...prev, status: data, error: "", misses: 0 });
    } else if (status === 404) {
      tn.nodes.set(node, { ...prev, ...tuneNotFound(prev, message) });
    } else {
      tn.nodes.set(node, { ...prev, error: message, misses: (prev.misses ?? 0) + 1 });
    }
  } catch (err) {
    // Keep the last sweep state on poll failure; count misses separately
    // so an unreachable node eventually stops being polled.
    tn.nodes.set(node, { ...prev, error: err.message, misses: (prev.misses ?? 0) + 1 });
  }
}

async function startTune() {
  const targets = tuneTargets();
  if (!targets.length) return;
  tn.stage = "run";
  $("tn-error").textContent = "";
  renderTune();
  await Promise.all(targets.map(async (node) => {
    const prev = tn.nodes.get(node) ?? {};
    try {
      // Omit context to use the loaded value and slots to use the doubling sweep.
      const { status, message, data } = await tuneCall("POST", node, { model: tn.model });
      if (status === 202) {
        tn.nodes.set(node, { status: data, error: "" });
      } else if (status === 409) {
        tn.nodes.set(node, { ...prev, error: "", attached: message });
      } else if (status === 503) {
        tn.nodes.set(node, { ...prev, unmanaged: message });
      } else if (status === 404) {
        tn.nodes.set(node, { ...prev, ...tuneNotFound(prev, message) });
      } else {
        tn.nodes.set(node, { ...prev, error: message });
      }
    } catch (err) {
      tn.nodes.set(node, { ...prev, error: err.message });
    }
    renderTune();
  }));
  const started = targets.some((node) => {
    const d = tn.nodes.get(node);
    return d?.status?.running || d?.attached;
  });
  if (!started) $("tn-error").textContent = "No sweep started.";
  pollTune();
}

function pollTune() {
  clearTimeout(tn.poll);
  tn.poll = setTimeout(async () => {
    if (!$("tn-dialog").open) return;
    const targets = tuneTargets().filter((node) => {
      const d = tn.nodes.get(node);
      return !d?.unmanaged && !d?.unsupported;
    });
    await Promise.all(targets.map((node) => tuneStatus(node)));
    renderTune();
    // A missed poll does not stop a remote sweep; retry while work remains.
    const live = targets.some((node) => {
      const d = tn.nodes.get(node);
      return d?.status?.running || (d?.error && !d?.status && (d.misses ?? 0) < TUNE_MISSES);
    });
    if (live) pollTune();
  }, TUNE_POLL_MS);
}

function stopTunePoll() {
  clearTimeout(tn.poll);
  tn.poll = 0;
}

// tuner.Run restores the original slot count even when cancelled.
async function cancelTune() {
  const running = tuneTargets().filter((node) => tn.nodes.get(node)?.status?.running);
  if (!running.length) return;
  showNotice(`Stopping the sweep on ${running.join(", ")}; each node goes back on the slot count it was found with.`);
  await Promise.all(running.map(async (node) => {
    try {
      await fetchJSON(nodeAPI(node, "/api/v1/tune"), { method: "DELETE" });
    } catch (err) {
      showNotice(`Could not stop the sweep on ${node}: ${err.message}`, "error");
    }
  }));
  pollTune();
}

// Applying reloads the engine and discards resident conversations;
// require per-node confirmation.
async function applyTune(node, report) {
  const slots = report.recommended;
  if (!slots) return;
  if (!confirm(`Load ${report.model} on ${node} with ${slots} slot${slots === 1 ? "" : "s"}?\n\n`
    + `This reloads the engine: every conversation resident on it is dropped, and ${node} is unavailable while it comes back.`)) return;
  tn.nodes.set(node, { ...tn.nodes.get(node), applying: true });
  renderTune();
  showNotice(`Loading ${report.model} on ${node} with ${slots} slot${slots === 1 ? "" : "s"}… a large model can take a minute.`);
  try {
    // Load settings must be top-level: nested "settings" is silently ignored.
    // Include vision and MTP values to preserve the measured configuration
    // instead of falling back to saved defaults.
    await fetchJSON(nodeAPI(node, "/api/v1/models/load"), {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: report.model,
        context_length: report.context,
        parallel: slots,
        vision: true,
        spec_mode: "mtp",
      }),
    });
    showNotice(`${report.model} is running on ${node} with ${slots} slot${slots === 1 ? "" : "s"}`, "success");
  } catch (err) {
    showNotice(`Could not apply on ${node}: ${err.message}`, "error");
  } finally {
    tn.nodes.set(node, { ...tn.nodes.get(node), applying: false });
    renderTune();
    tick();
  }
}

function renderTune() {
  if (tn.stage === "confirm") renderTuneConfirm(); else renderTuneResults();
}

function renderTuneConfirm() {
  const body = $("tn-body");
  const actions = $("tn-actions");
  body.replaceChildren();
  actions.replaceChildren();
  $("tn-error").textContent = "";
  $("tn-sub").textContent = "Measure how many requests each node should serve at once. Nothing is applied.";

  const models = tuneModels();
  if (!models.length) {
    body.append(el("div", "empty", "No model is loaded anywhere in the mesh. Load one, then tune the nodes running it."));
    return;
  }

  body.append(el("div", "tn-group-title", "Model"));
  if (models.length === 1) {
    body.append(el("div", "tn-model", tn.model));
  } else {
    const sel = el("select", "select");
    for (const id of models) {
      const opt = el("option", null, id);
      opt.value = id;
      opt.selected = id === tn.model;
      sel.append(opt);
    }
    sel.addEventListener("change", () => {
      tn.model = sel.value;
      // A node selected for the previous model may not hold the new model.
      tn.scope = "";
      tn.nodes = new Map();
      renderTune();
      attachTune();
    });
    body.append(sel);
  }

  const all = tuneNodesFor(tn.model);
  const seg = el("div", "seg seg-sm");
  const tab = (value, label) => {
    const b = el("button", "seg-btn" + (tn.scope === value ? " active" : ""), label);
    b.type = "button";
    b.addEventListener("click", () => { tn.scope = value; tn.nodes = new Map(); renderTune(); attachTune(); });
    seg.append(b);
  };
  tab("", `Every node (${all.length})`);
  for (const node of all) tab(node, node);
  body.append(el("div", "tn-group-title", "Nodes to sweep"));
  const segRow = el("div");
  segRow.append(seg);
  body.append(segRow);

  const targets = tuneTargets();
  const list = el("div", "wl-group");
  for (const node of targets) {
    const now = tuneRunning(node, tn.model);
    const line = el("div", "tn-node");
    line.append(el("span", "tn-node-name", node === selfNode ? `${node} (this node)` : node));
    line.append(el("span", "tn-node-now", now.slots
      ? `running ${now.slots} slot${now.slots === 1 ? "" : "s"}${now.context ? ` × ${now.context.toLocaleString()} tokens` : ""}`
      : "not loaded here"));
    const d = tn.nodes.get(node);
    const state = el("span", "tn-node-state");
    if (d?.status?.running) state.append(el("span", "spinner"), "already sweeping");
    else if (d?.unsupported) state.textContent = "older build";
    else if (d?.unmanaged) state.textContent = "manages no models";
    else if (d?.error) state.textContent = "not answering";
    else if (d?.status?.report?.at) {
      state.textContent = d.status.report.recommended
        ? `last swept ${relativeTime(d.status.report.at)}: ${d.status.report.recommended} slot${d.status.report.recommended === 1 ? "" : "s"}`
        : `last swept ${relativeTime(d.status.report.at)}: no answer`;
    }
    line.append(state);
    list.append(line);
  }
  if (!targets.length) list.append(el("p", "wl-empty", `No node in the mesh is serving ${tn.model}.`));
  body.append(list);

  // Each slot count reloads the engine, interrupting service and failing
  // requests routed there during restart.
  body.append(el("p", "tn-warn",
    `Each node reloads its engine once per slot count, so ${targets.length === 1 ? "it is" : "each is"} unavailable in turn while its own sweep runs. `
    + "Requests routed to a node mid-sweep will fail. This takes several minutes per node. "
    + "Nothing is applied: every node is put back on the count it was found with, and what to run afterwards is your call."));
  body.append(el("p", "tn-note",
    "Nodes sweep at the same time — they are separate machines measuring themselves. Each starts at 1 slot and doubles "
    + "until the engine will not load or the throughput stops improving, at the context it is already loaded with. "
    + "A node that is serving requests refuses to start: tuning reloads the engine."));

  // Offer existing reports separately; they may describe an older build.
  if ([...tn.nodes.values()].some((d) => d.status?.report)) {
    const last = el("button", "btn", "Show the last results");
    last.type = "button";
    last.addEventListener("click", () => { tn.stage = "run"; renderTune(); });
    actions.append(last);
  }
  const start = el("button", "btn primary", targets.length === 1 ? `Sweep ${targets[0]}` : `Sweep ${targets.length} nodes`);
  start.type = "button";
  start.disabled = targets.length === 0;
  start.addEventListener("click", startTune);
  actions.append(start);
}

// The node reports completed rows only. Infer the current count from
// the explicit list or the doubling sequence starting at 1.
function tuneNextSlots(status) {
  const done = status?.rows?.length ?? 0;
  const given = status?.config?.slots;
  if (given?.length) return given[done] ?? 0;
  const n = 1 << done;
  return n <= TUNE_MAX_SLOTS ? n : 0;
}

function tuneCell(row) {
  if (!row) return { text: "—", cls: "muted" };
  if (row.fit) return { text: Math.round(row.aggregate_tok_s).toLocaleString(), cls: "" };
  return /out of memory|oom/i.test(row.error || "")
    ? { text: "OOM", cls: "tn-bad" }
    : { text: "failed", cls: "tn-bad" };
}

function renderTuneResults() {
  const body = $("tn-body");
  const actions = $("tn-actions");
  body.replaceChildren();
  actions.replaceChildren();

  const targets = tuneTargets();
  const running = targets.filter((node) => tn.nodes.get(node)?.status?.running);
  $("tn-sub").textContent = running.length
    ? `Sweeping ${running.join(", ")} — ${tn.model}`
    : `${tn.model} · total tokens per second with that many requests in flight`;

  // Build columns from live rows: nodes stop at different slot counts,
  // and waiting for all reports would hide progress.
  const counts = [];
  const contexts = new Map();
  for (const node of targets) {
    const st = tn.nodes.get(node)?.status;
    if (!st) continue;
    const ctxLen = st.report?.context || st.config?.context || 0;
    if (ctxLen) contexts.set(ctxLen, [...(contexts.get(ctxLen) ?? []), node]);
    for (const row of st.rows ?? []) if (!counts.includes(row.slots)) counts.push(row.slots);
  }
  counts.sort((a, b) => a - b);

  // Warn when nodes use different contexts; their throughput is not comparable.
  if (contexts.size > 1) {
    const parts = [...contexts.entries()].sort((a, b) => a[0] - b[0])
      .map(([c, nodes]) => `${c.toLocaleString()} on ${nodes.join(", ")}`);
    body.append(el("p", "tn-warn",
      `These nodes were swept at different contexts (${parts.join("; ")}), so the columns below do not compare. `
      + "Load them with the same context and sweep again to get numbers that do."));
  } else if (contexts.size === 1) {
    const [[ctxLen]] = [...contexts.entries()];
    body.append(el("p", "tn-note", `Swept at a ${ctxLen.toLocaleString()}-token context per request.`));
  }

  const scroll = el("div", "table-scroll");
  const table = el("table", "dense");
  const thead = el("thead");
  const hr = el("tr");
  hr.append(el("th", "", "Node"));
  for (const c of counts) hr.append(el("th", "num", `${c} slot${c === 1 ? "" : "s"}`));
  hr.append(el("th", "", "Recommended"), el("th", "", ""));
  thead.append(hr);
  const tbody = el("tbody");
  for (const node of targets) {
    const d = tn.nodes.get(node) ?? {};
    const st = d.status;
    const tr = el("tr");
    const name = el("td", "mono");
    name.append(node);
    tr.append(name);
    const byCount = new Map((st?.rows ?? []).map((row) => [row.slots, row]));
    for (const c of counts) {
      const cell = tuneCell(byCount.get(c));
      tr.append(el("td", `num ${cell.cls}`.trim(), cell.text));
    }

    const rec = el("td");
    if (st?.running) {
      const next = tuneNextSlots(st);
      const s = el("span", "muted");
      s.append(el("span", "spinner"), next ? `measuring ${next} slot${next === 1 ? "" : "s"}…` : "measuring…");
      rec.append(s);
    } else if (d.unsupported) {
      rec.append(el("span", "muted", "older build"));
    } else if (d.unmanaged) {
      rec.append(el("span", "muted", "manages no models"));
    } else if (!st?.report && d.error) {
      rec.append(el("span", "muted", "did not run"));
    } else if (st?.report?.recommended) {
      rec.classList.add("tn-rec");
      rec.textContent = `${st.report.recommended} slot${st.report.recommended === 1 ? "" : "s"}`;
    } else if (st?.report) {
      rec.classList.add("tn-bad");
      rec.textContent = "no answer";
    } else {
      rec.append(el("span", "muted", "starting…"));
    }
    tr.append(rec);

    const act = el("td", "actions");
    if (st?.report?.recommended && !st.running) {
      const now = tuneRunning(node, tn.model);
      if (now.slots === st.report.recommended) {
        act.append(el("span", "pill fit-yes", "already set"));
      } else {
        const apply = el("button", "btn sm accent", `Apply ${st.report.recommended}`);
        apply.title = `Load ${tn.model} on ${node} with ${st.report.recommended} slots — this reloads the engine`;
        apply.disabled = Boolean(d.applying);
        apply.addEventListener("click", () => applyTune(node, st.report));
        act.append(apply);
      }
    }
    tr.append(act);
    tbody.append(tr);
  }
  table.append(thead, tbody);
  scroll.append(table);
  if (counts.length) body.append(scroll);
  else body.append(el("div", "empty", running.length ? "The first row is being measured; a reload plus a generation takes a minute or two." : "No node completed a row; there is nothing to compare."));

  for (const node of targets) {
    const d = tn.nodes.get(node) ?? {};
    const rep = d.status?.report;
    const lines = [];
    if (rep?.why) lines.push(["tn-why-line", rep.why]);
    if (rep?.stopped_because) lines.push(["tn-why-line", `Stopped: ${rep.stopped_because}`]);
    for (const w of rep?.warnings ?? []) lines.push(["tn-why-warn", w]);
    if (d.status?.error) lines.push(["tn-why-warn", d.status.error]);
    if (d.attached) lines.push(["tn-why-line", d.attached]);
    if (d.unmanaged) lines.push(["tn-why-line", d.unmanaged]);
    if (d.unsupported) lines.push(["tn-why-line", `${d.unsupported} — upgrade ModelFabric there to include it in a sweep.`]);
    if (d.error && !d.status) lines.push(["tn-why-warn", `${node} is not answering: ${d.error}. The sweep runs on the node, so it is not necessarily stopped.`]);
    if (!lines.length) continue;
    const block = el("div", "tn-why");
    block.append(el("div", "tn-why-node", node));
    for (const [cls, text] of lines) block.append(el("div", cls, text));
    body.append(block);
  }

  // Highlight restore failures: those nodes remain changed after the sweep.
  const done = targets.map((node) => [node, tn.nodes.get(node)?.status?.report]).filter(([, rep]) => rep);
  const restored = done.filter(([, rep]) => rep.restored_to > 0).map(([node, rep]) => `${node} (${rep.restored_to})`);
  const stranded = done.filter(([, rep]) => !(rep.restored_to > 0)).map(([node]) => node);
  if (stranded.length) {
    body.append(el("p", "tn-warn",
      `Check ${stranded.join(", ")}: the sweep could not put ${stranded.length === 1 ? "it" : "them"} back on the slot count found there. `
      + `Whatever the last row of the sweep loaded is what ${stranded.length === 1 ? "is" : "are"} running now.`));
  }
  if (restored.length) {
    body.append(el("p", "tn-note",
      `Nothing was changed — back on the slot count each was found with: ${restored.join(", ")}.`));
  }

  const cmds = done.filter(([, rep]) => rep.recommended);
  if (cmds.length) {
    body.append(el("div", "tn-group-title", "To apply by hand"));
    for (const [node, rep] of cmds) {
      const line = el("div", "tn-cmd");
      line.append(el("span", "tn-cmd-node mono", node));
      line.append(el("code", null, `mfsh load ${rep.model} -context ${rep.context} -parallel ${rep.recommended}`));
      body.append(line);
    }
    body.append(el("p", "tn-note", "Each runs on its own node."));
  }

  if (running.length) {
    const stop = el("button", "btn danger", running.length === 1 ? "Stop the sweep" : "Stop all sweeps");
    stop.type = "button";
    stop.title = "Stopping puts each node back on the slot count the sweep found";
    stop.addEventListener("click", cancelTune);
    actions.append(stop);
  } else {
    const again = el("button", "btn", "Sweep again");
    again.type = "button";
    again.addEventListener("click", () => { tn.stage = "confirm"; renderTune(); });
    actions.append(again);
  }
}

$("tn-open").addEventListener("click", openTuneDialog);
$("tn-close").addEventListener("click", () => $("tn-dialog").close());
// Closing stops polling only; reopening attaches to the node's background sweep.
$("tn-dialog").addEventListener("close", stopTunePoll);
