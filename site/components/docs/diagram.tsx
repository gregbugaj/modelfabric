import type { ReactNode } from 'react';

/*
 * An ASCII architecture diagram, rendered as-is.
 *
 * ModelFabric's own output is line-drawn text — `mfsh status`, the README's mesh
 * picture, the request-path sketches — so the documentation shows the same
 * thing rather than redrawing it as boxes in SVG. A reader who has run the
 * command recognises what is on the page.
 */
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
