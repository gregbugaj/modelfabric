"use client";

import { useEffect, useState } from "react";

/*
 * The dashboard, as a set of screenshots: one shown large, the rest as
 * thumbnails that swap it in. Clicking the large one opens it full size.
 *
 * The large frame has a fixed shape and shows the top of each screenshot.
 * They differ a lot in height (the Benchmark page is twice as tall as the
 * Mesh), and a frame that resized on every click made the page jump.
 */

export type GalleryShot = { src: string; label: string; alt: string };

export function Gallery({ shots }: { shots: GalleryShot[] }) {
  const [at, setAt] = useState(0);
  const [open, setOpen] = useState(false);
  const shot = shots[at];

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
      if (e.key === "ArrowRight") setAt((i) => (i + 1) % shots.length);
      if (e.key === "ArrowLeft") setAt((i) => (i + shots.length - 1) % shots.length);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, shots.length]);

  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label={`${shot.label}: open full size`}
        className="block aspect-[7/5] w-full cursor-zoom-in overflow-hidden rounded-md border border-[hsl(var(--mfsh-rule))] bg-[#12151c] p-0"
      >
        <img src={shot.src} alt={shot.alt} className="h-full w-full object-cover object-top" />
      </button>

      <div className="mt-3 grid grid-cols-4 gap-2 sm:grid-cols-8">
        {shots.map((s, i) => (
          <button
            key={s.src}
            type="button"
            onClick={() => setAt(i)}
            aria-pressed={i === at}
            className={
              "group min-w-0 rounded border bg-transparent p-0 text-left " +
              (i === at
                ? "border-[hsl(var(--mfsh-loopback))]"
                : "border-[hsl(var(--mfsh-rule))] hover:border-[hsl(var(--mfsh-loopback))]")
            }
          >
            <span className="block aspect-[16/10] overflow-hidden rounded-t bg-[#12151c]">
              <img src={s.src} alt="" loading="lazy" className="h-full w-full object-cover object-top" />
            </span>
            <span
              className={
                "block truncate px-1.5 py-1 text-xs " +
                (i === at ? "font-semibold text-[hsl(var(--mfsh-ink))]" : "text-[hsl(var(--mfsh-muted))]")
              }
            >
              {s.label}
            </span>
          </button>
        ))}
      </div>

      {open ? (
        <div
          role="dialog"
          aria-modal="true"
          aria-label={shot.label}
          onClick={() => setOpen(false)}
          className="fixed inset-0 z-50 cursor-zoom-out overflow-auto bg-black/85 p-4 sm:p-8"
        >
          <img src={shot.src} alt={shot.alt} className="mx-auto w-full max-w-[1400px] rounded-md" />
        </div>
      ) : null}
    </div>
  );
}
