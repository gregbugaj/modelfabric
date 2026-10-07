import { settingControl, settingRow } from "./setting-fields.js";
// Router settings use /api/v1/server-settings and take effect after restart.
// Preferred-node changes use a separate live API. llm-d controls are in routing.js.

import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON } from "./my-models.js";
import { el, meshView } from "./rendering.js";
import { ROUTER_SETTINGS, formToRouter, pendingRestart, routedBy, routerToForm, settingsChanges } from "./ui-model.js";
import { meshScheduler } from "./workload-presets.js";

const rr = { view: null, inputs: {}, rows: {}, built: false, loading: false };

const ROWS = [
  { key: "preferred", label: "Preferred node", kind: "select", live: true,
    desc: "When several nodes hold a model, try this one first. Applies at once.",
    help: "A preference, not a pin: if that node is down or lacks the model, the router chooses as usual. The same setting as Prefer on the Overview's Nodes table." },
  { key: "rate_weighted_routing", label: "Speed-aware routing", kind: "switch",
    desc: "Compare nodes by how long each would take to reach a request, not by queue length.",
    help: "Measured prefill on this mesh ran from about 300 tok/s on the Mac to 2,200 on an RTX 5090. Counting queued requests alone treats a slot on each as equal, which sent the Mac half the fleet's work. Off restores queue length only." },
  { key: "prefix_affinity", label: "Prefix affinity", kind: "switch",
    desc: "Send a conversation back to the engine that already holds its prompt in cache.",
    help: "Requests sharing a prompt prefix go to the engine that served it last, so the cached prefix is reused instead of read again. llm-d does the same for its model with its own prefix-aware scoring." },
  { key: "local_bias", label: "Local bias", kind: "number", min: 0, step: 0.5,
    desc: "Favour this node's own engines when load is otherwise equal, by this many requests.",
    help: "The default is 0.5. 0 treats this node like any other; 1 means a peer must be a whole request less busy before a request leaves this machine." },
  { key: "max_output_tokens", label: "Output ceiling", kind: "number", min: 0, step: 1024,
    desc: "The length limit filled into a request that sets none. 0 is no ceiling.",
    help: "A request with no limit is asking for the whole context window: one such request generated about 130,000 tokens over ninety minutes for an answer already abandoned. A request that sets its own limit is never changed." },
  { key: "cache_on", label: "Disk prompt cache", kind: "switch",
    desc: "Save a conversation's cached prompt to disk when its slot is needed, and restore it when it returns.",
    help: "Restoring a 6,000-token conversation took about 0.4 s against about 25 s to read it again (Qwen3-0.6B). llama.cpp engines only, and not vision models. The saved state contains the conversation's text, so it is off by default; files are owner-only. It works in each engine's shim, which every request passes, so it serves llm-d's model as well as the router's." },
  { key: "cache_gb", label: "Cache size (GB)", kind: "number", min: 1, step: 10, when: "cache_on", sub: true,
    desc: "The most disk it may use; the least recently used conversations are dropped first." },
  { key: "cache_disk_dir", label: "Cache directory", kind: "text", when: "cache_on", sub: true, placeholder: "state directory /slots",
    desc: "An absolute path. Empty keeps it under the node's state directory." },
];

function build() {
  if (rr.built) return;
  rr.built = true;
  const body = $("rg-router-body");
  for (const f of ROWS) {
    const input = settingControl(f, "rg");
    rr.inputs[f.key] = input;
    rr.rows[f.key] = settingRow(f, input);
    body.append(rr.rows[f.key]);
  }
  body.addEventListener("input", refresh);
  body.addEventListener("change", (e) => {
    if (e.target === rr.inputs.preferred) {
      void setPreferred(e.target.value);
      return;
    }
    if (e.target === rr.inputs.cache_on && e.target.checked && !rr.inputs.cache_gb.value) rr.inputs.cache_gb.value = "50";
    refresh();
  });
  $("rg-router-save").addEventListener("click", save);
}

// Refresh settings on open and after save; regular polls only update status.
export function renderRouter() {
  build();
  renderMethods();
  renderModels();
  renderPreferred();
  if (!rr.view && !rr.loading) void load();
}

function renderMethods() {
  const sched = meshScheduler();
  const box = $("rg-methods");
  const card = (title, active, lines, cls) => {
    const c = el("div", "rg-method" + (active ? " on" : "") + (cls ? ` ${cls}` : ""));
    const h = el("div", "rg-method-head");
    h.append(el("b", null, title), el("span", "rg-method-state", active ? "routing" : "off"));
    c.append(h);
    for (const l of lines) c.append(el("div", "rg-method-line", l));
    return c;
  };
  const total = (meshView?.models ?? []).length;
  const routerLine = sched
    ? `Every model except ${sched.model}${total ? ` (${total - 1} of ${total})` : ""}.`
    : `Every model${total ? ` (${total})` : ""}.`;
  box.replaceChildren(
    card("ModelFabric router", true, [routerLine, "Built in, on every node. Chooses a node by measured speed and load, and keeps conversations on the engine that has them cached."]),
    card("llm-d", Boolean(sched), [
      sched ? `${sched.model}, scheduled from ${sched.node}${sched.profile ? ` with the ${sched.profile} profile` : ""}.` : "Not scheduling any model.",
      "An alternative scheduler for one model at a time, with its own prefix-aware scoring and caching. Turn it on below to route a model its way and compare the two.",
    ], "rg-llmd"),
  );
}

