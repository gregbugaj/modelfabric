// Pure transform from the /z/mesh payload to what the page renders.
// Kept free of DOM access so it can be tested directly (see ui-model.test.mjs).

/**
 * Shape /api/v1/models into Local rows. `available: null` means no
 * supervisor, distinct from a supervisor with no discovered models.
 */
export function buildLocal(api, operations = []) {
  if (!api || !Array.isArray(api.models)) {
    return { supervised: false, models: [], loadedCount: 0 };
  }
  const busy = new Map();
  for (const op of operations) {
    if (op && op.state === "running" && op.model) busy.set(op.model, op);
  }

  const models = api.models.map((m) => {
    const instances = Array.isArray(m.loaded_instances) ? m.loaded_instances : [];
    const op = busy.get(m.key);
    return {
      key: m.key,
      displayName: m.display_name || m.key,
      type: m.type || "llm",
      params: m.params_string || "",
      quantization: m.quantization || "",
      sizeBytes: m.size_bytes ?? 0,
      vision: (m.capabilities ?? []).includes("vision"),
      instances: instances.map((i) => ({ id: i.id, config: i.config ?? {} })),
      loaded: instances.length > 0,
      // Block duplicate loads while an operation is in flight.
      busy: Boolean(op),
      busyKind: op ? op.kind : "",
      // How far the operation has got, 0 to 1, and where it is, when it says.
      busyFraction: op?.fraction > 0 ? op.fraction : 0,
      busyMessage: op?.message || "",
    };
  });

  return {
    supervised: true,
    models,
    loadedCount: models.reduce((n, m) => n + m.instances.length, 0),
  };
}

