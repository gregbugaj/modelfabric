import { $ } from "./core.js";
import { renderRouter } from "./routing-router.js";
import { kv, mm } from "./my-models.js";
import { el } from "./rendering.js";
import { available, operations, progressBar } from "./runtime.js";
import { buildRouting } from "./ui-model.js";
import { disableLLMD, enableLLMD, installLLMD } from "./vision.js";
import { renderPresets, routingView, setRoutingView } from "./workload-presets.js";

/* ---------- routing ---------- */

export let profilesApi = null; // static: fetched once
export let llmdApi = null;
let routingBusy = "";   // an action in flight, shown instead of buttons
let chosenModel = "";
let chosenProfile = "";

export function renderRouting(v) {
  // The router's half renders on every node; only llm-d's depends on it
  // being available here.
  renderRouter();
  $("rt-na").hidden = v.available;
  $("routing-body").hidden = !v.available;
  if (!v.available) return;

  // llm-d state and controls.
  const st = $("rg-llmd-state");
  const pill = el("span", `status ${v.llmd.running ? "up" : v.llmd.error ? "down" : "muted"}`);
  pill.append(el("span", "dot"), v.llmd.installed ? v.llmd.state : "not installed");
  st.replaceChildren(pill);

  const box = $("rg-llmd");
  box.replaceChildren();
  if (v.installing) {
    const row = el("div", "install-row");
    const head = el("div", "install-head");
    head.append(el("span", "spinner"), el("span", null, "Installing llm-d (EPP + Envoy, verified, no containers)"));
    row.append(head, progressBar(v.installing.fraction), el("div", "instance-line", v.installing.message));
    box.append(row);
    return;
  }
  if (!v.llmd.installed) {
    const row = el("div", "llmd-row");
    row.append(el("span", "muted", "llm-d's EPP and Envoy run as processes under the node — no containers. About 150 MB."));
    const b = el("button", "btn primary", "Install llm-d");
    b.addEventListener("click", installLLMD);
    row.append(b);
    box.append(row);
  } else {
    if (v.llmd.on) {
      // Four facts about a running scheduler read as one line; as four
      // bordered cells they read as a dashboard that is not one.
      const info = el("div", "meta-line");
      info.append(
        meta(v.llmd.model, "", "mono"),
        meta(v.llmd.profileTitle || v.llmd.profile, "profile"),
        meta(v.llmd.endpoints.join(", ") || "none",
          v.llmd.endpoints.length === 1 ? "engine" : "engines", "mono"),
        meta(v.llmd.peak ? `${v.llmd.peak.toLocaleString()} tok/s` : "—",
          v.llmd.calibrated ? "peak prefill, measured" : "peak prefill, default until measured"),
      );
      box.append(info);
      if (v.llmd.error) box.append(el("div", "err-text pad", v.llmd.error));
    }
    const row = el("div", "llmd-row");
    if (!chosenModel || !v.models.includes(chosenModel)) chosenModel = v.llmd.model || v.models[0] || "";
    if (!chosenProfile) chosenProfile = v.llmd.profile || "load-aware";
    const sel = el("select", "select");
    for (const m of v.models) {
      const o = el("option", null, m);
      o.value = m;
      o.selected = m === chosenModel;
      sel.append(o);
    }
    sel.disabled = !v.models.length;
    sel.addEventListener("change", () => (chosenModel = sel.value));
    row.append(el("span", "muted", "Model"), sel);
    const apply = el("button", "btn primary",
      routingBusy === "enable" ? "Starting…" : v.llmd.on ? "Apply" : "Enable llm-d");
    apply.disabled = Boolean(routingBusy) || !v.models.length;
    apply.title = v.models.length ? "" : "Load a model first";
    apply.addEventListener("click", () => enableLLMD(chosenModel, chosenProfile));
    row.append(el("span", "muted", `Profile: ${v.profiles.find((p) => p.name === chosenProfile)?.title ?? chosenProfile}`), apply);
    if (v.llmd.on) {
      const off = el("button", "btn danger", routingBusy === "disable" ? "Stopping…" : "Disable");
      off.disabled = Boolean(routingBusy);
      off.addEventListener("click", disableLLMD);
      row.append(off);
    }
    if (!v.models.length) row.append(el("span", "muted small", "No model is running; load one first."));
    box.append(row, kvControls(Boolean(routingBusy)));
  }

  // Profiles.
  const grid = $("rg-profiles");
  grid.replaceChildren();
  for (const p of v.profiles) {
    const chosen = p.name === chosenProfile;
    // A label with a real radio: picking a profile is local until Apply, which
    // is what a radio group means. (A <button> row would also carry the UA's
    // background and make a poor grid container.)
    const card = el("label", "choice" + (chosen ? " active" : ""));
    const radio = el("input", "choice-radio");
    radio.type = "radio";
    radio.name = "rg-profile";
    radio.value = p.name;
    radio.checked = chosen;
    card.append(radio, choiceMark(chosen));
    const main = el("div", "choice-main");
    const head = el("div", "choice-title", p.title);
    if (p.active) head.append(el("span", "self-tag", "in use"));
    if (p.experimental) head.append(el("span", "exp-tag", "experimental"));
    main.append(head, el("div", "choice-desc", p.summary));
    card.append(main);
    if (p.wellLit) {
      const a = el("a", "small", "llm-d well-lit path ↗");
      a.href = p.wellLit;
      a.target = "_blank";
      a.rel = "noopener";
      a.addEventListener("click", (e) => e.stopPropagation());
      main.append(a);
    }
    card.addEventListener("click", () => {
      chosenProfile = p.name;
      renderRouting(v);
    });
    grid.append(card);
  }
  const un = $("rg-unavailable");
  un.replaceChildren(el("div", "label pad-top", "Not available with llama.cpp"));
  for (const p of v.unavailable) {
    const row = el("div", "unavail-row");
    const t = p.wellLit ? el("a", null, p.title) : el("span", null, p.title);
    if (p.wellLit) {
      t.href = p.wellLit;
      t.target = "_blank";
      t.rel = "noopener";
    }
    row.append(t, el("span", "muted small", ` — ${p.reason}`));
    un.append(row);
  }
}

