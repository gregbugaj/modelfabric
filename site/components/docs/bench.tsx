import type { ReactNode } from 'react';
import data from '../../data/swe.json';

/*
 * Benchmark report components.
 *
 * These mirror the standalone HTML report in docs/reports/ so the docs page
 * carries the same reading — headline claim, KPI cards, the latency curve, the
 * placement evidence and the per-task detail — rather than flattening it all
 * into markdown tables.
 *
 * data/swe.json is written by bench/swe/report/build.py --docs-data, from the
 * same aggregation that builds the standalone page, so the two cannot disagree
 * and nothing here is retyped by hand. Swapping in a later run means
 * regenerating that file: the arm names, task counts and curves all come from
 * it.
 */

const DIRECT = 'hsl(28 92% 45%)'; // the reference arm
const LLMD = 'hsl(243 55% 56%)'; // the scheduled arm

// Both arms are ModelFabric on the same fleet. What changed is the routing mode, so
// the arms are named after the modes — not after llm-d, which is one of the
// schedulers ModelFabric can run, not ModelFabric's opponent.
const A = data.arms.a;
const B = data.arms.b;

// ---------------------------------------------------------------- spec chips

export function RunSpec({ items }: { items: [string, string][] }) {
  return (
    <div className="my-5 flex flex-wrap gap-2">
      {items.map(([k, v]) => (
        <span
          key={k}
          className="inline-flex items-baseline gap-2 rounded border border-[hsl(var(--mfsh-rule))]
                     bg-[hsl(var(--mfsh-surface-sunken))] px-2.5 py-1 text-[0.8rem]"
        >
          <span className="font-semibold text-[hsl(var(--mfsh-ink))]">{k}</span>
          <span className="text-[hsl(var(--mfsh-muted))]">{v}</span>
        </span>
      ))}
    </div>
  );
}

// ------------------------------------------------------------------ KPI cards

export function Kpis({
  cards,
}: {
  cards: { label: string; a: string; b: string; delta: string; good?: boolean }[];
}) {
  return (
    <div className="my-6 grid gap-px overflow-hidden rounded border border-[hsl(var(--mfsh-rule))]
                    bg-[hsl(var(--mfsh-rule))] sm:grid-cols-2 lg:grid-cols-4">
      {cards.map((c) => (
        <div key={c.label} className="bg-[hsl(var(--mfsh-surface))] p-4">
          <div className="text-[0.78rem] font-medium text-[hsl(var(--mfsh-muted))]">{c.label}</div>
          <dl className="mt-3 space-y-1.5">
            <Row name={A} value={c.a} color={DIRECT} />
            <Row name={B} value={c.b} color={LLMD} strong />
          </dl>
          <div
            className="mt-3 inline-block rounded px-1.5 py-0.5 font-mono text-[0.72rem]"
            style={{
              background: c.good === false ? 'hsl(var(--mfsh-rule))' : 'hsl(160 60% 38% / 0.14)',
              color: c.good === false ? 'hsl(var(--mfsh-muted))' : 'hsl(160 60% 30%)',
            }}
          >
            {c.delta}
          </div>
        </div>
      ))}
    </div>
  );
}

function Row({
  name,
  value,
  color,
  strong,
}: {
  name: string;
  value: string;
  color: string;
  strong?: boolean;
}) {
  return (
    // The run names are long ("optimized-baseline"); let the name wrap and
    // keep the number on one line, rather than the other way round.
    <div className="flex items-baseline justify-between gap-2">
      <dt className="flex min-w-0 items-baseline gap-1.5 text-[0.74rem] leading-4 text-[hsl(var(--mfsh-muted))]">
        <span className="mt-[1px] inline-block h-2 w-2 shrink-0 rounded-[2px]" style={{ background: color }} />
        <span className="break-words">{name}</span>
      </dt>
      <dd
        className={`shrink-0 whitespace-nowrap font-mono tabular-nums ${strong ? 'text-[1.05rem] font-semibold' : 'text-[0.95rem]'}`}
        style={{ color: strong ? color : 'hsl(var(--mfsh-ink))' }}
      >
        {value}
      </dd>
    </div>
  );
}

// --------------------------------------------------------------- latency CDF

// Latency read at a percentile, not a CDF.
//
// The CDF this replaced plotted share-under-a-latency on a log x. When the
// modes differ only in the tail it draws two near-identical S curves that
// separate inside the last two percent of their height — a few pixels under
// the 100% gridline. The finding was invisible in the chart meant to show it.
//
// Here the percentile is x, stretched by -log10(1-p) so each further nine gets
// equal width, and latency is a log y. The left edge is the median, where the
// modes are level; every step right is ten times rarer, and they fan apart.
const dur = (v: number) => (v < 90 ? `${Math.round(v)} s` : `${(v / 60).toFixed(v < 600 ? 1 : 0)} min`);