export function formatBytes(n) {
  if (!n || n < 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? v : v.toFixed(1)}${units[i]}`;
}

// Loopback engines are reachable only on their own machine. Tailnet
// binding permits direct consumers such as llm-d; routed requests
// can reach either through the node's front door.
const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost", ""]);

function loopbackOnly(instances) {
  return engineScope(instances) === "loopback";
}

// Engine reachability: "tailnet", "loopback", or "" when no engines are running.
export function engineScope(instances) {
  const ready = (instances ?? []).filter((i) => i.state === "ready");
  if (ready.length === 0) return "";
  return ready.every((i) => LOOPBACK.has(i.address ?? "")) ? "loopback" : "tailnet";
}

export function buildView(mesh) {
  const self = mesh?.self ?? {};
  const peers = Array.isArray(mesh?.peers) ? mesh.peers : [];
  const models = Array.isArray(mesh?.models) ? mesh.models : [];
  const preferred = mesh?.preferred_node || "";

  const nodes = [
    {
      name: self.node || "(unknown)",
      addr: self.addr || "local",
      platform: self.platform || "",
      osVersion: self.os_version || "",
      isSelf: true,
      alive: true,
      inflight: self.inflight ?? 0,
      // Front-door counts include Envoy queues and llm-d requests
      // that engine counts omit.
      accepted: self.accepted ?? 0,
      // Requests waiting at the router occupy no engine slot yet.
      queued: self.queued ?? 0,
      held: self.held ?? [],
      routed: self.routed ?? [],
      scheduler: self.scheduler ?? null,
      modelCount: (self.models ?? []).length,
      models: self.models ?? [],
      lastSeen: self.updated ?? null,
      preferred: preferred !== "" && preferred === self.node,
      loopbackEngines: loopbackOnly(self.instances),
      engineScope: engineScope(self.instances),
    },
    ...peers.map((p) => ({
      name: p.node || p.addr,
      addr: p.addr,
      platform: p.platform || "",
      osVersion: p.os_version || "",
      isSelf: false,
      alive: Boolean(p.alive),
      inflight: p.inflight ?? 0,
      accepted: p.accepted ?? 0,
      queued: p.queued ?? 0,
      held: p.held ?? [],
      routed: p.routed ?? [],
      scheduler: p.scheduler ?? null,
      modelCount: (p.models ?? []).length,
      models: p.models ?? [],
      lastSeen: p.last_seen ?? null,
      preferred: preferred !== "" && preferred === (p.node || p.addr),
      loopbackEngines: loopbackOnly(p.instances),
      engineScope: engineScope(p.instances),
    })),
  ];

  // Merge local router health by instance ID; peers publish their own instances.
  const health = new Map((self.engines ?? []).map((e) => [e.name, e]));
  const fromInstances = (node, list, isSelf, host) => (list ?? []).map((i) => {
    const live = isSelf ? health.get(i.id) : null;
    return {
      node,
      isSelf,
      // Host-RAM cache capacity and eviction counts; null when unreported.
      // This cache holds conversations swapped out of engine slots.
      cacheRamMib: i.cache_ram_mib > 0 ? i.cache_ram_mib : null,
      cacheDropped: typeof i.cache_dropped === "number" ? i.cache_dropped : (i.cache_ram_mib > 0 ? 0 : null),
      memoryMb: i.memory_mb > 0 ? i.memory_mb : null,
      // The GPUs the engine was confined to, as nvidia-smi numbers them; ""
      // when it was left to use every GPU, or the node is too old to say.
      gpu: typeof i.gpu === "string" ? i.gpu : "",
      // What the engine holds on each GPU now, measured: [{ gpu, mb }]. More
      // than one entry is a model split across cards.
      gpuMemory: Array.isArray(i.gpu_memory) ? i.gpu_memory : [],
      // Tokens one request may use on this engine; 0 when it did not say.
      contextLength: i.context_length > 0 ? i.context_length : 0,
      hostMemTotalMb: host?.mem_total_mb > 0 ? host.mem_total_mb : null,
      hostMemAvailableMb: host?.mem_available_mb > 0 ? host.mem_available_mb : null,
      id: i.id,
      model: i.model || "",
      runtime: i.runtime || "",
      engine: i.engine || "",
      address: i.address && i.port ? `${i.address}:${i.port}` : "",
      slots: i.slots ?? 0,
      // Measured prompt tokens per second; zero until enough work is measured.
      prefillTokS: i.prefill_tok_s ?? 0,
      // Display early estimates, but mark rates not yet reliable for scheduling.
      prefillTrusted: Boolean(i.prefill_trusted),
      decodeTokS: i.decode_tok_s ?? 0,
      // What the engine is writing per second right now, all slots together.
      // null when it does not say: an older node, or an engine whose log has no such figure.
      outputNow: typeof i.output_now_tok_s === "number" ? i.output_now_tok_s : null,
      specAccepted: typeof i.spec_accepted === "number" ? i.spec_accepted : -1,
      // Lifetime totals expose load imbalance that rates alone cannot show.
      promptTokens: i.prompt_tokens ?? 0,
      cachedTokens: i.cached_tokens ?? 0,
      outputTokens: i.output_tokens ?? 0,
      // Prefer the node's rolling average over browser samples. -1 means too few
      // samples; older peers may omit the field.
      loadAvg: typeof i.load_avg === "number" ? i.load_avg : -1,
      // Missing vision flags default to false, matching older peers' engines.
      vision: Boolean(i.vision),
      inflight: live ? live.inflight ?? 0 : i.inflight ?? 0,
      ...slotUse(live ? live.inflight ?? 0 : i.inflight ?? 0, i.slots ?? 0),
      kvUsage: typeof i.kv_usage === "number" ? i.kv_usage : -1,
      healthy: live ? Boolean(live.healthy) : i.state === "ready",
      state: i.state || "",
    };
  });
  const meshEngines = [
    ...fromInstances(self.node || "(unknown)", self.instances, true, self),
    ...peers.flatMap((p) => fromInstances(p.node || p.addr, p.instances, false, p)),
  ].sort((a, b) => (a.isSelf !== b.isSelf ? (a.isSelf ? -1 : 1) : a.node.localeCompare(b.node) || a.model.localeCompare(b.model)));

  const engines = (self.engines ?? []).map((e) => ({
    name: e.name,
    healthy: Boolean(e.healthy),
    inflight: e.inflight ?? 0,
    kvUsage: typeof e.kv_usage === "number" ? e.kv_usage : -1,
    models: e.models ?? [],
    error: e.error ?? "",
  }));

  // The scheduler may run on a peer, including an entrypoint without GPUs.
  const schedulerNode = nodes.find((n) => n.alive && n.scheduler) ?? null;

  const online = nodes.filter((n) => n.alive);
  // Only live nodes contribute load; a dead peer's last reported figure is
  // stale and would otherwise inflate the total indefinitely.
  const inflight = online.reduce((sum, n) => sum + n.inflight, 0);
  // Front-door counts include queued requests and llm-d traffic absent
  // from engine counts.
  const accepted = online.reduce((sum, n) => sum + (n.accepted ?? 0), 0);

  return {
    self: self.node || "",
    // Prefer models on this node; if it is absent, route to fallbacks.
    preferred,
    preferredOnline: preferred !== "" && nodes.some((n) => n.preferred && n.alive),
    scheduler: schedulerNode
      ? { node: schedulerNode.name, isSelf: Boolean(schedulerNode.isSelf), ...schedulerNode.scheduler }
      : null,
    stats: {
      nodesOnline: online.length,
      nodesTotal: nodes.length,
      models: models.length,
      inflight,
      accepted,
      enginesHealthy: engines.filter((e) => e.healthy).length,
      enginesTotal: engines.length,
    },
    nodes: nodes.sort(byNode),
    // An engine is a replica, and a node can run several of one model (one
    // per GPU). Counting nodes called three machines "3 replicas" when four
    // engines were serving.
    models: models.map((m) => {
      const per = new Map();
      for (const e of meshEngines) if (e.model === m.id) per.set(e.node, (per.get(e.node) ?? 0) + 1);
      const nodes = m.nodes ?? [];
      const counted = nodes.reduce((sum, n) => sum + (per.get(n) ?? 1), 0);
      return {
        id: m.id,
        nodes,
        servedBy: nodes.map((n) => ((per.get(n) ?? 1) > 1 ? `${n} ×${per.get(n)}` : n)),
        replicas: counted,
      };
    }),
    engines,
    meshEngines,
    capacity: {
      ...meshCapacity(meshEngines),
      held: online.reduce((sum, n) => sum + (n.queued ?? 0), 0),
    },
    heldRequests: heldRequests(online),
    movedIn: movedIn(online),
  };
}

// Split in-flight requests into running, waiting, and free slots.
// Without a reported slot count, waiting and free remain unknown (null).
export function slotUse(inflight, slots) {
  const n = Math.max(inflight || 0, 0);
  if (!slots) return { running: n, waiting: null, free: null };
  return { running: Math.min(n, slots), waiting: Math.max(n - slots, 0), free: Math.max(slots - n, 0) };
}

// Aggregate healthy engines; waitingBesideFree flags a queue on one
// engine while another has available slots.
export function meshCapacity(engines) {
  const known = (engines ?? []).filter((e) => e.healthy && e.slots);
  const sum = (k) => known.reduce((n, e) => n + (e[k] || 0), 0);
  const c = { slots: sum("slots"), running: sum("running"), waiting: sum("waiting"), free: sum("free") };
  // What the mesh is writing now. Engines that do not say are left out and
  // counted, so the total is never passed off as the whole mesh.
  const saying = (engines ?? []).filter((e) => e.healthy && typeof e.outputNow === "number");
  const outputNow = saying.length ? saying.reduce((n, e) => n + e.outputNow, 0) : null;
  const outputUnknown = (engines ?? []).filter((e) => e.healthy).length - saying.length;
  return { ...c, waitingBesideFree: c.waiting > 0 && c.free > 0, outputNow, outputUnknown };
}

function byNode(a, b) {
  if (a.isSelf !== b.isSelf) return a.isSelf ? -1 : 1;
  if (a.alive !== b.alive) return a.alive ? -1 : 1;
  return a.name.localeCompare(b.name);
}

export function relativeTime(iso, now = Date.now()) {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t) || t <= 0) return "—";
  const secs = Math.max(0, Math.round((now - t) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

/**
 * Token rows show the rotatable node key first, then named tokens newest
 * first. Only suffixes are available; token secrets are not retained.
 * Usage timestamps have minute precision; "never" means no recorded use.
 */
// maskedKey is a key as it is shown without showing it: how it begins, when
// the node says, and its last four characters. The beginning is the node's
// word and never assumed: a key made before the project was renamed begins
// "sk-llmz-", and was shown as "sk-mfsh-…" beside a Reveal that said otherwise.
export function maskedKey(prefix, hint) {
  return `${prefix || ""}…${hint ?? ""}`;
}

export function buildTokens(api, now = Date.now()) {
  const rows = [];
  if (api?.node_key) {
    rows.push({ id: "", name: "Node key", masked: maskedKey(api.node_key_prefix, api.node_key), created: "", lastUsed: "", builtin: true, rotatable: Boolean(api.rotatable) });
  }
  const tokens = [...(api?.tokens ?? [])].sort((a, b) => String(b.created).localeCompare(String(a.created)));
  for (const t of tokens) {
    rows.push({
      id: t.id,
      name: t.name,
      masked: maskedKey(t.prefix, t.hint),
      created: relativeTime(t.created, now),
      lastUsed: t.last_used ? relativeTime(t.last_used, now) : "never",
      builtin: false,
    });
  }
  return rows;
}

export const SERVER_SETTINGS = [
  "listen", "require_api_key", "public_listen", "cors_origins",
  "mcp_allow_ephemeral", "mcp_allow_configured",
  "jit_load", "jit_ttl", "jit_auto_evict", "mesh_admin", "engine_bind", "web_ui",
];

// Config omits empty strings and false; normalize them against absent keys.
const sameSetting = (a, b) => {
  if (Array.isArray(a) || Array.isArray(b)) return JSON.stringify(a ?? []) === JSON.stringify(b ?? []);
  if (typeof a === "boolean" || typeof b === "boolean") return Boolean(a) === Boolean(b);
  return (a ?? "") === (b ?? "");
};

/**
 * Send only changed settings so untouched config keys are preserved.
 */
export function settingsChanges(saved, form, keys = SERVER_SETTINGS) {
  const out = {};
  for (const k of keys) {
    if (k in form && !sameSetting(saved?.[k], form[k])) out[k] = form[k];
  }
  return out;
}

export function pendingRestart(view, keys = SERVER_SETTINGS) {
  if (!view) return [];
  const live = new Set(view.live ?? []);
  return keys.filter((k) => !live.has(k) && !sameSetting(view.saved?.[k], view.running?.[k]));
}

const loopbackHost = (addr) => {
  const host = String(addr ?? "").replace(/:\d+$/, "").replace(/^\[|\]$/g, "");
  return host === "" || host === "localhost" || host === "::1" || host.startsWith("127.");
};

const hostOf = (addr) => String(addr ?? "").replace(/:\d+$/, "");
const portOf = (addr) => (String(addr ?? "").match(/:(\d+)$/) ?? [])[1] ?? "";
const NO_TTL = new Set(["0", "off", "never"]);

export function serverToForm(saved) {
  const s = saved ?? {};
  const ttl = String(s.jit_ttl ?? "").trim();
  const unload = !NO_TTL.has(ttl);
  return {
    port: portOf(s.listen),
    front_on: Boolean(s.public_listen),
    front_port: portOf(s.public_listen),
    peer_admin: s.mesh_admin !== "off",
    engine_bind: s.engine_bind ?? "",
    web_ui: s.web_ui !== false,
    mesh_port: s.mesh_port,
    require_api_key: Boolean(s.require_api_key),
    mcp_allow_ephemeral: Boolean(s.mcp_allow_ephemeral),
    mcp_allow_configured: Boolean(s.mcp_allow_configured),
    cors_on: (s.cors_origins ?? []).length > 0,
    cors_origins: (s.cors_origins ?? []).join("\n"),
    jit_load: Boolean(s.jit_load),
    jit_unload: unload,
    jit_ttl: unload ? ttl : "",
    jit_auto_evict: s.jit_auto_evict !== false,
  };
}

/**
 * Preserve saved hosts that the form cannot edit so changing a port
 * cannot move a listener to another interface.
 */
export function formToServer(form, saved) {
  const s = saved ?? {};
  const f = form ?? {};
  const listenHost = hostOf(s.listen) || "127.0.0.1";
  // Default the public listener to loopback for a local TLS proxy or Funnel.
  // Preserve a host explicitly set in config.json.
  const frontHost = hostOf(s.public_listen) || "127.0.0.1";
  const origins = String(f.cors_origins ?? "").split(/[\s,]+/).map((o) => o.trim()).filter(Boolean);
  return {
    listen: f.port ? `${listenHost}:${f.port}` : (s.listen ?? ""),
    public_listen: f.front_on ? (f.front_port ? `${frontHost}:${f.front_port}` : (s.public_listen ?? "")) : "",
    // Preserve the saved enabled value: "same-owner" and "" are equivalent.
    mesh_admin: f.peer_admin === false ? "off" : (s.mesh_admin === "off" ? "" : (s.mesh_admin ?? "")),
    engine_bind: f.engine_bind ?? s.engine_bind ?? "",
    web_ui: f.web_ui ?? s.web_ui ?? true,
    require_api_key: f.require_api_key ?? Boolean(s.require_api_key),
    mcp_allow_ephemeral: f.mcp_allow_ephemeral ?? Boolean(s.mcp_allow_ephemeral),
    mcp_allow_configured: f.mcp_allow_configured ?? Boolean(s.mcp_allow_configured),
    cors_origins: f.cors_on ? origins : [],
    jit_load: f.jit_load ?? Boolean(s.jit_load),
    jit_ttl: f.jit_unload === false ? "0" : (f.jit_ttl ?? s.jit_ttl ?? ""),
    jit_auto_evict: f.jit_auto_evict ?? (s.jit_auto_evict !== false),
  };
}

export const ROUTER_SETTINGS = [
  "rate_weighted_routing", "prefix_affinity", "local_bias", "max_output_tokens", "cache_disk_mib", "cache_disk_dir",
];

/**
 * The form uses a switch and GB; config uses MiB, with 0 disabling cache.
 * Ignore size edits while disabled so re-enabling restores the last size.
 */
export function routerToForm(saved) {
  const s = saved ?? {};
  const mib = Number(s.cache_disk_mib) || 0;
  return {
    rate_weighted_routing: s.rate_weighted_routing !== false,
    prefix_affinity: s.prefix_affinity !== false,
    local_bias: String(s.local_bias ?? 0),
    max_output_tokens: String(s.max_output_tokens ?? 16384),
    cache_on: mib > 0,
    cache_gb: mib > 0 ? String(Math.round(mib / 1024)) : "",
    cache_disk_dir: s.cache_disk_dir ?? "",
  };
}

export function formToRouter(form, saved) {
  const f = form ?? {};
  const s = saved ?? {};
  const num = (v, def) => (v === "" || v === undefined || Number.isNaN(Number(v)) ? def : Number(v));
  return {
    rate_weighted_routing: f.rate_weighted_routing ?? s.rate_weighted_routing !== false,
    prefix_affinity: f.prefix_affinity ?? s.prefix_affinity !== false,
    local_bias: num(f.local_bias, s.local_bias ?? 0),
    max_output_tokens: num(f.max_output_tokens, s.max_output_tokens ?? 16384),
    cache_disk_mib: f.cache_on ? Math.round(num(f.cache_gb, 0) * 1024) : 0,
    cache_disk_dir: (f.cache_disk_dir ?? s.cache_disk_dir ?? "").trim(),
  };
}

/**
 * llm-d routes the model it schedules; ModelFabric routes the rest.
 */
export function routedBy(models, llmdModel) {
  return (models ?? []).map((m) => ({
    model: m.id,
    nodes: m.nodes ?? [],
    by: llmdModel && m.id === llmdModel ? "llmd" : "router",
  }));
}

export function displayPath(p) {
  return String(p ?? "").replace(/^\/(?:home|Users)\/[^/]+(?=\/|$)/, "~");
}

export function wideningWarnings(changes) {
  const out = [];
  if ("listen" in changes && !loopbackHost(changes.listen)) {
    out.push(`listen on ${changes.listen} serves this dashboard and model management beyond this machine.`);
  }
  if (changes.public_listen && !loopbackHost(changes.public_listen)) {
    out.push(`Public front door on ${changes.public_listen} answers other machines directly, in plain HTTP: a key would cross the network unencrypted. Keep it on 127.0.0.1 and publish it with Tailscale Funnel or a TLS proxy.`);
  }
  if ("engine_bind" in changes && !loopbackHost(changes.engine_bind)) {
    out.push("Engines on the tailnet: engine ports have no authentication of their own, so Tailscale ACLs become the only thing in front of them.");
  }
  if ((changes.cors_origins ?? []).includes("*")) {
    out.push("cors_origins \"*\" lets any web page you visit call /v1 on this node from your browser.");
  }
  if (changes.mcp_allow_ephemeral === true) {
    out.push("Per-request MCPs: anyone who can call /api/v1/chat can make this node connect to any address it can reach, including ones inside your network.");
  }
  if (changes.mcp_allow_configured === true) {
    out.push("mcp.json servers: a local server listed there is started as a process on this machine for each request that names it.");
  }
  if (changes.web_ui === false) out.push("web_ui off removes this dashboard after the next restart.");
  return out;
}

/**
 * Shape /api/v1/front for Overview. An empty `url` lets the caller use
 * the dashboard origin, which is served by the front door. Authentication
 * and public-listener state have no fallback and must remain unknown
 * unless reported by the node.
 */
export function buildFront(api) {
  const front = api ?? {};
  return {
    url: front.listen ? `http://${front.listen}/v1` : "",
    requireKey: Boolean(front.require_api_key),
    publicListen: front.public_listen || "",
  };
}

