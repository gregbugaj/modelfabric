import { settingControl, settingRow } from "./setting-fields.js";

import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { readSidePref, startSideResize, writeSidePref } from "./my-models.js";
import { tick } from "./polling.js";
import { el } from "./rendering.js";
import { renderTokens } from "./tokens.js";
import {
  SERVER_SETTINGS, displayPath, formToServer, pendingRestart, settingsChanges,
  serverToForm, wideningWarnings,
} from "./ui-model.js";

// A row is one control. `desc` is always shown; `help` is behind the "?".
// `when` names a switch that must be on for the row to show.
const TABS = [
  { id: "server", label: "Server", rows: [
    // Show local and tailnet hosts separately; equal port numbers still
    // refer to distinct listeners.
    { key: "port", label: "Server Port", kind: "port", prefix: true,
      desc: "This machine's address, for apps and this dashboard.",
      help: "Only this machine can reach it. `mfsh up -port N` overrides the port for one run without saving." },
    { key: "tailnet", label: "Tailnet", kind: "readonly",
      desc: "Your tailnet devices and other nodes reach this node here, with no key: Tailscale says who they are.",
      help: "The address is assigned by Tailscale and the port is the mesh port, which every node in the mesh must share; neither is set here. A node on a different mesh port drops out of the mesh, so it is changed in each node's config.json together. Management on this address is accepted only from devices Tailscale reports as yours." },
    { key: "front_on", label: "Public Front Door", kind: "switch", exposes: true,
      desc: "For apps outside your tailnet, published through Tailscale Funnel or a TLS proxy. Inference only; a key on every request.",
      help: "A second listener (public_listen) that serves /v1 and nothing else: never the dashboard or management, and an API token always required. It listens on 127.0.0.1; Funnel or the proxy gives it HTTPS and a public name. Tailnet devices do not need it. Any node can have one, whether or not it runs models." },
    { key: "front_port", label: "Front door port", kind: "port", when: "front_on", sub: true,
      desc: "On 127.0.0.1. Point Funnel or the proxy at it, e.g. tailscale funnel 1235." },
    { key: "peer_admin", label: "Managed from Other Nodes", kind: "switch",
      desc: "Let your other devices' dashboards load and unload models here.",
      help: "Tailscale decides who is asking: only devices it reports as your own (or sharing your tag) are accepted. Off, this node can only be managed from itself." },
    // engine_bind controls how this router reaches local engines. Peers
    // reach the mesh listener, which forwards to those engines.
    { key: "engine_bind", label: "Engine Bind", kind: "select", options: [],
      desc: "Where this node's engines listen, and where its router reaches them. Other nodes reach this one at the Tailnet address above, never at its engines.",
      help: "This machine only is enough for ModelFabric: a request for a model here, from an app or from another node, comes in through this node and is handed to the engine on this machine. Tailnet is for something that dials engines itself, which is llm-d scheduling from another node. Engine ports have no authentication of their own, so on the tailnet Tailscale ACLs are the only thing in front of them. The tailnet address is looked up from Tailscale at each start, never typed." },
    { key: "web_ui", label: "Serve Dashboard", kind: "switch",
      desc: "This page. Off removes it after the next restart; the API is unaffected." },
  ] },
  { id: "access", label: "Access", rows: [
    { key: "require_api_key", label: "Require Authentication", kind: "switch",
      desc: "Only accept requests to /v1 that carry an API token or the node key.",
      help: "Apps send it as Authorization: Bearer <token> (or x-api-key). Create one per app in the Tokens tab. Applies as soon as it is saved. Management and this dashboard never take a key: they are loopback-only." },
    { key: "cors_on", label: "Enable CORS", kind: "switch",
      desc: "Cross-Origin Resource Sharing: let web apps on other origins call /v1 from a browser.",
      help: "Without it a browser blocks, say, a chat UI served from localhost:3000 from reading this node's replies. It only ever opens /v1: management and this dashboard refuse other origins either way. Applies as soon as it is saved." },
    { key: "cors_origins", label: "Allowed origins", kind: "lines", when: "cors_on", sub: true,
      placeholder: "http://localhost:3000",
      desc: "One per line, as scheme://host[:port]. * allows any site you visit." },
  ] },
  { id: "tokens", label: "Tokens" },
  { id: "mcp", label: "MCP", rows: [
    { key: "mcp_allow_ephemeral", label: "Allow per-request MCPs", kind: "switch", exposes: true,
      desc: "Let a request to /api/v1/chat name an MCP server for this node to call (\"ephemeral_mcp\").",
      help: "The model's tool calls are run by this node against the server the request names, and the results are fed back to the model. Whoever can call /api/v1/chat can then make this node open a connection to any address it can reach, including ones inside your network, so turn on Require Authentication with it. Applies as soon as it is saved." },
    { key: "mcp_allow_configured", label: "Allow calling servers from mcp.json", kind: "switch", exposes: true,
      desc: "Let a request use the MCP servers listed in ~/.modelfabric/mcp.json, as \"mcp/<name>\".",
      help: "mcp.json is the file LM Studio, Cursor and Claude Desktop share: {\"mcpServers\": {\"name\": {\"url\": ...}}} for a remote server, or {\"command\", \"args\", \"env\"} for one started on this machine. A local server is a process this node starts for the request, and its tools act with this node's user's rights, so list only servers you would run yourself. Applies as soon as it is saved." },
  ] },
  { id: "loading", label: "Loading", rows: [
    { key: "jit_load", label: "Just-in-Time Model Loading", kind: "switch",
      desc: "Load a model when a request names one no node is serving.",
      help: "Also lists every downloaded model in /v1/models, loaded or not, as LM Studio does with JIT on." },
    { key: "jit_unload", label: "Auto unload unused JIT loaded models", kind: "switch",
      desc: "Unload a model loaded on demand once it has been idle." },
    { key: "jit_ttl", label: "Idle for", kind: "text", when: "jit_unload", sub: true, placeholder: "60m",
      desc: "A duration such as 30m or 2h. Models you load yourself are never unloaded for this." },
    { key: "jit_auto_evict", label: "Only Keep Last JIT Loaded Model", kind: "switch",
      desc: "Unload idle on-demand models before loading another, so one holds VRAM at a time." },
  ] },
];