export function LatencyCdf() {
  const W = 760;
  const H = 320;
  const M = { t: 16, r: 92, b: 46, l: 54 };
  const iw = W - M.l - M.r;
  const ih = H - M.t - M.b;

  const arms = [
    { name: A, pts: data.pct.a as number[][], color: DIRECT, calls: data.calls.a },
    { name: B, pts: data.pct.b as number[][], color: LLMD, calls: data.calls.b },
  ].filter((s) => s.pts.length > 1);
  if (!arms.length) return null;

  const tx = (pc: number) => -Math.log10(1 - pc / 100);
  const t0 = tx(arms[0].pts[0][0]);
  const t1 = Math.min(...arms.map((s) => tx(s.pts[s.pts.length - 1][0])));
  const x = (pc: number) => M.l + ((tx(pc) - t0) / (t1 - t0)) * iw;

  const ymax = Math.max(...arms.map((s) => Math.max(...s.pts.map((p) => p[1])))) * 1.15;
  const y = (v: number) => M.t + ih - (Math.log10(Math.max(v, 1)) / Math.log10(ymax)) * ih;
  const path = (pts: number[][]) =>
    pts.map((p, i) => `${i ? 'L' : 'M'}${x(p[0]).toFixed(1)},${y(p[1]).toFixed(1)}`).join('');

  // Seconds below a minute, minutes above: "1800 s" is a number to convert,
  // "30 min" is one to feel.
  const ylab = (v: number) => (v < 60 ? `${v} s` : `${v / 60} min`);
  const yticks = [1, 3, 10, 30, 60, 300, 600, 1800, 3600].filter((v) => v <= ymax);
  const xticks = [50, 75, 90, 95, 99, 99.9].filter((p) => tx(p) >= t0 && tx(p) <= t1);

  // Each arm's label sits on its own curve's end, nudged apart only if two
  // would collide.
  const ends = arms
    .map((s) => ({ name: s.name, color: s.color, v: s.pts[s.pts.length - 1][1] }))
    .map((e) => ({ ...e, yy: y(e.v) }))
    .sort((p, q) => p.yy - q.yy);
  ends.forEach((e, i) => {
    if (i && e.yy - ends[i - 1].yy < 15) e.yy = ends[i - 1].yy + 15;
  });

  return (
    <figure className="my-6">
      <div className="overflow-x-auto rounded border border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface))] p-3">
        <svg viewBox={`0 0 ${W} ${H}`} width="100%" role="img"
             aria-label="Model call latency read at each percentile, one curve per routing mode">
          {yticks.map((v) => (
            <g key={v}>
              <line x1={M.l} x2={W - M.r} y1={y(v)} y2={y(v)}
                    stroke="hsl(var(--mfsh-rule))" strokeWidth="1" />
              <text x={M.l - 8} y={y(v) + 4} textAnchor="end"
                    fontSize="11" fill="hsl(var(--mfsh-muted))">{ylab(v)}</text>
            </g>
          ))}
          {xticks.map((p) => (
            <g key={p}>
              <line x1={x(p)} x2={x(p)} y1={M.t} y2={H - M.b}
                    stroke="hsl(var(--mfsh-rule))" strokeWidth="1" />
              <text x={x(p)} y={H - M.b + 18} textAnchor="middle"
                    fontSize="11" fill="hsl(var(--mfsh-muted))">p{p}</text>
            </g>
          ))}
          <text x={M.l + iw / 2} y={H - 6} textAnchor="middle" fontSize="11"
                fill="hsl(var(--mfsh-muted))">percentile of model calls (each step right is 10× rarer)</text>

          {arms.map((s) => (
            <path key={s.name} d={path(s.pts)} fill="none" stroke={s.color} strokeWidth="2" />
          ))}
          {ends.map((e) => (
            <g key={e.name}>
              <circle cx={W - M.r} cy={y(e.v)} r="4" fill={e.color} />
              <text x={W - M.r + 8} y={e.yy + 4} fontSize="11" fill={e.color} fontWeight="600">
                {e.name} {dur(e.v)}
              </text>
            </g>
          ))}
        </svg>
      </div>
      <figcaption className="mt-2 text-sm text-[hsl(var(--mfsh-muted))]">
        {data.calls.a} model calls under {A}, {data.calls.b} under {B}. The two are level at the
        median and separate from about p90. The tail is a conversation re-placed onto an engine
        that no longer held its prefix, or queued behind the slowest node in the fleet.
      </figcaption>
    </figure>
  );
}

// ------------------------------------------------------------ pile-up evidence

