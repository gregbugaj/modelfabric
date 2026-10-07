// Pure transform from the /z/mesh payload to what the page renders.
// Kept free of DOM access so it can be tested directly (see ui-model.test.mjs).

/**
 * Shape the /api/v1/models payload into rows for the Local view.
 *
 * `available` is null when the node has no supervisor (route-only), which is
 * different from a node that supervises but has found no models.
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
      // A model with work in flight must not offer another action, or a second
      // click starts a duplicate load of something very large.
      busy: Boolean(op),
      busyKind: op ? op.kind : "",
      busyMessage: op ? op.message || "" : "",
    };
  });

  return {
    supervised: true,
    models,
    loadedCount: models.reduce((n, m) => n + m.instances.length, 0),
  };
}

/** Human-readable byte size. */
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

/**
 * @param {object} mesh - the /z/mesh response
 * @returns {object} view model
 */
// Which address an engine listens on. Bound to loopback it is private to its
// own machine; bound to the node's tailnet address any machine on the tailnet
// can reach it.
//
// Either way the node serves through its own front door, so the model looks
// fine from outside — the difference only shows when something dials an engine
// directly instead, which is the failure that is hard to see.
const LOOPBACK = new Set(["127.0.0.1", "::1", "localhost", ""]);

function loopbackOnly(instances) {
  return engineScope(instances) === "loopback";
}

