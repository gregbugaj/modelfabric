import { post, showNotice } from "./actions.js";
import { $ } from "./core.js";
import { tick } from "./polling.js";
import { el, fillTable } from "./rendering.js";
import { buildRuntime, formatBytes } from "./ui-model.js";

export let available = null;
export let checking = false;
let lastRuntime = buildRuntime(null);
export let runtimesApi = null;
export let operations = [];
let lastPolled = false;

function fitCell(r) {
  const td = el("td");
  const pill = el("span", `pill fit-${r.fit}`, r.fit === "yes" ? "yes" : r.fit === "no" ? "no" : "unknown");
  td.append(pill);
  if (r.fit !== "yes") for (const why of r.reasons) td.append(el("div", "reason", why));
  return td;
}

export function progressBar(fraction) {
  const bar = el("div", "progress");
  const fill = el("div", "progress-fill");
  fill.style.width = `${Math.round(Math.min(1, Math.max(0, fraction)) * 100)}%`;
  bar.append(fill);
  return bar;
}

export function renderRuntime(v) {
  lastRuntime = v;
  if (!runtimesApi && !v.managed && operations.length === 0 && !lastPolled) return;
  const hw = $("hw-grid");
  hw.replaceChildren();
  if (v.hardware) {
    const item = (label, value, sub) => {
      const box = el("div", "hw-item");
      box.append(el("div", "label", label), el("div", "hw-value", value));
      if (sub) box.append(el("div", "hw-sub", sub));
      hw.append(box);
    };
    item("CPU", v.hardware.cpu, `${formatBytes(v.hardware.memoryBytes)} RAM`);
    for (const g of v.hardware.gpus) {
      item("GPU", g.name, [formatBytes(g.memoryBytes), g.compute && `compute ${g.compute}`, g.driver && `driver ${g.driver}`]
        .filter(Boolean).join(" · "));
    }
    if (!v.hardware.gpus.length) item("GPU", "none detected", "CPU and Vulkan builds only");
    item("CUDA", v.hardware.cuda ? `up to ${v.hardware.cuda}` : "—", "what the driver supports");
    item("Vulkan", v.hardware.vulkan ? "available" : "not found", "");
  } else {
    hw.append(el("div", "empty", "No hardware survey on this node."));
  }

  $("rt-selection").textContent = v.selection?.auto
    ? "· automatic: the newest compatible build"
    : v.selection ? "· pinned by you" : "";
  $("rt-auto").hidden = !v.selection || v.selection.auto;

  fillTable("rt-body", "rt-empty", v.runtimes,
    () => [
      el("span", null, "No runtimes. Install one below, or with "),
      el("code", null, "mfsh runtime get"),
      el("span", null, "."),
    ],
    (r) => {
      const tr = el("tr");
      const name = el("td");
      name.append(el("div", "rt-name", r.displayName), el("div", "rt-id mono", r.name));
      const status = el("td");
      if (r.isDefault) status.append(el("span", "self-tag", v.selection.auto ? "default" : "pinned"));
      if (r.inUse) status.append(el("div", "instance-line", `serving ${r.inUse} instance${r.inUse > 1 ? "s" : ""}`));
      const actions = el("td", "actions");
      if (!r.isDefault || !v.selection.auto) {
        if (r.canSelect && !r.isDefault) {
          const use = el("button", "btn", "Use");
          use.title = "Make this the runtime for new loads";
          use.addEventListener("click", () => selectRuntime(r.name));
          actions.append(use);
        }
      }
      if (r.managed) {
        const rm = el("button", "btn danger", "Remove");
        rm.disabled = !r.canRemove;
        if (!r.canRemove) rm.title = "Unload the models running on it first";
        rm.addEventListener("click", () => removeRuntime(r.name));
        actions.append(document.createTextNode(" "), rm);
      }
      tr.append(name, el("td", r.build ? "mono" : "muted", r.build || "—"), el("td", null, r.backend),
        el("td", "muted nowrap", r.source), fitCell(r), status, actions);
      return tr;
    });

  const inst = $("rt-installing");
  inst.replaceChildren();
  for (const i of v.installing) {
    const row = el("div", "install-row");
    const head = el("div", "install-head");
    head.append(el("span", "spinner"), el("span", "mono", i.name));
    row.append(head, progressBar(i.fraction), el("div", "instance-line", i.message));
    inst.append(row);
  }

  const empty = $("rt-get-empty");
  $("rt-check").disabled = checking || !v.canInstall;
  if (!v.managed || !v.canInstall) {
    empty.hidden = false;
    empty.textContent = "This node does not install runtimes.";
    $("rt-options-table").hidden = $("rt-updates-table").hidden = true;
    return;
  }
  if (!v.checked) {
    empty.hidden = false;
    empty.replaceChildren(checking ? el("span", "spinner") : "", checking ? "checking upstream…" : "Check upstream for builds.");
    $("rt-options-table").hidden = $("rt-updates-table").hidden = true;
    return;
  }
  empty.hidden = true;
  if (v.newestBuild) $("rt-upstream-hint").textContent = `upstream llama.cpp is at ${v.newestBuild}; builds are verified and tested here`;

  $("rt-options-table").hidden = !v.options.length;
  const opts = $("rt-options");
  opts.replaceChildren();
  for (const o of v.options) {
    const tr = el("tr");
    const name = el("td");
    name.append(el("span", "rt-name", o.displayName));
    if (o.recommended) name.append(el("span", "self-tag", "recommended"));
    name.append(el("div", "rt-id mono", o.name));
    const act = el("td", "actions");
    if (o.installed) {
      act.append(el("span", "muted", "installed"));
    } else {
      const b = el("button", o.recommended ? "btn primary" : "btn", o.installing ? "Installing…" : "Install");
      b.disabled = o.installing;
      b.addEventListener("click", () => installRuntime(o.recommended ? "" : o.key, o.name));
      act.append(b);
    }
    tr.append(name, el("td", null, o.backend), el("td", "num", o.installed ? "—" : formatBytes(o.downloadBytes)),
      el("td", "muted small", o.reason), act);
    opts.append(tr);
  }

  $("rt-updates-table").hidden = !v.updates.length;
  const ups = $("rt-updates");
  ups.replaceChildren();
  for (const u of v.updates) {
    const tr = el("tr");
    const act = el("td", "actions");
    if (u.error) {
      act.append(el("span", "err-text", u.error));
    } else if (!u.available) {
      act.append(el("span", "status up"));
      act.lastChild.append(el("span", "dot"), "up to date");
    } else {
      const b = el("button", "btn primary", u.installing ? "Updating…" : `Update · ${formatBytes(u.downloadBytes)}`);
      b.disabled = u.installing;
      b.addEventListener("click", () =>
        installRuntime(u.backend, u.latestName, u.backend === "cuda" ? u.backendVersion : ""));
      act.append(b);
    }
    tr.append(el("td", "mono", u.family), el("td", "mono", u.installed), el("td", "mono", u.latest), act);
    ups.append(tr);
  }
}

