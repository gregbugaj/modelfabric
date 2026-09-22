import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { chips, el } from "./rendering.js";
import { PRESET_FIELDS, parseSettingsForm, settingsToForm } from "./ui-model.js";

/* ---------- settings & presets ---------- */

export let presets = [];
let catalogKeys = [];

export function fieldInput(f, value, models) {
  // Pills: a short, closed set of levels reads better as one click than as a
  // dropdown, and the same segmented control is already the idiom for nodes
  // and grouping above. The hidden input carries the value, so the form's
  // save loop (every [name] under it) needs no special case.
  if (f.type === "pills") {
    const wrap = el("div", "pills");
    const hidden = el("input");
    hidden.type = "hidden";
    hidden.name = f.key;
    hidden.value = value ?? "";
    wrap.append(hidden);
    for (const o of ["", ...(f.options ?? [])]) {
      const b = el("button", "pill" + ((value ?? "") === o ? " on" : ""));
      b.type = "button";
      b.dataset.value = o;
      b.textContent = o === "" ? "inherit" : o;
      b.addEventListener("click", () => {
        hidden.value = o;
        for (const x of wrap.querySelectorAll(".pill")) x.classList.toggle("on", x.dataset.value === o);
        hidden.dispatchEvent(new Event("change", { bubbles: true }));
      });
      wrap.append(b);
    }
    return wrap;
  }
  let input;
  if (f.type === "bool" || f.type === "select" || f.type === "model") {
    input = el("select", "select");
    const opts = f.type === "bool" ? ["on", "off"] : f.type === "model" ? models : f.options;
    input.append(Object.assign(el("option", null, "inherit"), { value: "" }));
    for (const o of opts) input.append(Object.assign(el("option", null, o), { value: o }));
    input.value = value ?? "";
  } else {
    input = el("input", "input");
    input.type = "text";
    input.inputMode = f.type === "text" ? "text" : "decimal";
    input.placeholder = "inherit";
    input.value = value ?? "";
  }
  input.name = f.key;
  return input;
}

function renderSettingsForm(groups, values, models) {
  const body = $("dlg-body");
  body.replaceChildren();
  for (const g of groups) {
    const box = el("fieldset", "dlg-group");
    box.append(el("legend", null, g.group));
    for (const f of g.fields) {
      const row = el("label", "dlg-field");
      row.append(el("span", "dlg-label", f.label), fieldInput(f, values[f.key], models));
      if (f.help) row.append(el("span", "dlg-help", f.help));
      box.append(row);
    }
    body.append(box);
  }
}

function formValues() {
  const values = {};
  for (const input of $("settings-form").querySelectorAll("[name]")) values[input.name] = input.value;
  return values;
}

export function openPresetEditor(existing) {
  $("dlg-title").textContent = existing ? `Preset ${existing.name}` : "New preset";
  $("dlg-sub").textContent = "Inference settings only, as in LM Studio; load settings belong to a model.";
  const values = settingsToForm(existing?.settings ?? {});
  values.__name = existing?.name ?? "";
  values.__description = existing?.description ?? "";
  renderSettingsForm([
    { group: "Preset", fields: [
      { key: "__name", label: "Name", type: "text" },
      { key: "__description", label: "Description", type: "text" },
    ] },
    { group: "Inference settings", fields: PRESET_FIELDS },
  ], values, []);
  $("dlg-error").textContent = "";
  const actions = $("dlg-actions");
  const saveBtn = el("button", "btn primary", "Save preset");
  saveBtn.type = "button";
  saveBtn.addEventListener("click", async () => {
    const v = formValues();
    const { settings, errors } = parseSettingsForm(v, PRESET_FIELDS);
    if (!v.__name.trim()) { $("dlg-error").textContent = "Name the preset"; return; }
    if (Object.keys(errors).length) {
      $("dlg-error").textContent = "Fix: " + Object.entries(errors).map(([k, e]) => `${k} (${e})`).join(", ");
      return;
    }
    const resp = await fetch(`/api/v1/presets/${encodeURIComponent(v.__name.trim())}`, {
      method: "PUT", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ description: v.__description, settings }),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) { $("dlg-error").textContent = data?.error?.message || `HTTP ${resp.status}`; return; }
    $("settings-dialog").close();
    showNotice(`Saved preset ${v.__name.trim()}`, "success");
    refreshPresets();
  });
  actions.replaceChildren(saveBtn);
  $("settings-dialog").showModal();
}