// Where a node's engines can be reached from: "tailnet", "loopback", or ""
// when it runs none, which is not a fault. Reported for every node rather than
// only when it is wrong.
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
      // Its tailnet address, not "local": this is the address peers reach it
      // on, and on a page about a tailnet that is the useful fact.
      addr: self.addr || "local",
      platform: self.platform || "",
      osVersion: self.os_version || "",
      isSelf: true,
      alive: true,
      inflight: self.inflight ?? 0,
      // What this node's front door is holding. Engine counts miss requests
      // queued in Envoy, and under llm-d miss the ones being served too, so
      // this is the figure that is always right.
      accepted: self.accepted ?? 0,
      // Requests this node's router is holding for a slot: waiting, but on
      // no engine yet, so no engine row can show them.
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

  // Every engine in the mesh, not only this node's: the Serving page answers
  // "what is running", and in a mesh that question spans the nodes. Peers
  // publish their instances; this node's come from its own state, with the
  // health its router tracks merged in by instance id.
  const health = new Map((self.engines ?? []).map((e) => [e.name, e]));
  const fromInstances = (node, list, isSelf, host) => (list ?? []).map((i) => {
    const live = isSelf ? health.get(i.id) : null;
    return {
      node,
      isSelf,
      // The engine's host-RAM prompt cache: its cap, and how many times it
      // has dropped a conversation to make room. With one more conversation
      // than slots an engine swaps them through this cache, so its size
      // decides the cache hit rate more than the slot count does. null where
      // the engine did not say: an older peer, or an engine without one.
      cacheRamMib: i.cache_ram_mib > 0 ? i.cache_ram_mib : null,
      cacheDropped: typeof i.cache_dropped === "number" ? i.cache_dropped : (i.cache_ram_mib > 0 ? 0 : null),
      // What the engine process holds in RAM, and what its machine has.
      memoryMb: i.memory_mb > 0 ? i.memory_mb : null,
      hostMemTotalMb: host?.mem_total_mb > 0 ? host.mem_total_mb : null,
      hostMemAvailableMb: host?.mem_available_mb > 0 ? host.mem_available_mb : null,
      id: i.id,
      model: i.model || "",
      runtime: i.runtime || "",
      engine: i.engine || "",
      address: i.address && i.port ? `${i.address}:${i.port}` : "",
      slots: i.slots ?? 0,
      // Measured prompt tokens per second, zero until the engine has served
      // enough to measure. On a mixed fleet this is the number that decides
      // whether an even share of requests is an even share of work: 228 tok/s
      // on a Mac against 1989 on a 5090 is the same request costing 9x.
      prefillTokS: i.prefill_tok_s ?? 0,
      // Whether the rate is settled enough to route by. Below it the number
      // is still shown — a rough figure beats a dash next to a busy engine —
      // but it is marked, because llm-d is not scheduling on it.
      prefillTrusted: Boolean(i.prefill_trusted),
      // The writing half of a turn, and how much of the drafting the model
      // kept. Prefill alone could not show what speculative decoding changes.
      decodeTokS: i.decode_tok_s ?? 0,
      specAccepted: typeof i.spec_accepted === "number" ? i.spec_accepted : -1,
      // Lifetime totals: what this engine was actually given, which is what a
      // placement decision produces. A fast engine handed nothing and a slow
      // one buried both look fine on rates alone.
      promptTokens: i.prompt_tokens ?? 0,
      cachedTokens: i.cached_tokens ?? 0,
      outputTokens: i.output_tokens ?? 0,
      // The node's own rolling average, -1 when it has too few samples and
      // absent from a peer too old to publish it. Preferred over the one the
      // dashboard computes, which only accumulates while the page is open.
      loadAvg: typeof i.load_avg === "number" ? i.load_avg : -1,
      // Whether this engine was launched to take images. Absent from a peer
      // too old to report it, which reads as "not vision" — the same default
      // its engines had before the field existed.
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

  // Whichever node is scheduling for the mesh. It is not always this one: an
  // entrypoint with no GPUs can run llm-d while every engine is elsewhere,
  // and a view that only reads the local node reported "not running" through
  // a whole benchmark that ran through it.
  const schedulerNode = nodes.find((n) => n.alive && n.scheduler) ?? null;

  const online = nodes.filter((n) => n.alive);
  // Only live nodes contribute load; a dead peer's last reported figure is
  // stale and would otherwise inflate the total indefinitely.
  const inflight = online.reduce((sum, n) => sum + n.inflight, 0);
  // Mesh-wide load as the front doors saw it. Engine counts are per engine and
  // blind under llm-d; a front door counts what it took, so the two differ by
  // exactly what is queued — which is worth seeing.
  const accepted = online.reduce((sum, n) => sum + (n.accepted ?? 0), 0);

  return {
    self: self.node || "",
    // LM Link's preferred device: models held there are used first. Set but
    // absent from the mesh means offline, and requests fall back.
    preferred,
    preferredOnline: preferred !== "" && nodes.some((n) => n.preferred && n.alive),
    // Named so the workload rail can say where, and act there.
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
    models: models.map((m) => ({
      id: m.id,
      nodes: m.nodes ?? [],
      replicas: (m.nodes ?? []).length,
    })),
    engines,
    meshEngines,
    capacity: {
      ...meshCapacity(meshEngines),
      // Held by a router for a slot, on whichever nodes took the requests.
      held: online.reduce((sum, n) => sum + (n.queued ?? 0), 0),
    },
    heldRequests: heldRequests(online),
    movedIn: movedIn(online),
  };
}

// slotUse splits an engine's requests into those running and those waiting
// for a slot, and says how many slots are free.
//
// The table used to show one figure, "2 / 1", for a one-slot engine with a
// request running and another queued behind it, under a tooltip that read "2
// of 1 slots busy". Through a whole benchmark nothing on the page said that
// two agents were waiting on the slowest machine while a GPU had slots open.
//
// An engine that does not report its slots has nothing to be full against, so
// waiting and free are null there: unknown, not zero.
export function slotUse(inflight, slots) {
  const n = Math.max(inflight || 0, 0);
  if (!slots) return { running: n, waiting: null, free: null };
  return { running: Math.min(n, slots), waiting: Math.max(n - slots, 0), free: Math.max(slots - n, 0) };
}

// meshCapacity adds those up across the healthy engines. waitingBesideFree is
// the case worth a second look: something is queued on one engine while
// another has a slot open.
export function meshCapacity(engines) {
  const known = (engines ?? []).filter((e) => e.healthy && e.slots);
  const sum = (k) => known.reduce((n, e) => n + (e[k] || 0), 0);
  const c = { slots: sum("slots"), running: sum("running"), waiting: sum("waiting"), free: sum("free") };
  return { ...c, waitingBesideFree: c.waiting > 0 && c.free > 0 };
}

// Self first, then live nodes, then alphabetical — the order you actually scan.
function byNode(a, b) {
  if (a.isSelf !== b.isSelf) return a.isSelf ? -1 : 1;
  if (a.alive !== b.alive) return a.alive ? -1 : 1;
  return a.name.localeCompare(b.name);
}

/** Compact relative time, e.g. "4s ago". Returns "—" for missing/zero times. */
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
 * Shape /api/v1/tokens for the token manager: the node key first, as a row
 * that is rotated rather than revoked (a node has exactly one), then the
 * named tokens, newest first.
 *
 * A token is shown only by its last four characters — the secret is not kept,
 * so there is nothing more to show. "never" is only said of a token the node
 * reported no use for; the node records use to the minute, so a token used
 * seconds ago can still read "just now" rather than its exact second.
 */
export function buildTokens(api, now = Date.now()) {
  const rows = [];
  if (api?.node_key) {
    rows.push({ id: "", name: "Node key", masked: `sk-mfsh-…${api.node_key}`, created: "", lastUsed: "", builtin: true, rotatable: Boolean(api.rotatable) });
  }
  const tokens = [...(api?.tokens ?? [])].sort((a, b) => String(b.created).localeCompare(String(a.created)));
  for (const t of tokens) {
    rows.push({
      id: t.id,
      name: t.name,
      masked: `sk-mfsh-…${t.hint}`,
      created: relativeTime(t.created, now),
      lastUsed: t.last_used ? relativeTime(t.last_used, now) : "never",
      builtin: false,
    });
  }
  return rows;
}

/** The Server settings the dialog edits, in the order it shows them. */
export const SERVER_SETTINGS = [
  "listen", "require_api_key", "public_listen", "cors_origins",
  "mcp_allow_ephemeral", "mcp_allow_configured",
  "jit_load", "jit_ttl", "jit_auto_evict", "mesh_admin", "engine_bind", "web_ui",
];

// Absent and empty are one setting, and so are absent and false: the config
// omits both, and the form cannot tell them apart.
const sameSetting = (a, b) => {
  if (Array.isArray(a) || Array.isArray(b)) return JSON.stringify(a ?? []) === JSON.stringify(b ?? []);
  if (typeof a === "boolean" || typeof b === "boolean") return Boolean(a) === Boolean(b);
  return (a ?? "") === (b ?? "");
};

/**
 * The settings the form changed from what is saved: only these are sent, so
 * a save never rewrites a key the person did not touch.
 */
export function settingsChanges(saved, form, keys = SERVER_SETTINGS) {
  const out = {};
  for (const k of keys) {
    if (k in form && !sameSetting(saved?.[k], form[k])) out[k] = form[k];
  }
  return out;
}

/**
 * Settings saved but not yet in effect: the node reads them at startup, so
 * until a restart it runs the value in `running`. Live ones never wait.
 */
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

/**
 * The form the Server settings flyout shows, from the saved settings. The
 * flyout speaks in switches and ports, as LM Studio's does; the config speaks
 * in addresses and durations. These two functions are the whole translation.
 */
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
 * The settings the form describes. Anything the form does not express — the
 * listen host, a public listener's host — is kept from what is saved, so a
 * port change never moves a listener to another interface.
 */
export function formToServer(form, saved) {
  const s = saved ?? {};
  const f = form ?? {};
  const listenHost = hostOf(s.listen) || "127.0.0.1";
  // The public front door is published by Tailscale Funnel or a TLS proxy
  // on this machine, so it listens on loopback (as the entrypoint docs set
  // it up) unless a host was chosen by hand in config.json. Tailnet devices
  // need none of this: they reach the mesh listener already.
  const frontHost = hostOf(s.public_listen) || "127.0.0.1";
  const origins = String(f.cors_origins ?? "").split(/[\s,]+/).map((o) => o.trim()).filter(Boolean);
  return {
    listen: f.port ? `${listenHost}:${f.port}` : (s.listen ?? ""),
    public_listen: f.front_on ? (f.front_port ? `${frontHost}:${f.front_port}` : (s.public_listen ?? "")) : "",
    // On keeps whatever on was saved as: "same-owner" and "" mean the same.
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

/** The router's settings the Routing page edits, saved like Server settings. */
export const ROUTER_SETTINGS = [
  "rate_weighted_routing", "prefix_affinity", "local_bias", "max_output_tokens", "cache_disk_mib", "cache_disk_dir",
];

/**
 * The Routing page's router form, from the saved settings, and back. The
 * disk cache is a switch and a size in GB on the page and one number of MiB
 * in the config, where 0 is off; a size typed with the switch off is not
 * saved, so turning it on again offers the last size.
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
 * Who routes each model: llm-d for the one it schedules, ModelFabric's
 * router for the rest. From the mesh view and llm-d's state, as the
 * Routing page's table shows them.
 */
export function routedBy(models, llmdModel) {
  return (models ?? []).map((m) => ({
    model: m.id,
    nodes: m.nodes ?? [],
    by: llmdModel && m.id === llmdModel ? "llmd" : "router",
  }));
}

/** A path with the home directory written as ~, for display. */
export function displayPath(p) {
  return String(p ?? "").replace(/^\/(?:home|Users)\/[^/]+(?=\/|$)/, "~");
}

/**
 * What a change would open beyond this machine, each said in a sentence the
 * page shows before saving. Widening a listener is never done quietly.
 */
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
 * Shape /api/v1/front for the Overview's front-door panel: the address apps
 * point at, what it asks of them, and whether anything is published beyond
 * loopback.
 *
 * `url` is empty when the endpoint could not be read, which the caller fills
 * with the page's own origin — the dashboard is served by the front door, so
 * that address is right even when nothing answered. The key and the public
 * listener have no such fallback: guessing "no key needed" is a lie on a node
 * that requires one, so they are only ever what the node reported.
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

/**
 * Shape /api/v1/runtimes (+ the optional /api/v1/runtimes/available answer and
 * the operations journal) into the Runtime view, as LM Studio's runtime page
 * shows it: the hardware, every engine with its fit, which one new loads use,
 * and what can be installed or updated.
 */
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
  // The one in use for new loads first, then newest engine first.
  runtimes.sort((a, b) =>
    (b.isDefault - a.isDefault) || (b.buildNumber - a.buildNumber) || a.name.localeCompare(b.name));

  const installing = operations
    .filter((op) => op && op.kind === "runtime-get" && op.state === "running")
    .map((op) => ({ id: op.id, name: op.model, message: op.message || "starting", fraction: op.fraction ?? 0 }));

  // Install choices: the recommendation, then each other backend that has a
  // build for this machine, each listed once.
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

/**
 * Shape /api/v1/llmd and the profile API into the Routing page: llm-d's state
 * and the scheduling profiles — with llm-d's well-lit paths that llama.cpp
 * cannot run shown, and why.
 */
export function buildRouting(llmd, profilesApi, operations = [], models = []) {
  // Null is a node that does not schedule with llm-d at all: the endpoint
  // answers 501 there, and the page says so instead of offering controls
  // that cannot act.
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
    // Models llm-d could schedule: every one with an engine somewhere in the
    // mesh, which is what the caller passes.
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
 * Every load and inference setting, grouped as the settings form shows them.
 * `lms` is LM Studio's name for the same setting.
 */
export const SETTINGS_SCHEMA = [
  { group: "Context & GPU", fields: [
    { key: "context_length", label: "Context length", type: "int", help: "tokens per request" },
    { key: "parallel", label: "Parallel slots", type: "int" },
    { key: "gpu_layers", label: "GPU layers", type: "int", help: "or use GPU offload ratio" },
    { key: "offload_ratio", label: "GPU offload ratio", type: "float", help: "0–1, LM Studio's GPU offload" },
    { key: "flash_attention", label: "Flash attention", type: "bool" },
    // Only meaningful on a model that has a projector; off loads it text-only,
    // which frees that memory and lets speculative decoding run.
    { key: "vision", label: "Serve images", type: "bool",
      help: "off loads a multimodal model without its projector: no images, but it can speculate — measured 134 tok/s against 66 on the same request" },
  ] },
  // Only the allowance lives here. llama.cpp's --reasoning on|off does the
  // same job as the Inference tab's Thinking, and showing both put two
  // controls named "Thinking" on two tabs of one model — one per concept, and
  // the model's own template variable is the one that belongs to the model.
  // The allowance stays on Load because it is fixed when the engine starts:
  // changing it needs a reload, which is what this tab means.
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
    { key: "cache_ram", label: "Prompt cache in RAM (MiB)", type: "int" },
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
    // The levels are the model template's, not llama.cpp's, so this is typed
    // rather than picked: this model takes xhigh, medium and low and raises a
    // Jinja exception on anything else.
    // Rendered as pills when the model's template names its levels, and as a
    // typed field when it does not — the levels are the template's, and
    // llama.cpp's generic list is wrong for most of them.
    { key: "reasoning_effort", label: "Thinking effort", type: "text",
      help: "the levels your model's template accepts" },
  ] },
];

/** The fields a preset may hold (inference only, as in LM Studio). */
export const PRESET_FIELDS = SETTINGS_SCHEMA.filter((g) => g.inference).flatMap((g) => g.fields)
  .concat([{ key: "seed", label: "Seed", type: "int" }]);

/**
 * Turn form values (strings; "" = inherit) into a settings object, or report
 * the fields that do not parse. Pure, so it is tested without a DOM.
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
      // Number.isNaN alone let Infinity, -Infinity and overflow like 1e309
      // through, and those do not survive JSON or mean anything to an engine.
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

/** The reverse: saved settings as form strings. */
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

/* ---------- My Models (every node) ---------- */

const CAPABILITY_ORDER = ["vision", "tool_use", "reasoning"];

/**
 * Merge each node's /api/v1/models into one catalog, LM Studio's "My Models"
 * across the mesh. `nodes` is [{ node, self, api, operations, error }]; a node
 * whose list could not be read (not the same owner, offline) is reported in
 * `unreachable` rather than silently missing.
 */
export function buildCatalog(nodes = []) {
  const rows = [];
  const unreachable = [];
  const perNode = [];
  // The thinking levels belong to the model file, not to the node holding it,
  // so one node that knows them answers for every row of that model. The fleet
  // runs different builds on purpose, and a node too old to report them was
  // showing a typed box for the same model that offered pills on its
  // neighbour — the same file, two different controls.
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
        // The thinking levels this model's template accepts, so the settings
        // form can offer them instead of asking for a typed level.
        reasoningEfforts: m.reasoning_efforts ?? effortsByKey.get(m.key) ?? [],
        instances: instances.map((i) => ({ id: i.id, config: i.config ?? {}, port: i.port ?? 0, origin: i.origin || "" })),
        loaded: instances.length > 0,
        busy: Boolean(op),
        busyKind: op ? op.kind : "",
      });
    }
    perNode.push({ node: n.node, self: Boolean(n.self), models: n.api.models.length, bytes });
  }
  rows.sort((a, b) => (a.self !== b.self ? (a.self ? -1 : 1) : a.node.localeCompare(b.node) || a.key.localeCompare(b.key)));
  return { rows, unreachable, perNode };
}

