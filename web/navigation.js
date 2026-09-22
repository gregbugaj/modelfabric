import { actScope, loadCapture, renderOperations, setActScope, setCapture, startTrafficStream } from "./activity.js";
import { $ } from "./core.js";
import { openBench } from "./bench.js";
import { renderDoctorNodes, runDoctor } from "./doctor.js";
import { readSidePref, writeSidePref } from "./my-models.js";
import { tick } from "./polling.js";
import { el } from "./rendering.js";
import { available, checkAvailable, checking, operations } from "./runtime.js";

/* ---------- navigation ---------- */

// A page's second line earns its place only by saying something the page does
// not already show. Overview's address is in the hero below it, Serving's
// scope is in its table heads, My Models and Mesh show their nodes, Routing
// names both methods and Benchmark's tabs each say what they measure, so
// those pages have no second line at all. "Every model on every node you
// own." under My Models read as a slogan.
const VIEWS = {
  overview: ["Overview", ""],
  models: ["Serving", ""],
  mesh: ["Mesh", ""],
  local: ["My Models", ""],
  routing: ["Routing", ""],
  runtime: ["Runtime", "Engine builds on this machine, and which one new loads use."],
  bench: ["Benchmark", ""],
  // The requests half of this changes with the scope control, so the subtitle
  // is set from setActScope rather than fixed here.
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
  $("page-sub").hidden = !sub; // no empty line holding open 18px of margin
  // Upstream is only asked when someone looks, and GitHub's anonymous limit
  // is 60 calls an hour, so the answer is cached server-side too.
  if (view === "runtime" && !available && !checking) checkAvailable(false);
  if (view === "doctor") { renderDoctorNodes(); runDoctor(); }
  if (view === "bench") openBench();
  if (view === "activity") {
    startTrafficStream(); loadCapture(); setActScope(actScope); renderOperations();
    if (typeof tick === "function") tick();
  }
  if ((view === "local" || view === "mesh") && typeof tick === "function") tick(); // fetch the other nodes now
  // The live tap shows what the model is writing. Leaving the page is a clear
  // "stop watching", and a stream nobody is looking at is one the server is
  // still doing work for.
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

// Which of the three Activity views is showing. The live tap follows it: a
// stream nobody has open is work the node is doing for nobody, and leaving
// the tab is as clear a "stop watching" as leaving the page.
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
  // The pulse belongs to Requests, which is the only tab that streams by
  // itself; on the others it would be claiming something that is not running.
  const live = $("act-live");
  if (live) live.hidden = actTab !== "requests";
  // Opening the tab is the deliberate act, so it starts the tap rather than
  // asking for a second click on a page whose only purpose is to show this.
  // The switch stays, as the obvious way to stop it without leaving.
  if (actTab === "replies") startTokenStream(); else stopTokenStream();
  if (actTab === "operations") renderOperations();
}

/* Live replies, this node only.
 *
 * Deliberately separate from the Requests table above, which has a mesh
 * scope: this reads /z/log/tokens, which the peer listener refuses, because
 * following it across the mesh would mean shipping reply text between
 * machines. It is also off until asked and stops the moment the page is left
 * — nothing is stored at either end, and a tap nobody is watching costs the
 * server nothing only if it is actually closed.
 */
let tokenSource = null;
// Newest last. Each entry owns the DOM it wrote, so a token appends to an
// existing text node instead of re-rendering the list: at sixty tokens a
// second, with several replies in flight, rebuilding per token was the most
// expensive thing on the page.
let tokenReqs = [];
// Per block, so one runaway answer cannot grow the page without bound. The
// count of replies is the reader's choice (#tok-keep); this is not, because
// nobody wants to tune it.
//
// The window follows the *end* of the block, not the start. Capping the start
// and stopping — what this did first — left the panel dead while the engine
// was still writing: this model spends most of its output thinking, so a long
// task passes 20,000 characters of it and the reader saw "… truncated" and
// then nothing, on a request that had minutes left to run. A live view that
// stops being live is worse than one that forgets.
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
  // A refused or dropped stream must say so rather than sit looking idle,
  // which is indistinguishable from a quiet node.
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
      // "not streamed" is not a footnote: without it a reply that appeared all
      // at once reads as a stalled stream, and the rate is an average over a
      // wait rather than a speed anything was arriving at.
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
    // appendChild rather than += : the latter re-reads and rewrites the whole
    // string on every token, which is quadratic over a long reply.
    r.textEl.appendChild(document.createTextNode(e.text));
    r.len += e.text.length;
    // Then drop from the front until it fits. Each token is its own text node,
    // so removing the oldest is cheap and the newest is always on screen.
    while (r.len > TOKEN_CHARS && r.textEl.firstChild) {
      const gone = r.textEl.firstChild;
      if (gone === r.trimMark) break; // never eat the marker itself
      r.len -= (gone.textContent || "").length;
      r.textEl.removeChild(gone);
      if (!r.trimMark) {
        r.trimMark = document.createTextNode("… earlier output trimmed\n");
        r.textEl.insertBefore(r.trimMark, r.textEl.firstChild);
      }
    }
  }

  // Only follow the tail when the reader is already there: scrolling back to
  // read something and being yanked to the bottom by the next token is worse
  // than losing the follow.
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
  // Structure is built as events arrive; the only thing that changes wholesale
  // is whether thinking shows, and that is a class rather than a rebuild.
  const root = $("tok-stream");
  if (root) root.classList.toggle("hide-thinking", $("tok-thinking")?.checked === false);
}

function updateTokenRate() {
  // One headline figure for the replies still running, which is what someone
  // glancing at the panel wants to know.
  const live = tokenReqs.filter((r) => !r.done);
  const tps = live.reduce((n, r) => n + (r.ms > 0 ? r.deltas / (r.ms / 1000) : 0), 0);
  const out = $("tok-rate");
  if (out) out.textContent = live.length ? `${live.length} generating · ${Math.round(tps)} tok/s` : "";
}

export function initNav() {
  showView(location.hash.slice(1));
  window.addEventListener("hashchange", () => showView(location.hash.slice(1)));
}
