import { test } from "node:test";
import assert from "node:assert/strict";
import {
  quantRows,
  buildView,
  relativeTime,
  platformBadge,
  kvBadge,
  presetDrift,
  groupByModel,
  buildTokens,
  settingsChanges,
  pendingRestart,
  wideningWarnings,
  serverToForm,
  formToServer,
  displayPath,
  benchTables,
  clusterTables,
  spreadText,
  ROUTER_SETTINGS,
  routerToForm,
  formToRouter,
  routedBy,
} from "./ui-model.js";

const mesh = {
  self: {
    node: "xpredator",
    models: ["qwen3.8-27b", "nomic-embed"],
    inflight: 2,
    engines: [
      { name: "gpu0", healthy: true, models: ["qwen3.8-27b"], inflight: 2 },
      { name: "gpu1", healthy: false, models: [], inflight: 0, error: "connection refused" },
    ],
    updated: "2026-09-17T19:00:00Z",
  },
  peers: [
    { node: "predator", addr: "100.91.190.79", alive: true, models: ["qwen3.8-27b"], inflight: 1, last_seen: "2026-09-17T19:00:00Z" },
    { node: "sites-01", addr: "100.69.171.44", alive: false, models: [], inflight: 9, last_seen: "2026-09-17T18:00:00Z" },
  ],
  models: [
    { id: "qwen3.8-27b", nodes: ["predator", "xpredator"] },
    { id: "nomic-embed", nodes: ["xpredator"] },
  ],
};

test("counts only live nodes as online", () => {
  const v = buildView(mesh);
  assert.equal(v.stats.nodesOnline, 2);
  assert.equal(v.stats.nodesTotal, 3);
});

test("excludes dead peers from in-flight total", () => {
  // Exclude the down peer's last reported load to avoid stale totals.
  assert.equal(buildView(mesh).stats.inflight, 3);
});

test("orders self first, then live nodes, then dead", () => {
  const names = buildView(mesh).nodes.map((n) => n.name);
  assert.deepEqual(names, ["xpredator", "predator", "sites-01"]);
});

test("marks the local node", () => {
  const [self] = buildView(mesh).nodes;
  assert.equal(self.isSelf, true);
  assert.equal(self.addr, "local");
});

test("reports engine health", () => {
  const v = buildView(mesh);
  assert.equal(v.stats.enginesHealthy, 1);
  assert.equal(v.stats.enginesTotal, 2);
  assert.equal(v.engines[1].error, "connection refused");
});

test("counts replicas per model", () => {
  const v = buildView(mesh);
  assert.equal(v.models.find((m) => m.id === "qwen3.8-27b").replicas, 2);
  assert.equal(v.models.find((m) => m.id === "nomic-embed").replicas, 1);
});

test("survives an empty or partial payload", () => {
  for (const input of [{}, { self: {} }, { peers: null, models: undefined }]) {
    const v = buildView(input);
    assert.equal(v.stats.models, 0);
    assert.equal(v.nodes.length, 1);
    assert.deepEqual(v.engines, []);
  }
});

test("relativeTime formats and handles missing values", () => {
  const now = Date.parse("2026-09-17T19:00:00Z");
  assert.equal(relativeTime("2026-09-17T18:59:56Z", now), "4s ago");
  assert.equal(relativeTime("2026-09-17T18:57:00Z", now), "3m ago");
  assert.equal(relativeTime("2026-09-17T17:00:00Z", now), "2h ago");
  assert.equal(relativeTime(null, now), "—");
  assert.equal(relativeTime("not a date", now), "—");
  assert.equal(relativeTime("0001-01-01T00:00:00Z", now), "—");
});

import { buildLocal, formatBytes } from "./ui-model.js";

const api = {
  models: [
    {
      key: "ggml-org/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M",
      type: "llm", display_name: "Qwen3.8-27B-Q4_K_M",
      params_string: "27B", quantization: "Q4_K_M", size_bytes: 19_000_000_000,
      capabilities: ["vision"],
      loaded_instances: [{ id: "inst-abc", config: { context_length: 4096 } }],
    },
    {
      key: "nomic/nomic-embed-text-v1.5.Q8_0",
      type: "embedding", quantization: "Q8_0", size_bytes: 100_000,
      loaded_instances: [],
    },
  ],
};

test("distinguishes a route-only node from one with no models", () => {
  // No supervisor at all: /api/v1/models 404s and we get null.
  assert.equal(buildLocal(null).supervised, false);
  const empty = buildLocal({ models: [] });
  assert.equal(empty.supervised, true);
  assert.deepEqual(empty.models, []);
});

test("reports loaded state and instances", () => {
  const v = buildLocal(api);
  const [qwen, nomic] = v.models;
  assert.equal(qwen.loaded, true);
  assert.equal(qwen.instances[0].id, "inst-abc");
  assert.equal(qwen.vision, true);
  assert.equal(nomic.loaded, false);
  assert.equal(nomic.vision, false);
  assert.equal(v.loadedCount, 1);
});

// An in-flight operation must block duplicate model loads.
test("marks a model busy while an operation is running", () => {
  const ops = [
    { kind: "load", model: "nomic/nomic-embed-text-v1.5.Q8_0", state: "running", message: "starting engine" },
    { kind: "load", model: "ggml-org/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M", state: "succeeded" },
  ];
  const v = buildLocal(api, ops);
  const nomic = v.models.find((m) => m.key.startsWith("nomic/"));
  assert.equal(nomic.busy, true);
  assert.equal(nomic.busyKind, "load");
  assert.equal(nomic.busyMessage, "starting engine");
  assert.equal(v.models[0].busy, false);
});

test("tolerates a malformed payload", () => {
  for (const bad of [undefined, {}, { models: null }, { models: [{}] }]) {
    const v = buildLocal(bad);
    assert.ok(Array.isArray(v.models));
  }
});

test("formatBytes is readable at model scale", () => {
  assert.equal(formatBytes(19_000_000_000), "17.7GB");
  assert.equal(formatBytes(0), "—");
  assert.equal(formatBytes(512), "512B");
});

import { buildRuntime } from "./ui-model.js";

const runtimesApi = {
  selection: "auto",
  can_install: true,
  hardware: {
    cpu: "AMD Ryzen 9", memory_mb: 65536, cuda_version: "13.0", vulkan: true,
    gpus: [{ name: "NVIDIA GeForce RTX 5090", memory_mb: 32607, compute_cap: "12.0", driver: "580.173.02" }],
  },
  runtimes: [
    { name: "llama.cpp-linux-x86_64-nvidia-cuda12-avx2@2.41.0", origin: "lmstudio", backend: "cuda", fit: "yes", llama_build: 11026 },
    { name: "llama.cpp-linux-x86_64-nvidia-cuda-avx2@2.31.2", origin: "lmstudio", backend: "cuda", fit: "no",
      reasons: ["built for compute 6.1/7.0, NVIDIA GeForce RTX 5090 is 12.0"] },
    { name: "llama.cpp-upstream-linux-x86_64-cuda-12.8@b11040", origin: "upstream", backend: "cuda", fit: "yes",
      llama_build: 11040, default: true, managed: true, in_use: ["inst-1"] },
    { name: "llama.cpp-upstream-linux-x86_64-cuda-12.8@b11039", origin: "upstream", backend: "cuda", fit: "yes",
      llama_build: 11039, managed: true },
  ],
};

test("runtime view: default first, then newest engine", () => {
  const v = buildRuntime(runtimesApi);
  assert.deepEqual(v.runtimes.map((r) => r.build), ["b11040", "b11039", "b11026", ""]);
  assert.equal(v.selection.auto, true);
  assert.equal(v.selection.name, "llama.cpp-upstream-linux-x86_64-cuda-12.8@b11040");
});

test("runtime view: removal only for ModelFabric builds not in use; never select a misfit", () => {
  const byBuild = Object.fromEntries(buildRuntime(runtimesApi).runtimes.map((r) => [r.build || r.version, r]));
  assert.equal(byBuild.b11040.canRemove, false, "in use");
  assert.equal(byBuild.b11039.canRemove, true);
  assert.equal(byBuild.b11026.canRemove, false, "LM Studio's package");
  assert.equal(byBuild.b11026.source, "LM Studio");
  assert.equal(byBuild["2.31.2"].canSelect, false, "does not fit");
  assert.match(byBuild["2.31.2"].reasons[0], /compute/);
});

test("runtime view: a pinned default cannot be pinned again", () => {
  const pinned = { ...runtimesApi, selection: "llama.cpp-upstream-linux-x86_64-cuda-12.8@b11040" };
  const v = buildRuntime(pinned);
  assert.equal(v.selection.auto, false);
  assert.equal(v.runtimes[0].canSelect, false);
});

test("runtime view: install options dedupe the recommendation and skip errors", () => {
  const available = {
    newest_build: "b11041",
    options: {
      recommended: { name: "x-cuda-12.8@b11041", backend: "cuda", build: "b11041", download_bytes: 168e6, reason: "RTX 5090 found" },
      cuda: { name: "x-cuda-12.8@b11041", backend: "cuda", build: "b11041" },
      vulkan: { name: "x-vulkan@b11041", backend: "vulkan", build: "b11041", download_bytes: 29e6 },
      rocm: { error: "no complete rocm build" },
    },
    updates: [{ family: "x-cuda-12.8", installed: 11040, available: true, latest: { name: "x-cuda-12.8@b11041", build: "b11041" } }],
  };
  const ops = [{ id: "op1", kind: "runtime-get", state: "running", model: "x-vulkan@b11041", message: "downloading engine", fraction: 0.5 }];
  const v = buildRuntime(runtimesApi, available, ops);
  assert.deepEqual(v.options.map((o) => o.key), ["recommended", "vulkan"]);
  assert.equal(v.options[1].installing, true);
  assert.equal(v.installing[0].fraction, 0.5);
  assert.equal(v.updates[0].installed, "b11040");
  assert.equal(v.updates[0].latest, "b11041");
});

