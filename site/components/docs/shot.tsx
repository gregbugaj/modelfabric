export function Shot({
  src,
  alt,
  caption,
  width,
}: {
  src: string;
  alt: string;
  caption?: string;
  /** Rendered width in px; the file itself is 2× for retina. */
  width?: number;
}) {
  return (
    <figure className="my-6">
      <img
        src={src}
        alt={alt}
        loading="lazy"
        style={width ? { maxWidth: width } : undefined}
        className="w-full rounded-md border border-[hsl(var(--mfsh-rule))]"
      />
      {caption ? (
        <figcaption className="mt-2 text-sm text-[hsl(var(--mfsh-muted))]">{caption}</figcaption>
      ) : null}
    </figure>
  );
}
