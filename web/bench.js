// The Benchmark page: the standard suite on a chosen node (internal/bench),
// and Tune slots beside it. A run is the node's own, through its
// /api/v1/bench, so it survives this tab closing; the page polls it.

import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, nodeAPI } from "./my-models.js";
import { el } from "./rendering.js";
import { openCluster } from "./bench-cluster.js";
import { openTuneDialog } from "./tune-slots.js";
import { benchTables } from "./ui-model.js";

const PP = [1024, 4096, 8192, 16384, 32768, 65536];
const PP_DEFAULT = new Set([1024, 4096, 8192, 16384, 32768]);
const BATCH = [2, 4, 8];

const bn = { node: "", model: "", prompts: "prose", poll: null, last: null, built: false, wasRunning: false };

export function checks(box, values, on, label) {
  box.replaceChildren();
  for (const v of values) {
    const l = el("label", "bn-check");
    const c = el("input");
    c.type = "checkbox";
    c.value = String(v);
    c.checked = on(v);
    l.append(c, label(v));
    box.append(l);
  }
}

export const picked = (box) => [...box.querySelectorAll("input:checked")].map((c) => Number(c.value));

function build() {
  if (bn.built) return;
  bn.built = true;
  checks($("bn-pp"), PP, (v) => PP_DEFAULT.has(v), (v) => `pp${v}`);
  checks($("bn-batch"), BATCH, () => true, (v) => `${v}x batch`);
  for (const b of $("bn-prompts").querySelectorAll(".seg-btn")) {
    b.addEventListener("click", () => {
      bn.prompts = b.dataset.v;
      for (const x of $("bn-prompts").querySelectorAll(".seg-btn")) x.classList.toggle("active", x === b);
    });
  }
  $("bn-node").addEventListener("change", (e) => { bn.node = e.target.value; void loadNode(); });
  $("bn-model").addEventListener("change", (e) => { bn.model = e.target.value; });
  $("bn-run").addEventListener("click", run);
  $("bn-stop").addEventListener("click", stop);
  for (const t of document.querySelectorAll("[data-bn-tab]")) {
    t.addEventListener("click", () => {
      for (const x of document.querySelectorAll("[data-bn-tab]")) {
        const on = x === t;
        x.classList.toggle("active", on);
        x.setAttribute("aria-selected", String(on));
      }
      $("bn-bench").hidden = t.dataset.bnTab !== "bench";
      $("bn-tune").hidden = t.dataset.bnTab !== "tune";
      $("bn-cluster").hidden = t.dataset.bnTab !== "cluster";
      if (t.dataset.bnTab === "cluster") void openCluster();
    });
  }
  $("bn-tune-open").addEventListener("click", openTuneDialog);
}

export async function openBench() {
  build();
  let mesh;
  try {
    mesh = await fetchJSON("/z/mesh");
  } catch (err) {
    // Opened while the node was restarting, the page said "Failed to fetch"
    // and stayed that way, its Node and Model boxes empty, until reloaded.
    // The node comes back in a second or two, so ask again while the page
    // is still the one on screen.
    $("bn-status").textContent = `Could not read the mesh (${err.message}); retrying…`;
    clearTimeout(bn.retry);
    bn.retry = setTimeout(() => { if (!document.querySelector('section[data-view="bench"]')?.hidden) void openBench(); }, 2000);
    return;
  }
  $("bn-status").textContent = "";
  const nodes = [mesh.self?.node, ...(mesh.peers ?? []).filter((p) => p.alive).map((p) => p.node)].filter(Boolean);
  const sel = $("bn-node");
  sel.replaceChildren(...nodes.map((n) => {
    const o = el("option", null, n === (mesh.self?.node) ? `${n} (this node)` : n);
    o.value = n;
    return o;
  }));
  if (!nodes.includes(bn.node)) bn.node = nodes[0] ?? "";
  sel.value = bn.node;
  await loadNode();
}

// The models loaded on the node: a run restores the load it found, so it
// benchmarks what is loaded rather than loading something itself.
async function loadNode() {
  const sel = $("bn-model");
  sel.replaceChildren();
  let api;
  try {
    api = await fetchJSON(nodeAPI(bn.node, "/api/v1/models"));
  } catch (err) {
    setStatus(`Could not read ${bn.node}'s models: ${err.message}`);
    return;
  }
  // An embedding model generates nothing, so there is nothing here to time.
  const loaded = (api.models ?? []).filter((m) => (m.loaded_instances ?? []).length && m.type !== "embedding");
  for (const m of loaded) {
    const o = el("option", null, m.key);
    o.value = m.key;
    sel.append(o);
  }
  if (!loaded.length) {
    const o = el("option", null, "nothing loaded on this node");
    o.value = "";
    sel.append(o);
  }
  if (!loaded.some((m) => m.key === bn.model)) bn.model = loaded[0]?.key ?? "";
  sel.value = bn.model;
  $("bn-run").disabled = !bn.model;
  setStatus(bn.model ? "" : "Load a model on this node first (My Models), then benchmark it.");
  await poll(); // a run already going, or the last one, on this node
}

