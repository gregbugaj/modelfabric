/*
 * The ModelFabric mark: three peers and the links between them, same geometry as the
 * dashboard's rail (web/index.html).
 *
 * It paints itself in the product blue rather than inheriting currentColor.
 * Inheriting made it render as plain navbar text — black in light mode, white
 * in dark — which is not a logo, it is a glyph.
 */
export function Logo({ size = 20, tone = 'brand' }: { size?: number; tone?: 'brand' | 'inherit' }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke={tone === 'brand' ? 'hsl(var(--mfsh-loopback))' : 'currentColor'}
      strokeWidth="2.5"
      strokeLinecap="round"
      aria-hidden="true"
      style={{ flexShrink: 0 }}
    >
      <circle cx="12" cy="5" r="2.5" />
      <circle cx="5" cy="19" r="2.5" />
      <circle cx="19" cy="19" r="2.5" />
      <path d="M12 7.5 6.5 16.5M12 7.5l5.5 9M7.5 19h9" />
    </svg>
  );
}
