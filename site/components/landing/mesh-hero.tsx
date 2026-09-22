import { Arch, Zone, Node, Port, Arrow } from '../arch';

/*
 * The landing page's mesh picture — the same graph as the Introduction's,
 * built from the same primitives so the two can't drift apart in style.
 */
export function MeshHero() {
  return (
    <Arch down legend={['loopback', 'tailnet']}>
      <Zone label="the machine your app runs on" tone="loopback">
        <Node title="any OpenAI client" sub="Claude Code, Codex, your scripts" />
        <Node title="modelfabric" sub="serves what it has, forwards what it doesn't" accent="loopback">
          <Port scope="loopback">localhost:1234</Port>
        </Node>
        <Node title="llama.cpp" sub="its own engines" accent="loopback" dashed />
      </Zone>

      <Arrow
        down
        tone="tailnet"
        label="your tailnet"
        sub="WireGuard · peers found by probing, not registration"
        both
      />

      <Zone
        label="every other machine you own"
        tone="tailnet"
        note="Each one runs the same agent and answers at its own localhost:1234. No node is in charge."
      >
        <Node title="predator" sub="ModelFabric + llama.cpp · qwen3.8-27b" accent="tailnet" />
        <Node title="xpredator" sub="ModelFabric + llama.cpp" accent="tailnet" />
        <Node title="entrypoint-01" sub="ModelFabric · routes only, no models" accent="tailnet" dashed />
      </Zone>
    </Arch>
  );
}
