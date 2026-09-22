import {
  BookOpen,
  Rocket,
  LayoutDashboard,
  Boxes,
  Cpu,
  Network,
  Plug,
  ServerCog,
  Terminal,
  Gauge,
} from 'lucide-react';

const I = ({ children }: { children: React.ReactNode }) => (
  <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>{children}</span>
);

export default {
  index: {
    title: (
      <I>
        <BookOpen size={16} />
        Introduction
      </I>
    ),
  },

  '---start': { type: 'separator', title: 'Start here' },
  'getting-started': {
    title: (
      <I>
        <Rocket size={16} />
        Getting started
      </I>
    ),
  },
  dashboard: {
    title: (
      <I>
        <LayoutDashboard size={16} />
        Dashboard
      </I>
    ),
  },

  '---models': { type: 'separator', title: 'Models' },
  models: {
    title: (
      <I>
        <Boxes size={16} />
        Models
      </I>
    ),
  },
  engines: {
    title: (
      <I>
        <Cpu size={16} />
        Engines
      </I>
    ),
  },

  '---mesh': { type: 'separator', title: 'The mesh' },
  routing: {
    title: (
      <I>
        <Network size={16} />
        Routing
      </I>
    ),
  },
  api: {
    title: (
      <I>
        <Plug size={16} />
        API
      </I>
    ),
  },

  '---measure': { type: 'separator', title: 'Measurement' },
  benchmark: {
    title: (
      <I>
        <Gauge size={16} />
        Benchmarks and tuning
      </I>
    ),
  },

  '---operate': { type: 'separator', title: 'Operate' },
  operations: {
    title: (
      <I>
        <ServerCog size={16} />
        Operations
      </I>
    ),
  },

  '---reference': { type: 'separator', title: 'Reference' },
  reference: {
    title: (
      <I>
        <Terminal size={16} />
        Reference
      </I>
    ),
  },
};
