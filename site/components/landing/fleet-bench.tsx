import data from '../../data/fleet-bench.json';

/*
 * How fast each machine in our own mesh runs one model, one request at a time.
 *
 * data/fleet-bench.json is written by bench/fleet/build.py from `mfsh bench`
 * reports, and this renders it and nothing else: no number here was typed by
 * hand. With no machines in the file the section is not rendered at all,
 * because an empty table, or one filled with placeholder figures, is the
 * invented-dashboard look the rest of the site avoids.
 *
 * The machines differ on purpose (a 5090, a 6000 Ada, a Mac): that spread is
 * what the router has to work with, so it is shown rather than averaged.
 */

type Size = { pp_tps: number; tg_tps: number; ttft_ms: number };
type Node = {
  node: string;
  gpu: string;
  os: string;
  runtime: string;
  sizes: Record<string, Size>;
};

const nf = new Intl.NumberFormat('en-US');

function k(n: number) {
  return `${n / 1024}K`;
}

function secs(ms: number) {
  return ms < 10_000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.round(ms / 1000)} s`;
}

// A size this machine was not measured at: a gap, never a guess.
function Gap() {
  return <span className="text-[hsl(var(--mfsh-muted))]">–</span>;
}

export function FleetBench() {
  const nodes = data.nodes as Node[];
  if (nodes.length === 0) return null;
  const sizes = data.sizes as number[];
  const mid = sizes[Math.floor(sizes.length / 2)];
  const first = sizes[0];

  const th = 'px-3 py-2 text-left text-[0.74rem] font-medium text-[hsl(var(--mfsh-muted))]';
  const td = 'px-3 py-2.5 align-top';
  const num = `${td} text-right font-mono tabular-nums text-[hsl(var(--mfsh-ink))]`;

  return (
    <section>
      <div className="mx-auto max-w-[90rem] px-6 pt-12">
        <h2 className="text-xl font-semibold text-[hsl(var(--mfsh-ink))]">Speed on each machine</h2>
        <p className="mt-2 max-w-3xl text-sm leading-6 text-[hsl(var(--mfsh-muted))]">
          {data.model} {data.quant}, one request at a time, measured {data.measured} on build{' '}
          <code className="font-mono">{data.build}</code>. Prefill is how fast a prompt is read;
          generation is how fast the answer is written. Both are in tokens per second.
        </p>

        <div className="mt-5 max-w-[72rem] overflow-x-auto rounded border border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface))]">
          <table className="w-full min-w-[40rem] border-collapse text-sm">
            <thead>
              <tr className="border-b border-[hsl(var(--mfsh-rule))]">
                <th className={th}>Machine</th>
                {sizes.map((s) => (
                  <th key={s} className={`${th} text-right`}>
                    Prefill · {k(s)}
                  </th>
                ))}
                <th className={`${th} text-right`}>Generation</th>
                <th className={`${th} text-right`}>First token · {k(mid)}</th>
              </tr>
            </thead>
            <tbody>
              {nodes.map((n) => (
                <tr key={n.node} className="border-b border-[hsl(var(--mfsh-rule))] last:border-b-0">
                  <td className={td}>
                    <div className="font-semibold text-[hsl(var(--mfsh-ink))]">{n.node}</div>
                    <div className="text-[0.78rem] text-[hsl(var(--mfsh-muted))]">
                      {n.gpu}
                      {n.os ? ` · ${n.os}` : ''}
                    </div>
                    <div className="mt-0.5 break-all font-mono text-[0.7rem] text-[hsl(var(--mfsh-muted))]">
                      {n.runtime}
                    </div>
                  </td>
                  {sizes.map((s) => {
                    const v = n.sizes[String(s)];
                    return (
                      <td key={s} className={num}>
                        {v ? nf.format(v.pp_tps) : <Gap />}
                      </td>
                    );
                  })}
                  <td className={num}>
                    {n.sizes[String(first)] ? n.sizes[String(first)].tg_tps : <Gap />}
                  </td>
                  <td className={num}>
                    {n.sizes[String(mid)] ? secs(n.sizes[String(mid)].ttft_ms) : <Gap />}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <p className="mt-3 text-sm text-[hsl(var(--mfsh-muted))]">
          Reproduce on your own machines: <code className="font-mono">{data.command}</code>
        </p>
      </div>
    </section>
  );
}
