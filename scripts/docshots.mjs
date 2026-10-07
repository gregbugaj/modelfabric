#!/usr/bin/env node
// Capture scrubbed dashboard screenshots using Chromium and Node 22+.
//
//   node scripts/docshots.mjs [http://127.0.0.1:1234] [site/public/img]
//
// Replace private hostnames and tailnet addresses before capture, as required
// by AGENTS.md. Load models and run node and cluster benchmarks beforehand.
// The script enables capture for sample requests and creates a docs-example
// token, then disables capture and revokes the token. Settings are not saved.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const base = process.argv[2] || "http://127.0.0.1:1234";
const out = process.argv[3] || "site/public/img";
const port = 9400 + Math.floor(Math.random() * 400);
const profile = mkdtempSync(join(tmpdir(), "mfsh-docshots-"));
const chrome = spawn(process.env.CHROMIUM || "chromium",
  ["--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, "about:blank"], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Runs in the page. Names are replaced as written; every tailnet address is
// given the next 100.64.0.x in the order it is first seen, so the same node
// keeps the same address across the page. A MutationObserver repeats it,
// because the page redraws itself as the mesh reports in.
const scrub = `(() => {
  const names = [["sites-01", "entrypoint-01"], ["gregbugaj.com", "api.example.com"], ["/home/greg", "~"]];
  const addrs = new Map();
  const fix = (s) => {
    for (const [a, b] of names) s = s.split(a).join(b);
    return s.replace(/\\b100\\.(?!64\\.0\\.)\\d{1,3}\\.\\d{1,3}\\.\\d{1,3}\\b/g, (m) => {
      if (!addrs.has(m)) addrs.set(m, "100.64.0." + (addrs.size + 1));
      return addrs.get(m);
    });
  };
  const run = () => {
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      const f = fix(n.nodeValue);
      if (f !== n.nodeValue) n.nodeValue = f;
    }
    for (const e of document.querySelectorAll("[title]")) {
      const f = fix(e.title);
      if (f !== e.title) e.title = f;
    }
    for (const e of document.querySelectorAll("input, textarea")) {
      const f = fix(e.value);
      if (f !== e.value) e.value = f;
    }
  };
  run();
  new MutationObserver(run).observe(document.body, { childList: true, subtree: true, characterData: true });
})()`;

const LEAK = String.raw`sites-01|gregbugaj|\b100\.(?!64\.0\.)\d+\.\d+\.\d+`;

try {
  let tab;
  for (let i = 0; i < 40 && !tab; i++) {
    await sleep(250);
    try {
      tab = await (await fetch(`http://127.0.0.1:${port}/json/new?${base}/`, { method: "PUT" })).json();
    } catch { /* not listening yet */ }
  }
  if (!tab) throw new Error("chromium did not start; set CHROMIUM to its path");
  const ws = new WebSocket(tab.webSocketDebuggerUrl);
  let id = 0;
  const wait = new Map();
  ws.onmessage = (m) => { const d = JSON.parse(m.data); wait.get(d.id)?.(d); };
  const send = (method, params = {}) => new Promise((r) => { wait.set(++id, r); ws.send(JSON.stringify({ id, method, params })); });
  const ev = async (e) => {
    const r = (await send("Runtime.evaluate", { expression: e, awaitPromise: true, returnByValue: true })).result;
    if (r.exceptionDetails) throw new Error(`in the page: ${r.exceptionDetails.exception?.description || r.exceptionDetails.text}\n  ${e.slice(0, 120)}`);
    return r.result.value;
  };
  await new Promise((r) => (ws.onopen = r));

  await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 1100, deviceScaleFactor: 2, mobile: false });
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
  await send("Page.reload");
  await sleep(3000);
  await ev(scrub);
  // A capture taller than the window is laid out without the scrollbar, so a
  // box measured with one was 15px too narrow and cut the right-hand column.
  await ev(`document.head.insertAdjacentHTML("beforeend", "<style>html{scrollbar-width:none}::-webkit-scrollbar{display:none}</style>")`);
  await sleep(500);

  const shoot = async (sel, file, maxHeight = 2200) => {
    await sleep(1800); // a redraw, then the scrub that follows it
    const q = JSON.stringify(sel);
    const box = await ev(`(() => { const e = document.querySelector(${q}); if (!e) return null;
      const r = e.getBoundingClientRect();
      // Crop flyouts to their content rather than their full window height.
      let h = Math.max(r.height, e.scrollHeight);
      if (e.classList.contains("side-panel")) {
        let end = r.top;
        // Measure visible text and controls; container height includes empty panel space.
        for (const c of e.querySelectorAll("*")) {
          const shows = c.matches("input, textarea, select, button, pre") ||
            [...c.childNodes].some((t) => t.nodeType === 3 && t.nodeValue.trim());
          const b = c.getBoundingClientRect();
          if (shows && b.width && b.height) end = Math.max(end, b.bottom);
        }
        h = Math.min(h, end - r.top + 24);
      }
      return { x: r.x + scrollX, y: r.y + scrollY, width: r.width, height: Math.min(h, ${maxHeight}) }; })()`);
    if (!box || !box.width) throw new Error(`${file}: nothing visible matches ${sel}`);
    const leak = await ev(`(() => { const e = document.querySelector(${q});
      const text = e.innerText + " " + [...e.querySelectorAll("input, textarea")].map((i) => i.value).join(" ");
      return (text.match(new RegExp(${JSON.stringify(LEAK)}, "g")) || []).join(" "); })()`);
    if (leak) throw new Error(`${file} is not scrubbed: ${leak}`);
    const s = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true, clip: { ...box, scale: 1 } });
    writeFileSync(join(out, file), Buffer.from(s.result.data, "base64"));
    console.log(`${file}  ${Math.round(box.width)}x${Math.round(box.height)}`);
  };
  const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)}).click()`);
  const page = async (name) => { await click(`.rail a[href="#${name}"]`); await sleep(1500); };
  const api = (path, method = "GET", body) => fetch(base + path, { method,
    headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  const view = (name) => `section[data-view="${name}"]`;

  await page("overview");
  await shoot(view("overview"), "dash-overview.png");

  await page("mesh");
  await click('#mesh-view [data-view="arch"]');
  await shoot(".mesh-panel", "mesh-architecture.png");
  await click('#mesh-view [data-view="star"]');
  await shoot(".mesh-panel", "mesh-constellation.png");

  await page("local");
  await shoot(view("local"), "dash-local.png");
  // Discover reads Hugging Face, so its list and the model card take a moment.
  await click("#dv-open");
  await sleep(6000);
  await shoot("#dv-dialog", "dash-discover.png", 1000);
  await ev(`document.getElementById("dv-dialog").close()`);
  await page("models");
  await shoot(view("models"), "dash-models.png");
  await page("routing");
  await shoot(view("routing"), "dash-routing.png");
  await page("runtime");
  await shoot(view("runtime"), "dash-runtime.png");

  await page("bench");
  await sleep(2500);
  await shoot(view("bench"), "dash-bench.png");
  await click('[data-bn-tab="cluster"]');
  await sleep(2500);
  await shoot(view("bench"), "dash-bench-cluster.png");

  await page("activity");
  const models = (await (await api("/v1/models")).json()).data ?? [];
  const model = (models.find((m) => !/embed/i.test(m.id)) ?? {}).id;
  if (!model) throw new Error("no chat model is loaded: the Activity pages would be empty");
  const capturing = () => ev(`document.getElementById("act-bodies").checked`);
  const was = await capturing();
  if (!was) await click("#act-bodies");
  await sleep(800);
  try {
    for (const q of ["Name three primary colours.", "What is the capital of France?", "Reply with exactly: ok"]) {
      await api("/v1/chat/completions", "POST", { model, max_tokens: 200, messages: [{ role: "user", content: q }],
        chat_template_kwargs: { enable_thinking: false } });
    }
    await sleep(2500);
    await shoot(view("activity"), "dash-activity.png", 760);
    await click('#act-scope [data-scope="mesh"]');
    await sleep(3500);
    await shoot(view("activity"), "dash-activity-mesh.png", 820);
    await click('#act-scope [data-scope="node"]');
    await sleep(1500);
    await click(`${view("activity")} .act-tab[data-tab="requests"] tbody tr`);
    await sleep(1200);
    await shoot("#act-side", "dash-activity-detail.png");
  } finally {
    if (!was && await capturing()) await click("#act-bodies");
  }

  // Shorten the window to keep the flyout Save bar near its content.
  await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 760, deviceScaleFactor: 2, mobile: false });
  await page("overview");
  await click("#ss-open");
  await sleep(1200);
  const tabTo = (name) => click(`#ss-side .side-tab[data-tab="${name}"]`);
  await tabTo("server");
  await shoot("#ss-side", "server-settings-server.png");
  await tabTo("loading");
  await shoot("#ss-side", "server-settings-loading.png");
  await tabTo("mcp");
  await shoot("#ss-side", "server-settings-mcp.png");
  // Last of the saved tabs: the unsaved CORS example shows in the Save bar.
  await tabTo("access");
  await ev(`(() => { const row = (k) => document.querySelector('#ss-side [data-key="' + k + '"]');
    const on = row("cors_on").querySelector("input"); if (!on.checked) on.click();
    const t = row("cors_origins").querySelector("textarea"); t.value = "http://localhost:5173";
    t.dispatchEvent(new Event("input", { bubbles: true })); })()`);
  await shoot("#ss-side", "server-settings-access.png");
  await tabTo("tokens");
  await sleep(800);
  try {
    await ev(`(() => { const f = document.querySelector("#ss-side .tk-create"); f.querySelector("input").value = "docs-example"; f.requestSubmit(); })()`);
    await shoot("#ss-side", "server-settings-tokens.png");
  } finally {
    await api("/api/v1/tokens/revoke", "POST", { id: "docs-example" });
  }
  ws.close();
} finally {
  chrome.kill();
  await sleep(300);
  rmSync(profile, { recursive: true, force: true });
}
