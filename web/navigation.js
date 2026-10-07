import { actScope, loadCapture, renderOperations, setActScope, setCapture, startTrafficStream } from "./activity.js";
import { $ } from "./core.js";
import { openBench } from "./bench.js";
import { renderDoctorNodes, runDoctor } from "./doctor.js";
import { readSidePref, writeSidePref } from "./my-models.js";
import { tick } from "./polling.js";
import { el } from "./rendering.js";
import { available, checkAvailable, checking, operations } from "./runtime.js";

const VIEWS = {
  overview: ["Overview", ""],
  models: ["Serving", ""],
  mesh: ["Mesh", ""],
  local: ["My Models", ""],
  routing: ["Routing", ""],
  runtime: ["Runtime", "Engine builds on this machine, and which one new loads use."],
  bench: ["Benchmark", ""],
  activity: ["Activity", "Requests as they are answered, and what every node has loaded and unloaded."],
  doctor: ["Doctor", "What this node depends on, and what to do about anything wrong."],
};

function showView(name) {
  const view = VIEWS[name] ? name : "overview";
  for (const el of document.querySelectorAll("section[data-view]")) {
    el.hidden = el.dataset.view !== view;
  }
  for (const a of document.querySelectorAll(".rail a[data-view]")) {
    a.classList.toggle("active", a.dataset.view === view);
  }
  $("page-title").textContent = VIEWS[view][0];
  const sub = VIEWS[view][1];
  $("page-sub").textContent = sub;
  $("page-sub").hidden = !sub;
  // Fetch upstream versions on demand; cache server-side to limit GitHub calls.
  if (view === "runtime" && !available && !checking) checkAvailable(false);
  if (view === "doctor") { renderDoctorNodes(); runDoctor(); }
  if (view === "bench") openBench();
  if (view === "activity") {
    startTrafficStream(); loadCapture(); setActScope(actScope); renderOperations();
    if (typeof tick === "function") tick();
  }
  if ((view === "local" || view === "mesh") && typeof tick === "function") tick();
  // Close the live tap when leaving the page to stop unused server work.
  if (view !== "activity") stopTokenStream();
}

export function initCapture() {
  $("act-bodies")?.addEventListener("change", (e) => setCapture({ bodies: e.target.checked }));
  $("act-keep")?.addEventListener("change", (e) => setCapture({ keep: Number(e.target.value) }));
  for (const b of document.querySelectorAll("#act-scope .seg-btn")) {
    b.addEventListener("click", () => setActScope(b.dataset.scope));
  }
  $("tok-on")?.addEventListener("change", (e) => (e.target.checked ? startTokenStream() : stopTokenStream()));
  $("tok-thinking")?.addEventListener("change", renderTokens);
  $("tok-keep")?.addEventListener("change", pruneTokenReqs);
  for (const b of document.querySelectorAll("#act-tabs .tab")) {
    b.addEventListener("click", () => setActTab(b.dataset.tab));
  }
  setActTab(readSidePref("mfsh.act.tab") || "requests");
}

let actTab = "requests";

function setActTab(name) {
  const tabs = ["requests", "replies", "operations"];
  actTab = tabs.includes(name) ? name : "requests";
  writeSidePref("mfsh.act.tab", actTab);
  for (const b of document.querySelectorAll("#act-tabs .tab")) {
    const on = b.dataset.tab === actTab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", on ? "true" : "false");
  }
  for (const el of document.querySelectorAll(".act-tab")) {
    el.hidden = el.dataset.tab !== actTab;
  }
  // Only Requests streams automatically, so other tabs must not show its pulse.
  const live = $("act-live");
  if (live) live.hidden = actTab !== "requests";
  if (actTab === "replies") startTokenStream(); else stopTokenStream();
  if (actTab === "operations") renderOperations();
}

// Live replies are local-only: the peer listener refuses /z/log/tokens
// to keep reply text from crossing machines. The tap stores nothing and
// closes when the page is left.
let tokenSource = null;
// Append to existing text nodes; rebuilding the list for every token
// becomes expensive with concurrent replies.
let tokenReqs = [];
// Bound each reply's text with a sliding window. Retaining the start
// and dropping new tokens makes long, active replies appear stalled.
const TOKEN_CHARS = 20000;

function tokenKeep() { return Number($("tok-keep")?.value) || 6; }

