import { showNotice } from "./actions.js";
import { $ } from "./core.js";
import { fetchJSON, nodeAPI, readSidePref, selfNode, writeSidePref } from "./my-models.js";
import { el, frontView, meshView } from "./rendering.js";
import { engineKind, entrypoints, publicTarget, tryTargets } from "./ui-model.js";

// "Try it": the curl command that calls a model, and a button that sends the
// same request from here. Opened from a model's name on the Serving page.
//
// Two commands, because they answer different questions. Through the mesh is
// what an app sends, and shows that the model is being served. To one engine
// shows that this engine, on this GPU, is answering: with two engines of a
// model on a node, a request through the mesh says nothing about the one it
// did not pick.
export function openTry(e) {
  const dlg = $("try-dialog");
  if (!dlg) return;
  const kind = engineKind(e.runtime, e.gpu, e.gpuMemory);
  $("try-title").textContent = `Try ${e.model}`;
  $("try-sub").textContent = `on ${e.node}${kind ? ` · ${kind}` : ""}`;
  const body = $("try-body");
  // The page's own address is the front door when the node did not say.
  const front = { ...frontView, url: frontView.url || `${location.origin}/v1` };
  body.replaceChildren(...tryTargets(front, e).map(block));
  if (!dlg.open) dlg.showModal();
  addPublic(body, e);
}

// addPublic adds, for each node that takes requests from outside, the
// command a caller out there sends. Which nodes those are comes from each
// node's own list of listeners, asked for now: the mesh's state does not
// carry it.
async function addPublic(body, e) {
  const shownFor = $("try-title").textContent;
  const names = [selfNode, ...(meshView?.nodes ?? []).filter((n) => n.alive && n.name !== selfNode).map((n) => n.name)].filter(Boolean);
  const topos = await Promise.all(names.map((n) => fetchJSON(nodeAPI(n, "/api/v1/topology")).catch(() => null)));
  // The dialog may have been closed, or opened for another engine, meanwhile.
  if (!$("try-dialog").open || $("try-title").textContent !== shownFor) return;
  for (const node of entrypoints(topos)) body.append(publicBlock(node, e));
}

function publicBlock(node, e) {
  const pref = `mfsh.try.public.${node}`;
  const box = el("section", "try-block");
  const draw = () => {
    const t = publicTarget(node, readSidePref(pref) || "", e.model);
    box.replaceChildren(el("div", "try-head", t.title), el("div", "try-note", t.note));
    // The name people outside use belongs to the TLS proxy in front of the
    // node, and the node is never told it. Typed once, it is remembered here.
    const row = el("label", "try-url");
    const input = el("input", "input mono");
    input.type = "url";
    input.placeholder = "https://api.example.com";
    input.value = readSidePref(pref) || "";
    input.addEventListener("change", () => { writeSidePref(pref, input.value.trim()); draw(); });
    row.append(el("span", null, "Public address"), input);
    box.append(row);
    if (!t.known) {
      box.append(el("div", "try-note", `ModelFabric does not know the address ${node} is published under, because the TLS proxy in front of it owns that name. Enter it above once and it is remembered in this browser.`));
    }
    const text = `${t.setup}\n${t.curl}`;
    box.append(el("pre", "try-cmd", text));
    const copy = el("button", "btn", "Copy");
    copy.type = "button";
    copy.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(text); showNotice("Copied", "success"); }
      catch (err) { showNotice(`Copy failed: ${err.message}`, "error"); }
    });
    const actions = el("div", "try-actions");
    actions.append(el("span", "try-note try-why", "Run it in a terminal: it needs a key, which this page does not hold."), copy);
    box.append(actions);
  };
  draw();
  return box;
}

function block(t) {
  const box = el("section", "try-block");
  box.append(el("div", "try-head", t.title), el("div", "try-note", t.note));
  const text = t.setup ? `${t.setup}\n${t.curl}` : t.curl;
  box.append(el("pre", "try-cmd", text));

  const out = el("div", "try-out");
  out.hidden = true;
  const copy = el("button", "btn", "Copy");
  copy.type = "button";
  copy.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(text); showNotice("Copied", "success"); }
    catch (err) { showNotice(`Copy failed: ${err.message}`, "error"); }
  });
  const send = el("button", "btn primary", "Send it now");
  send.type = "button";
  send.title = "Send this request from the browser and show the answer";
  send.addEventListener("click", () => run(t, send, out));
  const actions = el("div", "try-actions");
  actions.append(copy, send);
  box.append(actions, out);
  return box;
}

// run sends the request from the browser. Through the mesh that is this
// page's own address, which is the front door. To an engine it is another
// address; llama.cpp answers a browser from any origin, and an engine that
// does not, or cannot be reached from here, is reported as that.
async function run(t, btn, out) {
  btn.disabled = true;
  out.hidden = false;
  out.className = "try-out";
  out.replaceChildren(el("span", "muted", "Waiting for the answer…"));
  const url = t.key === "mesh" ? "/v1/chat/completions" : `${t.base}/chat/completions`;
  const started = performance.now();
  try {
    const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(t.body) });
    const raw = await r.text();
    const ms = Math.round(performance.now() - started);
    let data = null;
    try { data = JSON.parse(raw); } catch { /* shown as it came */ }
    const msg = data?.choices?.[0]?.message;
    const reply = msg?.content || "";
    // A reasoning model that ran out of tokens while thinking has no answer
    // yet; what it thought is shown, and said to be that.
    const thought = !reply && msg?.reasoning_content ? msg.reasoning_content : "";
    const facts = [`${r.status} in ${ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`}`];
    const by = [r.headers.get("X-Fabric-Node"), r.headers.get("X-Fabric-Engine")].filter(Boolean).join(" / ");
    if (by) facts.push(`answered by ${by}`);
    const u = data?.usage;
    if (u) facts.push(`${u.prompt_tokens ?? "?"} prompt tokens, ${u.completion_tokens ?? "?"} generated`);
    const tps = data?.timings?.predicted_per_second;
    if (tps) facts.push(`${tps.toFixed(1)} tok/s`);
    out.replaceChildren(el("div", "try-facts", facts.join(" · ")));
    if (!r.ok) {
      out.classList.add("bad");
      out.append(el("pre", "try-reply", data?.error?.message || raw.slice(0, 2000)));
      if (r.status === 401 && t.key === "mesh") {
        out.append(el("div", "try-note", "This address needs an API key, and the browser has none to send. Run the command above in a terminal."));
      }
    } else {
      if (thought) out.append(el("div", "try-note", "The model used every token thinking and had not started its answer. Its thinking:"));
      out.append(el("pre", "try-reply", reply || thought || raw.slice(0, 2000)));
    }
  } catch (err) {
    out.classList.add("bad");
    out.replaceChildren(el("div", "try-facts", "No answer"),
      el("div", "try-note", t.key === "mesh"
        ? `The request did not complete: ${err.message}`
        : `The browser could not reach ${t.base}. The engine may listen only on its own machine, or this computer may not be on the tailnet. The command above still works from a machine that can reach it.`));
  } finally {
    btn.disabled = false;
  }
}