/** Rows for one node ("" = all) whose text matches the filter. */
export function filterCatalog(rows, node = "", text = "") {
  const q = text.trim().toLowerCase();
  return rows.filter((r) => (!node || r.node === node) &&
    (!q || [r.key, r.arch, r.quant, r.params, r.publisher, r.node].some((v) => v.toLowerCase().includes(q))));
}

/**
 * What an empty inference setting falls back to: the publisher's model.yaml
 * value, shown as the field's placeholder.
 */
export function inheritedValue(spec, key) {
  const s = spec?.sampling ?? {};
  if (key in s && s[key] !== null && s[key] !== undefined) return String(s[key]);
  if (key === "enable_thinking" && spec?.template_vars && "enable_thinking" in spec.template_vars) {
    return spec.template_vars.enable_thinking ? "on" : "off";
  }
  // The publisher's own effort, which is what a request gets when nothing
  // overrides it — "xhigh" on this 27B, and the reason a one-word answer costs
  // hundreds of thinking tokens.
  if (key === "reasoning_effort" && spec?.template_vars && "reasoning_effort" in spec.template_vars) {
    return String(spec.template_vars.reasoning_effort);
  }
  return "";
}

/* ---------- Discover ---------- */

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
 * A model card's README as HTML. Model cards are written by strangers, so
 * this is deliberately small and safe: everything is HTML-escaped first, then
 * only headings, paragraphs, lists, code, emphasis, rules and http(s) links
 * are produced. Raw HTML in the card shows as text; images are dropped.
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

