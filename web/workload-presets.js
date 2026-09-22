import { post, showNotice } from "./actions.js";
import { $ } from "./core.js";
import { icon } from "./discover.js";
import { nodeAPI, selfNode } from "./my-models.js";
import { tick } from "./polling.js";
import { el, meshView } from "./rendering.js";
import { kvOpts, profilesApi } from "./routing.js";
import { presets } from "./settings-presets.js";
import { buildRouting } from "./ui-model.js";
import { openVisionDialog } from "./vision.js";

/* ---------- Workload presets: the scheduling profiles, by the traffic they suit ---------- */

// Three names for three profiles llm-d already offers, chosen because the
// technical names describe the mechanism and most people arrive knowing only
// the workload. A preset applies a profile; it is not a fourth one, and each
// says which it uses so the Routing page stays the whole truth.
//
// The mapping follows llm-d's own division into foundations and workloads:
// Optimized Baseline is the foundation guide, and the fan-out profile's
// well-lit path is workloads/agentic-serving.md.
const WORKLOADS = [
  {
    id: "chat",
    title: "Chat",
    profile: "optimized-baseline",
    blurb: "Interactive turns that share a conversation's prefix. Sends a request to the engine already holding that prefix unless the wait there would cost more than it saves.",
  },
  {
    id: "agentic",
    title: "Agentic coding",
    profile: "tuned",
    blurb: "Long tool loops and parallel fan-out over one big shared context: branches are co-located so the prompt is prefilled once, and cold prompts spread out.",
  },
  {
    id: "balanced",
    title: "Balanced",
    profile: "load-aware",
    blurb: "Queue depth and running requests, plus approximate prefix scoring. No experimental plugins — the one to fall back to.",
  },
];

// Not a workload. Every profile above can schedule a model that takes images;
// this is about what one image costs the engine. An image request is serialised
// on the vision encoder and its KV is per slot, so the shape is one slot with
// room for a large prompt, set where the engine is launched and holding under
// whichever profile is scheduling.
//
// It used to turn speculative decoding off too, against llama.cpp's "failed to
// process mtmd chunk". Current builds draft an image prompt correctly, so a
// vision load keeps its MTP head.
const ENGINE_SETTINGS = [
  {
    id: "vision",
    title: "Vision",
    kind: "load",
    blurb: "One slot and room for a large prompt: an image request is serialised on the vision encoder anyway, and KV is reserved per slot, so extra slots cost memory for work they cannot parallelise. This applies under any workload profile — raise the slots on a node with the memory to spare. Speculative decoding stays on; it was forced off here against a llama.cpp image bug that current builds have fixed.",
  },
];

// buildRouting keeps its own titleOf as a closure, so the rail needs one of
// its own rather than a second spelling of the titles.
// The workload a model is being scheduled with, or null. llm-d serves one
// model at a time, so at most one model in the list ever has one — which is
// itself worth seeing on this page.
export function workloadFor(modelKey) {
  const llmd = routingView?.llmd;
  if (!llmd?.running || !llmd.model || llmd.model !== modelKey) return null;
  return WORKLOADS.find((w) => w.profile === llmd.profile) ?? null;
}

function profileInfo(name) {
  return profilesApi?.profiles?.find((p) => p.name === name) ?? null;
}

export function profileTitle(name) {
  return profileInfo(name)?.title ?? name;
}

const WORKLOAD_ICONS = {
  chat: '<path d="M21 11.5a8.4 8.4 0 0 1-9 8.4 9 9 0 0 1-3.7-.8L3 21l1.9-5.1A8.4 8.4 0 0 1 4 11.5 8.5 8.5 0 0 1 12.5 3 8.4 8.4 0 0 1 21 11.5Z"/>',
  agentic: '<path d="m8 6-6 6 6 6M16 6l6 6-6 6M14 4l-4 16"/>',
  balanced: '<path d="M12 3v18M3 7h18M6 7l-3 6h6ZM18 7l-3 6h6Z"/>',
  vision: '<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>',
};

export function renderPresets() {
  renderRailGroup($("rail-presets"), WORKLOADS);
  renderRailGroup($("rail-engine"), ENGINE_SETTINGS);
}

// meshScheduler is whichever node is running llm-d for the mesh, or null.
// Preferred over routingView, which only ever knew about this node: with an
// entrypoint the scheduler runs on a machine with no GPUs and the rail showed
// nothing ticked while a benchmark ran entirely through it.
export function meshScheduler() {
  const s = meshView?.scheduler;
  if (s) return s;
  const l = routingView?.llmd;
  if (l?.running && l.model) {
    return { node: selfNode, isSelf: true, model: l.model, profile: l.profile, engines: (l.endpoints ?? []).length };
  }
  return null;
}

