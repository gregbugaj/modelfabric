import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, nodeAPI, readSidePref, selfNode, writeSidePref } from "./my-models.js";
import { el, frontView, lastView, meshView } from "./rendering.js";
import { AGENT_MIN_CONTEXT, connectPlaces, entrypoints, opencodeConfig, servedModels } from "./ui-model.js";

// "Connect an app": what to put in a coding agent's configuration so it
// uses this mesh. OpenCode first; each app is an entry in APPS with what it
// calls its config file and how that file is written.
//
// The part every such guide leaves out is the key. Which address an app
// uses, and whether it needs a key and whose, depends on where the app runs:
// on this machine, on another machine on the tailnet, or outside it. So the
// dialog asks that first, and writes the file for the answer.
const APPS = [{
  key: "opencode",
  name: "OpenCode",
  install: "curl -fsSL https://opencode.ai/install | bash",
  files: ["~/.config/opencode/opencode.json", "opencode.json in a project, for that project only"],
  config: opencodeConfig,
  run: "opencode",
  after: "In OpenCode, /models lists these models under ModelFabric.",
}];

const state = { app: "opencode", place: readSidePref("mfsh.connect.place") || "local", entry: null, meshAddr: "" };

export function initConnect() {
  $("connect-open")?.addEventListener("click", openConnect);
}

async function openConnect() {
  const dlg = $("connect-dialog");
  if (!dlg) return;
  draw();
  if (!dlg.open) dlg.showModal();
  // Which nodes take requests from outside, and this node's tailnet address,
  // come from each node's own list of listeners.
  const names = [selfNode, ...(meshView?.nodes ?? []).filter((n) => n.alive && n.name !== selfNode).map((n) => n.name)].filter(Boolean);
  const topos = await Promise.all(names.map((n) => fetchJSON(nodeAPI(n, "/api/v1/topology")).catch(() => null)));
  const self = topos.find((t) => t?.node === selfNode);
  state.meshAddr = (self?.listeners ?? []).find((l) => l.name === "mesh" && l.enabled)?.addr || "";
  const entry = entrypoints(topos)[0];
  state.entry = entry ? { node: entry } : null;
  if (dlg.open) draw();
}

function draw() {
  const body = $("connect-body");
  const app = APPS.find((a) => a.key === state.app) ?? APPS[0];
  const entry = state.entry ? { ...state.entry, url: readSidePref(`mfsh.try.public.${state.entry.node}`) || "" } : null;
  const front = { ...frontView, url: frontView.url || `${location.origin}/v1` };
  const places = connectPlaces(front, state.meshAddr, entry);
  const place = places.find((p) => p.key === state.place) ?? places[0];
  const models = servedModels(lastView?.meshEngines);
  $("connect-title").textContent = `Connect ${app.name}`;
  body.replaceChildren();

  // 1. where the app runs
  const where = el("section", "try-block");
  where.append(el("div", "try-head", `1. Where will ${app.name} run?`));
  const seg = el("div", "seg connect-seg");
  for (const p of places) {
    const b = el("button", "seg-btn" + (p.key === place.key ? " active" : ""), p.label);
    b.type = "button";
    b.addEventListener("click", () => { state.place = p.key; writeSidePref("mfsh.connect.place", p.key); draw(); });
    seg.append(b);
  }
  where.append(seg);
  if (place.note) where.append(el("div", "try-note", place.note));
  if (place.key === "public") {
    const row = el("label", "try-url");
    const input = el("input", "input mono");
    input.type = "url";
    input.placeholder = "https://api.example.com";
    input.value = entry?.url || "";
    input.addEventListener("change", () => { writeSidePref(`mfsh.try.public.${place.node}`, input.value.trim()); draw(); });
    row.append(el("span", null, "Public address"), input);
    where.append(row);
    if (!place.known) {
      where.append(el("div", "try-note", `ModelFabric does not know the address ${place.node} is published under, because the TLS proxy in front of it owns that name. Enter it once and it is remembered in this browser.`));
    }
  }
  body.append(where);

  // 2. the key, when that place needs one
  let step = 2;
  if (place.keyVar) {
    const key = el("section", "try-block");
    key.append(el("div", "try-head", `${step++}. Give it a key`), el("div", "try-note", place.keyHelp));
    key.append(cmd(`export ${place.keyVar}=<the token>   # in your shell profile, so ${app.name} finds it every time`));
    key.append(el("div", "try-note", `The file below names ${place.keyVar} and ${app.name} reads the key from your environment. The key itself is never written into the file.`));
    body.append(key);
  }

  // 3. the config file
  const file = el("section", "try-block");
  file.append(el("div", "try-head", `${step++}. Save this as ${app.files[0]}`));
  file.append(el("div", "try-note", `Or as ${app.files[1]}. If the file exists, add the "modelfabric" entry under "provider".`));
  if (!models.length) {
    file.append(el("div", "try-note connect-warn", "No model is loaded anywhere in the mesh, so the file lists none. Load one and open this again."));
  }
  file.append(cmd(app.config(place.base, models, place.keyVar)));
  const small = models.filter((m) => m.context > 0 && m.context < AGENT_MIN_CONTEXT);
  if (small.length) {
    file.append(el("div", "try-note connect-warn",
      `${small.map((m) => `${m.id} has ${m.context.toLocaleString()} tokens of context`).join("; ")}. A coding agent needs about 64,000 or more: it sends its tools and the files it has read with every request. Raise Context length in the model's Load settings.`));
  }
  if (models.some((m) => m.vision)) {
    file.append(el("div", "try-note", `"attachment" and "modalities" tell ${app.name} the model reads images. Without them it removes an attached image before sending, and the model answers that it does not support image input.`));
  }
  body.append(file);

  // 4. run it
  const run = el("section", "try-block");
  run.append(el("div", "try-head", `${step}. Start it`));
  run.append(cmd(`${app.install}   # once, if ${app.name} is not installed\n${app.run}`), el("div", "try-note", app.after));
  body.append(run);
}

// cmd is a block of text to copy, with its Copy button.
function cmd(text) {
  const wrap = el("div", "connect-cmd");
  const copy = el("button", "btn", "Copy");
  copy.type = "button";
  copy.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(text); showNotice("Copied", "success"); }
    catch (err) { showNotice(`Copy failed: ${err.message}`, "error"); }
  });
  wrap.append(el("pre", "try-cmd", text), copy);
  return wrap;
}