/** Compact counts: 872724 -> "873K". */
export function compactCount(n = 0) {
  if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (n >= 1e3) return Math.round(n / 1e3) + "K";
  return String(n);
}

/**
 * One card per node in Discover's download section: whether it can take this
 * download, and what it already has or is doing. `storage` is each node's
 * /api/v1/storage; `rows` is My Models' catalog; `ops` maps node -> its
 * operations.
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
      // Free space is already on the tile's spec line; repeating it here
      // printed it twice on every node that could take the download. What
      // belongs here is what the node already holds.
      t.note = other ? `has ${other.quant}; adds ${option?.quant}` : "";
      if (option && t.vramBytes && option.bytes > t.vramBytes) t.warn = `larger than its ${formatBytes(t.vramBytes)} GPU; loads partly on CPU`;
    }
    return t;
  });
}

/* ---------- Mesh: the architecture, drawn from each node's topology ---------- */

/**
 * The request paths from one node's front door, as edges between the boxes the
 * Mesh page draws. `topos` is every readable node's /api/v1/topology.
 *
 * There is no route table to read: ModelFabric's own router decides per request, so
 * the paths are derived from where the engines are. An engine is dialled
 * directly when it listens on its node's tailnet address, and through that
 * node's ModelFabric — its mesh listener, and on from there — when it listens on
 * loopback, because nothing else can reach it. The one model llm-d schedules
 * goes through the llmd listener (Envoy) to the endpoints its EPP was given.
 *
 * The source is the front listener, because the front door *is* ModelFabric's router:
 * a request that reached it has already arrived.
 */