function renderRailGroup(rail, items) {
  if (!rail) return;
  // The routing view model, derived the same way the Routing page derives it,
  // so the two cannot disagree about which profile is live. Reading llm-d's
  // state from anywhere else once had the rail saying "needs llm-d" while
  // llm-d was running.
  const sched = meshScheduler();
  const running = sched !== null;
  rail.replaceChildren();

  for (const w of items) {
    const b = el("button", "rail-preset");
    b.type = "button";
    const icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    icon.setAttribute("viewBox", "0 0 24 24");
    icon.setAttribute("width", "16");
    icon.setAttribute("height", "16");
    icon.setAttribute("fill", "none");
    icon.setAttribute("stroke", "currentColor");
    icon.setAttribute("stroke-width", "2");
    icon.setAttribute("stroke-linecap", "round");
    icon.setAttribute("stroke-linejoin", "round");
    icon.innerHTML = WORKLOAD_ICONS[w.id];
    b.append(icon);

    // Just the name. A badge beside it pushed "Agentic coding" into an
    // ellipsis, and the name is the whole point of a friendlier preset. That a
    // profile is experimental is said in full, in amber, in the dialog — which
    // opens before anything is applied, so nothing can be started without it.
    b.append(el("span", "rail-preset-title", w.title));

    // A mark at the end, not a filled row: the links above use a fill to say
    // "this is the page you are on", and one of these being *chosen* is a
    // different fact. Which profile it applies is in the dialog, one click
    // away, rather than on a second line here.
    const mark = el("span", "rail-preset-mark");
    if (presetBusy === w.id) {
      b.classList.add("pending");
      mark.append(el("span", "spinner"));
    } else if (w.kind === "load") {
      // Never ticked: this one is a setting to open, not a profile that is
      // either running or not. A tick here would claim vision models are
      // "off" whenever another workload was chosen, which they never are.
      mark.classList.add("unchosen");
    } else if (running && sched.profile === w.profile) {
      b.classList.add("active");
      b.setAttribute("aria-current", "true");
      const tick = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      tick.setAttribute("viewBox", "0 0 24 24");
      tick.setAttribute("width", "14");
      tick.setAttribute("height", "14");
      tick.setAttribute("fill", "none");
      tick.setAttribute("stroke", "currentColor");
      tick.setAttribute("stroke-width", "2.5");
      tick.setAttribute("stroke-linecap", "round");
      tick.setAttribute("stroke-linejoin", "round");
      tick.innerHTML = '<path d="M20 6 9 17l-5-5"/>';
      mark.append(tick);
    } else {
      // An empty slot on the others, so the group reads as a set of options
      // with one chosen — rather than three more links, two of which happen
      // to be unlit.
      mark.classList.add("unchosen");
    }
    b.append(mark);
    b.disabled = presetBusy !== "";
    if (w.kind === "load") {
      b.title = `${w.blurb}\n\nApplies to every model that carries an image projector, per node, under whichever workload profile is scheduling. Needs no llm-d.`;
      b.addEventListener("click", () => openVisionDialog(w));
      rail.append(b);
      continue;
    }
    const exp = profileInfo(w.profile)?.experimental
      ? "\n\nExperimental: it loads llm-d plugins at Alpha stability."
      : "";
    b.title = running
      ? `${w.blurb}\n\nApplies the ${profileTitle(w.profile)} profile to ${sched.model} on ${sched.node}.${exp}`
      : `${w.blurb}\n\nNeeds llm-d, which schedules one model at a time. Opens Routing to pick one.${exp}`;
    b.addEventListener("click", () => applyWorkload(w));
    rail.append(b);
  }

  if (items !== ENGINE_SETTINGS) {
    if (!running && !presetBusy) {
      const note = el("div", "rail-preset-note", "needs llm-d");
      note.title = "llm-d schedules one model at a time; choose it on the Routing page.";
      rail.append(note);
    } else if (running && !sched.isSelf) {
      // Which machine is scheduling, because it is not this one and every
      // other reading on this page is.
      const note = el("div", "rail-preset-note", `on ${sched.node}`);
      note.title = `llm-d is running on ${sched.node}, scheduling ${sched.model} across ${sched.engines} engine${sched.engines === 1 ? "" : "s"}. Choosing a workload here changes it there.`;
      rail.append(note);
    }
  }
}

let presetBusy = "";
export let routingView = null; // what the Routing page renders; the rail reads it too

// Clicking a preset opens the dialog rather than acting at once: it restarts
// llm-d's scheduler, and what it is about to schedule — which model, which
// profile — should be on screen before that happens, not only after.
function applyWorkload(w) {
  const llmd = routingView?.llmd;
  if (!llmd?.installed) {
    location.hash = "#routing";
    showNotice("llm-d is not installed on this node yet.", "info");
    return;
  }
  openWorkloadDialog(w);
}

