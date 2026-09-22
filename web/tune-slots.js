import { post, showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, mm, nodeAPI, selfNode } from "./my-models.js";
import { tick } from "./polling.js";
import { el, meshView } from "./rendering.js";
import { relativeTime } from "./ui-model.js";

/* ---------- Tune slots: how many requests each node should serve at once ---------- */

// Slots (`--parallel`) are set per node, and the number is almost always the
// same on every node because nothing ever offered a reason for a different one.
// On a mixed fleet that is wrong in both directions at once: measured here at a
// 65536-token context the best count was 2 on an RTX 5090, 4 on a 48GB card and
// 1 on an Apple Silicon Mac. On two slots each the Mac absorbed 49% of the
// fleet's prefill while producing a twelfth of the output — it was advertising
// capacity it could not honour, and every router in front of it believed the
// advertisement.
//
// The answer cannot be calculated, so this measures it: the dashboard's reading
// of `mfsh tune -fleet`. Each node sweeps itself through its own /api/v1/tune,
// because a slot count is a fact about one machine and nothing about a peer's
// GPU can be read from here. Nothing is applied — every node is put back on the
// count it was found with, and the table ends with the command per node.

// How often each node is asked for progress. One row is a reload plus a
// generation, tens of seconds at best, so this is frequent enough to look live
// and far from busy. The interval the CLI's fleet sweep polls at.
const TUNE_POLL_MS = 3000;

// The doubling sweep's ceiling, mirroring internal/tuner's sweepMax. Only used
// to say which count a node is measuring now, which the node does not report.
const TUNE_MAX_SLOTS = 32;

// How many polls a node may miss before it stops being followed. A sweep does
// survive a peer going quiet for a poll or two, so a single miss is not news;
// a node that has never answered is not going to start.
const TUNE_MISSES = 5;

export const tn = {
  model: "",         // what to sweep; the nodes are the ones serving it
  scope: "",         // "" = every node serving the model, otherwise that one
  stage: "confirm",  // "confirm" until a sweep is running or one has finished
  // node -> { status, error, misses, unmanaged, unsupported, attached, applying }
  nodes: new Map(),
  poll: 0,
};

// Models with an engine somewhere in the mesh. A model nobody is running has no
// context to sweep at and no node to sweep on: the node would spend the sweep
// loading weights it was not asked about.
function tuneModels() {
  return (meshView?.models ?? []).map((m) => m.id);
}

// The nodes serving a model, read from the mesh view this page already polls
// rather than asked for again. Same rule as the CLI's fleetNodes.
function tuneNodesFor(model) {
  return [...((meshView?.models ?? []).find((m) => m.id === model)?.nodes ?? [])].sort();
}

function tuneTargets() {
  const all = tuneNodesFor(tn.model);
  return tn.scope ? all.filter((n) => n === tn.scope) : all;
}

// What a node is running the model with right now, from what My Models already
// fetched. The context belongs in the confirmation: a sweep defaults to the
// context the model is loaded with, and throughput at 32k says nothing about
// the 64k someone actually runs.
function tuneRunning(node, model) {
  const row = (mm.catalog.rows ?? []).find((r) => r.node === node && r.key === model);
  const c = row?.instances[0]?.config;
  if (c) return { slots: c.parallel ?? 0, context: c.context_length ?? 0 };
  const e = (meshView?.meshEngines ?? []).find((x) => x.node === node && x.model === model);
  return { slots: e?.slots ?? 0, context: 0 };
}

export function openTuneDialog() {
  const models = tuneModels();
  // The selected row's model if one is selected and loaded somewhere: the
  // operator was already looking at it.
  const selected = (mm.catalog.rows ?? []).find((r) => r.id === mm.selected)?.key;
  tn.model = models.includes(selected) ? selected : models[0] ?? "";
  tn.scope = "";
  tn.stage = "confirm";
  tn.nodes = new Map();
  renderTune();
  $("tn-dialog").showModal();
  // A sweep runs on the node and survives this tab closing, so the first thing
  // to do is ask rather than assume nothing is running. Starting one while a
  // sweep is already in flight would be refused anyway — but the operator would
  // have clicked Start to find that out.
  attachTune();
}

