// API tokens: named, created and revoked one at a time. Rendered as the
// Tokens tab of the Server settings flyout.

import { post, showNotice } from "./actions.js";
import { fetchJSON } from "./my-models.js";
import { el, frontView } from "./rendering.js";
import { buildTokens } from "./ui-model.js";

// Where a token is actually asked for. Creating one on a node that checks
// nothing would otherwise look like it protected something.
function scopeNote() {
  const where = [];
  if (frontView.requireKey) where.push("this address");
  if (frontView.publicListen) where.push(`the public listener (${frontView.publicListen})`);
  const note = el("div", "muted side-note");
  if (where.length) {
    note.append("Checked on " + where.join(" and ") + ", for inference.");
  } else {
    note.append("No listener asks for a key yet. Turn on ", el("b", null, "Require Authentication"),
      " under Access, or set a public listener, for tokens to matter.");
  }
  return note;
}

/**
 * Render the token manager into body. The secret of a token just created is
 * shown once, in this render only: re-rendering (switching tabs, closing the
 * flyout) drops it, and nothing can show it again.
 */
export async function renderTokens(body, justCreated = null) {
  body.replaceChildren(scopeNote());

  const form = el("form", "tk-create");
  const name = el("input", "input");
  name.placeholder = "Name, e.g. laptop, ci, opencode";
  name.maxLength = 64;
  name.autocomplete = "off";
  name.setAttribute("aria-label", "Token name");
  const create = el("button", "btn primary sm", "Create");
  create.type = "submit";
  form.append(name, create);
  body.append(form);

  const err = el("div", "err-text");
  body.append(err);

  if (justCreated) {
    const box = el("div", "tk-secret");
    // A token's secret is not kept; the node key is, in the node's key file.
    box.append(el("div", "tk-secret-head", justCreated.nodeKey
      ? "The new node key. Give it to the apps that used the old one; mfsh key on this node prints it again."
      : `Copy "${justCreated.name}" now. It is not stored and cannot be shown again.`));
    const row = el("div", "tk-secret-row");
    const code = el("code", null, justCreated.token);
    const copy = el("button", "btn sm", "Copy");
    copy.type = "button";
    copy.addEventListener("click", () => copySecret(code));
    row.append(code, copy);
    box.append(row);
    body.append(box);
  }

  const list = el("div", "tk-list");
  body.append(list);

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.textContent = "";
    if (!name.value.trim()) { err.textContent = "Name it after what will use it."; name.focus(); return; }
    try {
      const t = await post("/api/v1/tokens", { name: name.value.trim() });
      await renderTokens(body, t);
    } catch (e2) {
      err.textContent = e2.message;
    }
  });

  let api;
  try {
    api = await fetchJSON("/api/v1/tokens");
  } catch (e) {
    err.textContent = `Could not read the tokens: ${e.message}`;
    return;
  }
  for (const row of buildTokens(api)) list.append(tokenRow(row, body));
  if (!(api?.tokens ?? []).length) {
    list.append(el("div", "muted side-note", "No named tokens yet. Create one per app, so each can be revoked alone."));
  }
}

function tokenRow(row, body) {
  const r = el("div", "tk-row" + (row.builtin ? " tk-builtin" : ""));
  const main = el("div", "tk-main");
  main.append(el("span", "tk-name", row.name), el("span", "mono tk-mask", row.masked));
  const meta = el("div", "tk-meta");
  if (row.builtin) {
    meta.append("printed by ", el("code", null, "mfsh key"), " · rotated, not revoked: a node always has one");
  } else {
    meta.append(`created ${row.created} · last used ${row.lastUsed}`);
  }
  r.append(main, meta);
  if (row.builtin && row.rotatable) {
    const b = el("button", "btn sm danger", "Rotate");
    b.type = "button";
    b.addEventListener("click", async () => {
      if (!confirm("Replace the node key? Every app using it is refused from its next request until it is given the new one. Named tokens keep working.")) return;
      try {
        const k = await post("/api/v1/key/rotate", {});
        showNotice("Node key rotated", "success");
        await renderTokens(body, { name: "Node key", token: k.key, nodeKey: true });
      } catch (e) {
        showNotice(e.message, "error");
      }
    });
    main.append(b);
  }
  if (!row.builtin) {
    const b = el("button", "btn sm danger", "Revoke");
    b.type = "button";
    b.addEventListener("click", async () => {
      if (!confirm(`Revoke "${row.name}"? Apps using it are refused from their next request.`)) return;
      try {
        await post("/api/v1/tokens/revoke", { id: row.id });
        showNotice(`Revoked ${row.name}`, "success");
        await renderTokens(body);
      } catch (e) {
        showNotice(e.message, "error");
      }
    });
    main.append(b);
  }
  return r;
}

async function copySecret(code) {
  try {
    await navigator.clipboard.writeText(code.textContent);
    showNotice("Token copied", "success");
  } catch {
    // Clipboard access is denied outside a secure context; select it so the
    // keyboard shortcut still works.
    const r = document.createRange();
    r.selectNodeContents(code);
    const sel = getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
    showNotice("Press Ctrl+C to copy", "info");
  }
}
