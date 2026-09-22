import nextra from 'nextra';
import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));

const withNextra = nextra({
  contentDirBasePath: '/docs',
  defaultShowCopyCode: true,
});

export default withNextra({
  reactStrictMode: true,
  output: 'standalone',
  outputFileTracingRoot: __dirname,
  experimental: { workerThreads: true, cpus: 4 },
  typescript: { ignoreBuildErrors: true },
});
