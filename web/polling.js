import { showNotice } from "./actions.js";
import { actScope, actSelected, closeActSide, refreshPeerTraffic, renderOperations, renderTraffic } from "./activity.js";
import { $ } from "./core.js";
import { dv, renderDownloads } from "./discover.js";
import { renderDoctorNodes } from "./doctor.js";
import { refreshTopology } from "./mesh.js";
import { closeServerSettings, serverSettingsOpen } from "./serversettings.js";
import { closePeerFeeds, closeSide, mm, refreshPeerModels, renderMyModels, selfNode, setSelfNode } from "./my-models.js";
import { el, meshView, recordLoad, render, setFrontView, setMeshView } from "./rendering.js";
import { llmdApi, profilesApi, renderRouting, setLLMDApi, setProfilesApi } from "./routing.js";
import { available, checkAvailable, operations, renderRuntime, runtimesApi, selectRuntime, setLastPolled, setOperations, setRuntimesApi, settleWatchedInstalls } from "./runtime.js";
import { refreshPresets, setCatalogKeys } from "./settings-presets.js";
import { tn } from "./tune-slots.js";
import { buildFront, buildLocal, buildRouting, buildRuntime, buildView } from "./ui-model.js";
import { renderPresets, routingView, setRoutingView } from "./workload-presets.js";

/* ---------- polling ---------- */

function setConn(ok, text) {
  const badge = $("conn");
  badge.classList.toggle("err", !ok);
  badge.replaceChildren(el("span", "dot"), document.createTextNode(text));
}

// tick is started by the poll interval, by navigation and by many action
// handlers. Without a guard a slow poll could finish after a newer one and
// paint stale state over it, or two polls could interleave their renders.
let ticking = false;
let tickAgain = false;

export async function tick() {
  if (ticking) {
    tickAgain = true; // coalesce: run once more when the current one lands
    return;
  }
  ticking = true;
  try {
    await tickOnce();
  } finally {
    ticking = false;
    if (tickAgain) {
      tickAgain = false;
      void tick();
    }
  }
}

/* ---------- the live feed ---------- */

// /api/v1/events pushes each of these as it changes; the body of each is what
// its GET returns, and null where that GET would not answer 200. While the
// feed holds all of them, a tick paints from it instead of asking again.
const FEED = ["mesh", "models", "operations", "runtimes", "llmd", "front"];
const live = { source: null, data: new Map() };
const feedReady = () => live.data.size === FEED.length;

function openFeed() {
  if (live.source) return;
  const es = new EventSource("/api/v1/events");
  live.source = es;
  for (const name of FEED) {
    es.addEventListener(name, (e) => {
      live.data.set(name, JSON.parse(e.data));
      if (feedReady()) void tick();
    });
  }
  es.onerror = () => {
    // Polling carries the page until the stream is back. The data is dropped
    // rather than kept: after a gap it is a state from before the gap, and on
    // reconnect the server sends every resource again anyway.
    live.data.clear();
    // CLOSED means the server answered, but not with a stream — a node older
    // than the feed. EventSource will not retry that, and neither do we.
    if (es.readyState === EventSource.CLOSED) live.source = null;
  };
}

function closeFeed() {
  live.source?.close();
  live.source = null;
  live.data.clear();
}

// A hidden tab asks nothing: an open, forgotten dashboard polled its node and,
// through it, every peer for as long as the browser stayed up.
export function initLive() {
  openFeed();
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      closeFeed();
      closePeerFeeds();
    } else {
      openFeed();
      void tick();
    }
  });
}

// pollState is the fallback: the same six answers, fetched.
async function pollState() {
  const [meshResp, apiResp, opsResp, rtResp, llmdResp, frontResp] = await Promise.all([
    fetch("/z/mesh", { cache: "no-store" }),
    // 404 here just means this node routes but does not manage models.
    fetch("/api/v1/models", { cache: "no-store" }).catch(() => null),
    fetch("/api/v1/operations", { cache: "no-store" }).catch(() => null),
    fetch("/api/v1/runtimes", { cache: "no-store" }).catch(() => null),
    // 404/501 here just means this node does not schedule with llm-d.
    fetch("/api/v1/llmd", { cache: "no-store" }).catch(() => null),
    fetch("/api/v1/front", { cache: "no-store" }).catch(() => null),
  ]);
  if (!meshResp.ok) throw new Error(`HTTP ${meshResp.status}`);
  const body = async (r) => (r && r.ok ? r.json() : null);
  return {
    mesh: await meshResp.json(),
    models: await body(apiResp),
    operations: await body(opsResp),
    runtimes: await body(rtResp),
    llmd: await body(llmdResp),
    front: await body(frontResp),
  };
}