export function buildEdges(topos = [], from = "") {
  const edges = [];
  const src = topos.find((t) => t.node === from);
  if (!src) return edges;
  const front = { node: from, kind: "listener", id: "front" };
  // The preferred node holds the model that gets used first, so the hop to it
  // is marked; the rest are the fallbacks the router would take after it.
  const preferred = src.preferred || "";
  const engineAt = new Map(); // "addr:port" as seen from `from` -> {node, id}
  for (const t of topos) {
    for (const e of t.engines ?? []) {
      // A peer's loopback engine has no address this node could dial, and its
      // "127.0.0.1:18000" would otherwise collide with this node's own.
      if (LOOPBACK.has(e.addr ?? "") && t.node !== from) continue;
      engineAt.set(`${e.addr}:${e.port}`, { node: t.node, id: e.id });
    }
  }
  // llm-d owns its model outright: while it schedules one, every request for
  // that model goes through Envoy and the router places none of them.
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
    // One hop per model, not per engine: several loopback engines on a peer are
    // all reached through the same ModelFabric.
    const forwarded = new Set();
    for (const e of t.engines ?? []) {
      if (e.model === llmdModel) continue;
      // Only this node's own engines are dialled straight. Another node's are
      // always reached through that node's tailnet listener, wherever they
      // are bound: the router's candidate for a peer is its mesh listener
      // (mesh.Candidates), never its engine. A tailnet-bound engine was drawn
      // as "direct to engine", a path the router does not take.
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
  // Every node this one can reach, model or not. With nothing loaded the
  // page drew no line between nodes at all, which read as "not connected"
  // when the mesh was whole. A peer a request already goes to needs no
  // second line.
  const reached = new Set(edges.filter((e) => e.to.kind === "listener" && e.to.id === "mesh").map((e) => e.to.node));
  for (const t of topos) {
    if (t.node === from || reached.has(t.node)) continue;
    if (!t.listeners?.some((l) => l.name === "mesh")) continue;
    edges.push({ from: front, to: { node: t.node, kind: "listener", id: "mesh" }, style: "peer" });
  }
  return edges;
}

/**
 * The Constellation view: one model at the centre, the nodes serving it
 * around it, and the entry points that route to it outside those. Built from
 * the same topologies as the architecture view.
 */
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
    // A hop through another node's ModelFabric shows as a link to that node.
    for (const e of buildEdges(topos, t.node)) {
      if (e.model === model && e.to.kind === "listener" && e.to.id === "mesh" && servingNodes.has(e.to.node)) {
        links.push({ to: e.to.node, style: e.style });
      }
    }
    // A node that reaches none of the engines is not an entry point for this
    // model — it is just a node that cannot serve it.
    if (!links.length) continue;
    const seen = new Set();
    entries.push({
      node: t.node, role: t.role,
      // What this node's front door does with the model: hands it to llm-d, or
      // places it itself.
      via: t.llmd?.model === model ? "llmd" : "direct",
      public: (t.listeners ?? []).some((l) => l.name === "public"),
      links: links.filter((l) => !seen.has(l.to + l.style) && seen.add(l.to + l.style)),
    });
  }
  const others = topos.filter((t) => !servingNodes.has(t.node) && !entries.find((e) => e.node === t.node)).map((t) => t.node);
  return { model, models, serving, entries, others };
}

