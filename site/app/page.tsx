import Link from "next/link";
import { MeshHero } from "../components/landing/mesh-hero";
import { Gallery } from "../components/landing/gallery";
import { FleetBench } from "../components/landing/fleet-bench";
import icons from "../data/brand-icons.json";

/*
 * The landing page makes a claim, then backs it with numbers that were
 * actually measured.
 *
 * An earlier version led with `localhost:1234` set large. It read well and
 * said nothing: a port number is not a description, and a first-time visitor
 * left without learning what ModelFabric is. The address is still here — it is the
 * whole idea — but as evidence under the claim rather than in place of it.
 */

// From the router comparison of 2026-10-07 (docs/benchmark/router-comparison,
// numbers in data/routing.json): one recorded coding-agent workload replayed
// through ModelFabric's router, llm-d and LiteLLM, three runs each. The caption
// says what was compared, because a number without its question is decoration.
// Update these with that page when its data is regenerated.
const PROOF = [
  { big: "20%", small: "faster than LiteLLM on the same agent workload" },
  { big: "4%", small: "faster than llm-d, from one binary with no proxy to run" },
  { big: "32%", small: "fewer prompt tokens computed again than LiteLLM" },
  { big: "4×", small: "fewer conversations moved off the engine that holds them" },
];

const TASKS = [
  {
    href: "/docs/getting-started",
    title: "Install and run a node",
    body: "One command. A static binary with no runtime dependencies, and the dashboard compiled in.",
  },
  {
    href: "/docs/getting-started/first-model",
    title: "Load your first model",
    body: "Download by name, load it, call it from an OpenAI client, and see which machine answered.",
  },
  {
    href: "/docs/getting-started/add-a-node",
    title: "Add a second machine",
    body: "No registration step. Both nodes find each other, and nothing about your apps changes.",
  },
  {
    href: "/docs/operations/entrypoint",
    title: "Put a public front door on it",
    body: "A small cloud VM as the address of the GPUs at home, with no port opened on your router.",
  },
  {
    href: "/docs/routing",
    title: "Model routing",
    body: "How a request picks a machine: load, locality, a preferred node, prefix affinity, and llm-d scheduling for a model that needs it.",
  },
  {
    href: "/docs/api",
    title: "Call it from your tools",
    body: "The OpenAI-compatible endpoints, setting the base URL in a client, and the headers that say which machine answered.",
  },
];

// Captured by scripts/docshots.mjs, which scrubs the entrypoint's name and
// the tailnet addresses.
const SHOTS = [
  { src: "/img/mesh-architecture.png", label: "Mesh", alt: "The dashboard's Mesh page in Architecture view: four nodes inside a tailnet boundary, each with its addresses, listeners and engines, and lines tracing a request between them." },
  { src: "/img/mesh-constellation.png", label: "Constellation", alt: "The Mesh page in Constellation view: the loaded models at the centre, joined to the nodes that hold them." },
  { src: "/img/dash-local.png", label: "My Models", alt: "The My Models page: every model on every node, grouped by model, with Load and Unload buttons." },
  { src: "/img/dash-discover.png", label: "Discover", alt: "The Discover models browser: a list of models on Hugging Face and one model's page, with a download panel showing each node." },
  { src: "/img/dash-models.png", label: "Serving", alt: "The Serving page: the models being served, each engine process with its load and rates, and the tokens each has processed." },
  { src: "/img/dash-routing.png", label: "Routing", alt: "The Routing page: the ModelFabric router and llm-d side by side, who routes each model, and the router's settings." },
  { src: "/img/dash-bench-cluster.png", label: "Benchmark", alt: "The Benchmark page's Cluster benchmark tab: results tables showing how many requests each node served." },
  { src: "/img/dash-activity-mesh.png", label: "Activity", alt: "The Activity page across the whole mesh: a live table of requests with the node each arrived at and the node that served it." },
];

