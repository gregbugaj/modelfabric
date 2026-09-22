import icons from '../data/brand-icons.json';

/*
 * What ModelFabric is built on — a compact strip for the footer.
 *
 * Only the projects ModelFabric actually stands on: the transport, the engines, the
 * model source, and what it optionally puts in the request path. Go and uv used
 * to be here and were cut; how something is compiled is not what it is built
 * on. A proxy a user chooses to put in front is not something ModelFabric is
 * built on either.
 *
 * Marks come from simple-icons (CC0) where the project has one. llama.cpp,
 * MLX and llm-d don't, so they get a typographic tile rather than a logo
 * someone invented for them. Each link carries its role as a title, so
 * the detail survives without cluttering a footer.
 */

type Item = {
  name: string;
  role: string;
  href: string;
  icon?: keyof typeof icons;
  mono?: string;
};

const ITEMS: Item[] = [
  {
    name: 'Tailscale',
    role: 'WireGuard transport, and the identity every node is checked against',
    href: 'https://tailscale.com',
    icon: 'Tailscale',
  },
  {
    name: 'llama.cpp',
    role: 'the engine ModelFabric supervises, and the builds it installs',
    href: 'https://github.com/ggml-org/llama.cpp',
    mono: 'll',
  },
  {
    name: 'MLX',
    role: "Apple silicon inference, through Apple's own mlx-lm server",
    href: 'https://github.com/ml-explore/mlx',
    mono: 'mx',
  },
  {
    name: 'Hugging Face',
    role: 'where models and their metadata come from',
    href: 'https://huggingface.co',
    icon: 'Hugging Face',
  },
  {
    name: 'llm-d',
    role: 'queue-aware and prefix-aware scheduling, as a routing profile',
    href: 'https://llm-d.ai',
    mono: 'ld',
  },
  {
    name: 'Envoy',
    role: "carries requests to llm-d's scheduler",
    href: 'https://www.envoyproxy.io',
    icon: 'Envoy Proxy',
  },
];

function Mark({ item }: { item: Item }) {
  if (item.icon) {
    const { path, hex } = icons[item.icon];
    return (
      <svg
        viewBox="0 0 24 24"
        width="16"
        height="16"
        aria-hidden="true"
        className="shrink-0 fill-current opacity-70 transition group-hover:fill-[var(--brand)] group-hover:opacity-100"
        style={{ ['--brand' as string]: hex }}
      >
        <path d={path} />
      </svg>
    );
  }
  return (
    <span
      aria-hidden="true"
      className="inline-flex h-4 w-4 shrink-0 items-center justify-center rounded-[4px]
                 border border-current font-mono text-[0.5rem] font-semibold opacity-70
                 transition group-hover:opacity-100"
    >
      {item.mono}
    </span>
  );
}

export function BuiltOn() {
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-2.5">
      <span className="text-[0.78rem] font-semibold uppercase tracking-wider opacity-60">
        Built on
      </span>
      {ITEMS.map((item) => (
        <a
          key={item.name}
          href={item.href}
          target="_blank"
          rel="noreferrer"
          title={item.role}
          className="group inline-flex items-center gap-1.5 text-sm no-underline hover:underline"
        >
          <Mark item={item} />
          {item.name}
        </a>
      ))}
    </div>
  );
}