/**
 * Every model at once: the models, each node with the models it serves, and
 * the entry points with the nodes they route to (any model).
 */
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
    // Only a node that reaches another node's engines: one whose every path
    // ends on itself is drawn as a serving node, not as an entry point into
    // the rest of the mesh.
    if (!links.size) continue;
    entries.push({ node: t.node, role: t.role, public: (t.listeners ?? []).some((l) => l.name === "public"), links: [...links.values()] });
  }
  const placed = new Set([...nodes.map((n) => n.node), ...entries.map((e) => e.node)]);
  const others = topos.map((t) => t.node).filter((n) => !placed.has(n));
  return { models: summary, nodes, entries, others };
}

/**
 * What a node's platform string means for the UI: a glyph key, a short label
 * and what only that platform can run. The fleet is mixed on purpose, and this
 * is the one place that decides how it reads.
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
    // An unknown platform still shows a version if the node reported one.
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
 * How an engine's KV-cache utilization should read. Unknown is not zero: an
 * engine nobody could ask is not an idle one, so it gets no bar at all.
 *
 * @param {number} usage - share of the KV pool in use, or -1 when unknown
 * @returns {{known: boolean, pct: number, label: string, level: string}}
 */
export function kvBadge(usage) {
  const known = typeof usage === "number" && usage >= 0;
  if (!known) return { known: false, pct: 0, label: "", level: "" };
  const pct = Math.min(100, Math.round(usage * 100));
  // The bands are about headroom, not beauty: past three quarters a llama.cpp
  // engine starts evicting the prefixes routing worked to reuse.
  const level = pct >= 75 ? "hot" : pct >= 40 ? "warm" : "cool";
  return { known: true, pct, label: `${pct}% KV`, level };
}

