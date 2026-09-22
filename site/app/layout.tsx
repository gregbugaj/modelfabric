import { Footer, Layout, Navbar } from 'nextra-theme-docs';
import { Head } from 'nextra/components';
import { getPageMap } from 'nextra/page-map';
import { Logo } from '../components/logo';
import { BuiltOn } from '../components/built-on';
import 'nextra-theme-docs/style.css';
import '../styles/globals.css';

const SITE = 'https://modelfabric.sh';
const DESCRIPTION =
  'An open peer-to-peer model mesh over Tailscale. Run one agent per machine and every machine serves every model you own.';

export const metadata = {
  metadataBase: new URL(SITE),
  title: {
    default: 'ModelFabric: every machine serves every model you own',
    template: '%s · ModelFabric',
  },
  description: DESCRIPTION,
  icons: { icon: '/favicon.svg' },
  alternates: { canonical: '/' },
  openGraph: {
    type: 'website',
    siteName: 'ModelFabric',
    url: SITE,
    title: 'ModelFabric: every machine serves every model you own',
    description: DESCRIPTION,
  },
  twitter: {
    card: 'summary',
    title: 'ModelFabric: every machine serves every model you own',
    description: DESCRIPTION,
  },
};

const navbar = (
  <Navbar
    logo={
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: '0.5rem' }}>
        <Logo size={22} />
        <span style={{ fontWeight: 700, fontSize: '1.15rem', letterSpacing: '-0.01em' }}>ModelFabric</span>
        <span style={{ fontSize: '0.8rem', opacity: 0.6, marginLeft: '0.15rem' }}>model mesh</span>
      </span>
    }
    projectLink="https://github.com/gregbugaj/modelfabric"
  />
);

const footer = (
  <Footer>
    <div className="w-full">
      <BuiltOn />
      <div className="mt-5 flex flex-wrap items-center justify-between gap-x-6 gap-y-2 text-sm opacity-60">
        <span>ModelFabric: a peer-to-peer model mesh over Tailscale.</span>
        <span>
          Marks from{' '}
          <a href="https://simpleicons.org" target="_blank" rel="noreferrer">
            simple-icons
          </a>{' '}
          (CC0). Trademarks belong to their owners; ModelFabric is not affiliated with or endorsed by any
          of them.
        </span>
      </div>
    </div>
  </Footer>
);

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" dir="ltr" suppressHydrationWarning>
      <Head>
        <meta name="viewport" content="width=device-width, initial-scale=1.0" />
      </Head>
      <body>
        <Layout
          navbar={navbar}
          pageMap={await getPageMap('/docs')}
          docsRepositoryBase="https://github.com/gregbugaj/modelfabric/tree/main/site"
          editLink="Edit this page on GitHub"
          sidebar={{ defaultMenuCollapseLevel: 1, toggleButton: true }}
          toc={{ backToTop: true }}
          footer={footer}
        >
          {children}
        </Layout>
      </body>
    </html>
  );
}
