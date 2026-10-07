import type { ReactNode } from 'react';

/* Preserve ASCII diagram alignment to match CLI output. */
export function Diagram({ children, label }: { children: ReactNode; label?: string }) {
  return (
    <figure className="my-6">
      <pre className="mfsh-diagram">{children}</pre>
      {label ? (
        <figcaption className="mt-2 text-sm text-[hsl(var(--mfsh-muted))]">{label}</figcaption>
      ) : null}
    </figure>
  );
}