test("runtime view: a route-only node has nothing to manage", () => {
  assert.equal(buildRuntime(null).managed, false);
});

import { buildRouting } from "./ui-model.js";

const profilesApi = { profiles: [
  { name: "load-aware", title: "Load + prefix (classic)", available: true, summary: "s" },
  { name: "optimized-baseline", title: "Optimized Baseline", available: true, summary: "s", well_lit_path: "https://x/ob" },
  { name: "tuned", title: "Optimized Baseline + fan-out", available: true, experimental: true, summary: "s" },
  { name: "pd-disaggregation", title: "Prefill/Decode Disaggregation", available: false, reason: "needs KV transfer" },
] };

test("routing: llm-d running with a profile", () => {
  const llmd = { state: "running", installed: true, model: "qwen/qwen3.8-27b", profile: "tuned",
                 peak_prefill_tok_s: 1900, calibrated: true, endpoints: ["127.0.0.1:18000", "100.100.69.3:18000"] };
  const v = buildRouting(llmd, profilesApi, [], ["qwen/qwen3.8-27b", "qwen/qwen3-0.6b", "qwen/qwen3.8-27b"]);
  assert.equal(v.llmd.profileTitle, "Optimized Baseline + fan-out");
  assert.equal(v.profiles.find((p) => p.active).name, "tuned");
  assert.equal(v.profiles.find((p) => p.name === "tuned").experimental, true);
  assert.deepEqual(v.models, ["qwen/qwen3-0.6b", "qwen/qwen3.8-27b"], "deduped and sorted");
  assert.equal(v.unavailable[0].reason, "needs KV transfer");
});

test("routing: llm-d off shows no active profile; install progress surfaces", () => {
  const v = buildRouting({ state: "disabled", installed: false }, profilesApi,
    [{ kind: "llmd-install", state: "running", message: "downloading EPP", fraction: 0.4 }]);
  assert.equal(v.llmd.on, false);
  assert.equal(v.profiles.some((p) => p.active), false);
  assert.equal(v.installing.fraction, 0.4);
  // A 501 llm-d response must disable scheduling controls.
  assert.equal(buildRouting(null).available, false);
});

import { parseSettingsForm, settingsToForm, PRESET_FIELDS } from "./ui-model.js";

test("settings form: empty inherits, values parse, bad input is flagged", () => {
  const { settings, errors } = parseSettingsForm({
    context_length: "16384", temperature: "0.6", flash_attention: "off", spec_mode: "mtp",
    top_k: "", gpu_layers: "lots", extra_args: "  --no-warmup  --mlock ",
  });
  assert.deepEqual(settings, { context_length: 16384, temperature: 0.6, flash_attention: false,
    spec_mode: "mtp", extra_args: ["--no-warmup", "--mlock"] });
  assert.deepEqual(errors, { gpu_layers: "whole number" });
});

test("settings form round-trips saved settings", () => {
  const saved = { context_length: 8192, kv_offload: false, temperature: 1, extra_args: ["--no-warmup"] };
  assert.deepEqual(parseSettingsForm(settingsToForm(saved)).settings, saved);
});

test("presets offer inference fields only", () => {
  const keys = PRESET_FIELDS.map((f) => f.key);
  assert.ok(keys.includes("temperature") && keys.includes("enable_thinking"));
  assert.ok(!keys.includes("context_length") && !keys.includes("gpu_layers"));
});

test("buildView marks the preferred node, and whether it is online", () => {
  const mesh = {
    self: { node: "xpredator", models: [] },
    peers: [
      { node: "minion", addr: "100.100.69.3", alive: true, models: ["q"] },
      { node: "mac", addr: "100.1.1.1", alive: false, models: [] },
    ],
    models: [],
    preferred_node: "minion",
  };
  let v = buildView(mesh);
  assert.equal(v.preferred, "minion");
  assert.equal(v.preferredOnline, true);
  assert.deepEqual(v.nodes.filter((n) => n.preferred).map((n) => n.name), ["minion"]);

  v = buildView({ ...mesh, preferred_node: "mac" });
  assert.equal(v.preferredOnline, false, "a preferred node that is offline falls back");

  v = buildView({ ...mesh, preferred_node: undefined });
  assert.equal(v.preferred, "");
  assert.ok(v.nodes.every((n) => !n.preferred));
});

import { buildFront } from "./ui-model.js";

// An unreadable front-door endpoint must not imply "no key needed".
test("buildFront says where apps connect and what it asks of them", () => {
  const f = buildFront({ listen: "127.0.0.1:1234", require_api_key: true, public_listen: "100.69.171.44:1235" });
  assert.equal(f.url, "http://127.0.0.1:1234/v1");
  assert.equal(f.requireKey, true);
  assert.equal(f.publicListen, "100.69.171.44:1235");
  const loopback = buildFront({ listen: "127.0.0.1:1234", require_api_key: false, public_listen: "" });
  assert.equal(loopback.requireKey, false);
  assert.equal(loopback.publicListen, "");
  // The caller may use its own origin, but authentication remains unknown.
  assert.deepEqual(buildFront(null), { url: "", requireKey: false, publicListen: "" });
});

import { buildCatalog, filterCatalog, inheritedValue } from "./ui-model.js";

test("buildCatalog merges every node's models and says which could not be read", () => {
  const c = buildCatalog([
    { node: "xpredator", self: true, api: { models: [
      { key: "qwen/qwen3-0.6b", architecture: "qwen3", params_string: "0.6B", quantization: "Q4_K_M", size_bytes: 400,
        capabilities: ["tool_use"], loaded_instances: [{ id: "i1", config: {} }] },
    ] }, operations: [] },
    { node: "minion", api: { models: [
      { key: "qwen/qwen3.8-27b", architecture: "qwen35", draft_layers: 1, size_bytes: 1700, format: "gguf",
        capabilities: ["reasoning", "vision", "speculative"], loaded_instances: [] },
    ] }, operations: [{ state: "running", model: "qwen/qwen3.8-27b", kind: "load" }] },
    { node: "sites-01", error: "not the same owner" },
  ]);
  assert.deepEqual(c.rows.map((r) => r.node), ["xpredator", "minion"], "this node first");
  const big = c.rows[1];
  assert.equal(big.mtp, true);
  assert.deepEqual(big.caps, ["vision", "reasoning"], "known capabilities, in a fixed order");
  assert.equal(big.busy, true);
  assert.equal(big.format, "GGUF");
  assert.equal(c.rows[0].loaded, true);
  assert.deepEqual(c.unreachable, [{ node: "sites-01", error: "not the same owner" }]);
  assert.deepEqual(filterCatalog(c.rows, "minion").map((r) => r.key), ["qwen/qwen3.8-27b"]);
  assert.deepEqual(filterCatalog(c.rows, "", "0.6").map((r) => r.node), ["xpredator"]);
});

test("inheritedValue shows what an empty setting falls back to", () => {
  const spec = { sampling: { temperature: 1, top_k: 20 }, template_vars: { enable_thinking: true } };
  assert.equal(inheritedValue(spec, "temperature"), "1");
  assert.equal(inheritedValue(spec, "enable_thinking"), "on");
  assert.equal(inheritedValue(spec, "min_p"), "");
  assert.equal(inheritedValue(null, "temperature"), "");
});

import { renderMarkdown, compactCount } from "./ui-model.js";

test("renderMarkdown renders a model card and never passes HTML through", () => {
  const html = renderMarkdown("# Qwen3 8B\n\nA **fast** model. See [docs](https://qwen.ai).\n\n- one\n- two\n\n```\ncode <b>\n```");
  assert.match(html, /<h2>Qwen3 8B<\/h2>/);
  assert.match(html, /<strong>fast<\/strong>/);
  assert.match(html, /<a href="https:\/\/qwen.ai" target="_blank" rel="noopener noreferrer">docs<\/a>/);
  assert.match(html, /<ul>\n<li>one<\/li>\n<li>two<\/li>\n<\/ul>/);
  assert.match(html, /<pre><code>code &lt;b&gt;<\/code><\/pre>/);
  assert.match(renderMarkdown("Creator: Qwen<br> Model: x<br/>"), /Creator: Qwen<br> Model: x<br>/);
  const evil = renderMarkdown('<script>alert(1)</script>\n\n<img src=x onerror=alert(1)>\n\n[x](javascript:alert(1))\n\n![p](https://t.example/p.png)');
  assert.ok(!/<script|<img|javascript:"|href="javascript/.test(evil), evil);
  assert.ok(!evil.includes("t.example"), "images are dropped, never fetched");
});

test("compactCount", () => {
  assert.equal(compactCount(872724), "873K");
  assert.equal(compactCount(4749304), "4.7M");
  assert.equal(compactCount(12), "12");
});

import { downloadTargets } from "./ui-model.js";

test("downloadTargets says what each node can do with a download", () => {
  const option = { quant: "Q4_K_M", bytes: 16e9, files: ["Qwen3.8-27B-Q4_K_M.gguf"] };
  const t = downloadTargets({
    repo: "lmstudio-community/Qwen3.8-27B-GGUF", option, self: "xpredator",
    nodes: ["xpredator", "minion", "sites-01", "mac", "small"],
    storage: {
      xpredator: { free_bytes: 500e9, gpus: [{ name: "RTX 5090", vram_mb: 32607 }] },
      minion: { free_bytes: 800e9, gpus: [{ name: "RTX 6000 Ada", vram_mb: 49140 }] },
      "sites-01": { entrypoint: true },
      mac: { free_bytes: 900e9, gpus: [{ name: "M2", vram_mb: 8000 }] },
      small: { free_bytes: 5e9 },
    },
    rows: [{ node: "minion", file: "Qwen3.8-27B-Q4_K_M.gguf", quant: "Q4_K_M" }],
    ops: { mac: [{ kind: "download", state: "running", model: "lmstudio-community/Qwen3.8-27B-GGUF@Q4_K_M", message: "40%" }] },
  });
  const by = Object.fromEntries(t.map((x) => [x.node, x]));
  assert.equal(by.xpredator.state, "ready");
  assert.equal(by.xpredator.selectable, true);
  assert.equal(by.minion.state, "have");
  assert.equal(by["sites-01"].state, "entrypoint");
  assert.equal(by.mac.state, "downloading");
  assert.equal(by.small.state, "nospace");
  assert.equal(t.filter((x) => x.selectable).length, 1);
});