async function tickOnce() {
  try {
    const st = feedReady() ? Object.fromEntries(live.data) : await pollState();
    // The feed sends a failed /z/mesh as null; polling throws on it above.
    if (!st.mesh) throw new Error("this node did not answer /z/mesh");

    const api = st.models;
    setOperations(st.operations?.operations ?? []);
    setRuntimesApi(st.runtimes);
    // Before render(), which paints the front-door panel from it.
    setFrontView(buildFront(st.front));
    const meshBody = st.mesh;
    setSelfNode(meshBody?.self?.node || "");
    setMeshView(buildView(meshBody));
    recordLoad(meshView.meshEngines);
    render(meshView, buildLocal(api, operations));
    // My Models: this node always; the others while the page is open.
    mm.nodes.set(selfNode, api ? { api, operations } : { error: "this node does not manage models" });
    const livePeers = (meshBody?.peers ?? []).filter((p) => p.alive).map((p) => p.node);
    for (const n of [...mm.nodes.keys()]) if (n !== selfNode && !livePeers.includes(n)) mm.nodes.delete(n);
    if (!document.querySelector('section[data-view="mesh"]').hidden) await refreshTopology(livePeers);
    const discovering = $("dv-dialog").open;
    const wantsPeers = !document.querySelector('section[data-view="local"]').hidden
      || !document.querySelector('section[data-view="activity"]').hidden || discovering;
    if (wantsPeers) await refreshPeerModels(livePeers); else closePeerFeeds();
    renderMyModels();
    if (discovering) renderDownloads();
    setLastPolled(true);
    setLLMDApi(st.llmd);
    if (!profilesApi) {
      setProfilesApi(await fetch("/api/v1/llmd/profiles").then((r) => (r.ok ? r.json() : null)).catch(() => null));
    }
    setCatalogKeys((api?.models ?? []).map((m) => m.key));
    // llm-d schedules across the mesh, not just this node: its endpoints are
    // whichever engines serve the model, wherever they run. Offering only the
    // models loaded here left the Model select showing one model while the
    // scheduler ran another, so Apply would not have done what it said.
    const loaded = (meshView.models ?? []).map((m) => m.id);
    setRoutingView(buildRouting(llmdApi, profilesApi, operations, loaded));
    renderRouting(routingView);
    renderPresets();
    renderRuntime(buildRuntime(runtimesApi, available, operations));
    settleWatchedInstalls(operations);
    if (!document.querySelector('section[data-view="doctor"]').hidden) renderDoctorNodes();
    if (!document.querySelector('section[data-view="activity"]').hidden) {
      renderOperations();
      // mm.nodes has just been refreshed, so the peer list is current.
      if (actScope === "mesh") refreshPeerTraffic(); else renderTraffic();
    }
    setConn(true, "live");
  } catch (err) {
    setConn(false, "disconnected");
    console.error("mesh poll failed:", err);
  }
}

document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if ($("settings-dialog").open || $("dv-dialog").open || $("tn-dialog").open) return;
  if (serverSettingsOpen()) { closeServerSettings(); return; }
  if (actSelected) { closeActSide(); return; }
  // Escape closes the flyout, but leaves a pinned panel alone: pinned is a
  // layout choice, not something you dismiss.
  if (!mm.selected || mm.pinned) return;
  closeSide();
});
$("fd-copy").addEventListener("click", async () => {
  const url = $("fd-url").textContent;
  try {
    await navigator.clipboard.writeText(url);
    showNotice("Address copied", "success");
  } catch {
    // Clipboard access is denied outside a secure context; select it instead
    // so the keyboard shortcut still works.
    const r = document.createRange();
    r.selectNodeContents($("fd-url"));
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
    showNotice("Press Ctrl+C to copy", "info");
  }
});
$("rt-check").addEventListener("click", () => checkAvailable(true));
refreshPresets();
$("rt-auto").addEventListener("click", () => selectRuntime(""));