const BACKEND_LABELS = { cuda: "CUDA", vulkan: "Vulkan", cpu: "CPU", rocm: "ROCm", metal: "Metal" };
const SOURCE_LABELS = { upstream: "modelfabric", lmstudio: "LM Studio", custom: "config", marie: "Marie" };

export function buildRuntime(api, available = null, operations = []) {
  if (!api || !Array.isArray(api.runtimes)) {
    return { managed: false, hardware: null, runtimes: [], installing: [], options: [], updates: [] };
  }
  const hw = api.hardware ?? null;
  const pinned = api.selection && api.selection !== "auto" ? api.selection : "";

  const runtimes = api.runtimes.map((r) => {
    const [family, version = ""] = String(r.name).split("@");
    const inUse = Array.isArray(r.in_use) ? r.in_use.length : 0;
    const managed = Boolean(r.managed);
    return {
      name: r.name,
      displayName: r.display_name || family,
      family,
      version,
      build: r.llama_build ? `b${r.llama_build}` : "",
      buildNumber: r.llama_build ?? 0,
      backend: BACKEND_LABELS[r.backend] ?? r.backend ?? "—",
      source: managed ? "modelfabric" : SOURCE_LABELS[r.origin] ?? r.origin ?? "—",
      fit: r.fit || "unknown",
      reasons: r.reasons ?? [],
      isDefault: Boolean(r.default),
      inUse,
      managed,
      // Never offer what cannot run here, nor re-pin the current pin.
      canSelect: r.fit !== "no" && !(r.default && pinned),
      // Removal is ModelFabric's own builds only, and never one serving a model.
      canRemove: managed && inUse === 0,
    };
  });
  runtimes.sort((a, b) =>
    (b.isDefault - a.isDefault) || (b.buildNumber - a.buildNumber) || a.name.localeCompare(b.name));

  const installing = operations
    .filter((op) => op && op.kind === "runtime-get" && op.state === "running")
    .map((op) => ({ id: op.id, name: op.model, message: op.message || "starting", fraction: op.fraction ?? 0 }));

  const options = [];
  const seen = new Set();
  for (const key of ["recommended", "cuda", "vulkan", "cpu", "rocm"]) {
    const o = available?.options?.[key];
    if (!o || o.error || seen.has(o.name)) continue;
    seen.add(o.name);
    options.push({
      key,
      recommended: key === "recommended",
      name: o.name,
      displayName: o.display_name || o.name,
      backend: BACKEND_LABELS[o.backend] ?? o.backend,
      build: o.build,
      reason: o.reason ?? "",
      downloadBytes: o.download_bytes ?? 0,
      installed: Boolean(o.installed),
      installing: installing.some((i) => i.name === o.name),
    });
  }

  const updates = (available?.updates ?? []).map((u) => ({
    family: u.family,
    installed: u.installed ? `b${u.installed}` : "—",
    latest: u.latest?.build ?? "—",
    latestName: u.latest?.name ?? "",
    backend: u.latest?.backend ?? "",
    backendVersion: u.latest?.backend_version ?? "",
    available: Boolean(u.available),
    downloadBytes: u.latest?.download_bytes ?? 0,
    error: u.error ?? "",
    installing: installing.some((i) => i.name === u.latest?.name),
  }));

  return {
    managed: true,
    canInstall: Boolean(api.can_install),
    selection: pinned ? { auto: false, name: pinned } : { auto: true, name: runtimes.find((r) => r.isDefault)?.name ?? "" },
    hardware: hw && {
      cpu: hw.cpu || "unknown CPU",
      memoryBytes: (hw.memory_mb ?? 0) * 1024 * 1024,
      gpus: (hw.gpus ?? []).map((g) => ({
        name: g.name,
        memoryBytes: (g.memory_mb ?? 0) * 1024 * 1024,
        compute: g.compute_cap || "",
        driver: g.driver || "",
      })),
      cuda: hw.cuda_version || "",
      vulkan: Boolean(hw.vulkan),
    },
    runtimes,
    installing,
    options,
    updates,
    checked: Boolean(available),
    newestBuild: available?.newest_build ?? "",
  };
}

export function buildRouting(llmd, profilesApi, operations = [], models = []) {
  // Null represents a node whose llm-d endpoint returns 501; disable controls.
  if (!llmd) return { available: false };
  const profiles = (profilesApi?.profiles ?? []);
  const titleOf = (name) => profiles.find((p) => p.name === name)?.title ?? name;
  const install = operations.find((op) => op && op.kind === "llmd-install" && op.state === "running");
  return {
    available: true,
    llmd: {
      installed: Boolean(llmd.installed),
      state: llmd.state,
      running: llmd.state === "running",
      on: llmd.state !== "disabled",
      model: llmd.model || "",
      profile: llmd.profile || "",
      profileTitle: llmd.profile ? titleOf(llmd.profile) : "",
      peak: llmd.peak_prefill_tok_s || 0,
      calibrated: Boolean(llmd.calibrated),
      endpoints: llmd.endpoints ?? [],
      error: llmd.error || "",
    },
    installing: install ? { message: install.message || "starting", fraction: install.fraction ?? 0 } : null,
    // The caller supplies every model with an engine in the mesh.
    models: [...new Set(models)].sort(),
    profiles: profiles.filter((p) => p.available).map((p) => ({
      name: p.name, title: p.title, summary: p.summary, wellLit: p.well_lit_path || "",
      experimental: Boolean(p.experimental), active: llmd.state !== "disabled" && p.name === llmd.profile,
    })),
    unavailable: profiles.filter((p) => !p.available).map((p) => ({
      title: p.title, reason: p.reason, wellLit: p.well_lit_path || "",
    })),
  };
}

/**
 * Load and inference fields grouped for the form.
 * `lms` names the corresponding LM Studio setting.
 */
