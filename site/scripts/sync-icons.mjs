// Extract used simple-icons (CC0) marks during prebuild so the client
// does not bundle the entire icon set. Trademarks belong to their owners.
import { writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as si from 'simple-icons';

const WANT = ['Tailscale', 'Hugging Face', 'Envoy Proxy'];

const byTitle = new Map(
  Object.values(si)
    .filter((i) => i && typeof i === 'object' && i.title && i.path)
    .map((i) => [i.title, i]),
);

const out = {};
for (const title of WANT) {
  const icon = byTitle.get(title);
  if (!icon) {
    console.error(`sync-icons: no mark for ${title} — the band would render a gap`);
    process.exit(1);
  }
  out[title] = { title: icon.title, hex: `#${icon.hex}`, path: icon.path };
}

const here = dirname(fileURLToPath(import.meta.url));
const dest = join(here, '..', 'data', 'brand-icons.json');
mkdirSync(dirname(dest), { recursive: true });
writeFileSync(dest, JSON.stringify(out));
console.log(`sync-icons: ${Object.keys(out).length} marks -> data/brand-icons.json`);
