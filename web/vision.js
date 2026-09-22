import { post, showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, nodeAPI, selfNode } from "./my-models.js";
import { tick } from "./polling.js";
import { el, meshView } from "./rendering.js";
import { kvOpts, rerenderRouting, setRoutingBusy } from "./routing.js";
import { renderPresets, setPresetBusy, setWlChoice, startWorkload, wlChoice } from "./workload-presets.js";

/* ---------- Vision: per-node load settings for models that take images ---------- */

// node -> {settings, default, loaded, error}. Held while the dialog is open so
// Save can tell what actually changed and write only those nodes.
let visionRows = new Map();

function visionNodes() {
  const peers = (meshView?.nodes ?? [])
    .filter((n) => n.alive && n.name && n.name !== selfNode)
    .map((n) => n.name)
    .sort();
  return [selfNode, ...peers].filter(Boolean);
}

export async function openVisionDialog(w) {
  setWlChoice({ workload: w, model: "" });
  visionRows = new Map();
  $("wl-title").textContent = w.title;
  $("wl-sub").textContent = "Working around llama.cpp's image chunking, per node.";
  $("wl-advanced").hidden = true;
  renderVisionDialog();
  $("wl-dialog").showModal();

  // Each node answers for itself; one unreachable peer must not hide the rest.
  await Promise.all(visionNodes().map(async (node) => {
    try {
      const r = await fetchJSON(nodeAPI(node, "/api/v1/vision-defaults"));
      visionRows.set(node, { settings: r.settings ?? {}, dflt: r.default ?? {}, loaded: true });
    } catch (err) {
      visionRows.set(node, { error: err.message, loaded: true });
    }
    renderVisionDialog();
  }));
}

function renderVisionDialog() {
  const w = wlChoice.workload;
  if (!w || w.kind !== "load") return;
  const body = $("wl-body");
  body.replaceChildren();
  body.append(el("p", "wl-blurb", w.blurb));

  const group = el("div", "wl-group");
  // The same two fields the model's Load tab already has, named the same, but
  // applied to every model that takes images instead of to one. They are one
  // decision: -c is context × slots, so a slot count set without a context
  // silently changes how large a prompt fits.
  const head = el("div", "wl-group-title");
  head.append(el("span", null, "Per node"));
  head.append(el("span", "wl-col-head", "Context length"));
  head.append(el("span", "wl-col-head", "Parallel slots"));
  group.append(head);
  const nodes = visionNodes();
  if (!nodes.length) group.append(el("p", "wl-empty", "No nodes yet."));

  for (const node of nodes) {
    const row = visionRows.get(node);
    const label = el("label", "wl-model");
    const text = el("span", "wl-model-text");
    text.append(el("span", "wl-model-name", node === selfNode ? `${node} (this node)` : node));
    if (!row?.loaded) {
      text.append(el("span", "wl-model-where", "reading…"));
    } else if (row.error) {
      text.append(el("span", "wl-model-where", row.error));
    }
    label.append(text);

    // What is running there now, and with how many slots. These flags are
    // fixed when the engine launches, so a saved change reaches a model only
    // on its next load — which is the question this row has to answer before
    // someone saves and waits for nothing to happen.
    if (row?.loaded && !row.error) {
      const want = row.settings.parallel ?? row.dflt.parallel ?? 1;
      row.stale = (meshView?.meshEngines ?? [])
        .filter((e) => e.vision && e.slots && e.slots !== want);
      row.stale = row.stale.filter((e) => e.node === node);
      if (row.stale.length) {
        text.append(el("span", "wl-model-where",
          `${row.stale.map((e) => `${e.model} is running with ${e.slots} slot${e.slots === 1 ? "" : "s"}`).join("; ")} — reload to apply`));
      }
    }

    if (row?.loaded && !row.error) {
      const num = (key, min, max, fallback) => {
        const n = el("input", "wl-slots");
        n.type = "number";
        n.min = String(min);
        n.max = String(max);
        n.value = String(row.settings[key] ?? row.dflt[key] ?? fallback);
        n.addEventListener("change", () => {
          row.settings = { ...row.settings, [key]: Math.max(min, Number(n.value) || fallback) };
          row.dirty = true;
        });
        return n;
      };
      // Context first, so the row reads in the order the two values multiply.
      label.append(num("context_length", 512, 1048576, 32768));
      label.append(num("parallel", 1, 64, 1));
    }
    group.append(label);
  }
  body.append(group);

  // Speculation used to be stated here rather than offered, because it failed
  // an image prompt on every engine. It does not any more, so the note says
  // where the old advice went — an operator who read it before will look.
  body.append(el("p", "wl-empty",
    "Speculative decoding is left to the model: a vision load keeps its own MTP head. It used to be forced off here, because llama.cpp failed a prompt carrying an image while drafting; that is fixed in current builds. What it is worth depends on the work \u2014 drafting pays when the model\u2019s output is predictable, as code is, and costs a little when it is not. Set Speculative decoding to off in the model's Load settings on an older build, or on a node where it does not pay."));
  body.append(el("p", "wl-empty",
    "Context is per request; the engine reserves context × slots of KV cache. Image prompts run large, so the default is 32k — a model's own Load settings still override this."));

  const start = $("wl-start");
  start.disabled = false;
  start.textContent = "Save";
  // Only when there is something to reload. A "Save & reload" that would
  // restart nothing is a button that unloads a 27B for no reason.
  const rl = $("wl-reload");
  const stale = [...visionRows.values()].some((r) => r.stale?.length);
  rl.hidden = !stale;
  rl.textContent = "Save & reload";
}