const ss = {
  tab: readSidePref("mfsh.ss.tab") || "server",
  width: Number(readSidePref("mfsh.ss.width")) || 460,
  pinned: false, // startSideResize reads it; this flyout is never pinned
  view: null,
  inputs: {},    // row key -> control, built once per open so edits survive tab switches
  rows: {},      // row key -> row element
  panes: {},
};

const side = () => $("ss-side");

function build() {
  const s = side();
  s.replaceChildren();
  s.style.setProperty("--side-w", `${ss.width}px`);
  const grip = el("div", "side-grip");
  grip.title = "Drag to resize";
  grip.addEventListener("pointerdown", (e) => startSideResize(e, "ss-side", ss, "mfsh.ss.width"));
  s.append(grip);

  const head = el("div", "side-head");
  const title = el("div", "side-title", "Server settings");
  const actions = el("div", "side-actions");
  const close = el("button", "btn icon", "×");
  close.type = "button";
  close.title = "Close (Esc)";
  close.addEventListener("click", closeServerSettings);
  actions.append(close);
  title.append(actions);
  const sub = el("div", "side-sub");
  const file = el("span", "mono ss-file", "…");
  file.id = "ss-file";
  sub.append(file);
  head.append(title, sub);
  s.append(head);

  const tabs = el("div", "side-tabs");
  tabs.setAttribute("role", "tablist");
  s.append(tabs);

  const form = el("form", "ss-form");
  form.id = "ss-form";
  form.noValidate = true;
  const body = el("div", "side-body");
  form.append(body);
  ss.inputs = {};
  ss.rows = {};
  ss.panes = {};
  for (const t of TABS) {
    const b = el("button", "side-tab", t.label);
    b.type = "button";
    b.dataset.tab = t.id;
    b.setAttribute("role", "tab");
    b.addEventListener("click", () => showTab(t.id));
    tabs.append(b);

    const pane = el("div", "ss-pane");
    pane.dataset.tab = t.id;
    ss.panes[t.id] = pane;
    if (t.id === "server") {
      const note = el("div", "ss-note", "This node loads no models: role \"entrypoint\" in config.json. It routes to the nodes that do.");
      note.id = "ss-role-note";
      note.hidden = true;
      pane.append(note);
    }
    for (const f of t.rows ?? []) {
      const input = settingControl(f);
      ss.inputs[f.key] = input;
      ss.rows[f.key] = settingRow(f, input);
      pane.append(ss.rows[f.key]);
    }
    body.append(pane);
  }

  const warn = el("div", "ss-warn");
  warn.id = "ss-warn";
  warn.hidden = true;
  const foot = el("div", "side-foot ss-foot");
  const status = el("div", "ss-status");
  status.id = "ss-status";
  const save = el("button", "btn primary", "Save");
  save.type = "submit";
  save.id = "ss-save";
  foot.append(status, save);
  form.append(warn, foot);
  s.append(form);

  form.addEventListener("submit", onSave);
  form.addEventListener("input", refresh);
  form.addEventListener("change", (e) => {
    // Enabling CORS defaults to any origin; the warning must reflect that.
    if (e.target === ss.inputs.cors_on && e.target.checked && !ss.inputs.cors_origins.value.trim()) {
      ss.inputs.cors_origins.value = "*";
    }
    if (e.target === ss.inputs.front_on && e.target.checked && !ss.inputs.front_port.value) {
      ss.inputs.front_port.value = String(Number(ss.inputs.port.value || 1234) + 1);
    }
    if (e.target === ss.inputs.jit_unload && e.target.checked && !ss.inputs.jit_ttl.value.trim()) {
      ss.inputs.jit_ttl.value = "60m";
    }
    refresh();
  });
}

