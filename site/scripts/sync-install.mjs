// Copy install.sh during build to keep /install.sh synchronized with the
// repo and avoid depending on raw.githubusercontent access.
import { copyFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, '..', '..', 'install.sh');
const dest = join(here, '..', 'public', 'install.sh');

if (!existsSync(src)) {
  console.error(`sync-install: ${src} not found — the site would serve a stale installer`);
  process.exit(1);
}
mkdirSync(dirname(dest), { recursive: true });
copyFileSync(src, dest);
console.log('sync-install: install.sh -> public/install.sh');