// choiceMark is the dot beside a row in a pick-one list: filled when chosen.
// It marks the selection on the row itself, so the state does not rest on a
// background tint alone.
function choiceMark(on) {
  const m = el("span", "choice-mark" + (on ? " on" : ""));
  return m;
}

// meta is one fact on a metadata line: the value, then its label in small
// muted type. A handful of these reads faster than the same facts in boxes.
export function meta(value, label, cls) {
  const box = el("span", "meta");
  box.append(el("span", "meta-v" + (cls ? " " + cls : ""), value));
  if (label) box.append(el("span", "meta-k", label));
  return box;
}

export function rerenderRouting() {
  // The models carried over from the last poll, not an empty list: this repaint
  // is an optimistic one between an action and the tick that follows it, and
  // handing it nothing emptied the Model select and disabled Apply for as long
  // as the action took.
  setRoutingView(buildRouting(llmdApi, profilesApi, operations, routingView?.models ?? []));
  renderRouting(routingView);
  renderPresets();
}

// KV options, off by default. On llama.cpp a full KV cache means warm, not
// busy — the tokens counted are prefixes kept for reuse — so neither of these
// is a load-balancing knob, and the panel says so.
export const kvOpts = { ceiling: 0, scorer: 0 };
const KV_CEILING_DEFAULT = 0.97;

// kvControls renders the two switches and keeps kvOpts in step.
function kvControls(disabled) {
  const box = el("div", "kv-opts");
  const guard = el("label", "kv-opt");
  const gc = el("input");
  gc.type = "checkbox";
  gc.checked = kvOpts.ceiling > 0;
  gc.disabled = disabled;
  const gn = el("input", "kv-num");
  gn.type = "number";
  gn.min = "0.5";
  gn.max = "1";
  gn.step = "0.01";
  gn.value = String(kvOpts.ceiling || KV_CEILING_DEFAULT);
  gn.disabled = disabled || !gc.checked;
  gc.addEventListener("change", () => {
    kvOpts.ceiling = gc.checked ? Number(gn.value) || KV_CEILING_DEFAULT : 0;
    rerenderRouting();
  });
  gn.addEventListener("change", () => {
    kvOpts.ceiling = gc.checked ? Number(gn.value) || KV_CEILING_DEFAULT : 0;
  });
  guard.append(gc, el("span", null, "Skip engines with KV fuller than"), gn);
  guard.title = "A guard against eviction thrash, applied before every other decision. Near 1: a warm engine reads high, so a low ceiling would exclude the engine holding your prefix.";

  const rank = el("label", "kv-opt");
  const rc = el("input");
  rc.type = "checkbox";
  rc.checked = kvOpts.scorer > 0;
  rc.disabled = disabled;
  const rn = el("input", "kv-num");
  rn.type = "number";
  rn.min = "1";
  rn.max = "10";
  rn.step = "1";
  rn.value = String(kvOpts.scorer || 1);
  rn.disabled = disabled || !rc.checked;
  rc.addEventListener("change", () => {
    kvOpts.scorer = rc.checked ? Number(rn.value) || 1 : 0;
    rerenderRouting();
  });
  rn.addEventListener("change", () => {
    kvOpts.scorer = rc.checked ? Number(rn.value) || 1 : 0;
  });
  rank.append(rc, el("span", null, "Prefer the emptier cache, weight"), rn);
  rank.title = "llm-d's kv-cache-utilization-scorer. It ranks the emptier cache higher, which on llama.cpp pulls against prefix affinity — measure it before trusting it.";

  box.append(guard, rank);
  box.append(el("div", "muted small",
    "Both read the KV gauge ModelFabric computes from /slots. A full cache means warm here, not busy — leave them off unless a benchmark says otherwise."));
  return box;
}


// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and routingBusy is written by the poll
// loop and read here.
export function setRoutingBusy(v) { routingBusy = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and llmdApi is written by the poll
// loop and read here.
export function setLLMDApi(v) { llmdApi = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and profilesApi is written by the poll
// loop and read here.
export function setProfilesApi(v) { profilesApi = v; }