export const SETTINGS_SCHEMA = [
  { group: "Context & GPU", fields: [
    { key: "context_length", label: "Context length", type: "int", help: "tokens per request" },
    { key: "parallel", label: "Parallel slots", type: "int" },
    // Shown only on a node with more than one GPU; its choices come from
    // that node's hardware (see gpuChoices).
    { key: "gpu", label: "GPU", type: "gpu",
      help: "which GPU this engine runs on, as nvidia-smi numbers them. Unset, one engine spreads the model across every GPU, which is for a model too large for one card" },
    { key: "gpu_layers", label: "GPU layers", type: "int", help: "or use GPU offload ratio" },
    { key: "offload_ratio", label: "GPU offload ratio", type: "float", help: "0–1, LM Studio's GPU offload" },
    { key: "flash_attention", label: "Flash attention", type: "bool" },
    // Disabling vision frees projector memory and permits speculative decoding.
    { key: "vision", label: "Serve images", type: "bool",
      help: "off loads a multimodal model without its projector: no images, but it can speculate — measured 134 tok/s against 66 on the same request" },
  ] },
  // Keep the thinking toggle in Inference to avoid duplicating --reasoning.
  // The allowance belongs in Load because changing it requires a reload.
  { group: "Thinking", fields: [
    { key: "reasoning_budget", label: "Allowance", type: "int",
      help: "tokens the model may spend thinking before it answers: -1 unrestricted, 0 ends it at once. Fixed at launch — changing it reloads the model. Measured on qwen3: 48 held reasoning to ~170 characters where -1 gave 700–1100" },
  ] },
  { group: "Speculative decoding", fields: [
    { key: "spec_mode", label: "Mode", type: "select", options: ["auto", "mtp", "draft", "off"],
      help: "auto: the MTP head when the model has one, including a vision load — llama.cpp drafts a prompt carrying an image correctly in current builds. Set off on a build that still fails one with \"failed to process mtmd chunk\"" },
    { key: "draft_model", label: "Draft model", type: "model" },
    { key: "draft_max", label: "Max draft tokens", type: "int" },
    { key: "draft_min", label: "Min draft tokens", type: "int" },
    { key: "draft_p_min", label: "Min draft probability", type: "float", help: "0–1" },
    { key: "draft_gpu_layers", label: "Draft GPU layers", type: "int" },
  ] },
  { group: "Batching & KV cache", fields: [
    { key: "batch_size", label: "Batch size", type: "int" },
    { key: "ubatch_size", label: "Physical batch size", type: "int" },
    { key: "cache_type_k", label: "K cache type", type: "select", options: ["f16", "bf16", "q8_0", "q5_1", "q5_0", "q4_1", "q4_0", "iq4_nl", "f32"] },
    { key: "cache_type_v", label: "V cache type", type: "select", options: ["f16", "bf16", "q8_0", "q5_1", "q5_0", "q4_1", "q4_0", "iq4_nl", "f32"] },
    { key: "kv_unified", label: "Unified KV cache", type: "bool" },
    { key: "kv_offload", label: "KV cache on GPU", type: "bool" },
    { key: "cache_reuse", label: "Cache reuse chunk", type: "int" },
    { key: "ctx_checkpoints", label: "Context checkpoints", type: "int" },
    { key: "cache_ram", label: "RAM cache (MiB)", type: "int",
      help: "host memory for conversations that are not in a slot right now. A conversation that loses its slot is saved here and restored in about half a second; when this is full the oldest is dropped and read again from the start. Unset: 8192 or an eighth of the machine's RAM, whichever is less. 0 turns it off. The Serving page shows how many each engine has dropped" },
  ] },
  { group: "Memory & MoE", fields: [
    { key: "keep_in_memory", label: "Keep in memory (mlock)", type: "bool" },
    { key: "try_mmap", label: "Memory-map model", type: "bool" },
    { key: "n_cpu_moe", label: "CPU expert layers", type: "int", help: "MoE: experts of the first N layers stay on the CPU" },
    { key: "cpu_moe_ratio", label: "CPU expert ratio", type: "float", help: "0–1 of the layers" },
  ] },
  { group: "Model & threads", fields: [
    { key: "rope_freq_base", label: "RoPE frequency base", type: "float" },
    { key: "rope_freq_scale", label: "RoPE frequency scale", type: "float" },
    { key: "threads", label: "Threads", type: "int" },
    { key: "threads_batch", label: "Batch threads", type: "int" },
    { key: "seed", label: "Seed", type: "int" },
  ] },
  { group: "Inference defaults", inference: true, fields: [
    { key: "temperature", label: "Temperature", type: "float" },
    { key: "top_k", label: "Top-k", type: "int" },
    { key: "top_p", label: "Top-p", type: "float" },
    { key: "min_p", label: "Min-p", type: "float" },
    { key: "repeat_penalty", label: "Repeat penalty", type: "float" },
    { key: "presence_penalty", label: "Presence penalty", type: "float" },
    { key: "frequency_penalty", label: "Frequency penalty", type: "float" },
    { key: "enable_thinking", label: "Thinking", type: "bool" },
    // Use levels declared by the model template; generic llama.cpp levels
    // can cause Jinja errors. Fall back to text input when none are declared.
    { key: "reasoning_effort", label: "Thinking effort", type: "text",
      help: "the levels your model's template accepts" },
  ] },
];

export const PRESET_FIELDS = SETTINGS_SCHEMA.filter((g) => g.inference).flatMap((g) => g.fields)
  .concat([{ key: "seed", label: "Seed", type: "int" }]);

/**
 * Parse form strings into settings ("" inherits), reporting invalid fields.
 */
export function parseSettingsForm(values, fields = SETTINGS_SCHEMA.flatMap((g) => g.fields)) {
  const settings = {};
  const errors = {};
  for (const f of fields) {
    const raw = (values[f.key] ?? "").toString().trim();
    if (raw === "") continue;
    if (f.type === "int") {
      if (!/^-?\d+$/.test(raw)) { errors[f.key] = "whole number"; continue; }
      settings[f.key] = Number(raw);
    } else if (f.type === "float") {
      const n = Number(raw);
      // Reject Infinity and overflow such as 1e309; Number.isNaN alone
      // accepts values that cannot be represented in JSON.
      if (!Number.isFinite(n)) { errors[f.key] = "number"; continue; }
      settings[f.key] = n;
    } else if (f.type === "bool") {
      if (raw !== "on" && raw !== "off") { errors[f.key] = "on or off"; continue; }
      settings[f.key] = raw === "on";
    } else {
      settings[f.key] = raw;
    }
  }
  if (values.extra_args && values.extra_args.trim()) {
    settings.extra_args = values.extra_args.trim().split(/\s+/);
  }
  return { settings, errors };
}

export function settingsToForm(settings = {}) {
  const values = {};
  for (const [k, v] of Object.entries(settings)) {
    if (k === "extra_args") values.extra_args = (v ?? []).join(" ");
    else if (typeof v === "boolean") values[k] = v ? "on" : "off";
    else if (v !== null && typeof v === "object") continue;
    else values[k] = String(v);
  }
  return values;
}


const CAPABILITY_ORDER = ["vision", "tool_use", "reasoning"];

/**
 * Merge node catalogs. `nodes` is [{ node, self, api, operations, error }].
 * Report unreadable catalogs in `unreachable` rather than omitting the node.
 */
export function buildCatalog(nodes = []) {
  const rows = [];
  const unreachable = [];
  const perNode = [];
  // Thinking levels belong to the model file; share known levels across
  // rows so older peers do not show different controls for the same model.
  const effortsByKey = new Map();
  for (const n of nodes) {
    for (const m of n.api?.models ?? []) {
      if (m.reasoning_efforts?.length && !effortsByKey.has(m.key)) {
        effortsByKey.set(m.key, m.reasoning_efforts);
      }
    }
  }
  for (const n of nodes) {
    if (!n.api || !Array.isArray(n.api.models)) {
      unreachable.push({ node: n.node, error: n.error || "no model list" });
      continue;
    }
    const busy = new Map();
    for (const op of n.operations ?? []) {
      if (op && op.state === "running" && op.model) busy.set(op.model, op);
    }
    let bytes = 0;
    for (const m of n.api.models) {
      const instances = Array.isArray(m.loaded_instances) ? m.loaded_instances : [];
      const caps = m.capabilities ?? [];
      const op = busy.get(m.key);
      bytes += m.size_bytes ?? 0;
      rows.push({
        id: `${n.node}::${m.key}`,
        node: n.node,
        self: Boolean(n.self),
        key: m.key,
        publisher: m.publisher || (m.key.includes("/") ? m.key.split("/")[0] : ""),
        name: m.key.includes("/") ? m.key.slice(m.key.indexOf("/") + 1) : m.key,
        arch: m.architecture || "",
        mtp: (m.draft_layers ?? 0) > 0,
        params: m.params_string || "",
        quant: m.quantization || "",
        format: (m.format || "").toUpperCase(),
        type: m.type || "llm",
        sizeBytes: m.size_bytes ?? 0,
        modified: m.modified || null,
        file: m.file || "",
        maxContext: m.max_context_length ?? 0,
        layers: m.layers ?? 0,
        minMemory: m.min_memory_bytes ?? 0,
        caps: CAPABILITY_ORDER.filter((c) => caps.includes(c)),
        spec: m.spec ?? null,
        reasoningEfforts: m.reasoning_efforts ?? effortsByKey.get(m.key) ?? [],
        instances: instances.map((i) => ({ id: i.id, config: i.config ?? {}, port: i.port ?? 0, origin: i.origin || "" })),
        loaded: instances.length > 0,
        busy: Boolean(op),
        busyKind: op ? op.kind : "",
        // How far the operation has got, 0 to 1, and where it is, when it says.
        busyFraction: op?.fraction > 0 ? op.fraction : 0,
        busyMessage: op?.message || "",
      });
    }
    perNode.push({ node: n.node, self: Boolean(n.self), models: n.api.models.length, bytes });
  }
  rows.sort((a, b) => (a.self !== b.self ? (a.self ? -1 : 1) : a.node.localeCompare(b.node) || a.key.localeCompare(b.key)));
  return { rows, unreachable, perNode };
}

export function filterCatalog(rows, node = "", text = "") {
  const q = text.trim().toLowerCase();
  return rows.filter((r) => (!node || r.node === node) &&
    (!q || [r.key, r.arch, r.quant, r.params, r.publisher, r.node].some((v) => v.toLowerCase().includes(q))));
}

/**
 * Use publisher model.yaml values as placeholders for inherited settings.
 */
export function inheritedValue(spec, key) {
  const s = spec?.sampling ?? {};
  if (key in s && s[key] !== null && s[key] !== undefined) return String(s[key]);
  if (key === "enable_thinking" && spec?.template_vars && "enable_thinking" in spec.template_vars) {
    return spec.template_vars.enable_thinking ? "on" : "off";
  }
  // Use the publisher's effort when nothing overrides it.
  if (key === "reasoning_effort" && spec?.template_vars && "reasoning_effort" in spec.template_vars) {
    return String(spec.template_vars.reasoning_effort);
  }
  return "";
}


const escapeHTML = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

