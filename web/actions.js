import { $ } from "./core.js";
import { tick } from "./polling.js";
import { el } from "./rendering.js";

/* ---------- actions ---------- */

let noticeTimer = null;

export function showNotice(message, tone = "info") {
  const el = $("notice");
  el.textContent = message;
  el.dataset.tone = tone;
  el.hidden = false;
  clearTimeout(noticeTimer);
  // Errors stay put; a load failure is worth reading properly.
  if (tone !== "error") noticeTimer = setTimeout(() => (el.hidden = true), 4500);
}

// The preferred-node control: LM Link's "Preferred Device" toggle.
export function preferCell(n) {
  const td = el("td");
  if (n.preferred) {
    td.append(el("span", "pill fit-yes", "Preferred"));
    const clear = el("button", "btn", "Clear");
    clear.title = "Stop preferring this node";
    clear.addEventListener("click", () => setPreferred(""));
    td.append(" ", clear);
  } else if (n.alive) {
    const pick = el("button", "btn", "Prefer");
    pick.title = `Send requests to ${n.name} first when it has the model`;
    pick.addEventListener("click", () => setPreferred(n.name));
    td.append(pick);
  } else {
    td.append(el("span", "muted", "—"));
  }
  return td;
}

async function setPreferred(node) {
  try {
    await post("/z/preferred", { node });
    showNotice(node ? `${node} is now the preferred node.` : "Preference cleared; the least busy node serves.");
    tick();
  } catch (err) {
    showNotice(`Could not change the preferred node: ${err.message}`, "error");
  }
}

export async function post(path, body) {
  const resp = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const text = await resp.text();
  let data = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    // Non-JSON error bodies still deserve a readable message.
  }
  if (!resp.ok) {
    throw new Error(data?.error?.message || text || `HTTP ${resp.status}`);
  }
  return data;
}
