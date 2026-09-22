import type { MetadataRoute } from 'next';
import { readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

// Walks content/ rather than keeping a hand-written list, so a new page is in
// the sitemap the moment it exists.
function pages(dir: string, base = ''): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...pages(full, `${base}/${entry}`));
    } else if (entry.endsWith('.mdx')) {
      const slug = entry === 'index.mdx' ? base : `${base}/${entry.slice(0, -4)}`;
      out.push(`/docs${slug}`);
    }
  }
  return out;
}

export default function sitemap(): MetadataRoute.Sitemap {
  const routes = ['/', ...pages(join(process.cwd(), 'content'))];
  const now = new Date();
  return routes.map((route) => ({
    url: `https://modelfabric.sh${route}`,
    lastModified: now,
    priority: route === '/' ? 1 : 0.7,
  }));
}
