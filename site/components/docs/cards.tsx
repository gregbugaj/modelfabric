import Link from 'next/link';
import type { ReactNode } from 'react';

/*
 * A two-column list of links, used at the end of a page to say where to go
 * next. Deliberately not a grid of bordered tiles with icons: the sidebar
 * already carries the navigation, and this is a short hand-off, so it is a
 * ruled list with the destination's name and one line about it.
 */

export function Cards({ children }: { children: ReactNode }) {
  return <div className="mfsh-cards mt-6 grid gap-x-8 gap-y-0 sm:grid-cols-2">{children}</div>;
}

export function Card({
  title,
  href,
  children,
}: {
  title: string;
  href: string;
  children?: ReactNode;
}) {
  return (
    <Link
      href={href}
      className="group border-t border-[hsl(var(--mfsh-rule))] py-3.5 no-underline
                 first:border-t-0 sm:[&:nth-child(2)]:border-t-0"
    >
      <span className="block text-[0.95rem] font-semibold text-[hsl(var(--mfsh-ink))] group-hover:text-[var(--x-color-primary-600,#1a6dd9)]">
        {title}
      </span>
      {children ? (
        <span className="mt-0.5 block text-sm leading-6 text-[hsl(var(--mfsh-muted))]">{children}</span>
      ) : null}
    </Link>
  );
}
