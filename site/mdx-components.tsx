import { useMDXComponents as getDocsMDXComponents } from 'nextra-theme-docs';
import { Callout, Steps, Tabs, FileTree } from 'nextra/components';
import { Cards, Card } from './components/docs/cards';
import { Diagram } from './components/docs/diagram';
import { BarCompare } from './components/docs/bar-compare';
import { Arch, Zone, Node as ArchNode, Port, Arrow, Stack } from './components/arch';
import { Shot } from './components/docs/shot';
import {
  RunSpec, Kpis, LatencyCdf, EngineState, AllMeasurements, PerTask, BenchNote,
} from './components/docs/bench';
import {
  RouterKpis, RunSpread, MovesVsRead, RouterLatency, EnginesByRouter, RunTable,
} from './components/docs/routing-bench';

const docsComponents = getDocsMDXComponents();

// Register shared components globally so MDX pages need no repeated imports.
export function useMDXComponents(components?: Record<string, React.ComponentType>) {
  return {
    ...docsComponents,
    Callout,
    Steps,
    Tabs,
    FileTree,
    Cards,
    Card,
    Diagram,
    BarCompare,
    Arch,
    Zone,
    ArchNode,
    Port,
    Arrow,
    Stack,
    Shot,
    RunSpec,
    Kpis,
    LatencyCdf,
    EngineState,
    AllMeasurements,
    PerTask,
    BenchNote,
    RouterKpis,
    RunSpread,
    MovesVsRead,
    RouterLatency,
    EnginesByRouter,
    RunTable,
    ...components,
  };
}