// One card per engine, with free-form rows. The rows used to be fixed
// ("requests in flight", "prefill work") because the pilot had one thing to
// say about two GPUs at one moment. A three-node run compares occupancy per
// mode instead, so the page supplies its own labels; `hot` marks the row worth
// looking at rather than the whole card.
export function EngineState({
  nodes,
  cols = 2,
}: {
  nodes: { name: string; spec: string; rows: { label: string; value: string; hot?: boolean }[] }[];
  cols?: 2 | 3;
}) {
  return (
    <div
      className={`my-5 grid gap-px overflow-hidden rounded border border-[hsl(var(--mfsh-rule))]
                  bg-[hsl(var(--mfsh-rule))] ${cols === 3 ? 'sm:grid-cols-3' : 'sm:grid-cols-2'}`}
    >
      {nodes.map((n) => (
        <div key={n.name} className="bg-[hsl(var(--mfsh-surface))] p-4">
          <div className="font-semibold text-[hsl(var(--mfsh-ink))]">{n.name}</div>
          <div className="text-[0.78rem] text-[hsl(var(--mfsh-muted))]">{n.spec}</div>
          <dl className="mt-3 space-y-1.5 text-[0.85rem]">
            {n.rows.map((r) => (
              <div key={r.label} className="flex justify-between gap-3">
                <dt className="text-[hsl(var(--mfsh-muted))]">{r.label}</dt>
                <dd
                  className="font-mono tabular-nums"
                  style={
                    r.hot
                      ? { color: 'hsl(28 92% 40%)', fontWeight: 600 }
                      : { color: 'hsl(var(--mfsh-ink))' }
                  }
                >
                  {r.value}
                </dd>
              </div>
            ))}
          </dl>
        </div>
      ))}
    </div>
  );
}

// ------------------------------------------------------------ all measurements

const MEASURES: [string, keyof typeof data.agg.a, (v: number) => string, 'lower' | 'higher'][] = [
  ['Model calls', 'turns', (v) => String(v), 'higher'],
  ['Cache hit share', 'cache_pct', (v) => `${v}%`, 'higher'],
  ['Prompt tokens', 'prompt', (v) => `${(v / 1e6).toFixed(2)}M`, 'higher'],
  ['Prefilled from scratch', 'prefilled', (v) => `${(v / 1e6).toFixed(2)}M`, 'lower'],
  ['Output tokens', 'out', (v) => `${(v / 1e6).toFixed(2)}M`, 'higher'],
  ['Call latency p50', 'p50', (v) => `${v} s`, 'lower'],
  ['Call latency p90', 'p90', (v) => `${v} s`, 'lower'],
  ['Call latency p95', 'p95', (v) => `${v} s`, 'lower'],
  ['Call latency p99', 'p99', (v) => `${v} s`, 'lower'],
  ['Slowest call', 'max', (v) => `${v} s`, 'lower'],
  ['Total model wait', 'model_min', (v) => `${Math.round(v)} min`, 'lower'],
];