export let wlChoice = { workload: null, model: "" };

function openWorkloadDialog(w) {
  const llmd = routingView?.llmd;
  // Already serving? That model is the answer, and the dialog is a
  // confirmation. Otherwise the first model with an engine is a reasonable
  // default and the list is right there.
  const models = routingView?.models ?? [];
  wlChoice = { workload: w, model: llmd?.model || models[0] || "" };
  $("wl-advanced").hidden = false;
  $("wl-reload").hidden = true;
  renderWorkloadDialog();
  $("wl-dialog").showModal();
}

function renderWorkloadDialog() {
  const w = wlChoice.workload;
  if (!w) return;
  const llmd = routingView?.llmd;
  const models = routingView?.models ?? [];
  const info = profileInfo(w.profile);

  const sched = meshScheduler();
  $("wl-title").textContent = w.title;
  $("wl-sub").textContent = sched && !sched.isSelf
    ? `Applies the ${profileTitle(w.profile)} profile on ${sched.node}.`
    : `Applies the ${profileTitle(w.profile)} profile.`;

  const body = $("wl-body");
  body.replaceChildren();
  body.append(el("p", "wl-blurb", w.blurb));

  if (info?.experimental) {
    const warn = el("p", "wl-warn");
    warn.append(el("strong", null, "Experimental. "));
    warn.append(document.createTextNode(
      "This profile loads llm-d plugins at Alpha stability. It runs, but it has had less exposure than the others."));
    body.append(warn);
  }

  const group = el("div", "wl-group");
  // llm-d schedules one model at a time, which is the one thing ModelFabric cannot
  // decide for you. Everything else the preset already answers.
  group.append(el("div", "wl-group-title",
    llmd?.running ? "Scheduling" : "Schedule which model?"));
  if (!models.length) {
    group.append(el("p", "wl-empty",
      "No model has an engine yet. Load one on the Serving page, then come back."));
  }
  for (const m of models) {
    const label = el("label", "wl-model");
    const radio = el("input");
    radio.type = "radio";
    radio.name = "wl-model";
    radio.value = m;
    radio.checked = m === wlChoice.model;
    radio.addEventListener("change", () => { wlChoice.model = m; });
    label.append(radio);
    const text = el("span", "wl-model-text");
    text.append(el("span", "wl-model-name", m));
    const where = nodesServing(m);
    if (where) text.append(el("span", "wl-model-where", where));
    label.append(text);
    if (llmd?.running && llmd.model === m) label.append(el("span", "chip", "serving now"));
    group.append(label);
  }
  body.append(group);

  const start = $("wl-start");
  const same = llmd?.running && llmd.model === wlChoice.model && llmd.profile === w.profile;
  start.disabled = !models.length || same;
  start.textContent = same ? "Already set" : (llmd?.running ? "Switch" : "Start");
}

// Which machines hold this model, for the model list. Read from the mesh view
// rather than asked for again.
export function nodesServing(model) {
  const nodes = (meshView?.models ?? []).find((m) => m.id === model)?.nodes ?? [];
  return nodes.length ? `on ${nodes.join(", ")}` : "";
}

// Applying a preset restarts llm-d's scheduler. That is the same act as
// picking the profile on the Routing page, so it goes through the same call
// rather than a second path that could drift.
export async function startWorkload() {
  const w = wlChoice.workload;
  const model = wlChoice.model;
  if (!w || !model) return;
  presetBusy = w.id;
  renderPresets();
  // The node already scheduling is the one to change. Posting to this node
  // instead would start a second llm-d here while the one actually in the
  // request path kept its old profile — the rail would then show the new
  // workload and nothing would be routing by it.
  const sched = meshScheduler();
  const target = sched && !sched.isSelf ? sched.node : "";
  const where = target ? ` on ${target}` : "";
  showNotice(`${w.title}: scheduling ${model} with ${profileTitle(w.profile)}${where}…`);
  try {
    await post(target ? nodeAPI(target, "/api/v1/llmd/enable") : "/api/v1/llmd/enable",
      { model, profile: w.profile, kv_ceiling: kvOpts.ceiling, kv_scorer: kvOpts.scorer });
    showNotice(`${w.title}: ${model} now scheduled with ${profileTitle(w.profile)}${where}`, "success");
  } catch (err) {
    showNotice(`Could not switch: ${err.message}`, "error");
  } finally {
    presetBusy = "";
    tick();
  }
}


// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and routingView is written by the poll
// loop and read here.
export function setRoutingView(v) { routingView = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and wlChoice is written by the poll
// loop and read here.
export function setWlChoice(v) { wlChoice = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and presetBusy is written by the poll
// loop and read here.
export function setPresetBusy(v) { presetBusy = v; }