import { buildEdges } from "./ui-model.js";

// Only local engines are dialled directly; peer candidates are mesh
// listeners, and the llm-d model goes through Envoy. The previous
// expectation drew direct peer-engine paths the router never takes.
test("buildEdges reaches engines directly, through a peer's ModelFabric, and through llm-d", () => {
  const topos = [
    { node: "sites-01", preferred: "xpredator", listeners: [{ name: "front", addr: "127.0.0.1:1234" }, { name: "mesh", addr: "100.69.171.44:1234" }], engines: [] },
    { node: "xpredator", listeners: [{ name: "front", addr: "127.0.0.1:1234" }, { name: "mesh", addr: "100.107.225.6:1234" }, { name: "llmd", addr: "127.0.0.1:8090" }],
      engines: [{ id: "e1", model: "small", addr: "127.0.0.1", port: 18000 }],
      llmd: { state: "running", model: "small", listen: "127.0.0.1:8090", endpoints: ["127.0.0.1:18000"] } },
    { node: "minion", listeners: [{ name: "front", addr: "127.0.0.1:1234" }, { name: "mesh", addr: "100.100.69.3:1234" }],
      engines: [{ id: "e2", model: "big", addr: "100.100.69.3", port: 18000 }] },
  ];
  const e = buildEdges(topos, "sites-01").map((x) => `${x.from.node}.${x.from.id}>${x.to.node}.${x.to.id}:${x.style}`);
  assert.deepEqual(e, [
    "sites-01.front>xpredator.mesh:preferred",
    "xpredator.mesh>xpredator.e1:forwarded",
    "sites-01.front>minion.mesh:forwarded",
    "minion.mesh>minion.e2:forwarded",
  ]);
  // The local scheduler owns "small"; "big" still routes through the peer.
  const l = buildEdges(topos, "xpredator").map((x) => `${x.from.id}>${x.to.node}.${x.to.id}:${x.style}`);
  assert.deepEqual(l, ["front>xpredator.llmd:llmd", "llmd>xpredator.e1:llmd", "front>minion.mesh:forwarded", "mesh>minion.e2:forwarded",
    "front>sites-01.mesh:peer"]);
  const m = buildEdges(topos, "minion").map((x) => `${x.from.node}.${x.from.id}>${x.to.node}.${x.to.id}:${x.style}`);
  assert.deepEqual(m, [
    "minion.front>xpredator.mesh:forwarded",
    "xpredator.mesh>xpredator.e1:forwarded",
    "minion.front>minion.e2:direct",
    "minion.front>sites-01.mesh:peer",
  ]);
  assert.deepEqual(buildEdges(topos, "nobody"), [], "a node that did not answer has no paths");
});

test("buildEdges links every reachable peer even with no models loaded", () => {
  const topos = ["sites-01", "minion", "xpredator"].map((node, i) => ({
    node, engines: [], listeners: [{ name: "front", addr: "127.0.0.1:1234" }, { name: "mesh", addr: `100.64.0.${i + 1}:1234` }],
  }));
  const e = buildEdges(topos, "sites-01").map((x) => `${x.from.id}>${x.to.node}.${x.to.id}:${x.style}`);
  assert.deepEqual(e, ["front>minion.mesh:peer", "front>xpredator.mesh:peer"]);
});

// Without a peer mesh listener, a loopback engine has no reachable path.
test("buildEdges cannot reach a peer's loopback engine with no tailnet listener", () => {
  const topos = [
    { node: "sites-01", listeners: [{ name: "front", addr: "127.0.0.1:1234" }], engines: [] },
    { node: "xpredator", listeners: [{ name: "front", addr: "127.0.0.1:1234" }],
      engines: [{ id: "e1", model: "small", addr: "127.0.0.1", port: 18000 }] },
  ];
  assert.deepEqual(buildEdges(topos, "sites-01"), []);
});

import { constellation } from "./ui-model.js";

test("constellation puts a model at the centre with who serves and who routes to it", () => {
  const topos = [
    { node: "sites-01", role: "entrypoint", listeners: [{ name: "public" }, { name: "mesh", addr: "100.69.171.44:1234" }], engines: [] },
    { node: "minion", role: "gpu", listeners: [{ name: "mesh", addr: "100.100.69.3:1234" }],
      engines: [{ id: "e2", model: "big", addr: "100.100.69.3", port: 18000, slots: 2, inflight: 1, prefill_tok_s: 900 }] },
    { node: "xpredator", role: "gpu", listeners: [{ name: "mesh", addr: "100.107.225.6:1234" }],
      engines: [{ id: "e1", model: "small", addr: "127.0.0.1", port: 18000, slots: 2 }] },
  ];
  const c = constellation(topos, "big");
  assert.equal(c.model, "big");
  assert.deepEqual(c.models, ["big", "small"]);
  assert.deepEqual(c.serving.map((s) => [s.node, s.slots, s.inflight]), [["minion", 2, 1]]);
  // Rewritten to require peer mesh listeners: the earlier expectation
  // drew direct paths to tailnet engines that the router never takes.
  assert.deepEqual(c.entries.map((e) => [e.node, e.public, e.links.map((l) => `${l.to}:${l.style}`)]),
    [["sites-01", true, ["minion:forwarded"]], ["minion", false, ["minion:direct"]], ["xpredator", false, ["minion:forwarded"]]]);
  assert.deepEqual(c.others, [], "a node that can reach the model is an entry point, not an outsider");
  const s = constellation(topos, "small");
  assert.deepEqual(s.entries.find((e) => e.node === "sites-01").links.map((l) => `${l.to}:${l.style}`),
    ["xpredator:forwarded"], "xpredator's engine is on loopback, so through its ModelFabric");
  assert.equal(constellation(topos, "nope").model, "big", "an unknown model falls back to the first");
});

test("constellation says which entry points hand a model to llm-d", () => {
  const topos = [
    { node: "sites-01", role: "entrypoint", listeners: [{ name: "mesh", addr: "100.69.171.44:1234" }], engines: [] },
    { node: "xpredator", role: "gpu", listeners: [{ name: "mesh", addr: "100.107.225.6:1234" }, { name: "llmd", addr: "127.0.0.1:8090" }],
      engines: [{ id: "e1", model: "small", addr: "100.107.225.6", port: 18000, slots: 2 }],
      llmd: { state: "running", model: "small", listen: "127.0.0.1:8090", endpoints: ["100.107.225.6:18000"] } },
  ];
  const c = constellation(topos, "small");
  const by = Object.fromEntries(c.entries.map((e) => [e.node, e.via]));
  assert.deepEqual(by, { "sites-01": "direct", xpredator: "llmd" },
    "only the node running the scheduler hands the model to it");
});

import { constellationAll } from "./ui-model.js";

test("constellationAll: every model, who serves each, and who routes where", () => {
  const topos = [
    { node: "sites-01", role: "entrypoint", listeners: [{ name: "public" }], engines: [] },
    { node: "minion", role: "gpu", listeners: [{ name: "mesh", addr: "100.100.69.3:1234" }],
      engines: [{ id: "a", model: "big", addr: "100.100.69.3", port: 18000, slots: 2, inflight: 1 },
                { id: "b", model: "small", addr: "100.100.69.3", port: 18001, slots: 2 }] },
    { node: "xpredator", role: "gpu", listeners: [{ name: "mesh", addr: "100.107.225.6:1234" }],
      engines: [{ id: "c", model: "small", addr: "127.0.0.1", port: 18000, slots: 4, inflight: 2 }] },
  ];
  const c = constellationAll(topos);
  assert.deepEqual(c.models.map((m) => [m.model, m.nodes, m.slots, m.inflight]),
    [["big", ["minion"], 2, 1], ["small", ["minion", "xpredator"], 6, 2]]);
  assert.deepEqual(c.nodes.map((n) => [n.node, n.serves.map((s) => s.model)]), [["minion", ["big", "small"]], ["xpredator", ["small"]]]);
  // Peer links use mesh listeners regardless of engine binding; local
  // engines are serving nodes. The prior direct-peer expectation was wrong.
  assert.deepEqual(c.entries.map((e) => [e.node, e.links.map((l) => `${l.to}:${l.style}:${l.models.join("+")}`)]),
    [["sites-01", ["minion:forwarded:big+small", "xpredator:forwarded:small"]],
     ["minion", ["xpredator:forwarded:small"]],
     ["xpredator", ["minion:forwarded:big+small"]]]);
  assert.deepEqual(c.others, []);
});

test("platformBadge reads a node's platform, and says so when it cannot", () => {
  const mac = platformBadge("darwin/arm64");
  assert.equal(mac.key, "mac");
  assert.equal(mac.label, "macOS");
  assert.match(mac.title, /Apple silicon/);
  assert.match(mac.title, /Metal and MLX/);

  const linux = platformBadge("linux/amd64");
  assert.equal(linux.key, "linux");
  assert.equal(linux.label, "Linux");
  assert.match(linux.title, /llm-d/);
  assert.equal(platformBadge("windows/amd64").key, "windows");

  // Cover old peers without platform metadata and invalid values.
  for (const p of ["", null, undefined, "plan9/386"]) {
    const b = platformBadge(p);
    assert.equal(b.key, "unknown");
    assert.equal(b.label, "");
  }
});

test("platformBadge prefers the OS version a node reports", () => {
  const mac = platformBadge("darwin/arm64", "macOS 26.6.2");
  assert.equal(mac.version, "macOS 26.6.2");
  assert.match(mac.title, /^macOS 26\.6\.2 \(Apple silicon\) — runs Metal and MLX$/);

  assert.match(platformBadge("linux/amd64", "Ubuntu 24.04.1 LTS").title, /^Ubuntu 24\.04\.1 LTS — runs/);
  assert.match(platformBadge("linux/amd64").title, /^Linux — runs/);
  assert.equal(platformBadge("", "Ubuntu 24.04").title, "Ubuntu 24.04");
});