// Preset management lives in a dialog opened from the settings panel, the way
// LM Studio keeps presets beside the settings they carry rather than as a
// catalogue of their own.
export function openPresetManager() {
  $("dlg-title").textContent = "Presets";
  $("dlg-sub").textContent = "Named inference settings, applied to any model. Load settings belong to a model.";
  const body = $("dlg-body");
  body.replaceChildren();
  if (!presets.length) {
    body.append(el("div", "empty", "No presets yet. Create one, or import a preset file exported from LM Studio."));
  } else {
    const table = el("table");
    const tb = el("tbody");
    for (const p of presets) {
      const tr = el("tr");
      const name = el("td");
      name.append(el("div", "rt-name", p.name));
      if (p.description) name.append(el("div", "instance-line", p.description));
      const vals = el("td");
      vals.append(chips(Object.entries(p.settings ?? {}).map(([k, v]) => `${k} ${v}`)));
      const act = el("td", "actions");
      const edit = el("button", "btn", "Edit");
      edit.type = "button";
      edit.addEventListener("click", () => openPresetEditor(p));
      const del = el("button", "btn danger", "Delete");
      del.type = "button";
      del.addEventListener("click", async () => {
        if (!confirm(`Delete preset ${p.name}?`)) return;
        // A refused delete (a model still names this preset) or a network
        // failure used to look exactly like success.
        try {
          const resp = await fetch(`/api/v1/presets/${encodeURIComponent(p.name)}`, { method: "DELETE" });
          if (!resp.ok) {
            const body = await resp.json().catch(() => null);
            throw new Error(body?.error?.message ?? `HTTP ${resp.status}`);
          }
        } catch (err) {
          showNotice(`Could not delete ${p.name}: ${err.message}`, "error");
          return;
        }
        await refreshPresets();
        openPresetManager();
      });
      act.append(edit, document.createTextNode(" "), del);
      tr.append(name, vals, act);
      tb.append(tr);
    }
    table.append(tb);
    body.append(table);
  }
  $("dlg-error").textContent = "";
  const actions = $("dlg-actions");
  actions.replaceChildren();
  const imp = el("label", "btn", "Import LM Studio preset");
  imp.setAttribute("for", "preset-import");
  const file = el("input");
  file.type = "file";
  file.id = "preset-import";
  file.accept = ".json,application/json";
  file.hidden = true;
  file.addEventListener("change", importPresetFile);
  const neu = el("button", "btn primary", "New preset");
  neu.type = "button";
  neu.addEventListener("click", () => openPresetEditor(null));
  // The dialog head already carries a Close; a second one in the footer just
  // competes with the action that matters.
  actions.append(imp, file, neu);
  $("settings-dialog").showModal();
}

async function importPresetFile(e) {
  const file = e.target.files?.[0];
  if (!file) return;
  const resp = await fetch("/api/v1/presets/import", { method: "POST", body: await file.text() });
  const data = await resp.json().catch(() => ({}));
  e.target.value = "";
  if (!resp.ok) { showNotice(`Import failed: ${data?.error?.message || resp.status}`, "error"); return; }
  const skipped = data.skipped?.length ? ` (skipped: ${data.skipped.join(", ")})` : "";
  showNotice(`Imported preset ${data.preset.name}${skipped}`, "success");
  await refreshPresets();
  openPresetManager();
}

export async function refreshPresets() {
  try {
    const r = await fetch("/api/v1/presets", { cache: "no-store" });
    presets = r.ok ? (await r.json()).presets ?? [] : [];
  } catch { presets = []; }
}


// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and presets is written by the poll
// loop and read here.
export function setPresets(v) { presets = v; }

// Assigned from another module, so it travels as a setter: ES modules
// make an imported binding read-only, and catalogKeys is written by the poll
// loop and read here.
export function setCatalogKeys(v) { catalogKeys = v; }