function showTab(id) {
  ss.tab = id;
  writeSidePref("mfsh.ss.tab", id);
  for (const b of side().querySelectorAll(".side-tab")) {
    const on = b.dataset.tab === id;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
  for (const [tid, pane] of Object.entries(ss.panes)) pane.hidden = tid !== id;
  // Token actions save immediately; the settings Save button does not apply.
  side().querySelector(".ss-foot").hidden = id === "tokens";
  if (id === "tokens") void renderTokens(ss.panes.tokens);
}

// Recognize configured tailnet addresses as Tailnet, but preserve
// custom LAN addresses rather than rewriting them.
function engineBindOptions(saved, tailnet) {
  const sel = ss.inputs.engine_bind;
  sel.replaceChildren();
  const add = (value, text) => {
    const o = el("option", null, text);
    o.value = value;
    sel.append(o);
  };
  add("", "This machine only");
  add("tailnet", tailnet.addr ? `Tailnet (${tailnet.addr})` : "Tailnet (no address: is Tailscale up?)");
  const v = saved?.engine_bind ?? "";
  const desc = ss.rows.engine_bind.querySelector(".ss-desc");
  desc.textContent = TABS[0].rows.find((r) => r.key === "engine_bind").desc;
  if (v && v !== "tailnet") {
    add(v, `${v} (set in config.json)`);
    desc.textContent = `${v} is not a tailnet address, so engines answer there with no authentication. Choose Tailnet or This machine only to replace it.`;
  }
}

function fill(saved) {
  engineBindOptions(saved, ss.view?.tailnet ?? {});
  const form = serverToForm(saved);
  form.tailnet = ss.view?.tailnet?.listen || "not on a tailnet";
  const host = String(saved?.listen ?? "").replace(/:\d+$/, "") || "127.0.0.1";
  $("ss-port-host").textContent = `${host}:`;
  // Every node routes; the entrypoint role only disables local model loading.
  const note = $("ss-role-note");
  note.hidden = saved?.role !== "entrypoint";
  for (const [k, input] of Object.entries(ss.inputs)) {
    const v = form[k];
    if (input.type === "checkbox") input.checked = Boolean(v);
    else if (input.tagName === "CODE") input.textContent = String(v ?? "—");
    else input.value = v ?? "";
  }
}

function readForm() {
  const form = {};
  for (const [k, input] of Object.entries(ss.inputs)) {
    if (input.tagName === "CODE") continue;
    form[k] = input.type === "checkbox" ? input.checked : input.value.trim();
  }
  return formToServer(form, ss.view?.saved);
}

const changes = () => settingsChanges(ss.view?.saved, readForm());

const ROW_OF = { listen: "port", public_listen: "front_on", mesh_admin: "peer_admin", jit_ttl: "jit_unload" };

function refresh() {
  for (const t of TABS) {
    for (const f of t.rows ?? []) {
      if (f.when) ss.rows[f.key].hidden = !ss.inputs[f.when].checked;
    }
  }
  const s = side();
  for (const n of s.querySelectorAll(".ss-restart")) n.remove();
  const pending = pendingRestart(ss.view);
  for (const k of pending) {
    const name = ss.rows[ROW_OF[k] ?? k]?.querySelector(".ss-name");
    if (!name) continue;
    const running = ss.view.running?.[k];
    const shown = typeof running === "boolean" ? (running ? "on" : "off") : (running || "default");
    const chip = el("span", "ss-restart", "restart");
    chip.title = `Saved. The node runs ${shown} until it restarts.`;
    name.append(chip);
  }
  const dirty = Object.keys(changes());
  const status = $("ss-status");
  status.replaceChildren();
  if (dirty.length) status.append(el("span", null, `${dirty.length} unsaved`));
  else if (pending.length) status.append(el("span", "ss-pending", `${pending.length} waiting for a restart`));
  $("ss-save").disabled = dirty.length === 0;

  const warn = $("ss-warn");
  const list = wideningWarnings(changes());
  warn.replaceChildren(...list.map((w) => el("div", null, w)));
  warn.hidden = list.length === 0;
}

async function load() {
  const status = $("ss-status");
  try {
    const resp = await fetch("/api/v1/server-settings", { cache: "no-store" });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(data?.error?.message || `HTTP ${resp.status}`);
    ss.view = data;
  } catch (err) {
    ss.view = null;
    status.replaceChildren(el("span", "err-text", err.message));
    $("ss-save").disabled = true;
    return;
  }
  const file = $("ss-file");
  file.textContent = ss.view.config_shown || displayPath(ss.view.config_file);
  file.title = `Saved to ${ss.view.config_file}. Only what you change is written; the previous file is kept as .bak.`;
  fill(ss.view.saved);
  refresh();
}

async function onSave(e) {
  e.preventDefault();
  const status = $("ss-status");
  const settings = readForm();
  if (ss.inputs.cors_on.checked && settings.cors_origins.length === 0) {
    status.replaceChildren(el("span", "err-text", "Add an origin, or * for any, or turn CORS off."));
    showTab("access");
    ss.inputs.cors_origins.focus();
    return;
  }
  const body = changes();
  const keys = Object.keys(body);
  if (!keys.length) return;
  const warnings = wideningWarnings(body);
  if (warnings.length && !confirm(`${warnings.join("\n\n")}\n\nSave anyway?`)) return;
  try {
    const resp = await fetch("/api/v1/server-settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) throw new Error(data?.error?.message || `HTTP ${resp.status}`);
    ss.view = data;
  } catch (err) {
    status.replaceChildren(el("span", "err-text", err.message));
    return;
  }
  fill(ss.view.saved);
  refresh();
  const live = new Set(ss.view.live ?? []);
  const now = keys.filter((k) => live.has(k));
  const later = keys.filter((k) => !live.has(k));
  showNotice(
    [now.length && `Applied: ${now.join(", ")}`, later.length && `after a restart: ${later.join(", ")}`]
      .filter(Boolean).join(" · "),
    "success",
  );
  void tick();
}

export function openServerSettings(tab) {
  build();
  side().hidden = false;
  showTab(tab || ss.tab);
  void load();
}

export function closeServerSettings() {
  side().hidden = true;
  // A token shown once must not survive in the page once the flyout closes.
  ss.panes.tokens?.replaceChildren();
  $("ss-open")?.focus({ preventScroll: true });
}

export const serverSettingsOpen = () => !side().hidden;

export function initServerSettings() {
  $("ss-open").addEventListener("click", () => (serverSettingsOpen() ? closeServerSettings() : openServerSettings()));
}

for (const k of SERVER_SETTINGS) {
  if (!(k in formToServer({}, {}))) throw new Error(`server setting ${k} has no control`);
}