function inlineMarkdown(s) {
  // Input is already escaped; only these constructs become markup.
  return s
    .replace(/&lt;br\s*\/?&gt;/gi, "<br>") // model cards use <br> for line breaks; the one tag let through
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<em>$2</em>")
    .replace(/!\[[^\]]*\]\([^)]*\)/g, "") // images are dropped: no remote fetches from a model card
    .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
}

/**
 * Render untrusted model cards by escaping HTML before applying a limited
 * Markdown subset: headings, paragraphs, lists, code, emphasis, rules,
 * and http(s) links. Raw HTML stays text; images are omitted.
 */
export function renderMarkdown(md = "") {
  const lines = md.replace(/\r\n/g, "\n").split("\n");
  const out = [];
  let list = null;  // "ul" | "ol"
  let para = [];
  const flush = () => {
    if (para.length) { out.push(`<p>${inlineMarkdown(para.join(" "))}</p>`); para = []; }
  };
  const closeList = () => { if (list) { out.push(`</${list}>`); list = null; } };
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    if (/^```/.test(raw)) {
      flush(); closeList();
      const code = [];
      for (i++; i < lines.length && !/^```/.test(lines[i]); i++) code.push(lines[i]);
      out.push(`<pre><code>${escapeHTML(code.join("\n"))}</code></pre>`);
      continue;
    }
    const line = escapeHTML(raw);
    let m;
    if ((m = line.match(/^(#{1,6})\s+(.*)$/))) {
      flush(); closeList();
      const level = Math.min(m[1].length + 1, 6); // the page already has an h1
      out.push(`<h${level}>${inlineMarkdown(m[2])}</h${level}>`);
    } else if (/^\s*([-*_])\s*\1\s*\1[\s\-*_]*$/.test(raw)) {
      flush(); closeList(); out.push("<hr>");
    } else if ((m = line.match(/^\s*[-*+]\s+(.*)$/)) || (m = line.match(/^\s*\d+[.)]\s+(.*)$/))) {
      flush();
      const kind = /^\s*\d/.test(line) ? "ol" : "ul";
      if (list !== kind) { closeList(); out.push(`<${kind}>`); list = kind; }
      out.push(`<li>${inlineMarkdown(m[1])}</li>`);
    } else if (!line.trim()) {
      flush(); closeList();
    } else if (/^\s*\|/.test(line)) {
      flush(); closeList();
      out.push(`<pre class="md-table">${line}</pre>`);
    } else {
      closeList(); para.push(line.trim());
    }
  }
  flush(); closeList();
  return out.join("\n");
}

export function compactCount(n = 0) {
  if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (n >= 1e3) return Math.round(n / 1e3) + "K";
  return String(n);
}

/**
 * Download eligibility and existing state per node. `storage` holds node
 * /api/v1/storage responses, `rows` is the catalog, and `ops` maps node
 * to operations.
 */
export function downloadTargets({ repo, option, nodes = [], self = "", storage = {}, rows = [], ops = {} }) {
  const files = new Set((option?.files ?? []).map((f) => f.split("/").pop()));
  return nodes.map((node) => {
    const st = storage[node] ?? null;
    const t = { node, self: node === self, gpu: "", vramBytes: 0, freeBytes: null, state: "ready", note: "", selectable: true, op: null };
    const g = st?.gpus?.[0];
    if (g) { t.gpu = g.name; t.vramBytes = (g.vram_mb ?? 0) * 1024 * 1024; }
    if (st && typeof st.free_bytes === "number") t.freeBytes = st.free_bytes;
    const mine = rows.filter((r) => r.node === node);
    const running = (ops[node] ?? []).find((o) =>
      o && o.kind === "download" && o.state === "running" && typeof o.model === "string" && o.model.startsWith(repo));
    if (st?.entrypoint) {
      Object.assign(t, { state: "entrypoint", note: "entrypoint — runs no models", selectable: false });
    } else if (running) {
      Object.assign(t, { state: "downloading", note: running.message || "starting", selectable: false, op: running });
    } else if (mine.some((r) => files.has(r.file))) {
      Object.assign(t, { state: "have", note: `has ${option.quant}`, selectable: false });
    } else if (t.freeBytes !== null && option && t.freeBytes < option.bytes) {
      Object.assign(t, { state: "nospace", selectable: false,
        note: `needs ${formatBytes(option.bytes)}, ${formatBytes(t.freeBytes)} free` });
    } else {
      const other = mine.find((r) => r.file && r.file.toLowerCase().startsWith(repo.split("/").pop().replace(/-gguf$/i, "").toLowerCase()));
      t.note = other ? `has ${other.quant}; adds ${option?.quant}` : "";
      if (option && t.vramBytes && option.bytes > t.vramBytes) t.warn = `larger than its ${formatBytes(t.vramBytes)} GPU; loads partly on CPU`;
    }
    return t;
  });
}

/**
 * Derive request paths from node topologies because routing is decided
 * per request. `topos` contains readable /api/v1/topology responses.
 * The front listener is the router; llm-d uses Envoy and its EPP endpoints.
 */
export function buildEdges(topos = [], from = "") {
  const edges = [];
  const src = topos.find((t) => t.node === from);
  if (!src) return edges;
  const front = { node: from, kind: "listener", id: "front" };
  const preferred = src.preferred || "";
  const engineAt = new Map(); // "addr:port" as seen from `from` -> {node, id}
  for (const t of topos) {
    for (const e of t.engines ?? []) {
      // Peer loopback addresses are unreachable here and may collide
      // with local engine addresses.
      if (LOOPBACK.has(e.addr ?? "") && t.node !== from) continue;
      engineAt.set(`${e.addr}:${e.port}`, { node: t.node, id: e.id });
    }
  }
  // Requests for the llm-d model go through Envoy, bypassing router placement.
  const llmdModel = src.llmd?.model || "";
  if (llmdModel) {
    const box = { node: from, kind: "listener", id: "llmd" };
    edges.push({ from: front, to: box, style: "llmd", model: llmdModel });
    for (const ep of src.llmd.endpoints ?? []) {
      const target = engineAt.get(ep);
      if (target) edges.push({ from: box, to: { node: target.node, kind: "engine", id: target.id }, style: "llmd", model: llmdModel });
    }
  }
  for (const t of topos) {
    const mesh = t.listeners?.find((l) => l.name === "mesh");
    const marked = t.node === preferred;
    // Multiple engines on a peer share its mesh listener; draw one hop per model.
    const forwarded = new Set();
    for (const e of t.engines ?? []) {
      if (e.model === llmdModel) continue;
      // mesh.Candidates uses the peer's mesh listener, regardless of engine
      // binding. Only local engines are dialled directly.
      if (t.node === from) {
        edges.push({ from: front, to: { node: t.node, kind: "engine", id: e.id }, style: marked ? "preferred" : "direct", model: e.model });
        continue;
      }
      if (!mesh) continue; // a peer with no tailnet listener cannot be reached
      const box = { node: t.node, kind: "listener", id: "mesh" };
      if (!forwarded.has(e.model)) {
        forwarded.add(e.model);
        edges.push({ from: front, to: box, style: marked ? "preferred" : "forwarded", model: e.model });
      }
      edges.push({ from: box, to: { node: t.node, kind: "engine", id: e.id }, style: "forwarded", model: e.model });
    }
  }
  // Show reachable peers even without loaded models so an idle mesh
  // does not appear disconnected. Avoid duplicate request-path links.
  const reached = new Set(edges.filter((e) => e.to.kind === "listener" && e.to.id === "mesh").map((e) => e.to.node));
  for (const t of topos) {
    if (t.node === from || reached.has(t.node)) continue;
    if (!t.listeners?.some((l) => l.name === "mesh")) continue;
    edges.push({ from: front, to: { node: t.node, kind: "listener", id: "mesh" }, style: "peer" });
  }
  return edges;
}

export function constellation(topos = [], model = "") {
  const models = [...new Set(topos.flatMap((t) => (t.engines ?? []).map((e) => e.model)))].sort();
  if (!model || !models.includes(model)) model = models[0] ?? "";
  const serving = [];
  for (const t of topos) {
    const engines = (t.engines ?? []).filter((e) => e.model === model);
    if (!engines.length) continue;
    serving.push({
      node: t.node, role: t.role, gpu: t.gpus?.[0] ?? null,
      platform: t.platform || "", osVersion: t.os_version || "",
      slots: engines.reduce((n, e) => n + (e.slots || 0), 0),
      inflight: engines.reduce((n, e) => n + (e.inflight || 0), 0),
      prefill: Math.max(0, ...engines.map((e) => e.prefill_tok_s || 0)),
      context: Math.max(0, ...engines.map((e) => e.context || 0)),
      engines: engines.map((e) => e.id),
    });
  }
  const servingNodes = new Set(serving.map((s) => s.node));
  const entries = [];
  for (const t of topos) {
    const links = buildEdges(topos, t.node)
      .filter((e) => e.model === model && e.to.kind === "engine" && servingNodes.has(e.to.node))
      .map((e) => ({ to: e.to.node, style: e.style === "direct" && e.from.id === "mesh" ? "forwarded" : e.style }));
    for (const e of buildEdges(topos, t.node)) {
      if (e.model === model && e.to.kind === "listener" && e.to.id === "mesh" && servingNodes.has(e.to.node)) {
        links.push({ to: e.to.node, style: e.style });
      }
    }
    // A node with no path to this model cannot be its entry point.
    if (!links.length) continue;
    const seen = new Set();
    entries.push({
      node: t.node, role: t.role,
      via: t.llmd?.model === model ? "llmd" : "direct",
      public: (t.listeners ?? []).some((l) => l.name === "public"),
      links: links.filter((l) => !seen.has(l.to + l.style) && seen.add(l.to + l.style)),
    });
  }
  const others = topos.filter((t) => !servingNodes.has(t.node) && !entries.find((e) => e.node === t.node)).map((t) => t.node);
  return { model, models, serving, entries, others };
}