test("kvBadge distinguishes an idle cache from an unmeasured one", () => {
  const idle = kvBadge(0);
  assert.equal(idle.known, true);
  assert.equal(idle.label, "0% KV");
  assert.equal(idle.level, "cool");

  for (const missing of [-1, undefined, null, "x"]) {
    assert.equal(kvBadge(missing).known, false, `${missing} should be unknown`);
    assert.equal(kvBadge(missing).label, "");
  }

  assert.equal(kvBadge(0.5).label, "50% KV");
  assert.equal(kvBadge(0.5).level, "warm");
  assert.equal(kvBadge(0.92).level, "hot");
  // Over capacity (context shifting) still reads as full, never more.
  assert.equal(kvBadge(1.4).pct, 100);
});

test("presetDrift counts overrides, not inherited fields", () => {
  const preset = { temperature: 0.3, top_p: 0.9 };

  // Empty fields inherit a newly selected preset; they must not mark it unsaved.
  assert.deepEqual(presetDrift({}, preset), { dirty: false, changed: [] });

  assert.deepEqual(presetDrift({ temperature: "0.85" }, preset),
    { dirty: true, changed: ["temperature"] });

  assert.equal(presetDrift({ temperature: "0.3" }, preset).dirty, false);
  assert.equal(presetDrift({ temperature: "0.30" }, preset).dirty, false);

  assert.deepEqual(presetDrift({ top_k: "40" }, preset), { dirty: true, changed: ["top_k"] });

  assert.equal(presetDrift({}, {}).dirty, false);
  assert.equal(presetDrift({ temperature: "0.5" }, {}).dirty, true);
});

test("buildView lists every engine in the mesh, not just this node's", () => {
  const v = buildView({
    self: {
      node: "xpredator",
      engines: [{ name: "inst-a", healthy: true, inflight: 2, kv_usage: 0.4 }],
      instances: [{ id: "inst-a", model: "qwen/q", runtime: "llama.cpp@1", engine: "llama.cpp",
        address: "127.0.0.1", port: 18000, slots: 2, state: "ready", kv_usage: 0.4 }],
    },
    peers: [
      { node: "minion", alive: true, instances: [
        { id: "inst-b", model: "qwen/q", runtime: "llama.cpp@2", engine: "llama.cpp",
          address: "100.100.69.3", port: 18000, slots: 2, inflight: 1, state: "ready", kv_usage: 0.1 }] },
      { node: "helion", alive: true, instances: [
        { id: "inst-c", model: "qwen/q", runtime: "mlx-lm@0.31", engine: "mlx",
          address: "127.0.0.1", port: 18000, slots: 4, state: "ready", kv_usage: -1 }] },
    ],
  });

  assert.deepEqual(v.meshEngines.map((e) => e.node), ["xpredator", "helion", "minion"],
    "this node first, then peers by name");
  const [self, helion, minion] = v.meshEngines;
  // The local engine's live numbers come from the router, not the instance.
  assert.equal(self.inflight, 2);
  assert.equal(self.healthy, true);
  assert.equal(self.isSelf, true);
  assert.equal(minion.inflight, 1);
  assert.equal(minion.address, "100.100.69.3:18000");
  assert.equal(minion.engine, "llama.cpp");
  // An engine that cannot report KV stays unknown rather than reading as idle.
  assert.equal(helion.kvUsage, -1);
  assert.equal(helion.engine, "mlx");
  assert.deepEqual(buildView({ self: { node: "n" }, peers: [] }).meshEngines, []);
});

test("groupByModel answers where a model lives, not just what a node holds", () => {
  const rows = [
    { id: "1", node: "minion", key: "qwen/q27", arch: "qwen35", params: "27B", quant: "Q4_K_M", bytes: 10, loaded: true, caps: ["vision"] },
    { id: "2", node: "xpredator", self: true, key: "qwen/q27", arch: "", params: "", quant: "Q4_K_M", bytes: 10, loaded: false, caps: [] },
    { id: "3", node: "helion", key: "qwen/q27", arch: "qwen35", params: "27B", quant: "4bit", bytes: 9, loaded: true, mtp: true, caps: ["vision", "tool_use"] },
    { id: "4", node: "minion", key: "qwen/q06", arch: "qwen3", params: "0.6B", quant: "Q4_K_M", bytes: 1, loaded: false, caps: [] },
  ];
  const groups = groupByModel(rows);

  assert.deepEqual(groups.map((g) => g.key), ["qwen/q06", "qwen/q27"]);
  const big = groups[1];
  assert.deepEqual(big.nodes.map((n) => n.node), ["xpredator", "helion", "minion"],
    "this node first, then alphabetical");
  assert.equal(big.loaded, 2, "loaded on two of three nodes");
  assert.equal(big.bytes, 29, "disk across every copy");
  assert.equal(big.arch, "qwen35");
  assert.equal(big.params, "27B");
  assert.equal(big.mtp, true);
  assert.deepEqual(big.caps, ["vision", "tool_use"]);
  // Per-node files can differ under one key: GGUF here, MLX there.
  assert.deepEqual(big.nodes.map((n) => n.quant), ["Q4_K_M", "4bit", "Q4_K_M"]);
  assert.deepEqual(groupByModel([]), []);
});

test("a download target does not repeat what the tile already shows", () => {
  const targets = downloadTargets({
    repo: "lmstudio-community/gemma-4-E4B-it-GGUF",
    option: { quant: "Q4_K_M", bytes: 5.9e9, files: ["gemma-4-E4B-it-Q4_K_M.gguf"] },
    nodes: ["xpredator", "sites-01"],
    rows: [],
    storage: {
      xpredator: { free_bytes: 155e9, gpus: [{ name: "RTX 5090", vram_mb: 32607 }] },
      "sites-01": { free_bytes: 67e9, entrypoint: true },
    },
    ops: {},
    self: "xpredator",
  });
  const ready = targets.find((t) => t.node === "xpredator");
  assert.equal(ready.state, "ready");
  assert.equal(ready.note, "", "free space is on the spec line; the note repeated it");
  assert.equal(ready.freeBytes, 155e9, "the tile still knows the free space");
  assert.equal(targets.find((t) => t.node === "sites-01").note, "entrypoint — runs no models");
});

test("quantRows states what is constant once and keeps what differs", () => {
  const options = [
    { quant: "Q4_K_M", bytes: 17741860192, recommended: true, projector: "mmproj-f16.gguf", files: ["a"] },
    { quant: "Q6_K", bytes: 23407288320, projector: "mmproj-f16.gguf", files: ["a"] },
    { quant: "Q8_0", bytes: 29957397504, projector: "mmproj-f16.gguf", files: ["a", "b"] },
  ];
  const { rows, common } = quantRows(options, "Q6_K");
  assert.equal(common.count, 3);
  assert.equal(common.format, "GGUF");
  assert.equal(common.projector, true, "all three ship a projector, so say it once");
  assert.deepEqual(rows.map((r) => r.projector), ["", "", ""], "a constant badge is not repeated per row");
  assert.deepEqual(rows.map((r) => r.active), [false, true, false]);
  assert.equal(rows[0].sizeLabel, "16.5GB");
  assert.equal(rows[2].frac, 1, "the largest file fills the bar");
  assert.ok(Math.abs(rows[0].frac - 17741860192 / 29957397504) < 1e-9, "the rest are drawn against it");
  assert.equal(rows[2].fileCount, 2, "a sharded option still says how many files");
});

test("quantRows keeps a badge that tells two rows apart", () => {
  const { rows, common } = quantRows([
    { quant: "Q4_K_M", bytes: 1e9, projector: "mmproj.gguf" },
    { quant: "Q8_0", bytes: 2e9 },
  ]);
  assert.equal(common.projector, false);
  assert.deepEqual(rows.map((r) => r.projector), ["mmproj.gguf", ""]);
});

test("quantRows survives an empty repository", () => {
  const { rows, common } = quantRows([], "Q4_K_M");
  assert.deepEqual(rows, []);
  assert.equal(common.count, 0);
});

// llm-d dials engines directly and cannot reach a peer's loopback engine.
test("buildView flags nodes whose engines are loopback-bound", () => {
  const view = buildView({
    self: {
      node: "xpredator",
      addr: "100.107.225.6",
      models: ["q"],
      instances: [{ id: "a", model: "q", state: "ready", address: "127.0.0.1", port: 18000 }],
      engines: [],
    },
    peers: [
      {
        node: "minion", addr: "100.100.69.3", alive: true, models: ["q"],
        instances: [{ id: "b", model: "q", state: "ready", address: "100.100.69.3", port: 18000 }],
        engines: [],
      },
      {
        node: "helion", addr: "100.66.9.93", alive: true, models: ["q"],
        instances: [{ id: "c", model: "q", state: "ready", address: "127.0.0.1", port: 18000 }],
        engines: [],
      },
      { node: "sites-01", addr: "100.69.171.44", alive: true, models: [], instances: [], engines: [] },
    ],
  });

  const by = Object.fromEntries(view.nodes.map((n) => [n.name, n.loopbackEngines]));
  assert.equal(by.xpredator, true, "this node's loopback engine should be flagged too");
  assert.equal(by.minion, false, "a tailnet-bound engine is reachable");
  assert.equal(by.helion, true, "a peer on loopback cannot be scheduled by llm-d");
  assert.equal(by["sites-01"], false, "a node with no engines is not misconfigured");

  const scope = Object.fromEntries(view.nodes.map((n) => [n.name, n.engineScope]));
  assert.equal(scope.minion, "tailnet");
  assert.equal(scope.helion, "loopback");
  assert.equal(scope.xpredator, "loopback");
  assert.equal(scope["sites-01"], "", "no engines is neither reading");

  assert.equal(view.nodes.find((n) => n.name === "xpredator").addr, "100.107.225.6");
});