async function saveVisionDefaults(reload = false) {
  const changed = [...visionRows.entries()].filter(([, r]) => r.dirty && !r.error);
  const stale = [...visionRows.entries()].filter(([, r]) => r.stale?.length && !r.error);
  if (!changed.length && !(reload && stale.length)) {
    $("wl-dialog").close();
    return;
  }
  setPresetBusy("vision");
  renderPresets();
  try {
    for (const [node, row] of changed) {
      await fetchJSON(nodeAPI(node, "/api/v1/vision-defaults"), {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          context_length: row.settings.context_length,
          parallel: row.settings.parallel,
          spec_mode: "off",
        }),
      });
    }
    // Engines already running keep the settings they were launched with; this
    // decides the next load, and saying so avoids "I saved it and nothing
    // changed".
    if (changed.length) {
      // No node restart: the file is read on every load. The engine's own
      // flags are not, which is the part worth saying.
      showNotice(`Vision settings saved on ${changed.map(([n]) => n).join(", ")}.${reload ? "" : " No restart needed — reload a vision model there to apply them."}`, "success");
    }
    if (reload) await reloadVisionModels(stale);
    $("wl-dialog").close();
  } catch (err) {
    showNotice(`Could not save: ${err.message}`, "error");
  } finally {
    setPresetBusy("");
    tick();
  }
}

// Unload then load, the same two calls the model settings dialog makes — the
// engine's flags are fixed at launch, so this is the only way a saved slot
// count reaches a model that is already running.
async function reloadVisionModels(stale) {
  for (const [node, row] of stale) {
    for (const e of row.stale) {
      showNotice(`Reloading ${e.model} on ${node}… a large model can take a minute.`);
      try {
        await fetchJSON(nodeAPI(node, "/api/v1/models/unload"), {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ instance_id: e.id }),
        });
        await fetchJSON(nodeAPI(node, "/api/v1/models/load"), {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ model: e.model }),
        });
        showNotice(`${e.model} reloaded on ${node}`, "success");
      } catch (err) {
        // Keep going: one node failing should not strand the others
        // half-reloaded with nothing said about it.
        showNotice(`Reload of ${e.model} on ${node} failed: ${err.message}`, "error");
      }
    }
  }
}

export function initWorkloadDialog() {
  const form = $("wl-form");
  if (!form) return;
  form.addEventListener("submit", (e) => {
    // A dialog form's submitter carries the button's value; "start" is the
    // only one that acts, so Escape and Close cannot schedule anything.
    const v = e.submitter?.value;
    if (v === "save-reload") { saveVisionDefaults(true); return; }
    if (v !== "start") return;
    if (wlChoice.workload?.kind === "load") saveVisionDefaults(false);
    else startWorkload();
  });
  $("wl-advanced")?.addEventListener("click", () => $("wl-dialog").close());
}

export async function enableLLMD(model, profile) {
  setRoutingBusy("enable");
  rerenderRouting();
  showNotice(`Starting llm-d for ${model} with the ${profile} profile…`);
  try {
    await post("/api/v1/llmd/enable",
      { model, profile, kv_ceiling: kvOpts.ceiling, kv_scorer: kvOpts.scorer });
    showNotice(`llm-d is scheduling ${model} (${profile})`, "success");
  } catch (err) {
    showNotice(`llm-d did not start: ${err.message}`, "error");
  } finally {
    setRoutingBusy("");
    tick();
  }
}

export async function disableLLMD() {
  setRoutingBusy("disable");
  rerenderRouting();
  try {
    await post("/api/v1/llmd/disable", {});
    showNotice("llm-d stopped; ModelFabric's router places requests again", "success");
  } catch (err) {
    showNotice(`Could not stop llm-d: ${err.message}`, "error");
  } finally {
    setRoutingBusy("");
    tick();
  }
}

export async function installLLMD() {
  try {
    await post("/api/v1/llmd/install", {});
  } catch (err) {
    showNotice(`Install failed: ${err.message}`, "error");
  }
  tick();
}