export async function checkAvailable(refresh) {
  checking = true;
  renderRuntime(buildRuntime(runtimesApi, available, operations));
  try {
    const resp = await fetch(`/api/v1/runtimes/available${refresh ? "?refresh=1" : ""}`, { cache: "no-store" });
    const body = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(body?.error?.message || `HTTP ${resp.status}`);
    available = body;
    if (refresh) showNotice(`Checked upstream: newest build is ${body.newest_build ?? "unknown"}`, "success");
  } catch (err) {
    showNotice(`Could not check for runtimes: ${err.message}`, "error");
  } finally {
    checking = false;
    renderRuntime(buildRuntime(runtimesApi, available, operations));
  }
}

// Track operations started here so completion can be announced.
const watchedInstalls = new Map();

async function installRuntime(backend, name, cuda = "") {
  try {
    const r = await post("/api/v1/runtimes/get", { backend, cuda });
    watchedInstalls.set(r.operation.id, name);
    showNotice(`Installing ${name}… verifying and testing it on this machine before it is used.`);
  } catch (err) {
    showNotice(`Install failed: ${err.message}`, "error");
  }
  tick();
}

export async function selectRuntime(name) {
  try {
    await post("/api/v1/runtimes/select", { name });
    showNotice(name ? `New loads will use ${name}` : "Selecting the runtime automatically", "success");
  } catch (err) {
    showNotice(`Could not select: ${err.message}`, "error");
  }
  tick();
}

async function removeRuntime(name) {
  if (!confirm(`Remove ${name}? Its files are deleted; LM Studio's packages are never touched.`)) return;
  try {
    await post("/api/v1/runtimes/remove", { name });
    showNotice(`Removed ${name}`, "success");
    available = null;
    checkAvailable(false);
  } catch (err) {
    showNotice(`Remove failed: ${err.message}`, "error");
  }
  tick();
}

export function settleWatchedInstalls(ops) {
  for (const [id, name] of watchedInstalls) {
    const op = ops.find((o) => o.id === id);
    if (!op || op.state === "running") continue;
    watchedInstalls.delete(id);
    if (op.state === "succeeded") {
      showNotice(`Installed ${name}`, "success");
      available = null;
      checkAvailable(false);
    } else {
      showNotice(`Install of ${name} failed: ${op.error || "unknown error"}`, "error");
    }
  }
}

export function setOperations(v) { operations = v; }

export function setRuntimesApi(v) { runtimesApi = v; }

export function setLastPolled(v) { lastPolled = v; }