export function AllMeasurements() {
  const { a, b, a_all: c } = data.agg;
  const n = data.n.common;
  // The baseline's own full run is only worth a column when a mode lost a
  // task. When none did it is a character-for-character copy of the first
  // column, which reads as a third run rather than the same one twice.
  const showAll = data.n.all !== n;
  return (
    <div className="my-6 overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr>
            <th className="text-left font-medium text-[hsl(var(--mfsh-muted))]">Measure</th>
            <th className="text-right font-medium" style={{ color: DIRECT }}>{A}, same {n}</th>
            <th className="text-right font-medium" style={{ color: LLMD }}>{B}, same {n}</th>
            {showAll && (
              <th className="text-right font-medium text-[hsl(var(--mfsh-muted))]">
                {A}, all {data.n.all}
              </th>
            )}
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>Tasks resolved</td>
            <td className="text-right font-mono tabular-nums">{a.resolved} of {a.tasks}</td>
            <td className="text-right font-mono tabular-nums">{b.resolved} of {b.tasks}</td>
            {showAll && (
              <td className="text-right font-mono tabular-nums text-[hsl(var(--mfsh-muted))]">
                {c.resolved} of {c.tasks}
              </td>
            )}
          </tr>
          <tr>
            <td>Patches submitted</td>
            <td className="text-right font-mono tabular-nums">{a.submitted} of {a.tasks}</td>
            <td className="text-right font-mono tabular-nums">{b.submitted} of {b.tasks}</td>
            {showAll && (
              <td className="text-right font-mono tabular-nums text-[hsl(var(--mfsh-muted))]">
                {c.submitted} of {c.tasks}
              </td>
            )}
          </tr>
          {MEASURES.map(([label, key, fmt, better]) => {
            const av = a[key] as number;
            const bv = b[key] as number;
            const win = better === 'lower' ? bv < av : bv > av;
            return (
              <tr key={label}>
                <td>{label}</td>
                <td className="text-right font-mono tabular-nums">{fmt(av)}</td>
                <td
                  className="text-right font-mono tabular-nums"
                  style={win ? { color: 'hsl(160 60% 32%)', fontWeight: 600 } : undefined}
                >
                  {fmt(bv)}
                </td>
                {showAll && (
                  <td className="text-right font-mono tabular-nums text-[hsl(var(--mfsh-muted))]">
                    {fmt(c[key] as number)}
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// -------------------------------------------------------------- per-task table

function Chip({ kind }: { kind: 'resolved' | 'unresolved' | 'no patch' | 'timed out' }) {
  const tone: Record<string, [string, string]> = {
    resolved: ['hsl(160 60% 38% / 0.16)', 'hsl(160 60% 28%)'],
    unresolved: ['hsl(var(--mfsh-rule))', 'hsl(var(--mfsh-muted))'],
    'no patch': ['hsl(var(--mfsh-rule))', 'hsl(var(--mfsh-muted))'],
    'timed out': ['hsl(28 92% 48% / 0.16)', 'hsl(28 92% 34%)'],
  };
  const [bg, fg] = tone[kind];
  return (
    <span className="inline-block whitespace-nowrap rounded px-1.5 py-0.5 text-[0.7rem] font-medium"
          style={{ background: bg, color: fg }}>
      {kind}
    </span>
  );
}

function outcome(side: any): 'resolved' | 'unresolved' | 'no patch' | 'timed out' {
  if (!side) return 'timed out';
  if (side.resolved) return 'resolved';
  return side.exit === 'Submitted' ? 'unresolved' : 'no patch';
}

export function PerTask() {
  return (
    <div className="my-6 overflow-x-auto">
      <table className="w-full text-[0.82rem]">
        <thead>
          <tr>
            <th className="text-left font-medium text-[hsl(var(--mfsh-muted))]">Task</th>
            <th className="px-2 text-left font-medium" style={{ color: DIRECT }} colSpan={4}>{A}</th>
            <th className="mfsh-group px-2 text-left font-medium" style={{ color: LLMD }} colSpan={4}>{B}</th>
          </tr>
          <tr className="text-[0.75rem] text-[hsl(var(--mfsh-muted))]">
            <th />
            <th className="px-2 text-left font-normal">result</th>
            <th className="px-2 text-right font-normal">turns</th>
            <th className="px-2 text-right font-normal">cache</th>
            <th className="px-2 text-right font-normal">model</th>
            <th className="mfsh-group px-2 text-left font-normal">result</th>
            <th className="px-2 text-right font-normal">turns</th>
            <th className="px-2 text-right font-normal">cache</th>
            <th className="px-2 text-right font-normal">model</th>
          </tr>
        </thead>
        <tbody>
          {data.tasks.map((t: any) => {
            const lost = !t.b;
            return (
              <tr key={t.id} style={lost ? { background: 'hsl(28 92% 48% / 0.07)' } : undefined}>
                <td className="whitespace-nowrap font-mono text-[0.78rem]">
                  {t.id.replace('astropy__astropy-', 'astropy-')}
                </td>
                <Side s={t.a} />
                {lost ? (
                  <>
                    <td className="mfsh-group px-2"><Chip kind="timed out" /></td>
                    <td className="px-2 text-right text-[hsl(var(--mfsh-muted))]">—</td>
                    <td className="px-2 text-right text-[hsl(var(--mfsh-muted))]">—</td>
                    <td className="px-2 text-right text-[hsl(var(--mfsh-muted))]">—</td>
                  </>
                ) : (
                  <Side s={t.b} group />
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function Side({ s, group }: { s: any; group?: boolean }) {
  const low = s.cache < 80;
  return (
    <>
      <td className={`px-2 ${group ? 'mfsh-group' : ''}`}><Chip kind={outcome(s)} /></td>
      <td className="px-2 text-right font-mono tabular-nums">{s.turns}</td>
      <td className="px-2 text-right font-mono tabular-nums"
          style={low ? { color: 'hsl(28 92% 34%)', fontWeight: 600 } : undefined}>
        {s.cache}%
      </td>
      <td className="px-2 text-right font-mono tabular-nums">{Math.round(s.model_s / 60)} min</td>
    </>
  );
}

export function BenchNote({ children }: { children: ReactNode }) {
  return (
    <div className="my-6 rounded border-l-2 border-[hsl(28_92%_48%)] bg-[hsl(28_92%_48%_/_0.06)] p-4">
      {children}
    </div>
  );
}
