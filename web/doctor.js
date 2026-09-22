import { $ } from "./core.js";
import { fetchJSON, mm, nodeAPI, selfNode } from "./my-models.js";
import { el, meshView } from "./rendering.js";
import { checking } from "./runtime.js";

/* ---------- Doctor: the node's own checks, for this node or a peer ---------- */

// The report is not cached. A diagnosis is about the state right now, and a
// stale one is worse than none — the thing being diagnosed is usually changing
// while you look at it.
let doctorNode = "";
let doctorBusy = false;

export function renderDoctorNodes() {
  const sel = $("dr-node");
  if (!sel) return;
  // From the mesh view, which every page polls — mm.nodes only fills while My
  // Models is open, so the picker was empty on arrival here.
  const peers = (meshView?.nodes ?? [])
    .filter((n) => n.alive && n.name && n.name !== selfNode)
    .map((n) => n.name)
    .sort();
  const names = [selfNode, ...peers].filter(Boolean);
  if (names.length === 0) return;
  const chosen = doctorNode || selfNode;
  sel.replaceChildren();
  for (const n of names) {
    if (!n) continue;
    const o = el("option", null, n === selfNode ? `${n} (this node)` : n);
    o.value = n;
    o.selected = n === chosen;
    sel.append(o);
  }
  doctorNode = chosen;
}

export async function runDoctor() {
  const body = $("dr-body");
  const empty = $("dr-empty");
  if (!body || doctorBusy) return;
  doctorBusy = true;
  $("dr-run").disabled = true;
  $("dr-hint").textContent = `checking ${doctorNode || "this node"}…`;
  try {
    const r = await fetchJSON(nodeAPI(doctorNode || selfNode, "/api/v1/doctor"));
    renderDoctor(r.checks ?? []);
    empty.hidden = true;
  } catch (err) {
    body.replaceChildren();
    empty.hidden = false;
    // A peer that cannot answer is its own finding: an older build has no
    // such endpoint, and one that is down cannot be asked at all.
    empty.textContent = /no such endpoint/i.test(err.message)
      ? `${doctorNode} runs a build without health checks — open its own dashboard, or update it.`
      : `Could not reach ${doctorNode}: ${err.message}`;
  } finally {
    doctorBusy = false;
    $("dr-run").disabled = false;
  }
}

function renderDoctor(checks) {
  const body = $("dr-body");
  body.replaceChildren();
  let ok = 0, warn = 0, fail = 0;
  let section = "";
  for (const c of checks) {
    if (c.section !== section) {
      section = c.section;
      body.append(el("div", "section-head", section));
    }
    const row = el("div", "dr-row dr-" + c.status);
    row.append(el("span", "dr-mark", { ok: "✓", warn: "!", fail: "✗" }[c.status] ?? "·"));
    row.append(el("span", "dr-name", c.name));
    const detail = el("span", "dr-detail");
    detail.append(el("span", null, c.detail));
    // The fix is shown only where something needs doing, as in the CLI.
    if (c.fix && (c.status === "warn" || c.status === "fail")) {
      const fix = el("div", "dr-fix");
      fix.append(el("span", "dr-arrow", "→"), el("code", null, c.fix));
      detail.append(fix);
    }
    row.append(detail);
    body.append(row);
    if (c.status === "ok") ok++;
    else if (c.status === "warn") warn++;
    else if (c.status === "fail") fail++;
  }
  const parts = [`${ok} ok`];
  if (warn) parts.push(`${warn} warning${warn === 1 ? "" : "s"}`);
  if (fail) parts.push(`${fail} problem${fail === 1 ? "" : "s"}`);
  $("dr-hint").textContent = parts.join(", ") + (warn || fail ? "" : " — ready");
}

export function initDoctor() {
  $("dr-run")?.addEventListener("click", runDoctor);
  $("dr-node")?.addEventListener("change", (e) => {
    doctorNode = e.target.value;
    runDoctor();
  });
}