function lockForm(locked) {
  for (const c of document.querySelectorAll("#bn-node, #bn-model, #bn-reps, #bn-pp input, #bn-batch input, #bn-prompts .seg-btn")) {
    c.disabled = locked;
  }
}

function showModel(model) {
  const sel = $("bn-model");
  if (![...sel.options].some((o) => o.value === model)) {
    const o = el("option", null, model);
    o.value = model;
    sel.replaceChildren(o);
  }
  sel.value = bn.model = model;
}

function setStatus(msg) {
  $("bn-status").textContent = msg;
}

async function run() {
  const pp = picked($("bn-pp"));
  const batch = picked($("bn-batch"));
  if (!pp.length && !batch.length) {
    setStatus("Pick at least one test.");
    return;
  }
  const body = { model: bn.model, prompts: bn.prompts, pp, batch, reps: Number($("bn-reps").value) };
  try {
    await fetchJSON(nodeAPI(bn.node, "/api/v1/bench"), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
  } catch (err) {
    setStatus(benchError(err));
    return;
  }
  await poll();
}

// A node older than the benchmark answers "no such endpoint" to everything
// about it. The Run button showed that bare, which reads as a bug in this
// dashboard instead of a peer that needs the current build.
export function benchError(err) {
  return /no such endpoint/i.test(err.message)
    ? `${bn.node} runs a build without benchmarking; deploy the current one there.`
    : err.message;
}

async function stop() {
  try {
    await fetchJSON(nodeAPI(bn.node, "/api/v1/bench"), { method: "DELETE" });
    setStatus("Stopping; the engine is being put back.");
  } catch (err) {
    setStatus(benchError(err));
  }
}

async function poll() {
  clearTimeout(bn.poll);
  let st;
  try {
    st = await fetchJSON(nodeAPI(bn.node, "/api/v1/bench"));
  } catch (err) {
    setStatus(benchError(err));
    $("bn-results").replaceChildren();
    // Nothing to run on a node that cannot benchmark: clicking only repeated
    // the same message.
    if (/no such endpoint/i.test(err.message)) $("bn-run").disabled = true;
    return;
  }
  $("bn-run").hidden = st.running;
  $("bn-stop").hidden = !st.running;
  // A run reloads the engine for each phase, so the node's loaded models are
  // empty for a moment between them. Read then, the Model box said "nothing
  // loaded on this node" for the rest of a run that was benchmarking the
  // 27B. While a run goes on the form shows that run's model and is locked —
  // it cannot change mid-run — and the list is read again once it ends.
  lockForm(st.running);
  if (st.running && st.report?.model) showModel(st.report.model);
  const ended = bn.wasRunning && !st.running;
  bn.wasRunning = st.running;
  if (ended) {
    void loadNode();
    return;
  }
  if (st.running) {
    const r = st.report;
    // From the run's start, not took_s: the node updates that only when a
    // phase changes, so a two-minute 32K test showed the same number
    // throughout (and the page read a field that does not exist: "0s").
    const started = Date.parse(r?.at ?? "");
    const secs = Number.isFinite(started) ? Math.max(0, Math.round((Date.now() - started) / 1000)) : Math.round(r?.took_s ?? 0);
    setStatus(r?.phase ? `${secs}s · ${r.phase}` : "starting…");
    bn.poll = setTimeout(poll, 1500);
  } else if (st.error) {
    setStatus(`The last run failed: ${st.error}`);
  } else if (bn.model) {
    setStatus("");
  }
  renderResults(st.report, st.running);
}

export function table(title, hint, t, opts = {}) {
  const panel = el("div", "panel bn-panel");
  const head = el("div", "panel-head");
  head.append(el("span", null, title));
  if (hint) head.append(el("span", "hint", hint));
  panel.append(head);
  if (!t.rows.length) {
    panel.append(el("div", "empty", opts.empty || "Not run."));
    return panel;
  }
  const tbl = el("table", "bn-table");
  const tr = el("tr");
  for (const h of t.head) tr.append(el("th", null, h));
  const thead = el("thead");
  thead.append(tr);
  const tbody = el("tbody");
  for (const r of t.rows) {
    const row = el("tr");
    if (r.error) {
      row.append(el("td", "mono", r.cells[0]));
      const td = el("td", "err-text", r.error);
      td.colSpan = t.head.length - 1;
      row.append(td);
    } else {
      r.cells.forEach((c, i) => {
        // The generation column is the headline, and a speedup worth having
        // is marked, as on the standard layout.
        const cls = i === 0 ? "mono" : (opts.bold === i ? "num strong" : (opts.speedup === i && r.speedup >= 2 ? "num good" : "num"));
        row.append(el("td", cls, c));
      });
    }
    tbody.append(row);
  }
  tbl.append(thead, tbody);
  panel.append(tbl);
  return panel;
}

function renderResults(rep, running) {
  const box = $("bn-results");
  const t = benchTables(rep);
  if (!t || (!t.single.rows.length && !t.same.rows.length && !t.diff.rows.length && !running)) {
    box.replaceChildren();
    return;
  }
  bn.last = rep;
  const out = [
    table("Single request results", `${rep.model} on ${rep.node}`, t.single, { bold: 4, empty: running ? "Running…" : "Not run." }),
    table("Continuous batching — same prompt", `pp${t.batchPP} / tg${t.tg} · one prompt, shared`, t.same, { bold: 1, speedup: 2, empty: running ? "Waiting for the prompt sweep…" : "Not run." }),
    table("Continuous batching — different prompts", `pp${t.batchPP} / tg${t.tg} · nothing shared`, t.diff, { bold: 1, speedup: 2, empty: running ? "Waiting…" : "Not run." }),
  ];
  for (const n of t.notes) out.push(el("div", "bn-warn", n));
  if (t.partial) out.push(el("div", "bn-warn", "Stopped before the end: these are partial results."));

  // What a reader needs to repeat it, and the command that does.
  const repro = el("div", "panel bn-panel");
  const rh = el("div", "panel-head");
  rh.append(el("span", null, "Reproduce this run"));
  repro.append(rh);
  const kv = el("div", "bn-kv");
  for (const [k, v] of t.meta) kv.append(el("span", "bn-k", k), el("span", "bn-v", v));
  repro.append(kv);
  if (t.command) {
    const cmd = el("div", "bn-cmd");
    cmd.append(el("code", null, t.command), copyButton("Copy", () => t.command));
    repro.append(cmd);
  }
  out.push(repro);

  if (!running) {
    const text = el("div", "panel bn-panel");
    const th = el("div", "panel-head");
    th.append(el("span", null, "Results as text"), el("span", "hint", "to paste into an issue or a chat"));
    const actions = el("span", "bn-text-actions");
    actions.append(copyButton("Copy", async () => (await fetch(nodeAPI(bn.node, "/api/v1/bench?format=text"))).text()),
      downloadButton(rep));
    th.append(actions);
    text.append(th);
    // The text itself, as the CLI prints it: the same renderer, on the node.
    const pre = el("pre", "bn-pre", "…");
    text.append(pre);
    fetch(nodeAPI(bn.node, "/api/v1/bench?format=text")).then((r) => (r.ok ? r.text() : "")).then((t) => { pre.textContent = t; });
    out.push(text);
    out.push(metricsReference());
  }
  box.replaceChildren(...out);
}

export function copyButton(label, getText) {
  const b = el("button", "btn sm", label);
  b.type = "button";
  b.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(await getText());
      showNotice("Copied", "success");
    } catch (err) {
      showNotice(`Copy failed: ${err.message}`, "error");
    }
  });
  return b;
}

