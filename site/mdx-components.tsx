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

const docsComponents = getDocsMDXComponents();

// Registered globally rather than imported per page: nearly every page here
// uses at least one of them, and an import line at the top of 30 MDX files is
// 30 places to forget.
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
    ...components,
  };
}
