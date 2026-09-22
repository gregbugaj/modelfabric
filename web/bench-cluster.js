// The Benchmark page's third tab: the cluster benchmark (internal/bench,
// RunCluster). Requests go through this node's own front door and are placed
// as an app's are, so it measures the whole setup rather than one machine.
// The run is held by this node, through /api/v1/bench/cluster, so it survives
// this tab closing; the page polls it.

import { $ } from "./core.js";
import { checks, copyButton, picked, table } from "./bench.js";
import { fetchJSON } from "./my-models.js";
import { el } from "./rendering.js";
import { clusterTables } from "./ui-model.js";

const PP = [1024, 4096, 16384, 32768];
const PP_DEFAULT = new Set([1024, 4096, 16384]);
const CONC = [1, 2, 4, 8, 16, 32];
const CONC_DEFAULT = new Set([1, 2, 4, 8, 16]);
const API = "/api/v1/bench/cluster";

const bc = { model: "", prompts: "prose", holders: {}, poll: null, built: false };

function build() {
  if (bc.built) return;
  bc.built = true;
  checks($("bc-pp"), PP, (v) => PP_DEFAULT.has(v), (v) => `pp${v}`);
  checks($("bc-conc"), CONC, (v) => CONC_DEFAULT.has(v), (v) => `${v} at once`);
  for (const b of $("bc-prompts").querySelectorAll(".seg-btn")) {
    b.addEventListener("click", () => {
      bc.prompts = b.dataset.v;
      for (const x of $("bc-prompts").querySelectorAll(".seg-btn")) x.classList.toggle("active", x === b);
    });
  }
  $("bc-model").addEventListener("change", (e) => { bc.model = e.target.value; bc.picked = true; showHolders(); });
  $("bc-run").addEventListener("click", run);
  $("bc-stop").addEventListener("click", stop);
}

// Every model loaded anywhere in the mesh, with the nodes holding it: the
// run is through the front door, so a model need not be loaded here.
export async function openCluster() {
  build();
  let mesh;
  try {
    mesh = await fetchJSON("/z/mesh");
  } catch (err) {
    // As on the node benchmark: a node that is restarting is back in a moment.
    setStatus(`Could not read the mesh (${err.message}); retrying…`);
    clearTimeout(bc.retry);
    bc.retry = setTimeout(() => { if (!$("bn-cluster").hidden && !document.querySelector('section[data-view="bench"]')?.hidden) void openCluster(); }, 2000);
    return;
  }
  $("bc-door").textContent = `through ${mesh.self?.node ?? "this node"}'s front door, to every node holding the model`;
  const holders = {};
  const add = (node, models) => {
    for (const m of models ?? []) (holders[m] ??= []).push(node);
  };
  add(mesh.self?.node, mesh.self?.models);
  for (const p of mesh.peers ?? []) if (p.alive) add(p.node, p.models);
  bc.holders = holders;
  const models = Object.keys(holders).sort();
  const sel = $("bc-model");
  sel.replaceChildren(...models.map((m) => {
    const o = el("option", null, m);
    o.value = m;
    return o;
  }));
  if (!models.length) {
    const o = el("option", null, "nothing loaded in the mesh");
    o.value = "";
    sel.append(o);
  }
  // An embedding model sorts first by name and generates nothing: the box
  // opened on it, above results for the model that had actually been run.
  if (!models.includes(bc.model)) bc.model = models.find((m) => !/embed/i.test(m)) ?? models[0] ?? "";
  sel.value = bc.model;
  $("bc-run").disabled = !bc.model;
  setStatus(bc.model ? "" : "Load a model on one or more nodes first (My Models), then benchmark the cluster.");
  showHolders();
  await poll();
}

function showHolders() {
  const nodes = bc.holders[bc.model] ?? [];
  $("bc-holders").replaceChildren(...(nodes.length
    ? [el("span", null, "Held by"), ...nodes.map((n) => el("span", "bc-holder", n))]
    : []));
}

function setStatus(msg) {
  $("bc-status").textContent = msg;
}

function lockForm(locked) {
  for (const c of document.querySelectorAll("#bc-model, #bc-reps, #bc-pp input, #bc-conc input, #bc-prompts .seg-btn")) {
    c.disabled = locked;
  }
}

