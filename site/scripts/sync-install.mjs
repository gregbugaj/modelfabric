// Copies the repo's install.sh into public/ so the site serves it at
// /install.sh — which is what `curl -fsSL https://modelfabric.sh/install.sh | sh`
// fetches.
//
// Copied rather than redirected on purpose: a redirect to raw.githubusercontent
// only works once the repo is public, and the install command should work the
// moment the site is up. Copied at build time rather than committed, so the
// published script can never drift from the one in the repo.
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
