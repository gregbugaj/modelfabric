#!/usr/bin/env node
// Captures every dashboard screenshot the docs use, scrubbed.
//
//   node scripts/docshots.mjs [http://127.0.0.1:1234] [site/public/img]
//
// Docs screenshots must not carry a real tailnet address or the entrypoint's
// real hostname (AGENTS.md), and are re-captured rather than edited by hand.
// This does both: it drives a headless Chromium at a running node, rewrites
// those strings in the page as it renders, and saves each page. Needs Node
// 22+ (for WebSocket) and chromium on PATH.
//
// It photographs the node as it is, so set the scene first: load models, and
// run `mfsh bench -quick` and `mfsh bench -cluster -quick` for the Benchmark
// tabs to have results. On the way it makes a few chat requests with capture
// switched on (for the Activity pages) and creates a token named
// "docs-example" (for the Tokens tab); it switches capture back off and
// revokes the token before it exits. It saves no settings.

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

// What must never be in a saved image's text.
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

  // shoot saves the element sel matches. A leak anywhere in it stops the run:
  // a screenshot that slipped through would be published.
  const shoot = async (sel, file, maxHeight = 2200) => {
    await sleep(1800); // a redraw, then the scrub that follows it
    const q = JSON.stringify(sel);
    const box = await ev(`(() => { const e = document.querySelector(${q}); if (!e) return null;
      const r = e.getBoundingClientRect();
      // A flyout is as tall as the window whatever it holds: cut it where
      // its content ends rather than photograph the empty panel below.
      let h = Math.max(r.height, e.scrollHeight);
      if (e.classList.contains("side-panel")) {
        let end = r.top;
        // Measured by what can be seen (text and controls): the containers
        // around them stretch to the panel's full height.
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

  // Activity: switch capture on and make a few requests of our own, so the
  // rows and the bodies shown are this script's and nobody's real prompts.
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

  // Server settings. The CORS rows are filled in to show the tab in use and
  // are not saved; the token is real, so it is revoked below.
  // A shorter window for the flyout: its Save bar sits at the bottom of the
  // panel, a long way below a tab with three rows on it.
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