test("buildView ignores instances that are not ready when judging binding", () => {
  const view = buildView({
    self: { node: "a", models: [], instances: [{ id: "x", model: "q", state: "loading", address: "127.0.0.1" }], engines: [] },
    peers: [],
  });
  assert.equal(view.nodes[0].loopbackEngines, false);
});

// Vision comes from the launched instance and may differ by node.
// Missing flags from older peers default to false.
test("meshEngines carry each instance's vision flag", () => {
  const view = buildView({
    self: {
      node: "a",
      instances: [
        { id: "i1", model: "seer", slots: 1, vision: true, state: "ready" },
        { id: "i2", model: "scribe", slots: 4, state: "ready" },
      ],
    },
    peers: [],
    models: [],
  });
  const seer = view.meshEngines.find((e) => e.model === "seer");
  const scribe = view.meshEngines.find((e) => e.model === "scribe");
  assert.equal(seer.vision, true);
  assert.equal(seer.slots, 1);
  assert.equal(scribe.vision, false, "an engine that says nothing is not a vision engine");
});

// Share model template levels with older peers so the same model
// has consistent controls across nodes.
test("reasoning levels carry across nodes holding the same model", () => {
  const view = buildCatalog([
    { node: "new", self: true, api: { models: [{ key: "seer", reasoning_efforts: ["xhigh", "low"] }] } },
    { node: "old", api: { models: [{ key: "seer" }, { key: "other" }] } },
  ]);
  const old = view.rows.find((r) => r.node === "old" && r.key === "seer");
  assert.deepEqual(old.reasoningEfforts, ["xhigh", "low"]);
  const other = view.rows.find((r) => r.key === "other");
  assert.deepEqual(other.reasoningEfforts, [], "a model nobody described gets no invented levels");
});

// Front-door counts must include Envoy queues and llm-d requests
// that engine counts omit.
test("mesh load counts what the front doors accepted", () => {
  const view = buildView({
    self: { node: "a", inflight: 0, accepted: 2, engines: [], instances: [] },
    peers: [
      { node: "b", alive: true, inflight: 0, accepted: 3, engines: [], instances: [] },
      { node: "c", alive: false, inflight: 0, accepted: 9, engines: [], instances: [] },
    ],
    models: [],
  });
  assert.equal(view.stats.accepted, 5, "a dead peer's last figure is stale and must not be counted");
});

test("meshEngines carry each engine's measured prefill rate", () => {
  const view = buildView({
    self: { node: "a", instances: [{ id: "i1", model: "m", slots: 2, prefill_tok_s: 1989.4, state: "ready" }] },
    peers: [{ node: "b", alive: true, instances: [{ id: "i2", model: "m", slots: 2, state: "ready" }] }],
    models: [],
  });
  assert.equal(view.meshEngines.find((e) => e.id === "i1").prefillTokS, 1989.4);
  // Zero means unmeasured; the table renders a dash.
  assert.equal(view.meshEngines.find((e) => e.id === "i2").prefillTokS, 0);
});

// Prefer the node average, which covers time before the dashboard opened.
// -1 means insufficient samples, not an idle engine.
test("meshEngines carry the node's own load average", () => {
  const view = buildView({
    self: { node: "a", instances: [
      { id: "i1", model: "m", slots: 2, load_avg: 1.37, state: "ready" },
      { id: "i2", model: "m", slots: 2, load_avg: -1, state: "ready" },
      { id: "i3", model: "m", slots: 2, state: "ready" },
    ] },
    peers: [], models: [],
  });
  const by = Object.fromEntries(view.meshEngines.map((e) => [e.id, e.loadAvg]));
  assert.equal(by.i1, 1.37);
  assert.equal(by.i2, -1, "too few samples is -1, not 0");
  assert.equal(by.i3, -1, "a peer too old to publish it reads as unmeasured");
});

// Find schedulers on peers; llm-d can run on an entrypoint without GPUs.
test("the mesh names whichever node is scheduling", () => {
  const view = buildView({
    self: { node: "xpredator", instances: [], engines: [] },
    peers: [
      { node: "minion", alive: true, instances: [], engines: [] },
      { node: "sites-01", alive: true, instances: [], engines: [],
        scheduler: { model: "qwen/qwen3.8-27b", profile: "tuned", engines: 3 } },
    ],
    models: [],
  });
  assert.equal(view.scheduler.node, "sites-01");
  assert.equal(view.scheduler.profile, "tuned");
  assert.equal(view.scheduler.engines, 3);
  assert.equal(view.scheduler.isSelf, false, "so the rail can say where, and act there");
});

test("no scheduler in the mesh reads as none", () => {
  const view = buildView({
    self: { node: "a", instances: [], engines: [] },
    peers: [{ node: "b", alive: true, instances: [], engines: [] }],
    models: [],
  });
  assert.equal(view.scheduler, null);
});

// A dead peer's last known scheduler is stale: it is not scheduling anything.
test("an offline node is not the scheduler", () => {
  const view = buildView({
    self: { node: "a", instances: [], engines: [] },
    peers: [{ node: "b", alive: false, instances: [], engines: [],
              scheduler: { model: "m", profile: "tuned", engines: 2 } }],
    models: [],
  });
  assert.equal(view.scheduler, null);
});

test("a rough prefill rate is carried with its trust flag", () => {
  const view = buildView({
    self: { node: "a", instances: [
      { id: "i1", model: "m", slots: 2, prefill_tok_s: 262, state: "ready" },
      { id: "i2", model: "m", slots: 2, prefill_tok_s: 1809, prefill_trusted: true, state: "ready" },
    ] },
    peers: [], models: [],
  });
  const by = Object.fromEntries(view.meshEngines.map((e) => [e.id, e]));
  assert.equal(by.i1.prefillTokS, 262);
  assert.equal(by.i1.prefillTrusted, false, "shown, but not what routing uses");
  assert.equal(by.i2.prefillTrusted, true);
});

test("buildTokens lists the node key first, then tokens newest first, never a secret", () => {
  const now = Date.parse("2026-10-01T12:00:00Z");
  const rows = buildTokens({
    node_key: "ab12",
    tokens: [
      { id: "a", name: "laptop", hint: "1111", created: "2026-09-01T12:00:00Z", last_used: "2026-10-01T11:58:00Z" },
      { id: "b", name: "ci", hint: "2222", created: "2026-09-30T12:00:00Z" },
    ],
  }, now);
  assert.deepEqual(rows.map((r) => r.name), ["Node key", "ci", "laptop"]);
  assert.equal(rows[0].builtin, true);
  // The node did not say how its key begins, so nothing is put in front of it.
  assert.equal(rows[0].masked, "…ab12");
  // Seen 2026-10-08: a key from before the rename begins sk-llmz-, and the
  // page showed sk-mfsh-…bedf for it.
  assert.equal(buildTokens({ node_key: "bedf", node_key_prefix: "sk-llmz-" })[0].masked, "sk-llmz-…bedf");
  assert.equal(buildTokens({ tokens: [{ id: "1", name: "new", hint: "e72c", prefix: "sk-mfsh-", created: "2026-10-08T00:00:00Z" }] })[0].masked, "sk-mfsh-…e72c");
  assert.equal(buildTokens({ tokens: [{ id: "2", name: "old", hint: "bea7", created: "2026-10-01T00:00:00Z" }] })[0].masked, "…bea7");
  assert.equal(rows[0].rotatable, false);
  assert.equal(buildTokens({ node_key: "ab12", rotatable: true })[0].rotatable, true);
  assert.equal(rows[1].lastUsed, "never");
  assert.equal(rows[2].lastUsed, "2m ago");
  assert.equal(rows[2].created, "30d ago");
});

test("buildTokens with no answer shows nothing rather than an invented node key", () => {
  assert.deepEqual(buildTokens(null), []);
  assert.deepEqual(buildTokens({ tokens: [] }), []);
});

test("settingsChanges sends only what the form changed", () => {
  const saved = { listen: "127.0.0.1:1234", require_api_key: false, cors_origins: [], jit_ttl: "" };
  assert.deepEqual(settingsChanges(saved, { ...saved }), {});
  assert.deepEqual(
    settingsChanges(saved, { ...saved, require_api_key: true, cors_origins: ["http://localhost:3000"] }),
    { require_api_key: true, cors_origins: ["http://localhost:3000"] },
  );
  // An absent key and an empty one are the same setting.
  assert.deepEqual(settingsChanges({ jit_ttl: undefined }, { jit_ttl: "" }), {});
});

test("pendingRestart names saved settings the node is not running, never a live one", () => {
  const view = {
    live: ["require_api_key", "cors_origins"],
    saved: { listen: "127.0.0.1:3000", require_api_key: true, cors_origins: ["*"] },
    running: { listen: "127.0.0.1:1234", require_api_key: false, cors_origins: [] },
  };
  assert.deepEqual(pendingRestart(view), ["listen"]);
  assert.deepEqual(pendingRestart(null), []);
});

test("wideningWarnings says what a change opens, and nothing for loopback", () => {
  assert.deepEqual(wideningWarnings({ listen: "127.0.0.1:3000", public_listen: "127.0.0.1:1235", engine_bind: "" }), []);
  const w = wideningWarnings({ listen: "0.0.0.0:1234", engine_bind: "tailnet", cors_origins: ["*"], web_ui: false });
  assert.equal(w.length, 4);
  assert.match(w[0], /beyond this machine/);
  assert.match(w[1], /ACLs/);
});

test("the settings form round-trips: opening and saving untouched changes nothing", () => {
  for (const saved of [
    { listen: "127.0.0.1:1234", mesh_admin: "same-owner", jit_ttl: "", cors_origins: [], web_ui: true, jit_auto_evict: true },
    { listen: "127.0.0.1:1234", mesh_admin: "", jit_ttl: "0", cors_origins: ["*"], public_listen: "127.0.0.1:1235", web_ui: false, jit_auto_evict: false, require_api_key: true },
  ]) {
    assert.deepEqual(settingsChanges(saved, formToServer(serverToForm(saved), saved)), {}, JSON.stringify(saved));
  }
});