function startTokenStream() {
  const on = $("tok-on");
  if (on) on.checked = true;
  if (tokenSource) return;
  $("tok-stream").hidden = false;
  $("tok-empty").hidden = tokenReqs.length > 0;
  $("tok-note").textContent = "Live. Nothing is recorded; leaving this tab stops it.";
  tokenSource = new EventSource("/z/log/tokens");
  tokenSource.onmessage = (ev) => {
    let e;
    try { e = JSON.parse(ev.data); } catch { return; }
    onTokenEvent(e);
  };
  // Report a dropped or refused stream so it cannot appear merely idle.
  tokenSource.onerror = () => {
    $("tok-note").textContent = "Stream interrupted — the node may have restarted. Switch it off and on to retry.";
  };
}

function stopTokenStream() {
  if (tokenSource) { tokenSource.close(); tokenSource = null; }
  const on = $("tok-on");
  if (on) on.checked = false;
  const note = $("tok-note");
  if (note) note.textContent = "Stopped. Nothing is recorded; leaving this tab stops it.";
  const rate = $("tok-rate");
  if (rate) rate.textContent = "";
  const stream = $("tok-stream");
  if (stream) stream.hidden = true;
  const empty = $("tok-empty");
  if (empty) empty.hidden = true;
}

function onTokenEvent(e) {
  const root = $("tok-stream");
  if (!root) return;
  let r = tokenReqs.find((x) => x.trace === e.trace);
  if (!r) {
    r = { trace: e.trace, model: e.model || "", deltas: 0, ms: 0, done: false,
          kind: "", textEl: null, len: 0, trimMark: null };
    r.box = el("div", "tok-req");
    const head = el("div", "tok-head");
    head.append(el("span", "tok-id", (e.trace || "").slice(0, 8)),
                el("span", "tok-model", r.model));
    r.box.append(head);
    root.append(r.box);
    tokenReqs.push(r);
    pruneTokenReqs();
    $("tok-empty").hidden = true;
  }
  r.deltas = e.deltas || r.deltas;
  r.ms = e.ms || r.ms;

  if (e.kind === "done") {
    if (!r.done) {
      r.done = true;
      const rate = r.ms > 0 ? Math.round(r.deltas / (r.ms / 1000)) : 0;
      // Non-streamed replies arrive at once; their displayed rate is an average.
      const how = e.at_once ? " · not streamed" : "";
      r.box.append(el("div", "tok-done", `done · ${r.deltas} tokens · ${rate} tok/s${how}`));
    }
  } else if (e.text) {
    const kind = e.kind === "reasoning" ? "thinking" : "reply";
    if (r.kind !== kind) {
      r.box.append(el("div", "tok-kind " + kind, kind));
      r.textEl = el("div", "tok-text " + kind, "");
      r.box.append(r.textEl);
      r.kind = kind;
      r.len = 0;
      r.trimMark = null;
    }
    // appendChild avoids rewriting the whole string per token, which is quadratic.
    r.textEl.appendChild(document.createTextNode(e.text));
    r.len += e.text.length;
    while (r.len > TOKEN_CHARS && r.textEl.firstChild) {
      const gone = r.textEl.firstChild;
      if (gone === r.trimMark) break; // Preserve the truncation marker.
      r.len -= (gone.textContent || "").length;
      r.textEl.removeChild(gone);
      if (!r.trimMark) {
        r.trimMark = document.createTextNode("… earlier output trimmed\n");
        r.textEl.insertBefore(r.trimMark, r.textEl.firstChild);
      }
    }
  }

  // Follow new tokens only when already at the bottom; preserve manual scrolling.
  if (root.scrollHeight - root.scrollTop - root.clientHeight < 80) {
    root.scrollTop = root.scrollHeight;
  }
  updateTokenRate();
}

function pruneTokenReqs() {
  const keep = tokenKeep();
  while (tokenReqs.length > keep) {
    const gone = tokenReqs.shift();
    gone.box?.remove();
  }
}

function renderTokens() {
  const root = $("tok-stream");
  if (root) root.classList.toggle("hide-thinking", $("tok-thinking")?.checked === false);
}

function updateTokenRate() {
  const live = tokenReqs.filter((r) => !r.done);
  const tps = live.reduce((n, r) => n + (r.ms > 0 ? r.deltas / (r.ms / 1000) : 0), 0);
  const out = $("tok-rate");
  if (out) out.textContent = live.length ? `${live.length} generating · ${Math.round(tps)} tok/s` : "";
}

export function initNav() {
  showView(location.hash.slice(1));
  window.addEventListener("hashchange", () => showView(location.hash.slice(1)));
}