// Ask every node serving the model what it is doing. Anything already running
// puts the dialog straight into its results; a finished report is offered
// rather than shown, because it may be hours old.
async function attachTune() {
  const targets = tuneTargets();
  await Promise.all(targets.map((node) => tuneStatus(node)));
  if ([...tn.nodes.values()].some((d) => d.status?.running)) {
    tn.stage = "run";
    pollTune();
  }
  renderTune();
}

// These endpoints are read by status code as much as by body: 409 is a sweep
// already running and therefore something to attach to, 503 a node that manages
// no models, 404 a node on a build from before slot tuning existed. None of
// those is a failure to report as one, and fetchJSON and post() both throw the
// status away — so this reads the response itself, the way importPreset does.
async function tuneCall(method, node, body) {
  const resp = await fetch(nodeAPI(node, "/api/v1/tune"), {
    method,
    cache: "no-store",
    ...(body ? { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : {}),
  });
  const text = await resp.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { /* keep the text */ }
  return { ok: resp.ok, status: resp.status, message: data?.error?.message || text || `HTTP ${resp.status}`, data };
}

// Two different 404s reach this code, and they must not read the same. The node
// proxy answers "no live node named …" when a peer has left the mesh between
// polls; the peer's own router answers 404 when it is running a build from
// before slot tuning existed. One is offline, the other is upgradeable, and
// only the body tells them apart.
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
    // A poll that fails is not a sweep that failed. Keep whatever was last
    // read, and say that this node is not answering rather than dropping it.
    // The misses are counted so a node that is simply gone stops being polled
    // for as long as the dialog stays open.
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
      // Model only: the node sweeps at the context it is already loaded with,
      // and an empty slot list is the doubling sweep — asking the operator
      // which counts to try is asking for part of the answer they ran this to
      // get, and the ceiling is a fact about the machine rather than a setting.
      const { status, message, data } = await tuneCall("POST", node, { model: tn.model });
      if (status === 202) {
        tn.nodes.set(node, { status: data, error: "" });
      } else if (status === 409) {
        // Already sweeping. The poll below picks it up, so this is a note
        // rather than a failure.
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
  // Every node refused, so there is no table coming and no row to carry the
  // reason. The per-node reasons are below; this says the thing the operator
  // came back to the screen for.
  const started = targets.some((node) => {
    const d = tn.nodes.get(node);
    return d?.status?.running || d?.attached;
  });
  if (!started) $("tn-error").textContent = "No sweep started.";
  pollTune();
}

// One chained timer for the whole fleet rather than one per node: a slow peer
// then delays only the next round, and a closed dialog stops everything.
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
    // Keep polling while anything is running. A node that is not answering is
    // followed too: the sweep is running there whether or not the tailnet hop
    // worked this second.
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

// Stopping puts the node back: tuner.Run restores the count it found even on a
// cancelled sweep, so this leaves the node as it was rather than on whichever
// row was in flight.
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

// Applying is a reload: it drops every conversation resident on that engine,
// and whether now is the moment is not something this tool can know. So it is
// per node, on a click, after a confirmation naming what happens.
async function applyTune(node, report) {
  const slots = report.recommended;
  if (!slots) return;
  if (!confirm(`Load ${report.model} on ${node} with ${slots} slot${slots === 1 ? "" : "s"}?\n\n`
    + `This reloads the engine: every conversation resident on it is dropped, and ${node} is unavailable while it comes back.`)) return;
  tn.nodes.set(node, { ...tn.nodes.get(node), applying: true });
  renderTune();
  showNotice(`Loading ${report.model} on ${node} with ${slots} slot${slots === 1 ? "" : "s"}… a large model can take a minute.`);
  try {
    // The settings go inline in the load body, not nested under a "settings"
    // key: a nested object is accepted and silently ignored, and the load then
    // falls back to the model's saved defaults — which is the slot count this
    // sweep was run to replace. Vision and MTP speculation are stated rather
    // than left out for the same reason: a saved default for either would
    // otherwise change a second thing along with the slot count, and then what
    // is running is not what was measured.
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

  // Named either way. A dialog about to take three engines down that does not
  // say which model it is sweeping is asking to be confirmed blind — but with
  // one model in the mesh a picker is a control with one option, so that case
  // is a statement rather than a choice.
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
      // The nodes are a property of the model, so a node picked for the last
      // one may not hold this one.
      tn.scope = "";
      tn.nodes = new Map();
      renderTune();
      attachTune();
    });
    body.append(sel);
  }

  const all = tuneNodesFor(tn.model);
  // Every node by default: one node's answer is not the fleet's, and the
  // comparison is what says whether the uniform count is costing anything.
  // A single node is still worth offering — one machine changed, one sweep.
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
  // Wrapped: the dialog body is a grid, and a bare segmented control stretched
  // to the full width read as a toolbar rather than a choice.
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

  // The warning is the point of this stage, and every clause of it has cost
  // somebody a run: a sweep reloads the engine once per slot count, which takes
  // the node out of service each time, and anything routed to it meanwhile
  // fails with the engine restarting.
  body.append(el("p", "tn-warn",
    `Each node reloads its engine once per slot count, so ${targets.length === 1 ? "it is" : "each is"} unavailable in turn while its own sweep runs. `
    + "Requests routed to a node mid-sweep will fail. This takes several minutes per node. "
    + "Nothing is applied: every node is put back on the count it was found with, and what to run afterwards is your call."));
  body.append(el("p", "tn-note",
    "Nodes sweep at the same time — they are separate machines measuring themselves. Each starts at 1 slot and doubles "
    + "until the engine will not load or the throughput stops improving, at the context it is already loaded with. "
    + "A node that is serving requests refuses to start: tuning reloads the engine."));

  // A report already on a node is worth reading even when nothing is running,
  // but it may be hours old and measured on a different build — so it is
  // offered rather than shown.
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