test("the form speaks ports and switches; the config keeps its hosts", () => {
  const saved = { listen: "127.0.0.1:1234", public_listen: "", jit_ttl: "", mesh_admin: "" };
  const form = { ...serverToForm(saved), port: "3000", front_on: true, front_port: "3001", peer_admin: false, jit_unload: false };
  const out = formToServer(form, saved);
  assert.equal(out.listen, "127.0.0.1:3000"); // a port change never widens the bind
  // Default the public listener to loopback for Funnel or a local proxy.
  assert.equal(out.public_listen, "127.0.0.1:3001");
  assert.equal(out.mesh_admin, "off");
  assert.equal(out.jit_ttl, "0");
  assert.equal(formToServer({ front_on: true, front_port: "9000" }, { public_listen: "100.64.0.7:1235" }).public_listen, "100.64.0.7:9000");
});

test("displayPath writes the home directory as ~", () => {
  assert.equal(displayPath("/home/greg/.config/modelfabric/config.json"), "~/.config/modelfabric/config.json");
  assert.equal(displayPath("/Users/sam/x.json"), "~/x.json");
  assert.equal(displayPath("/etc/modelfabric.json"), "/etc/modelfabric.json");
});

test("benchTables lays a report out as the standard tables, failures in place", () => {
  const t = benchTables({
    model: "m", command: "mfsh bench -model m",
    config: { reps: 3, batch_pp: 1024, tg: 128 },
    machine: { gpu: "RTX 6000 Ada", vram_mb: 49140, driver: "580", os: "Ubuntu", platform: "linux/amd64", modelfabric: "v1" },
    engine: { runtime: "llama.cpp b1", file: "m.gguf", quant: "Q4_K_M", size_mb: 461, timings: "engine" },
    corpus: { name: "prose", version: "v1", sha256: "265afb7e7fb703c3aaaa" },
    single: [
      { test: "pp1024/tg128", ttft_ms: 55.1, tpot_ms: 1.87, pp_tps: 50443.1, tg_tps: 533.5, e2e_s: 0.29, throughput_tps: 3981, peak_mem_mb: 1454 },
      { test: "pp65536/tg128", error: "did not fit" },
    ],
    batch_same_prompt: [{ n: 2, tg_tps: 889.4, speedup: 1.67, pp_tps: 25877.9, pp_tps_per_request: 12938.9, cached_pct: 50, avg_ttft_ms: 39.6, e2e_s: 0.327 }],
  });
  assert.deepEqual(t.single.rows[0].cells, ["pp1024/tg128", "55.1", "1.87", "50443.1 tok/s", "533.5 tok/s", "0.290s", "3981.0 tok/s", "1.42 GB"]);
  assert.equal(t.single.rows[1].error, "did not fit");
  assert.deepEqual(t.same.rows[0].cells.slice(0, 3), ["2x", "889.4 tok/s", "1.67x"]);
  assert.equal(t.same.rows[0].cells[5], "50%");
  assert.deepEqual(t.diff.rows, []);
  assert.equal(t.meta.find((m) => m[0] === "Prompts")[1], "prose-v1 · sha256 265afb7e7fb703c3");
  assert.equal(t.command, "mfsh bench -model m");
  assert.equal(benchTables(null), null);
});

test("the router form round-trips, and only what changed is sent", () => {
  const saved = { rate_weighted_routing: true, prefix_affinity: true, local_bias: 0, max_output_tokens: 16384, cache_disk_mib: 0, cache_disk_dir: "" };
  assert.deepEqual(settingsChanges(saved, formToRouter(routerToForm(saved), saved), ROUTER_SETTINGS), {});
  const on = { ...routerToForm(saved), cache_on: true, cache_gb: "50", prefix_affinity: false };
  assert.deepEqual(settingsChanges(saved, formToRouter(on, saved), ROUTER_SETTINGS), { prefix_affinity: false, cache_disk_mib: 51200 });
  assert.equal(formToRouter({ ...routerToForm(saved), cache_on: false, cache_gb: "50" }, saved).cache_disk_mib, 0);
  // 0 is "no ceiling", and survives the round trip as 0, not the default.
  assert.equal(formToRouter({ ...routerToForm(saved), max_output_tokens: "0" }, saved).max_output_tokens, 0);
});

test("routedBy gives llm-d its one model and the router the rest", () => {
  const rows = routedBy([{ id: "big", nodes: ["minion"] }, { id: "small", nodes: ["helion", "minion"] }], "big");
  assert.deepEqual(rows.map((r) => `${r.model}:${r.by}`), ["big:llmd", "small:router"]);
  assert.deepEqual(routedBy([{ id: "small" }], "").map((r) => r.by), ["router"]);
});

test("clusterTables shows where each request went, and says when it is not known", () => {
  const t = clusterTables({
    model: "m", entry: "entrypoint-01", routing: "ModelFabric router", command: "mfsh bench -cluster -model m",
    config: { reps: 1, load_pp: 1024, tg: 128 },
    holders: [{ node: "minion", platform: "linux/amd64", engines: 1, slots: 4 }, { node: "helion", platform: "darwin/arm64" }],
    single: [{ test: "pp1024/tg128", ttft_ms: 120, pp_tps: 8000, tg_tps: 40, e2e_s: 3.3, node: "minion" }, { test: "pp4096/tg128", ttft_ms: 1, pp_tps: 1, tg_tps: 1, e2e_s: 1 }],
    load: [
      { n: 4, tg_tps: 120, speedup: 3, pp_tps: 9000, avg_ttft_ms: 300, p95_ttft_ms: 500, cached_pct: 0, e2e_s: 4.2, spread: { helion: 1, minion: 3 } },
      { n: 8, error: "502 Bad Gateway", failed: 8, spread: {} },
    ],
  });
  assert.equal(t.single.rows[0].cells[5], "minion");
  assert.equal(t.single.rows[1].cells[5], "unknown");
  assert.equal(t.load.rows[0].cells[8], "minion 3 · helion 1");
  assert.equal(t.load.rows[0].cells[2], "3.00x");
  assert.equal(t.load.rows[1].error, "502 Bad Gateway");
  assert.deepEqual(t.shared.rows, []);
  assert.deepEqual(t.meta.filter((m) => m[0] === "Held by" || m[0] === "").map((m) => m[1]),
    ["minion · linux/amd64 · 1 engine · 4 slots", "helion · darwin/arm64 · engines not reported"]);
  assert.equal(spreadText({ b: 2, a: 2, unknown: 1 }), "a 2 · b 2 · unknown 1");
  assert.equal(clusterTables(null), null);
});

test("turning an MCP switch on is warned about, and both round-trip through the form", () => {
  const saved = { listen: "127.0.0.1:1234", mcp_allow_ephemeral: false, mcp_allow_configured: false };
  const form = { ...serverToForm(saved), mcp_allow_ephemeral: true };
  const changes = settingsChanges(saved, formToServer(form, saved), ["mcp_allow_ephemeral", "mcp_allow_configured"]);
  assert.deepEqual(changes, { mcp_allow_ephemeral: true });
  assert.equal(wideningWarnings(changes).length, 1);
  assert.match(wideningWarnings(changes)[0], /any address it can reach/);
  assert.deepEqual(wideningWarnings({ mcp_allow_ephemeral: false }), []);
});

import { slotUse, meshCapacity } from "./ui-model.js";

// Separate running and queued requests instead of showing "2 / 1"
// for a one-slot engine with a queued request.
test("slotUse separates running from waiting and counts free slots", () => {
  for (const [name, inflight, slots, want] of [
    ["one queued behind a one-slot engine", 2, 1, { running: 1, waiting: 1, free: 0 }],
    ["two of four slots busy", 2, 4, { running: 2, waiting: 0, free: 2 }],
    ["idle", 0, 4, { running: 0, waiting: 0, free: 4 }],
    ["exactly full, nothing waiting", 2, 2, { running: 2, waiting: 0, free: 0 }],
    ["slots unknown: waiting and free are not zero, they are unknown", 3, 0, { running: 3, waiting: null, free: null }],
  ]) {
    assert.deepEqual(slotUse(inflight, slots), want, name);
  }
});

test("meshCapacity flags work waiting on one engine while another has a slot free", () => {
  const e = (inflight, slots, healthy = true) => ({ healthy, slots, ...slotUse(inflight, slots) });
  assert.deepEqual(meshCapacity([e(1, 2), e(2, 1), e(0, 4)]),
    // These engines report no live write rate, so the total is unknown and all three are counted as not saying.
    { slots: 7, running: 2, waiting: 1, free: 5, waitingBesideFree: true, outputNow: null, outputUnknown: 3 });
  assert.equal(meshCapacity([e(3, 2), e(2, 1)]).waitingBesideFree, false, "waiting, but nothing free");
  assert.equal(meshCapacity([e(1, 2), e(0, 4)]).waitingBesideFree, false, "free, and nothing waiting");
  assert.equal(meshCapacity([e(0, 4, false), e(2, 1)]).free, 0);
  assert.deepEqual(meshCapacity(undefined), { slots: 0, running: 0, waiting: 0, free: 0, waitingBesideFree: false, outputNow: null, outputUnknown: 0 });
});

import { heldRequests, movedIn, waitedText } from "./ui-model.js";

test("heldRequests lists what every router is holding, longest wait first", () => {
  const nodes = [
    { name: "entry", held: [{ waited_ms: 4000, home: "minion", why: "its own engine is full" }] },
    { name: "other", held: [{ waited_ms: 361000, why: "a new conversation" }] },
    { name: "gpu" },
  ];
  assert.deepEqual(heldRequests(nodes), [
    { at: "other", waitedMs: 361000, home: "", why: "a new conversation" },
    { at: "entry", waitedMs: 4000, home: "minion", why: "its own engine is full" },
  ]);
  assert.deepEqual(heldRequests(undefined), []);
});

