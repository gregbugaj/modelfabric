import type { ReactNode } from 'react';


export type Scope = 'loopback' | 'tailnet' | 'public' | 'internal';

const SCOPE: Record<Scope, { label: string; v: string }> = {
  loopback: { label: 'loopback', v: '--mfsh-loopback' },
  tailnet: { label: 'tailnet', v: '--mfsh-tailnet' },
  public: { label: 'public', v: '--mfsh-public' },
  internal: { label: 'internal', v: '--mfsh-internal' },
};

const c = (s: Scope) => `hsl(var(${SCOPE[s].v}))`;
const cAlpha = (s: Scope, a: number) => `hsl(var(${SCOPE[s].v}) / ${a})`;


export function Arch({
  children,
  legend,
  down,
}: {
  children: ReactNode;
  legend?: Scope[];
  /** Stack trust tiers vertically. */
  down?: boolean;
}) {
  return (
    <figure className="my-6">
      <div className="overflow-x-auto rounded border border-[hsl(var(--mfsh-rule))] bg-[hsl(var(--mfsh-surface-sunken))] p-5">
        <div
          className={
            down
              ? 'flex flex-col items-stretch gap-0'
              : 'flex flex-wrap items-stretch justify-center gap-3'
          }
        >
          {children}
        </div>
        {legend?.length ? (
          <div className="mt-5 flex flex-wrap items-center gap-x-4 gap-y-1.5 border-t border-[hsl(var(--mfsh-rule))] pt-3 text-[0.72rem] text-[hsl(var(--mfsh-muted))]">
            <span className="uppercase tracking-wider opacity-70">reachable from</span>
            {legend.map((s) => (
              <span key={s} className="inline-flex items-center gap-1.5">
                <span
                  className="inline-block h-2 w-2 rounded-[2px]"
                  style={{ background: c(s) }}
                />
                {SCOPE[s].label}
              </span>
            ))}
          </div>
        ) : null}
      </div>
    </figure>
  );
}


export function Zone({
  label,
  tone = 'internal',
  children,
  note,
}: {
  label: string;
  tone?: Scope;
  children: ReactNode;
  note?: string;
}) {
  return (
    <div
      className="relative flex flex-col justify-center rounded-md border px-4 pb-4 pt-6"
      style={{ borderColor: cAlpha(tone, 0.45), background: cAlpha(tone, 0.04) }}
    >
      <span
        className="absolute left-3 top-0 -translate-y-1/2 rounded bg-[hsl(var(--mfsh-surface-sunken))] px-1.5
                   text-[0.68rem] font-semibold uppercase tracking-wider"
        style={{ color: c(tone) }}
      >
        {label}
      </span>
      <div className="flex flex-wrap items-stretch gap-3">{children}</div>
      {note ? (
        <div className="mt-3 text-[0.72rem] leading-5 text-[hsl(var(--mfsh-muted))]">{note}</div>
      ) : null}
    </div>
  );
}


export function Node({
  title,
  sub,
  children,
  accent,
  dashed,
}: {
  title: string;
  sub?: string;
  children?: ReactNode;
  accent?: Scope;
  dashed?: boolean;
}) {
  return (
    <div
      className="flex min-w-[8.5rem] flex-col justify-center rounded-md border bg-[hsl(var(--mfsh-surface))] px-3.5 py-3"
      style={{
        borderColor: accent ? cAlpha(accent, 0.5) : 'hsl(var(--mfsh-rule))',
        borderStyle: dashed ? 'dashed' : 'solid',
        borderLeftWidth: accent ? 3 : 1,
        borderLeftColor: accent ? c(accent) : 'hsl(var(--mfsh-rule))',
      }}
    >
      <div className="text-[0.88rem] font-semibold leading-5 text-[hsl(var(--mfsh-ink))]">
        {title}
      </div>
      {sub ? (
        <div className="mt-0.5 text-[0.72rem] leading-4 text-[hsl(var(--mfsh-muted))]">{sub}</div>
      ) : null}
      {children ? <div className="mt-2 flex flex-col gap-1">{children}</div> : null}
    </div>
  );
}


export function Port({ scope, children }: { scope: Scope; children: ReactNode }) {
  return (
    <span
      className="inline-flex w-fit items-center gap-1.5 rounded px-1.5 py-0.5 font-mono text-[0.68rem]"
      style={{ background: cAlpha(scope, 0.12), color: c(scope) }}
    >
      <span className="inline-block h-1.5 w-1.5 rounded-full" style={{ background: c(scope) }} />
      {children}
    </span>
  );
}


export function Arrow({
  label,
  sub,
  tone,
  dashed,
  both,
  down,
}: {
  label?: string;
  sub?: string;
  tone?: Scope;
  dashed?: boolean;
  both?: boolean;
  down?: boolean;
}) {
  const stroke = tone ? c(tone) : 'hsl(var(--mfsh-muted))';

  if (down) {
    return (
      <div className="flex items-center justify-center gap-2.5 py-2">
        <svg width="10" height="26" viewBox="0 0 10 26" aria-hidden="true">
          {both ? <path d="M5 4 L2 9 M5 4 L8 9" stroke={stroke} strokeWidth="1.5" fill="none" /> : null}
          <line
            x1="5"
            y1={both ? 5 : 0}
            x2="5"
            y2="20"
            stroke={stroke}
            strokeWidth="1.5"
            strokeDasharray={dashed ? '4 3' : undefined}
          />
          <path d="M5 22 L2 17 M5 22 L8 17" stroke={stroke} strokeWidth="1.5" fill="none" />
        </svg>
        {label ? (
          <span className="text-[0.72rem] font-medium leading-4" style={{ color: stroke }}>
            {label}
            {sub ? (
              <span className="ml-1.5 font-normal text-[hsl(var(--mfsh-muted))]">{sub}</span>
            ) : null}
          </span>
        ) : null}
      </div>
    );
  }

  return (
    <div className="flex min-w-[4.5rem] flex-1 flex-col items-center justify-center self-center px-1" style={{ maxWidth: '7rem' }}>
      {label ? (
        <span
          className="mb-1 whitespace-nowrap text-[0.7rem] font-medium leading-4"
          style={{ color: stroke }}
        >
          {label}
        </span>
      ) : null}
      <svg width="100%" height="10" viewBox="0 0 80 10" preserveAspectRatio="none" aria-hidden="true">
        {both ? <path d="M8 5 L2 2 M8 5 L2 8" stroke={stroke} strokeWidth="1.5" fill="none" /> : null}
        <line
          x1={both ? 3 : 0}
          y1="5"
          x2="72"
          y2="5"
          stroke={stroke}
          strokeWidth="1.5"
          strokeDasharray={dashed ? '4 3' : undefined}
        />
        <path d="M72 5 L66 2 M72 5 L66 8" stroke={stroke} strokeWidth="1.5" fill="none" />
      </svg>
      {sub ? (
        <span className="mt-1 whitespace-nowrap text-[0.66rem] leading-4 text-[hsl(var(--mfsh-muted))]">
          {sub}
        </span>
      ) : null}
    </div>
  );
}


export function Stack({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <div className="flex flex-col justify-center gap-2">
      {label ? (
        <span className="text-[0.68rem] uppercase tracking-wider text-[hsl(var(--mfsh-muted))]">
          {label}
        </span>
      ) : null}
      {children}
    </div>
  );
}