// Which count a node is measuring now. The node publishes the rows it has
// finished, not the one in flight, so this is derived: an explicit list is
// counted through, and an empty one is the doubling sweep from 1.
function tuneNextSlots(status) {
  const done = status?.rows?.length ?? 0;
  const given = status?.config?.slots;
  if (given?.length) return given[done] ?? 0;
  const n = 1 << done;
  return n <= TUNE_MAX_SLOTS ? n : 0;
}

// One measurement, or why there is none. A column never tried and a column that
// failed mean different things: "not reached" against "this machine cannot".
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

  // The union of the counts actually tried, live rather than from the reports:
  // nodes stop at different places — a 5090 runs out of memory two steps before
  // a 48GB card does — so a fixed set of columns would either invent rows or
  // hide them, and a table that waits for every report shows nothing for
  // minutes.
  const counts = [];
  const contexts = new Map(); // context -> nodes swept at it
  for (const node of targets) {
    const st = tn.nodes.get(node)?.status;
    if (!st) continue;
    const ctxLen = st.report?.context || st.config?.context || 0;
    if (ctxLen) contexts.set(ctxLen, [...(contexts.get(ctxLen) ?? []), node]);
    for (const row of st.rows ?? []) if (!counts.includes(row.slots)) counts.push(row.slots);
  }
  counts.sort((a, b) => a - b);

  // Each node sweeps at the context it is loaded with, so a fleet set up by
  // hand can be swept at two contexts at once — and throughput at 32k is not
  // comparable to throughput at 64k. Said, rather than presented as if the
  // columns lined up.
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

    // The recommendation, or the reason there is not one yet. A node still
    // sweeping says which count it is on: a row takes minutes, and a panel
    // that looks frozen is indistinguishable from one that is stuck.
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

  // Why, per node, because the number alone does not say whether the sweep
  // found a ceiling or a knee, and those call for different next steps.
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

  // Whether anything was left changed. Restoring is the sweep's own last act,
  // so the normal case is a reassurance — but a node it could not put back is
  // the one outcome that leaves work to do, and it is said first and in amber
  // rather than folded into the same sentence.
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

  // The commands, beside the buttons rather than instead of them: an operator
  // reloading an engine at 3am wants the line they can paste into the node's
  // own shell, and loading is not something one node does to another.
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
// Closing stops the polling, not the sweep: the node runs it in the background,
// and reopening the dialog attaches to whatever is still going.
$("tn-dialog").addEventListener("close", stopTunePoll);