test("movedIn adds up what every router sent each node", () => {
  const nodes = [
    { routed: [{ node: "minion", calls: 200, moved: 9 }, { node: "helion", calls: 30, moved: 1 }] },
    { routed: [{ node: "minion", calls: 51, moved: 0 }] },
    {},
  ];
  assert.deepEqual(movedIn(nodes), { minion: { calls: 251, moved: 9 }, helion: { calls: 30, moved: 1 } });
  // A node nobody has routed to is absent, not zero: the page shows a dash.
  assert.equal(movedIn(nodes).xpredator, undefined);
});

test("waitedText says a wait the way a person would", () => {
  for (const [ms, want] of [[400, "0s"], [19500, "20s"], [61000, "1m 01s"], [361000, "6m 01s"]]) {
    assert.equal(waitedText(ms), want);
  }
});

import { gib } from "./ui-model.js";

test("an engine row carries its RAM cache, what it dropped, and its machine's memory", () => {
  const view = buildView({
    self: {
      node: "minion", mem_total_mb: 15853, mem_available_mb: 4795,
      instances: [{ id: "i1", model: "m", state: "ready", slots: 4, cache_ram_mib: 8192, cache_dropped: 12, memory_mb: 7372 }],
    },
    peers: [
      // Missing cache statistics from older peers remain unknown.
      { node: "old", alive: true, instances: [{ id: "i2", model: "m", state: "ready", slots: 2 }] },
      { node: "xpredator", alive: true, mem_total_mb: 128000, mem_available_mb: 87000,
        instances: [{ id: "i3", model: "m", state: "ready", slots: 2, cache_ram_mib: 32768, memory_mb: 17300 }] },
    ],
  });
  const by = Object.fromEntries(view.meshEngines.map((e) => [e.node, e]));
  assert.deepEqual(
    [by.minion.cacheRamMib, by.minion.cacheDropped, by.minion.memoryMb, by.minion.hostMemTotalMb, by.minion.hostMemAvailableMb],
    [8192, 12, 7372, 15853, 4795]);
  assert.deepEqual([by.xpredator.cacheRamMib, by.xpredator.cacheDropped], [32768, 0]);
  assert.deepEqual(
    [by.old.cacheRamMib, by.old.cacheDropped, by.old.memoryMb, by.old.hostMemTotalMb],
    [null, null, null, null]);
});

test("gib says a size in MiB the way a person would", () => {
  for (const [mib, want] of [[8192, "8.0 GB"], [6144, "6.0 GB"], [32768, "32 GB"], [7372, "7.2 GB"], [null, "—"], [undefined, "—"]]) {
    assert.equal(gib(mib), want);
  }
});

import { devlogAppend, devlogBody, devlogClock, devlogFields, devlogMatches } from "./ui-model.js";

test("the developer log keeps entries in time order, once each, and no more than its cap", () => {
  const at = (s, seq, node = "a") => ({ node, seq, time: `2026-10-07T10:00:${String(s).padStart(2, "0")}.000Z`, msg: `m${seq}` });
  const list = [];
  devlogAppend(list, at(1, 1), 3);
  devlogAppend(list, at(3, 2), 3);
  // A peer's entry that happened between the two arrives late.
  devlogAppend(list, at(2, 1, "b"), 3);
  assert.deepEqual(list.map((e) => e.node + e.seq), ["a1", "b1", "a2"]);
  // A reconnected stream replays its backlog: nothing is doubled.
  devlogAppend(list, at(3, 2), 3);
  assert.equal(list.length, 3);
  // Past the cap the oldest goes.
  devlogAppend(list, at(4, 3), 3);
  assert.deepEqual(list.map((e) => e.node + e.seq), ["b1", "a2", "a3"]);
});

test("the developer log's filter needs every word, anywhere in the row", () => {
  const e = { msg: "sent to minion: it holds 94% of this prompt", model: "qwen/qwen3.8-27b", trace: "a1b2c3d4", level: "info", node: "sites-01" };
  for (const [filter, want] of [["", true], ["minion", true], ["MINION 94%", true], ["a1b2c3", true], ["qwen sent", true], ["helion", false], ["minion helion", false]]) {
    assert.equal(devlogMatches(e, filter), want, filter);
  }
});

test("a captured body is indented when it is whole JSON and left alone when it is not", () => {
  assert.equal(devlogBody('{"a":[1,2]}', false), '{\n  "a": [\n    1,\n    2\n  ]\n}');
  assert.equal(devlogBody('data: {"x":1}\n\n', false), 'data: {"x":1}\n\n');
  // Cut at the cap, it is not valid JSON and must not be reported as malformed.
  assert.equal(devlogBody('{"a":[1,', true), '{"a":[1,\n… cut at the capture limit');
  assert.equal(devlogBody("", false), "");
});

test("the developer log's clock and figures read at a glance", () => {
  const d = new Date(2026, 9, 7, 9, 5, 3, 42);
  assert.equal(devlogClock(d.toISOString()), "09:05:03.042");
  assert.equal(devlogClock("nonsense"), "");
  assert.equal(devlogFields({ status: 200, holds_percent: { minion: 94 } }), 'status=200  holds_percent={"minion":94}');
  assert.equal(devlogFields({ tokens_per_sec: 83.95871814412483, ms: 9311 }), "tokens_per_sec=83.96  ms=9311");
  assert.equal(devlogFields(undefined), "");
});

import { gpuLabel } from "./ui-model.js";

test("an engine confined to a GPU says which, and one left alone says nothing", () => {
  for (const [gpu, want] of [["0", "GPU 0"], ["1", "GPU 1"], ["0,1", "GPUs 0,1"], ["", ""], [undefined, ""], [null, ""]]) {
    assert.equal(gpuLabel(gpu), want);
  }
  const view = buildView({
    self: { node: "minion", instances: [
      { id: "i1", model: "m", state: "ready", slots: 4, gpu: "0" },
      { id: "i2", model: "m", state: "ready", slots: 2, gpu: "1" },
    ] },
    // A peer on a build from before GPU selection reports nothing.
    peers: [{ node: "old", alive: true, instances: [{ id: "i3", model: "m", state: "ready", slots: 2 }] }],
  });
  assert.deepEqual(view.meshEngines.map((e) => [e.node, e.gpu]), [["minion", "0"], ["minion", "1"], ["old", ""]]);
});

import { freeGPU, gpuChoices, parseSettingsForm as parseForm } from "./ui-model.js";

test("a GPU selector lists a node's cards as nvidia-smi numbers them", () => {
  const gpus = [{ name: "NVIDIA RTX 6000 Ada Generation", vram_mb: 49140 }, { name: "NVIDIA GeForce RTX 4090", vram_mb: 24564 }];
  assert.deepEqual(gpuChoices(gpus), [
    { value: "0", label: "GPU 0 · RTX 6000 Ada · 48 GB" },
    { value: "1", label: "GPU 1 · RTX 4090 · 24 GB" },
  ]);
  assert.deepEqual(gpuChoices(undefined), []);
  // The chosen card is saved as the number, and unset means every card.
  assert.deepEqual(parseForm({ gpu: "1", parallel: "2" }).settings, { gpu: "1", parallel: 2 });
  assert.deepEqual(parseForm({ gpu: "" }).settings, {});
});

test("adding an engine offers the card that has none", () => {
  const gpus = [{ name: "A", vram_mb: 49140 }, { name: "B", vram_mb: 24564 }];
  for (const [name, instances, want] of [
    ["nothing loaded: the first card", [], "0"],
    ["one engine on GPU 0: the other card", [{ config: { gpu: "0" } }], "1"],
    ["one engine on GPU 1: the first card", [{ config: { gpu: "1" } }], "0"],
    ["both cards have an engine", [{ config: { gpu: "0" } }, { config: { gpu: "1" } }], ""],
    ["an engine spread over every card leaves none free", [{ config: {} }], ""],
  ]) {
    assert.equal(freeGPU(gpus, instances), want, name);
  }
});

import { engineKind, engineSummary } from "./ui-model.js";

test("an engine is named by its GPU, or by what it runs on", () => {
  for (const [runtime, gpu, want] of [
    ["llama.cpp-upstream-linux-x86_64-cuda-12.8@b11153", "1", "GPU 1"],
    ["llama.cpp-upstream-linux-x86_64-cuda-12.8@b11153", "", "CUDA"],
    ["llama.cpp-mac-arm64-apple-metal-advsimd@2.46.0", "", "Metal"],
    ["llama.cpp-linux-x86_64-cpu-avx2@2.40.0", "", "CPU"],
    ["mlx-lm@0.28", undefined, "MLX"],
    ["something-else", "", ""],
  ]) assert.equal(engineKind(runtime, gpu), want, runtime);
  assert.equal(engineSummary({ gpu: "0", parallel: 4 }), "GPU 0 · 4 slots");
  assert.equal(engineSummary({ parallel: 1, runtime: "llama.cpp-linux-x86_64-cpu-avx2" }), "CPU · 1 slot");
  assert.equal(engineSummary({}), "");
});

// Seen 2026-10-08: minion ran two engines of one model, one per GPU, and the
// Serving page said "3 replicas" for four engines on three machines.
test("a model's replicas are its engines, and a node running two says so", () => {
  const inst = (id, gpu) => ({ id, model: "m", state: "ready", slots: 1, gpu });
  const view = buildView({
    models: [{ id: "m", nodes: ["helion", "minion", "xpredator"] }],
    self: { node: "xpredator", instances: [inst("a")] },
    peers: [
      { node: "minion", alive: true, instances: [inst("b", "0"), inst("c", "1")] },
      { node: "helion", alive: true, instances: [inst("d")] },
    ],
  });
  const m = view.models.find((x) => x.id === "m");
  assert.equal(m.replicas, 4);
  assert.deepEqual([...m.servedBy].sort(), ["helion", "minion ×2", "xpredator"]);
});

import { gpuMemoryText } from "./ui-model.js";