export function constellationAll(topos = []) {
  const models = [...new Set(topos.flatMap((t) => (t.engines ?? []).map((e) => e.model)))].sort();
  const nodes = [];
  for (const t of topos) {
    const engines = t.engines ?? [];
    if (!engines.length) continue;
    const byModel = {};
    for (const e of engines) {
      const m = (byModel[e.model] ??= { model: e.model, slots: 0, inflight: 0 });
      m.slots += e.slots || 0;
      m.inflight += e.inflight || 0;
    }
    nodes.push({
      node: t.node, role: t.role, gpu: t.gpus?.[0] ?? null,
      platform: t.platform || "", osVersion: t.os_version || "",
      slots: engines.reduce((n, e) => n + (e.slots || 0), 0),
      inflight: engines.reduce((n, e) => n + (e.inflight || 0), 0),
      prefill: Math.max(0, ...engines.map((e) => e.prefill_tok_s || 0)),
      serves: Object.values(byModel),
    });
  }
  const summary = models.map((model) => {
    const on = nodes.filter((n) => n.serves.some((s) => s.model === model));
    return {
      model, nodes: on.map((n) => n.node),
      slots: on.reduce((k, n) => k + n.serves.find((s) => s.model === model).slots, 0),
      inflight: on.reduce((k, n) => k + n.serves.find((s) => s.model === model).inflight, 0),
    };
  });
  const entries = [];
  for (const t of topos) {
    const links = new Map();
    for (const m of models) {
      const c = constellation(topos, m);
      for (const l of c.entries.find((e) => e.node === t.node)?.links ?? []) {
        if (l.to === t.node) continue;
        const k = `${l.to}|${l.style}`;
        if (!links.has(k)) links.set(k, { to: l.to, style: l.style, models: [] });
        links.get(k).models.push(m);
      }
    }
    // Nodes whose routes all end locally are serving nodes, not entry points
    // to other nodes.
    if (!links.size) continue;
    entries.push({ node: t.node, role: t.role, public: (t.listeners ?? []).some((l) => l.name === "public"), links: [...links.values()] });
  }
  const placed = new Set([...nodes.map((n) => n.node), ...entries.map((e) => e.node)]);
  const others = topos.map((t) => t.node).filter((n) => !placed.has(n));
  return { models: summary, nodes, entries, others };
}

/**
 * Platform glyph, label, and runtime capabilities.
 *
 * @param {string} platform - "linux/amd64", "darwin/arm64", … or "" when a peer
 *   is too old to report one.
 * @param {string} [osVersion] - what the OS calls itself ("macOS 26.6.2",
 *   "Ubuntu 24.04.1 LTS"); shown in place of the bare OS name when known.
 * @returns {{key: string, label: string, title: string, arch: string, version: string}}
 */
export function platformBadge(platform, osVersion = "") {
  const [os = "", arch = ""] = String(platform || "").split("/");
  const known = {
    darwin: { key: "mac", label: "macOS", can: "Metal and MLX" },
    linux: { key: "linux", label: "Linux", can: "CUDA, ROCm, Vulkan and llm-d" },
    windows: { key: "windows", label: "Windows", can: "CUDA and Vulkan" },
  }[os.toLowerCase()];
  const version = String(osVersion || "").trim();
  if (!known) {
    return { key: "unknown", label: "", arch: "", version, title: version || "This node does not report its platform" };
  }
  const arm = arch === "arm64" ? (known.key === "mac" ? " (Apple silicon)" : " (arm64)") : "";
  return {
    key: known.key,
    label: known.label,
    arch,
    version,
    title: `${version || known.label}${arm} — runs ${known.can}`,
  };
}

/**
 * Unknown KV utilization gets no bar, since it does not imply an idle engine.
 *
 * @param {number} usage - share of the KV pool in use, or -1 when unknown
 * @returns {{known: boolean, pct: number, label: string, level: string}}
 */
export function kvBadge(usage) {
  const known = typeof usage === "number" && usage >= 0;
  if (!known) return { known: false, pct: 0, label: "", level: "" };
  const pct = Math.min(100, Math.round(usage * 100));
  // The high-usage band marks reduced headroom for reusable prefixes.
  const level = pct >= 75 ? "hot" : pct >= 40 ? "warm" : "cool";
  return { known: true, pct, label: `${pct}% KV`, level };
}

/**
 * Model settings override presets; empty fields inherit and are not changes.
 *
 * @param {object} formValues - current form values, keyed by field
 * @param {object} presetSettings - saved preset settings
 * @returns {{dirty: boolean, changed: string[]}} changed field keys, sorted
 */
export function presetDrift(formValues, presetSettings) {
  const now = parseSettingsForm(formValues || {}, PRESET_FIELDS).settings;
  const was = parseSettingsForm(settingsToForm(presetSettings || {}), PRESET_FIELDS).settings;
  const changed = Object.keys(now)
    .filter((k) => String(now[k] ?? "") !== String(was[k] ?? ""))
    .sort();
  return { dirty: changed.length > 0, changed };
}

/**
 * Group catalog rows by model key. Keep quantization and size per node:
 * the same key can identify different files, including GGUF and MLX weights.
 *
 * @param {object[]} rows - catalog rows, one per node and model
 * @returns {object[]} groups sorted by key; local node first within each
 *   group, then alphabetical
 */
export function groupByModel(rows) {
  const groups = new Map();
  for (const r of rows ?? []) {
    let g = groups.get(r.key);
    if (!g) {
      g = { key: r.key, arch: r.arch, mtp: r.mtp, caps: r.caps ?? [], params: r.params, nodes: [] };
      groups.set(r.key, g);
    }
    // Missing metadata on one node must not blank the group's identity.
    g.arch = g.arch || r.arch;
    g.params = g.params || r.params;
    g.mtp = g.mtp || r.mtp;
    if ((r.caps ?? []).length > g.caps.length) g.caps = r.caps;
    g.nodes.push(r);
  }
  const out = [...groups.values()];
  for (const g of out) {
    g.nodes.sort((a, b) => (a.self !== b.self ? (a.self ? -1 : 1) : a.node.localeCompare(b.node)));
    g.loaded = g.nodes.filter((n) => n.loaded).length;
    // Rows carry sizeBytes; summing `bytes` made every group total zero.
    g.bytes = g.nodes.reduce((n, x) => n + (x.sizeBytes ?? x.bytes ?? 0), 0);
  }
  return out.sort((a, b) => a.key.localeCompare(b.key));
}

/**
 * Factor metadata shared by all quantizations into `common`; leave
 * differences on each row. `frac` is file size divided by the largest size.
 */
export function quantRows(options = [], chosen = "") {
  const opts = options.filter(Boolean);
  if (!opts.length) return { rows: [], common: { count: 0, format: "", projector: false } };
  const max = Math.max(...opts.map((o) => o.bytes || 0), 1);
  const formats = new Set(opts.map((o) => (o.format || "gguf").toUpperCase()));
  const common = {
    count: opts.length,
    format: formats.size === 1 ? [...formats][0] : "",
    projector: opts.every((o) => !!o.projector),
  };
  const rows = opts.map((o) => ({
    quant: o.quant,
    bytes: o.bytes || 0,
    sizeLabel: formatBytes(o.bytes || 0),
    frac: Math.min((o.bytes || 0) / max, 1),
    recommended: !!o.recommended,
    fileCount: o.files?.length ?? 1,
    projector: common.projector ? "" : o.projector || "",
    active: o.quant === chosen,
  }));
  return { rows, common };
}

/**
 * Format benchmark tables and reproduction details. Failed rows retain
 * their place and error.
 */
export function benchTables(rep) {
  if (!rep) return null;
  const n1 = (v) => (v || v === 0 ? Number(v).toFixed(1) : "—");
  const n2 = (v) => (v || v === 0 ? Number(v).toFixed(2) : "—");
  const tps = (v) => `${n1(v)} tok/s`;
  const memGB = (mb) => (mb > 0 ? `${(mb / 1024).toFixed(2)} GB` : "—");
  const single = {
    head: ["Test", "TTFT (ms)", "TPOT (ms/tok)", "pp TPS", "tg TPS", "E2E Latency", "Throughput", "Peak Mem"],
    rows: (rep.single ?? []).map((r) => (r.error
      ? { error: r.error, cells: [r.test] }
      : { cells: [r.test, n1(r.ttft_ms), n2(r.tpot_ms), tps(r.pp_tps), tps(r.tg_tps), `${Number(r.e2e_s).toFixed(3)}s`, tps(r.throughput_tps), memGB(r.peak_mem_mb)] })),
  };
  const batch = (rows) => ({
    head: ["Batch Size", "tg TPS", "Speedup", "pp TPS", "pp TPS/req", "Cached", "Avg TTFT (ms)", "E2E Latency"],
    rows: (rows ?? []).map((b) => (b.error
      ? { error: b.error, cells: [`${b.n}x`] }
      : { cells: [`${b.n}x`, tps(b.tg_tps), `${n2(b.speedup)}x`, tps(b.pp_tps), tps(b.pp_tps_per_request), `${Math.round(b.cached_pct)}%`, n1(b.avg_ttft_ms), `${Number(b.e2e_s).toFixed(3)}s`], speedup: b.speedup })),
  });
  const m = rep.machine ?? {};
  const e = rep.engine ?? {};
  const c = rep.corpus;
  const meta = [
    ["GPU", m.gpu ? `${m.gpu}, ${Math.round((m.vram_mb || 0) / 1024)} GB${m.driver ? ` (driver ${m.driver})` : ""}` : "unknown"],
    ["System", [m.os, m.platform].filter(Boolean).join(" · ") || "unknown"],
    ["ModelFabric", m.modelfabric || "unknown"],
    ["Engine", e.runtime || "unknown"],
    ["Model", [e.file, e.quant, e.size_mb ? `${(e.size_mb / 1024).toFixed(1)} GB` : ""].filter(Boolean).join(" · ") || rep.model],
    ["Prompts", c ? `${c.name}-${c.version} · sha256 ${c.sha256.slice(0, 16)}` : "—"],
    ["Timings", `from the ${e.timings || "engine"} · ${rep.config?.reps ?? 1} run(s) per test, median`],
  ];
  return {
    single, same: batch(rep.batch_same_prompt), diff: batch(rep.batch_different_prompts),
    meta, command: rep.command || "", notes: rep.notes ?? [], partial: Boolean(rep.partial),
    batchPP: rep.config?.batch_pp ?? 1024, tg: rep.config?.tg ?? 128,
  };
}