/**
 * Which settings this model overrides on top of the preset it uses.
 *
 * A model's saved settings are overrides, not a copy of the preset: an empty
 * field inherits the preset's value, so it is not a change. A field that is
 * set and differs is — that is what LM Studio marks unsaved, and it is the
 * only way to tell "uses focused" from "uses focused, except hotter".
 *
 * @param {object} formValues - the form's current values, keyed by field
 * @param {object} presetSettings - the preset's saved settings
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
 * The same rows seen model-first: one entry per model, with the nodes that
 * hold it.
 *
 * The node-first table answers "what is on this machine"; a mesh also needs
 * the other direction — "where does this model live, and where is it loaded"
 * — which no single-machine tool has to answer.
 *
 * Identity is the model key. Quant and size stay on the node rows, because two
 * nodes can hold different files under one key: a GGUF on one and MLX weights
 * on another.
 *
 * @param {object[]} rows - catalog rows, one per node and model
 * @returns {object[]} groups, sorted by key; nodes within a group put this
 *   node first, then alphabetically
 */
export function groupByModel(rows) {
  const groups = new Map();
  for (const r of rows ?? []) {
    let g = groups.get(r.key);
    if (!g) {
      g = { key: r.key, arch: r.arch, mtp: r.mtp, caps: r.caps ?? [], params: r.params, nodes: [] };
      groups.set(r.key, g);
    }
    // Keep whichever copy states the most: a node that could not read a
    // model's metadata should not blank the group's identity.
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
 * The quantization list for Discover's download step.
 *
 * A repo's quantizations differ in one thing that matters — how many bytes of
 * weights you are about to pull onto a machine — and are otherwise identical:
 * same format, usually the same vision projector. Repeating "GGUF" and an eye
 * on every row spends the loudest ink in the list on the one fact that never
 * varies, so anything constant across the options is lifted out to `common`
 * and stated once; a badge stays on a row only when the rows disagree.
 *
 * `frac` is each file's size against the largest, which is what makes the
 * list readable at a glance: the choice is quality against disk.
 */
export function quantRows(options = [], chosen = "") {
  const opts = options.filter(Boolean);
  if (!opts.length) return { rows: [], common: { count: 0, format: "", projector: false } };
  const max = Math.max(...opts.map((o) => o.bytes || 0), 1);
  const formats = new Set(opts.map((o) => (o.format || "gguf").toUpperCase()));
  const common = {
    count: opts.length,
    format: formats.size === 1 ? [...formats][0] : "",
    // Only "every option has one" is worth hoisting: if some do and some do
    // not, that difference is the reason to show the badge per row.
    projector: opts.every((o) => !!o.projector),
  };
  const rows = opts.map((o) => ({
    quant: o.quant,
    bytes: o.bytes || 0,
    sizeLabel: formatBytes(o.bytes || 0),
    frac: Math.min((o.bytes || 0) / max, 1),
    recommended: !!o.recommended,
    fileCount: o.files?.length ?? 1,
    // Shown per row only when it distinguishes this row from another.
    projector: common.projector ? "" : o.projector || "",
    active: o.quant === chosen,
  }));
  return { rows, common };
}

/**
 * A benchmark report as the Benchmark page shows it: three tables in the
 * standard layout, and what a reader needs to repeat the run. Numbers are
 * formatted here, once, so the page and the tests agree on what "34.0 tok/s"
 * looks like. A failed row keeps its place and says why.
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

// spreadText is where a load level's requests went, busiest node first:
// "minion 6 · helion 2". "unknown" is a request whose node the front door did
// not name; it is shown as such rather than credited to anyone.
export function spreadText(spread) {
  return Object.entries(spread ?? {})
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([n, c]) => `${n} ${c}`).join(" · ");
}

// clusterTables lays out a cluster run (internal/bench.ClusterReport): one
// request at a time and where it landed, then the load sweep with each
// level's spread across the nodes.
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

// heldRequests lists every request a router in the mesh is holding for a
// slot, longest wait first, with the node holding it.
//
// The header count said "2 held at the router" and nothing else. In the runs
// of 2026-10-06 finding out that one of them had waited six minutes, and for
// what, meant reading a node's traffic log over SSH.
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

// movedIn adds up, per serving node, what every router in the mesh has sent
// it and how many of those requests were a conversation arriving from another
// engine. A move is where re-reading comes from: in one run 2.6M of 3.6M
// tokens read again were read on the call straight after one.
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

// waitedText is a wait in the unit a person would say it in.
export function waitedText(ms) {
  const s = Math.round((ms ?? 0) / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

// gib is a size in MiB said in GiB, to one decimal below ten.
export function gib(mib) {
  if (mib === null || mib === undefined) return "—";
  const g = mib / 1024;
  return `${g < 10 ? g.toFixed(1) : Math.round(g)} GB`;
}