async function run() {
  const pp = picked($("bc-pp"));
  const concurrency = picked($("bc-conc"));
  if (!concurrency.length) {
    setStatus("Pick at least one level of requests at once.");
    return;
  }
  const body = { model: bc.model, prompts: bc.prompts, pp, concurrency, reps: Number($("bc-reps").value) };
  try {
    await fetchJSON(API, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  } catch (err) {
    setStatus(err.message);
    return;
  }
  await poll();
}

async function stop() {
  try {
    await fetchJSON(API, { method: "DELETE" });
    setStatus("Stopping; requests already sent finish on their engines.");
  } catch (err) {
    setStatus(err.message);
  }
}

async function poll() {
  clearTimeout(bc.poll);
  let st;
  try {
    st = await fetchJSON(API);
  } catch (err) {
    setStatus(/no such endpoint/i.test(err.message)
      ? "This node runs a build without the cluster benchmark; update it."
      : err.message);
    $("bc-run").disabled = true;
    return;
  }
  $("bc-run").hidden = st.running;
  $("bc-stop").hidden = !st.running;
  lockForm(st.running);
  if (st.running) {
    const r = st.report;
    if (r?.model && r.model !== bc.model) {
      bc.model = r.model;
      $("bc-model").value = r.model;
      showHolders();
    }
    const started = Date.parse(r?.at ?? "");
    const secs = Number.isFinite(started) ? Math.max(0, Math.round((Date.now() - started) / 1000)) : 0;
    setStatus(r?.phase ? `${secs}s · ${r.phase}` : "starting…");
    bc.poll = setTimeout(poll, 1500);
  } else if (st.error) {
    setStatus(`The last run failed: ${st.error}`);
  } else if (bc.model) {
    setStatus("");
  }
  // Until a model is chosen by hand, the box shows the one the results below
  // are for.
  if (!st.running && !bc.picked && st.report?.model && bc.holders[st.report.model] && st.report.model !== bc.model) {
    bc.model = st.report.model;
    $("bc-model").value = bc.model;
    showHolders();
  }
  render(st.report, st.running);
}

function render(rep, running) {
  const box = $("bc-results");
  const t = clusterTables(rep);
  if (!t || (!t.single.rows.length && !t.load.rows.length && !t.shared.rows.length && !running)) {
    box.replaceChildren();
    return;
  }
  const where = `${rep.model} through ${rep.entry}'s front door · ${rep.routing}`;
  const waiting = running ? "Waiting…" : "Not run.";
  const out = [
    table("One request at a time", where, t.single, { bold: 3, empty: running ? "Running…" : "Not run." }),
    table("The cluster under load: each request its own prompt", `pp${t.loadPP} / tg${t.tg} · nothing shared`, t.load, { bold: 1, speedup: 2, empty: waiting }),
    table("The cluster under load: one prompt shared", `pp${t.loadPP} / tg${t.tg} · as a shared system prompt is`, t.shared, { bold: 1, speedup: 2, empty: waiting }),
  ];
  for (const n of t.notes) out.push(el("div", "bn-warn", n));
  if (t.partial) out.push(el("div", "bn-warn", "Stopped before the end: these are partial results."));

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
    actions.append(copyButton("Copy", async () => (await fetch(`${API}?format=text`)).text()), downloadButton(rep));
    th.append(actions);
    text.append(th);
    const pre = el("pre", "bn-pre", "…");
    text.append(pre);
    fetch(`${API}?format=text`).then((r) => (r.ok ? r.text() : "")).then((s) => { pre.textContent = s; });
    out.push(text);
  }
  box.replaceChildren(...out);
}

function downloadButton(rep) {
  const b = el("button", "btn sm", "Download JSON");
  b.type = "button";
  b.addEventListener("click", () => {
    const blob = new Blob([JSON.stringify(rep, null, 2)], { type: "application/json" });
    const a = el("a");
    a.href = URL.createObjectURL(blob);
    a.download = `bench-cluster-${String(rep.model).replace(/[^A-Za-z0-9._-]+/g, "_")}-${String(rep.at).slice(0, 19).replace(/[:T]/g, "")}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  });
  return b;
}