function renderModels() {
  const sched = meshScheduler();
  const rows = routedBy(meshView?.models, sched?.model);
  const tbody = $("rg-models");
  tbody.replaceChildren();
  if (!rows.length) {
    const tr = el("tr");
    const td = el("td", "muted", "No model is loaded anywhere in the mesh.");
    td.colSpan = 3;
    tr.append(td);
    tbody.append(tr);
    return;
  }
  for (const r of rows) {
    const tr = el("tr");
    const by = el("td");
    by.append(el("span", `rg-by rg-by-${r.by}`, r.by === "llmd" ? "llm-d" : "ModelFabric router"));
    tr.append(el("td", "mono", r.model), by, el("td", "mono muted", r.nodes.join(", ")));
    tbody.append(tr);
  }
}

function renderPreferred() {
  const sel = rr.inputs.preferred;
  if (!sel || document.activeElement === sel) return;
  const nodes = (meshView?.nodes ?? []).map((n) => n.name).filter(Boolean);
  const want = meshView?.preferred || "";
  const opts = ["", ...nodes];
  if (sel.options.length !== opts.length || [...sel.options].some((o, i) => o.value !== opts[i])) {
    sel.replaceChildren(...opts.map((n) => {
      const o = el("option", null, n || "none: by speed and load only");
      o.value = n;
      return o;
    }));
  }
  sel.value = want;
}

async function setPreferred(node) {
  try {
    await fetchJSON("/z/preferred", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ node }) });
    showNotice(node ? `Preferring ${node}` : "No preferred node", "success");
  } catch (err) {
    showNotice(err.message, "error");
  }
}

async function load() {
  rr.loading = true;
  try {
    rr.view = await fetchJSON("/api/v1/server-settings");
    fill();
    refresh();
  } catch (err) {
    $("rg-router-msg").textContent = /without a config file/i.test(err.message)
      ? "This node runs without a config file, so the router's settings cannot be saved from here."
      : err.message;
  } finally {
    rr.loading = false;
  }
}

function fill() {
  const f = routerToForm(rr.view?.saved);
  for (const [k, input] of Object.entries(rr.inputs)) {
    if (k === "preferred") continue;
    if (input.type === "checkbox") input.checked = Boolean(f[k]);
    else input.value = f[k] ?? "";
  }
}

function readForm() {
  const f = {};
  for (const [k, input] of Object.entries(rr.inputs)) {
    if (k === "preferred") continue;
    f[k] = input.type === "checkbox" ? input.checked : input.value.trim();
  }
  return formToRouter(f, rr.view?.saved);
}

const changes = () => (rr.view ? settingsChanges(rr.view.saved, readForm(), ROUTER_SETTINGS) : {});

const ROW_OF = { cache_disk_mib: "cache_on" };

function refresh() {
  for (const f of ROWS) if (f.when) rr.rows[f.key].hidden = !rr.inputs[f.when].checked;
  for (const n of document.querySelectorAll("#rg-router .ss-restart")) n.remove();
  const pending = pendingRestart(rr.view, ROUTER_SETTINGS);
  for (const k of pending) {
    const name = rr.rows[ROW_OF[k] ?? k]?.querySelector(".ss-name");
    if (!name) continue;
    const chip = el("span", "ss-restart", "restart");
    chip.title = `Saved. The node runs ${JSON.stringify(rr.view.running?.[k])} until it restarts.`;
    name.append(chip);
  }
  const dirty = Object.keys(changes());
  $("rg-router-save").disabled = dirty.length === 0;
  $("rg-router-msg").textContent = dirty.length
    ? `${dirty.length} unsaved`
    : pending.length ? `${pending.length} waiting for a restart: mfsh down, then mfsh up` : "";
}

async function save() {
  const body = changes();
  if (!Object.keys(body).length) return;
  if (body.cache_disk_mib > 0 && !rr.view.saved.cache_disk_mib &&
      !confirm("The disk prompt cache writes conversations' cached prompts, their text included, to disk (owner-only files). Turn it on?")) return;
  try {
    rr.view = await fetchJSON("/api/v1/server-settings", {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
  } catch (err) {
    $("rg-router-msg").textContent = err.message;
    return;
  }
  fill();
  refresh();
  showNotice(`Saved; applies after a restart: ${Object.keys(body).join(", ")}`, "success");
}
