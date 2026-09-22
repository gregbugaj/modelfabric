import type { ReactNode } from 'react';

/*
 * A two-series bar comparison.
 *
 * Mermaid's xychart-beta draws several `bar` series at the same x position,
 * one in front of the other, with no legend — two series read as one and the
 * smaller is simply hidden. For a comparison, that is worse than a table. This
 * draws both bars, labelled, with the number printed at the end of each, so
 * nothing has to be inferred from a colour.
 */

export type BarRow = {
  label: string;
  a: number;
  b: number;
  /** Rendered instead of the raw numbers when the unit needs saying. */
  aText?: string;
  bText?: string;
};

export function BarCompare({
  rows,
  seriesA,
  seriesB,
  max,
  caption,
}: {
  rows: BarRow[];
  seriesA: string;
  seriesB: string;
  max?: number;
  caption?: ReactNode;
}) {
  const top = max ?? Math.max(...rows.flatMap((r) => [r.a, r.b]));
  const pct = (v: number) => `${Math.max((v / top) * 100, v > 0 ? 1.2 : 0)}%`;

  return (
    <figure className="my-6">
      <div className="mb-3 flex flex-wrap items-center gap-x-5 gap-y-1 text-sm text-[hsl(var(--mfsh-muted))]">
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 rounded-[2px] bg-[hsl(var(--mfsh-internal))]" />
          {seriesA}
        </span>
        <span className="inline-flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 rounded-[2px] bg-[hsl(var(--mfsh-loopback))]" />
          {seriesB}
        </span>
      </div>

      <div className="flex flex-col gap-3.5">
        {rows.map((r) => (
          <div key={r.label} className="grid grid-cols-[6.5rem_1fr] items-center gap-x-3 sm:grid-cols-[8rem_1fr]">
            <div className="text-sm text-[hsl(var(--mfsh-ink))]">{r.label}</div>
            <div className="flex flex-col gap-1">
              <Bar width={pct(r.a)} tone="a" text={r.aText ?? String(r.a)} />
              <Bar width={pct(r.b)} tone="b" text={r.bText ?? String(r.b)} />
            </div>
          </div>
        ))}
      </div>

      {caption ? (
        <figcaption className="mt-3 text-sm text-[hsl(var(--mfsh-muted))]">{caption}</figcaption>
      ) : null}
    </figure>
  );
}

function Bar({ width, tone, text }: { width: string; tone: 'a' | 'b'; text: string }) {
  const bg = tone === 'a' ? 'hsl(var(--mfsh-internal))' : 'hsl(var(--mfsh-loopback))';
  return (
    <div className="flex items-center gap-2">
      <div
        className="h-3 rounded-[2px]"
        style={{ width, background: bg, minWidth: width === '0%' ? 0 : undefined }}
      />
      <span className="font-mono text-xs tabular-nums text-[hsl(var(--mfsh-muted))]">{text}</span>
    </div>
  );
}