export default function Home() {
  return (
    <main>
      {/* ---------------------------------------------------------- hero */}
      {/* Two columns from lg up: the claim on the left, the picture of it on
          the right. The picture used to sit a screen and a half down, under
          the numbers, while the right half of the hero was empty. */}
      <section className="mx-auto grid max-w-[90rem] items-center gap-x-12 gap-y-8 px-6 pt-14 pb-10 md:pt-16 lg:grid-cols-2">
        {/* min-w-0 on both: a grid item is otherwise as wide as its longest
            unbreakable line, and the install command pushed a phone sideways. */}
        <div className="min-w-0">
          {/* No logo/wordmark eyebrow here: the navbar carries both, a few
            pixels above. Repeating them just delays the headline. */}
          <h1 className="max-w-3xl text-4xl font-bold leading-[1.1] tracking-tight text-[hsl(var(--mfsh-ink))] sm:text-5xl">
            Every machine serves{" "}
            <span className="text-[hsl(var(--mfsh-loopback))]">
              every model you own.
            </span>
          </h1>

          <p className="mt-6 max-w-2xl text-lg leading-8 text-[hsl(var(--mfsh-ink))]">
            ModelFabric is an open peer-to-peer model mesh over{" "}
            <a
              href="https://tailscale.com"
              className="inline-flex items-baseline gap-1.5 whitespace-nowrap font-semibold text-[hsl(var(--mfsh-ink))] underline decoration-[hsl(var(--mfsh-rule))] underline-offset-4 hover:decoration-[hsl(var(--mfsh-loopback))]"
            >
              {/* The same simple-icons mark the footer uses, in the text colour
                  so it reads on both themes. */}
              <svg
                viewBox="0 0 24 24"
                aria-hidden="true"
                className="h-[0.85em] w-[0.85em] self-center fill-current"
              >
                <path d={icons.Tailscale.path} />
              </svg>
              Tailscale
            </a>
            . Run one agent per machine, point every app at{" "}
            <code className="font-mono text-[0.95em]">localhost:1234</code>, and
            it serves the union of every model in the mesh, wherever the weights
            live.
          </p>

          <p className="mt-4 max-w-2xl leading-7 text-[hsl(var(--mfsh-muted))]">
            No central server, no control plane, no account. A node gets its
            peer list from the local tailscaled and connects to each peer on the
            agreed port.
          </p>

          <div className="mt-8 flex flex-wrap items-center gap-3">
            <Link
              href="/docs/getting-started"
              className="rounded bg-[hsl(var(--mfsh-loopback))] px-4 py-2 text-sm font-semibold text-white no-underline hover:opacity-90"
            >
              Get started
            </Link>
            <Link
              href="/docs/benchmark/router-comparison"
              className="rounded border border-[hsl(var(--mfsh-rule))] px-4 py-2 text-sm font-semibold text-[hsl(var(--mfsh-ink))] no-underline hover:border-[hsl(var(--mfsh-loopback))]"
            >
              See the benchmark
            </Link>
          </div>

          {/* The real installer URL, served by the docs site itself (see
            scripts/sync-install.mjs). */}
          <div className="mt-5 max-w-2xl overflow-x-auto rounded border border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface-sunken))] px-3.5 py-2.5">
            <code className="whitespace-nowrap font-mono text-[0.8rem] text-[hsl(var(--mfsh-ink))]">
              <span className="select-none text-[hsl(var(--mfsh-muted))]">
                ${" "}
              </span>
              curl -fsSL https://modelfabric.sh/install.sh | sh
            </code>
          </div>
          <p className="mt-2 text-sm text-[hsl(var(--mfsh-muted))]">
            Detects your platform, verifies the download against the release
            checksums, installs to{" "}
            <code className="font-mono text-[0.9em]">~/.local/bin</code>. Or{" "}
            <Link
              href="/docs/getting-started"
              className="text-[hsl(var(--mfsh-loopback))]"
            >
              build from source
            </Link>
            .
          </p>
        </div>

        <div className="min-w-0">
          <MeshHero />
          <p className="text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
            Apps talk to the node on their own machine. It serves what it has
            and forwards what it doesn&rsquo;t, so nothing is reconfigured when
            a model moves.
          </p>
        </div>
      </section>

      {/* --------------------------------------------------------- proof */}
      <section className="border-y border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface-sunken))]">
        <div className="mx-auto max-w-[90rem] px-6 py-8">
          <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-4">
            {PROOF.map((p) => (
              <div key={p.small}>
                <div className="font-mono text-2xl font-semibold tabular-nums text-[hsl(var(--mfsh-loopback))]">
                  {p.big}
                </div>
                <div className="mt-1 text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
                  {p.small}
                </div>
              </div>
            ))}
          </div>
          <p className="mt-6 text-sm text-[hsl(var(--mfsh-muted))]">
            One recorded coding-agent workload, replayed with NVIDIA AIPerf
            through three routers on the same three machines, three runs each.{" "}
            <Link
              href="/docs/benchmark/router-comparison"
              className="text-[hsl(var(--mfsh-loopback))]"
            >
              See the benchmark →
            </Link>
          </p>
        </div>
      </section>

      {/* ---------------------------------------------- per-machine speed */}
      {/* Rendered only once bench/fleet/build.py has written real reports. */}
      <FleetBench />

      {/* ------------------------------------------------- the real thing */}
      {/* The hero's diagram is a drawing of the idea. These are the dashboard
          drawing an actual mesh, captured by scripts/docshots.mjs (which
          scrubs the entrypoint's name and the tailnet addresses). */}
      <section>
        <div className="mx-auto max-w-[90rem] px-6 pt-12">
          <h2 className="text-xl font-semibold text-[hsl(var(--mfsh-ink))]">
            Dashboard
          </h2>
          <div className="mt-6 max-w-[72rem]">
            <Gallery shots={SHOTS} />
          </div>
        </div>
      </section>

      {/* ------------------------------------------------------ start here */}
      <section>
        <div className="mx-auto max-w-[90rem] px-6 py-12">
          <h2 className="text-xl font-semibold text-[hsl(var(--mfsh-ink))]">
            Start here
          </h2>

          <div className="mt-7 grid gap-x-10 gap-y-0 sm:grid-cols-2 lg:grid-cols-3">
            {TASKS.map((t) => (
              <Link
                key={t.href}
                href={t.href}
                className="group border-t border-[hsl(var(--mfsh-rule))] py-4 no-underline"
              >
                <span className="block font-semibold text-[hsl(var(--mfsh-ink))] group-hover:text-[hsl(var(--mfsh-loopback))]">
                  {t.title}
                </span>
                <span className="mt-1 block text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
                  {t.body}
                </span>
              </Link>
            ))}
          </div>
        </div>
      </section>

      {/* --------------------------------------------------- what it isn't */}
      <section className="border-t border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface-sunken))]">
        <div className="mx-auto max-w-[90rem] px-6 py-12">
          <h2 className="text-xl font-semibold text-[hsl(var(--mfsh-ink))]">
            What it isn&rsquo;t
          </h2>
          <div className="mt-5 grid gap-7 sm:grid-cols-3">
            <div>
              <div className="font-semibold text-[hsl(var(--mfsh-ink))]">
                Not a chat client
              </div>
              <p className="mt-1.5 text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
                <code className="font-mono">mfsh&nbsp;chat</code> is a
                terminal check that a model answers, not an app. This is
                infrastructure: point a client at the address.
              </p>
            </div>
            <div>
              <div className="font-semibold text-[hsl(var(--mfsh-ink))]">
                Not an orchestrator
              </div>
              <p className="mt-1.5 text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
                No Kubernetes, no containers, no database. Engines are processes
                the node starts and stops.
              </p>
            </div>
            <div>
              <div className="font-semibold text-[hsl(var(--mfsh-ink))]">
                Not a service
              </div>
              <p className="mt-1.5 text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
                Nothing phones home. Your tailnet is the transport, and
                Tailscale is the only identity system involved.
              </p>
            </div>
          </div>
          <Link
            href="/docs"
            className="mt-7 inline-block text-sm font-semibold text-[hsl(var(--mfsh-loopback))]"
          >
            Read the introduction →
          </Link>
        </div>
      </section>
    </main>
  );
}