function downloadButton(rep) {
  const b = el("button", "btn sm", "Download JSON");
  b.type = "button";
  b.addEventListener("click", () => {
    const blob = new Blob([JSON.stringify(rep, null, 2)], { type: "application/json" });
    const a = el("a");
    a.href = URL.createObjectURL(blob);
    a.download = `bench-${rep.node}-${String(rep.model).replace(/[^A-Za-z0-9._-]+/g, "_")}-${String(rep.at).slice(0, 19).replace(/[:T]/g, "")}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  });
  return b;
}

const METRICS = [
  ["TTFT", "Time to first token: from sending the request to the first token arriving, as the client saw it. Includes reading the prompt."],
  ["TPOT", "Time per output token, after the first: the engine's generation time over the tokens it generated."],
  ["pp TPS", "Prompt tokens read per second. Tokens served from the prompt cache are not counted: they cost nothing."],
  ["tg TPS", "Tokens generated per second. In a batch, all requests' tokens over the time from the first token anywhere to the last."],
  ["E2E Latency", "End to end: from sending to the last token, as the client saw it. In a batch, until the last request finished."],
  ["Throughput", "Prompt and generated tokens together, over the end-to-end time."],
  ["Speedup", "A batch's tg TPS over one request's, on the same load."],
  ["Cached", "The share of prompt tokens reused from the prompt cache, as the engine reported it."],
  ["Peak Mem", "The engine process's own GPU memory at its highest during the test, from nvidia-smi. Not the whole GPU's: other work on the card is not counted."],
];

export function metricsReference() {
  const d = el("details", "panel bn-panel bn-metrics");
  d.append(el("summary", "panel-head", "Metrics reference"));
  const kv = el("div", "bn-kv");
  for (const [k, v] of METRICS) kv.append(el("span", "bn-k", k), el("span", "bn-v", v));
  d.append(kv);
  return d;
}

