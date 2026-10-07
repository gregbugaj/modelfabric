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

// Workload names map to existing llm-d profiles. The baseline follows
// its foundation guide; fan-out follows workloads/agentic-serving.md.
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

// Vision settings apply to engine loads independently of scheduling
// profiles. Image encoding is serialized and KV is per slot, so use
// one slot with room for a large prompt. Preserve MTP on current builds.
const ENGINE_SETTINGS = [
  {
    id: "vision",
    title: "Vision",
    kind: "load",
    blurb: "One slot and room for a large prompt: an image request is serialised on the vision encoder anyway, and KV is reserved per slot, so extra slots cost memory for work they cannot parallelise. This applies under any workload profile — raise the slots on a node with the memory to spare. Speculative decoding stays on; it was forced off here against a llama.cpp image bug that current builds have fixed.",
  },
];

// Reuse profile titles for the rail. llm-d schedules one model at a
// time, so at most one model has an active workload.
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

// Use the mesh scheduler, not local routingView: llm-d may run on a peer.
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

    b.append(el("span", "rail-preset-title", w.title));

    const mark = el("span", "rail-preset-mark");
    if (presetBusy === w.id) {
      b.classList.add("pending");
      mark.append(el("span", "spinner"));
    } else if (w.kind === "load") {
      // Vision opens load settings; it is not an active scheduling profile.
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
      const note = el("div", "rail-preset-note", `on ${sched.node}`);
      note.title = `llm-d is running on ${sched.node}, scheduling ${sched.model} across ${sched.engines} engine${sched.engines === 1 ? "" : "s"}. Choosing a workload here changes it there.`;
      rail.append(note);
    }
  }
}

let presetBusy = "";
export let routingView = null;

// Applying restarts llm-d; confirm the model and profile in the dialog first.
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

export function nodesServing(model) {
  const nodes = (meshView?.models ?? []).find((m) => m.id === model)?.nodes ?? [];
  return nodes.length ? `on ${nodes.join(", ")}` : "";
}

export async function startWorkload() {
  const w = wlChoice.workload;
  const model = wlChoice.model;
  if (!w || !model) return;
  presetBusy = w.id;
  renderPresets();
  // Apply on the node already scheduling. Posting locally could start
  // a second scheduler while leaving the active request path unchanged.
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

export function setRoutingView(v) { routingView = v; }

export function setWlChoice(v) { wlChoice = v; }

export function setPresetBusy(v) { presetBusy = v; }