// Seen 2026-10-08: a model loaded on "every GPU" was split 25.6 GB and 15.2 GB
// across two cards, and the dashboard called the engine "CUDA".
test("an engine split across cards says so, from what the cards report", () => {
  const split = [{ gpu: 0, mb: 25610 }, { gpu: 1, mb: 15220 }];
  const cuda = "llama.cpp-upstream-linux-x86_64-cuda-12.8@b11153";
  assert.equal(engineKind(cuda, "", split), "split across GPUs 0+1");
  // Left to choose and landing on one card, it is on that card.
  assert.equal(engineKind(cuda, "", [{ gpu: 1, mb: 21094 }]), "GPU 1");
  // Nothing measured: what was asked for, then the backend.
  assert.equal(engineKind(cuda, "0", []), "GPU 0");
  assert.equal(engineKind(cuda, "", undefined), "CUDA");
  assert.equal(gpuMemoryText(split), "GPU 0: 25 GB · GPU 1: 15 GB");
  assert.equal(gpuMemoryText([]), "");
  const view = buildView({ self: { node: "minion", instances: [{ id: "i1", model: "m", state: "ready", slots: 4, gpu_memory: split }] } });
  assert.deepEqual(view.meshEngines[0].gpuMemory, split);
});

test("a model's row carries how far its load has got", () => {
  const nodes = [{
    node: "minion", self: false,
    api: { models: [{ key: "m", path: "/m.gguf", loaded_instances: [] }, { key: "idle", path: "/i.gguf", loaded_instances: [] }] },
    operations: [{ id: "op1", kind: "load", model: "m", state: "running", fraction: 0.5, message: "loading weights onto the GPU: 8.5 of 17.0 GB" }],
  }];
  const by = Object.fromEntries(buildCatalog(nodes).rows.map((r) => [r.key, r]));
  assert.deepEqual([by.m.busy, by.m.busyKind, by.m.busyFraction, by.m.busyMessage], [true, "load", 0.5, "loading weights onto the GPU: 8.5 of 17.0 GB"]);
  assert.deepEqual([by.idle.busy, by.idle.busyFraction, by.idle.busyMessage], [false, 0, ""]);
});

import { curlFor, tryTargets } from "./ui-model.js";

test("a curl command to try a model, for the mesh and for one engine", () => {
  const e = { node: "minion", model: "qwen/qwen3.8-27b", address: "100.64.0.3:18002" };
  const [mesh, engine] = tryTargets({ url: "http://127.0.0.1:1234/v1", requireKey: false }, e);
  assert.equal(mesh.curl.split("\n")[0], "curl http://127.0.0.1:1234/v1/chat/completions \\");
  assert.ok(!mesh.curl.includes("Authorization"), "no key is needed on loopback, so none is shown");
  assert.equal(engine.curl.split("\n")[0], "curl http://100.64.0.3:18002/v1/chat/completions \\");
  assert.ok(engine.curl.includes('"model": "qwen/qwen3.8-27b"'));
  // A front door that wants a key: the command reads it from the environment,
  // and never has the key itself in it.
  const [keyed] = tryTargets({ url: "http://100.64.0.1:1234/v1", requireKey: true }, e);
  assert.ok(keyed.curl.includes('-H "Authorization: Bearer $MFSH_KEY"'));
  assert.equal(keyed.setup, "export MFSH_KEY=$(mfsh key)");
  // An engine on loopback is said to be reachable only from its own machine.
  const [, local] = tryTargets({ url: "http://127.0.0.1:1234/v1" }, { ...e, address: "127.0.0.1:18000" });
  assert.match(local.note, /works only on minion itself/);
  // No address reported: only the mesh's command is offered.
  assert.equal(tryTargets({ url: "http://127.0.0.1:1234/v1" }, { ...e, address: "" }).length, 1);
  // A quote in the prompt must not end the shell's string early.
  assert.ok(curlFor("http://h/v1/", { messages: [{ content: "it's" }] }).includes(`it'\\''s`));
});

import { entrypoints, publicTarget } from "./ui-model.js";

test("the command for calling from outside names the entrypoint and always carries a key", () => {
  const topo = [
    { node: "xpredator", listeners: [{ name: "front", enabled: true }, { name: "public", enabled: false }] },
    { node: "entrypoint-01", listeners: [{ name: "mesh", enabled: true }, { name: "public", enabled: true }] },
    null,
  ];
  assert.deepEqual(entrypoints(topo), ["entrypoint-01"]);
  assert.deepEqual(entrypoints(undefined), []);
  // The address is not something the node knows; until it is typed, a
  // reserved example stands in, and the block says it is not known.
  const unknown = publicTarget("entrypoint-01", "", "m");
  assert.equal(unknown.known, false);
  assert.equal(unknown.curl.split("\n")[0], "curl https://api.example.com/v1/chat/completions \\");
  // Typed with or without /v1 or a trailing slash, it is one address.
  for (const typed of ["https://api.example.org", "https://api.example.org/", "https://api.example.org/v1/"]) {
    const t = publicTarget("entrypoint-01", typed, "m");
    assert.equal(t.curl.split("\n")[0], "curl https://api.example.org/v1/chat/completions \\", typed);
    assert.equal(t.known, true);
    assert.ok(t.curl.includes('-H "Authorization: Bearer $MFSH_KEY"'));
  }
  assert.match(unknown.note, /must be one of entrypoint-01's own/);
});

import { connectPlaces, opencodeConfig, servedModels } from "./ui-model.js";

test("an opencode config for the mesh: its models, their real context, and a key only by name", () => {
  const models = servedModels([
    { model: "qwen/qwen3.8-27b", contextLength: 65536, vision: true },
    // The router may send a request to either engine, so the smaller window is the one to promise.
    // One engine reading images is enough: a request with an image is sent only to those.
    { model: "qwen/qwen3.8-27b", contextLength: 49152, vision: false },
    { model: "small", contextLength: 0 },
  ]);
  assert.deepEqual(models, [{ id: "qwen/qwen3.8-27b", context: 49152, vision: true }, { id: "small", context: 0, vision: false }]);

  const local = JSON.parse(opencodeConfig("http://127.0.0.1:1234/v1", models));
  const p = local.provider.modelfabric;
  assert.equal(p.npm, "@ai-sdk/openai-compatible");
  assert.deepEqual(p.options, { baseURL: "http://127.0.0.1:1234/v1" });
  // Without attachment and modalities opencode strips an attached image before sending, and the model
  // answers "this model doesn't support image input" though it is loaded to read images.
  assert.deepEqual(p.models["qwen/qwen3.8-27b"], {
    name: "qwen/qwen3.8-27b", limit: { context: 49152, output: 12288 },
    attachment: true, modalities: { input: ["text", "image"], output: ["text"] },
  });
  assert.deepEqual(p.models.small, { name: "small" });

  // With a key, the file names an environment variable and never holds the key.
  const keyed = opencodeConfig("https://api.example.com/v1", models, "MFSH_KEY");
  assert.equal(JSON.parse(keyed).provider.modelfabric.options.apiKey, "{env:MFSH_KEY}");
  assert.ok(!/sk-/.test(keyed));
});

test("where an app is decides its address and whether it needs a key", () => {
  const places = connectPlaces({ url: "http://127.0.0.1:1234/v1", requireKey: false }, "100.64.0.1:1234", { node: "entrypoint-01", url: "https://api.example.org/" });
  assert.deepEqual(places.map((p) => [p.key, p.base, p.keyVar]), [
    ["local", "http://127.0.0.1:1234/v1", ""],
    ["tailnet", "http://100.64.0.1:1234/v1", ""],
    ["public", "https://api.example.org/v1", "MFSH_KEY"],
  ]);
  assert.match(places[2].keyHelp, /must be one of entrypoint-01's own/);
  // No entrypoint in the mesh, no tailnet address known: only this machine.
  assert.deepEqual(connectPlaces({ url: "http://127.0.0.1:1234/v1" }, "", null).map((p) => p.key), ["local"]);
  // require_api_key is checked on the loopback front door only. The dialog once asked for a key on the
  // tailnet address too, which the mesh listener never checks: Tailscale membership is the credential there.
  const keyed = connectPlaces({ url: "http://127.0.0.1:1234/v1", requireKey: true }, "100.64.0.1:1234", null);
  assert.deepEqual(keyed.map((p) => [p.key, p.keyVar]), [["local", "MFSH_KEY"], ["tailnet", ""]]);
  assert.match(keyed[1].note, /being on the tailnet is the only credential/);
  // The public address is not something the node knows.
  assert.equal(connectPlaces({}, "", { node: "entrypoint-01", url: "" })[1].known, false);
});

test("what the mesh is writing now adds the engines that say, and counts the ones that do not", () => {
  // Four busy slots at 22 tok/s each showed as 22 beside a one-slot engine at 78: the per-request
  // speed made the engine producing the most look the slowest.
  const c = meshCapacity([
    { healthy: true, slots: 4, running: 4, outputNow: 88 },
    { healthy: true, slots: 1, running: 1, outputNow: 78 },
    { healthy: true, slots: 1, running: 1, outputNow: null }, // an older node
    { healthy: false, slots: 2, outputNow: 500 },
  ]);
  assert.equal(c.outputNow, 166);
  assert.equal(c.outputUnknown, 1);
  // No engine reports it: unknown, not zero.
  assert.equal(meshCapacity([{ healthy: true, slots: 1, outputNow: null }]).outputNow, null);
  // An idle mesh that does report it reads 0.
  assert.equal(meshCapacity([{ healthy: true, slots: 1, outputNow: 0 }]).outputNow, 0);
});

import { aliveNodes } from "./ui-model.js";

test("the nodes to ask are the ones the mesh says are up, this node first", () => {
  // The Requests tab's "Whole mesh" and the developer log took their node list from the My Models
  // cache, which holds only this node until a page showing peers' models has been opened.
  const view = buildView({
    self: { node: "b-self", alive: true },
    peers: [{ node: "zeta", alive: true }, { node: "down", alive: false }, { node: "alpha", alive: true }],
  });
  assert.deepEqual(aliveNodes(view), ["b-self", "alpha", "zeta"]);
  assert.deepEqual(aliveNodes(null), []);
});