// Format request counts by serving node, busiest first. Preserve "unknown"
// when the front door did not identify a node.
export function spreadText(spread) {
  return Object.entries(spread ?? {})
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([n, c]) => `${n} ${c}`).join(" · ");
}

export function clusterTables(rep) {
  if (!rep) return null;
  const n1 = (v) => (v || v === 0 ? Number(v).toFixed(1) : "—");
  const tps = (v) => `${n1(v)} tok/s`;
  const single = {
    head: ["Test", "TTFT (ms)", "pp TPS", "tg TPS", "E2E Latency", "Served by"],
    rows: (rep.single ?? []).map((r) => (r.error
      ? { error: r.error, cells: [r.test] }
      : { cells: [r.test, n1(r.ttft_ms), tps(r.pp_tps), tps(r.tg_tps), `${Number(r.e2e_s).toFixed(3)}s`, r.node || "unknown"] })),
  };
  const load = (rows) => ({
    head: ["At once", "tg TPS", "Speedup", "pp TPS", "Avg TTFT (ms)", "p95 TTFT (ms)", "Cached", "E2E Latency", "Served by"],
    rows: (rows ?? []).map((l) => (l.error && !Object.keys(l.spread ?? {}).length
      ? { error: l.error, cells: [String(l.n)] }
      : {
        cells: [String(l.n), tps(l.tg_tps), l.speedup ? `${Number(l.speedup).toFixed(2)}x` : "—", tps(l.pp_tps), n1(l.avg_ttft_ms), n1(l.p95_ttft_ms),
          `${Math.round(l.cached_pct)}%`, `${Number(l.e2e_s).toFixed(3)}s`, spreadText(l.spread) + (l.failed ? ` · ${l.failed} failed` : "")],
        speedup: l.speedup,
      })),
  });
  const holder = (h) => [h.node, h.platform, h.engines ? `${h.engines} engine${h.engines === 1 ? "" : "s"}` : "engines not reported",
    h.slots ? `${h.slots} slot${h.slots === 1 ? "" : "s"}` : ""].filter(Boolean).join(" · ");
  const c = rep.corpus;
  const meta = [
    ["Entry", rep.entry ? `${rep.entry}'s front door` : "unknown"],
    ["Routing", rep.routing || "unknown"],
    ...(rep.holders ?? []).map((h, i) => [i ? "" : "Held by", holder(h)]),
    ["Prompts", c ? `${c.name}-${c.version} · sha256 ${c.sha256.slice(0, 16)}` : "—"],
    ["Timings", `from the ${rep.timings || "engine"} · ${rep.config?.reps ?? 1} run(s) per single test, median`],
  ];
  return {
    single, load: load(rep.load), shared: load(rep.load_shared_prompt),
    meta, command: rep.command || "", notes: rep.notes ?? [], partial: Boolean(rep.partial),
    loadPP: rep.config?.load_pp ?? 1024, tg: rep.config?.tg ?? 128,
  };
}

export function heldRequests(nodes) {
  return (nodes ?? [])
    .flatMap((n) => (n.held ?? []).map((h) => ({
      at: n.name,
      waitedMs: h.waited_ms ?? 0,
      home: h.home || "",
      why: h.why || "",
    })))
    .sort((a, b) => b.waitedMs - a.waitedMs);
}

// Aggregate requests sent to each serving node and conversations moved
// from another engine; moves can require re-reading context.
export function movedIn(nodes) {
  const out = {};
  for (const n of nodes ?? []) {
    for (const r of n.routed ?? []) {
      const t = (out[r.node] ??= { calls: 0, moved: 0 });
      t.calls += r.calls ?? 0;
      t.moved += r.moved ?? 0;
    }
  }
  return out;
}

export function waitedText(ms) {
  const s = Math.round((ms ?? 0) / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

export function gib(mib) {
  if (mib === null || mib === undefined) return "—";
  const g = mib / 1024;
  return `${g < 10 ? g.toFixed(1) : Math.round(g)} GB`;
}

// ---- Developer log --------------------------------------------------------

// devlogKey identifies an entry across the nodes a page follows: each node
// numbers its own.
export function devlogKey(e) {
  return `${e.node ?? ""}#${e.seq ?? ""}`;
}

// devlogAppend adds an entry to a list held oldest first, keeping it in time
// order and no longer than cap. An entry already present is ignored: a stream
// that reconnects replays its backlog.
export function devlogAppend(list, entry, cap) {
  const key = devlogKey(entry);
  // Entries arrive nearly in order, so look back from the end.
  const t = Date.parse(entry.time) || 0;
  let i = list.length;
  for (let k = list.length - 1; k >= 0 && k >= list.length - 400; k--) {
    if (devlogKey(list[k]) === key) return list;
    if ((Date.parse(list[k].time) || 0) > t) i = k;
  }
  list.splice(i, 0, entry);
  if (list.length > cap) list.splice(0, list.length - cap);
  return list;
}

// devlogMatches reports whether an entry is shown for the text in the filter
// box. Every word must appear somewhere in what the row shows.
export function devlogMatches(entry, filter) {
  const words = String(filter ?? "").toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const hay = [entry.msg, entry.model, entry.engine, entry.trace, entry.node, entry.level, entry.source]
    .filter(Boolean).join(" ").toLowerCase();
  return words.every((w) => hay.includes(w));
}

// devlogClock is an entry's time of day to the millisecond: requests a few
// hundred milliseconds apart are the usual thing being told apart here.
export function devlogClock(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

// devlogBody is a captured body as shown: indented where it is whole JSON,
// as it came where it is not (a stream of events, or one cut at the cap).
export function devlogBody(body, truncated) {
  if (!body) return "";
  if (!truncated) {
    try { return JSON.stringify(JSON.parse(body), null, 2); } catch { /* not JSON */ }
  }
  return truncated ? `${body}\n… cut at the capture limit` : body;
}

// devlogFields is an entry's figures on one line, key=value, objects as JSON.
export function devlogFields(fields) {
  return Object.entries(fields ?? {})
    .map(([k, v]) => {
      if (v !== null && typeof v === "object") return `${k}=${JSON.stringify(v)}`;
      // The engine reports rates to fourteen places.
      return `${k}=${typeof v === "number" && !Number.isInteger(v) ? Number(v.toFixed(2)) : v}`;
    })
    .join("  ");
}

// gpuLabel names the GPUs an engine is confined to: "GPU 1", "GPUs 0,1", or
// "" when it was left to use every GPU it sees.
export function gpuLabel(gpu) {
  const ids = String(gpu ?? "").split(",").map((s) => s.trim()).filter(Boolean);
  if (!ids.length) return "";
  return ids.length === 1 ? `GPU ${ids[0]}` : `GPUs ${ids.join(",")}`;
}

// gpuName is a GPU's name without the vendor prefixes everyone skips.
export function gpuName(name) {
  return String(name ?? "").replace(/^(NVIDIA|AMD|Intel)\s+(GeForce\s+)?/i, "").replace(/\s+Generation$/i, "");
}

// gpuChoices is what a GPU selector offers for a node's GPUs, as
// /api/v1/topology lists them: one entry per card, numbered as nvidia-smi
// numbers them, which is the order of the list.
export function gpuChoices(gpus) {
  return (gpus ?? []).map((g, i) => ({
    value: String(i),
    label: `GPU ${i} · ${gpuName(g.name)} · ${Math.round((g.vram_mb ?? 0) / 1024)} GB`,
  }));
}

// freeGPU is the first GPU no running engine is confined to, "" when every
// card has one or an engine is spread across all of them. It is what "add an
// engine" offers first.
export function freeGPU(gpus, instances) {
  const used = new Set();
  for (const i of instances ?? []) {
    const g = String(i.config?.gpu ?? "");
    if (!g) return "";
    for (const id of g.split(",")) used.add(id.trim());
  }
  const open = gpuChoices(gpus).find((c) => !used.has(c.value));
  return open ? open.value : "";
}

// engineKind says what an engine computes on, in a word or two: the GPU it is
// confined to, or else its backend. It is what tells two engines on one node
// apart, and a CPU engine from a GPU one.
export function engineKind(runtime, gpu, gpuMemory) {
  // What is measured wins over what was asked for: an engine told nothing
  // lands on one card or is split across several, and only the cards say.
  const cards = (gpuMemory ?? []).filter((g) => g.mb > 0).map((g) => g.gpu);
  if (cards.length > 1) return `split across GPUs ${cards.join("+")}`;
  if (cards.length === 1) return `GPU ${cards[0]}`;
  const pinned = gpuLabel(gpu);
  if (pinned) return pinned;
  const r = String(runtime ?? "").toLowerCase();
  for (const [mark, label] of [["cuda", "CUDA"], ["rocm", "ROCm"], ["metal", "Metal"], ["mlx", "MLX"], ["vulkan", "Vulkan"], ["cpu", "CPU"]]) {
    if (r.includes(mark)) return label;
  }
  return "";
}

// engineSummary is one running engine in a few words, for a list of a
// model's engines on a node: "GPU 1 · 1 slot".
export function engineSummary(config, runtime, gpuMemory) {
  const c = config ?? {};
  const parts = [engineKind(runtime ?? c.runtime, c.gpu, gpuMemory)];
  if (c.parallel > 0) parts.push(`${c.parallel} slot${c.parallel === 1 ? "" : "s"}`);
  return parts.filter(Boolean).join(" · ");
}

// gpuMemoryText is what an engine holds on each card, for a person:
// "GPU 0: 25 GB · GPU 1: 15 GB". "" when nothing was measured.
export function gpuMemoryText(gpuMemory) {
  return (gpuMemory ?? []).filter((g) => g.mb > 0).map((g) => `GPU ${g.gpu}: ${gib(g.mb)}`).join(" · ");
}

// ---- Trying a model from the Serving page ---------------------------------

// tryRequest is the smallest chat request that shows a model is answering.
export function tryRequest(model, prompt = "Say hello in five words.") {
  // Room for a reasoning model to think and still answer: at 64 tokens one
  // spent them all thinking and the reply came back empty.
  return { model, messages: [{ role: "user", content: prompt }], max_tokens: 512 };
}

// curlFor is a curl command that sends body to base's chat completions, as a
// person would paste it into a terminal. base ends in /v1. keyVar names an
// environment variable holding the API key, "" when none is needed.
export function curlFor(base, body, keyVar = "") {
  // Single-quoted for the shell: a quote inside the JSON closes, escapes and reopens.
  const json = JSON.stringify(body, null, 2).replaceAll("'", `'\\''`);
  const lines = [`curl ${String(base).replace(/\/+$/, "")}/chat/completions \\`, `  -H 'Content-Type: application/json' \\`];
  if (keyVar) lines.push(`  -H "Authorization: Bearer $${keyVar}" \\`);
  lines.push(`  -d '${json}'`);
  return lines.join("\n");
}

// tryTargets is the two ways to reach an engine's model: through the mesh,
// as an app does, and the engine itself. front is the page's front door
// (buildFront); e is an engine row from the Serving page.
export function tryTargets(front, e) {
  const body = tryRequest(e.model);
  const mesh = {
    key: "mesh",
    title: "Through the mesh",
    note: "What an app sends. The router picks the engine, so any node serving this model may answer; the reply says which one did.",
    base: front?.url || "",
    curl: front?.url ? curlFor(front.url, body, front.requireKey ? "MFSH_KEY" : "") : "",
    setup: front?.requireKey ? "export MFSH_KEY=$(mfsh key)" : "",
    body,
  };
  const base = e.address ? `http://${e.address}/v1` : "";
  const direct = {
    key: "engine",
    title: `This engine only (${e.node})`,
    note: e.address && /^(127\.|localhost|\[?::1)/.test(e.address)
      ? `Straight to the engine, past the router. It listens on loopback, so this works only on ${e.node} itself.`
      : "Straight to the engine, past the router: no key, no placement, nothing recorded in Activity. It answers anyone who can reach its address on the tailnet.",
    base,
    curl: base ? curlFor(base, body) : "",
    setup: "",
    body,
  };
  return [mesh, direct].filter((t) => t.curl);
}

// entrypoints is the nodes that take requests from outside the tailnet: those
// whose topology lists a public listener that is switched on.
export function entrypoints(topologies) {
  return (topologies ?? [])
    .filter((t) => t && (t.listeners ?? []).some((l) => l.name === "public" && l.enabled))
    .map((t) => t.node)
    .filter(Boolean)
    .sort();
}

// publicTarget is the command for calling a model from outside, through an
// entrypoint. The public listener always wants a key, and it has to be one
// of that node's own. url is the address people outside use; ModelFabric
// does not know it (a TLS proxy in front of the node owns the name), so it
// is whatever the reader last typed, or a placeholder.
export function publicTarget(node, url, model) {
  const base = String(url ?? "").trim().replace(/\/+$/, "").replace(/\/v1$/, "");
  const body = tryRequest(model);
  return {
    key: "public",
    node,
    title: `From outside, through ${node}`,
    note: `What a caller on the internet sends. The public address always asks for a key, and it must be one of ${node}'s own: its node key, or a named token created on ${node}. A key from another node is refused.`,
    known: Boolean(base),
    base: base ? `${base}/v1` : "",
    setup: `export MFSH_KEY=...   # a token from ${node}: its dashboard, Server settings, Tokens; or \`mfsh key\` run on ${node}`,
    curl: curlFor(`${base || "https://api.example.com"}/v1`, body, "MFSH_KEY"),
    body,
  };
}

// ---- Connecting a coding agent --------------------------------------------

// servedModels is each model the mesh serves with the context a request can
// count on: the smallest among its engines, since the router may send a
// request to any of them. 0 when no engine said. vision is whether any
// engine has it loaded to read images: one is enough, because the router
// sends a request carrying an image only to engines that can read it.
export function servedModels(meshEngines) {
  const by = new Map();
  for (const e of meshEngines ?? []) {
    if (!e.model) continue;
    const have = by.get(e.model);
    const ctx = e.contextLength || 0;
    by.set(e.model, {
      context: have === undefined ? ctx : (ctx && have.context ? Math.min(ctx, have.context) : ctx || have.context),
      vision: Boolean(have?.vision || e.vision),
    });
  }
  return [...by.entries()].map(([id, m]) => ({ id, ...m })).sort((a, b) => a.id.localeCompare(b.id));
}

// connectPlaces is where an app can be, and so which address and what key it
// uses. front is the page's front door; mesh is this node's tailnet address
// and port ("" when unknown); entry is { node, url } for a node that takes
// requests from outside, or null.
export function connectPlaces(front, mesh, entry) {
  const places = [{
    key: "local",
    label: "On this machine",
    base: front?.url || "http://127.0.0.1:1234/v1",
    keyVar: front?.requireKey ? "MFSH_KEY" : "",
    keyHelp: front?.requireKey ? "This node is set to ask for a key on 127.0.0.1. `mfsh key` prints it; a named token is better for an app." : "",
    note: front?.requireKey ? "" : "No key is needed on the machine itself.",
  }];
  if (mesh) {
    places.push({
      key: "tailnet",
      label: "On another of your machines",
      base: `http://${mesh}/v1`,
      // require_api_key covers the loopback front door only. The mesh
      // listener never checks a key: Tailscale has already said who is calling.
      keyVar: "",
      keyHelp: "",
      note: "Any machine on your tailnet can use this address, and being on the tailnet is the only credential" + (front?.requireKey ? ", even though this node asks for a key on 127.0.0.1" : "") + ". A machine running ModelFabric itself should use its own 127.0.0.1 instead.",
    });
  }
  if (entry?.node) {
    const base = String(entry.url ?? "").trim().replace(/\/+$/, "").replace(/\/v1$/, "");
    places.push({
      key: "public",
      label: `From outside, through ${entry.node}`,
      base: `${base || "https://api.example.com"}/v1`,
      known: Boolean(base),
      node: entry.node,
      keyVar: "MFSH_KEY",
      keyHelp: `The public address always asks for a key, and it must be one of ${entry.node}'s own: create a named token on ${entry.node} (its dashboard, Server settings, Tokens, or \`mfsh key create opencode\` there).`,
      note: "",
    });
  }
  return places;
}

// opencodeConfig is an opencode.json that adds ModelFabric as a provider.
// The key, when one is needed, is read from the environment by opencode
// ("{env:NAME}") and is never written into the file.
export function opencodeConfig(base, models, keyVar = "") {
  const options = { baseURL: base };
  if (keyVar) options.apiKey = `{env:${keyVar}}`;
  const entries = {};
  for (const m of models ?? []) {
    entries[m.id] = { name: m.id };
    // Without a limit opencode assumes a small window and compacts early.
    if (m.context > 0) entries[m.id].limit = { context: m.context, output: Math.min(16384, Math.floor(m.context / 4)) };
    // opencode assumes a model it does not know reads text only. Unless told
    // otherwise it removes an attached image before sending and the model
    // answers that it "doesn't support image input", with vision loaded and
    // working. Reproduced with opencode 1.18.35; these two fields fix it.
    if (m.vision) {
      entries[m.id].attachment = true;
      entries[m.id].modalities = { input: ["text", "image"], output: ["text"] };
    }
  }
  return JSON.stringify({
    $schema: "https://opencode.ai/config.json",
    provider: {
      modelfabric: { npm: "@ai-sdk/openai-compatible", name: "ModelFabric", options, models: entries },
    },
  }, null, 2);
}

// A coding agent sends its tools, its instructions and the files it has read
// with every request. Under this it runs out of room within a few steps.
export const AGENT_MIN_CONTEXT = 64000;

// aliveNodes is the names of the nodes that are up, this node first, from
// the mesh view. It is what anything that asks every node something should
// iterate. The My Models cache (mm.nodes) is not: it only learns a peer
// while a page that shows peers' models is open, so the Requests tab's
// "Whole mesh" and the developer log's node list came up with this node
// alone on a fresh page.
export function aliveNodes(view) {
  const nodes = (view?.nodes ?? []).filter((n) => n.alive && n.name);
  return [...nodes.filter((n) => n.isSelf), ...nodes.filter((n) => !n.isSelf).sort((a, b) => a.name.localeCompare(b.name))].map((n) => n.name);
}
