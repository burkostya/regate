<script lang="ts">
  import { SvelteFlow, Controls, Background, BackgroundVariant, MiniMap, addEdge, type Node as FlowNode, type Edge as FlowEdge } from '@xyflow/svelte';
  import '@xyflow/svelte/dist/style.css';
  import type { Pipeline, EdgeDef } from './api';

  type Props = {
    pipeline: Pipeline;
    positions: Record<string, { x: number; y: number }>;
    onchange: (edges: EdgeDef[], positions: Record<string, { x: number; y: number }>) => void;
    onselect: (id: string) => void;
  };

  let { pipeline, positions, onchange, onselect }: Props = $props();

  let nodes = $state.raw<FlowNode[]>([]);
  let edges = $state.raw<FlowEdge[]>([]);

  $effect(() => {
    nodes = Object.values(pipeline.nodes).map((n, index) => ({
      id: n.name,
      data: { label: `${n.name}\n${n.kind}` },
      position: positions[n.name] ?? { x: 80 + (index % 4) * 235, y: 80 + Math.floor(index / 4) * 125 },
      type: n.kind === 'output' ? 'output' : n.name === pipeline.entry ? 'input' : 'default',
      style: `border-radius:9px;min-width:150px;border:1px solid ${n.kind.includes('policy') ? '#60a5fa' : n.kind.includes('hook') ? '#a78bfa' : '#495a70'};background:#202d3d;color:#e6edf6;font-size:12px;white-space:pre-line;`
    }));
    edges = pipeline.edges.map((e, i) => ({ id: `${e.from}-${e.to}-${i}`, source: e.from, target: e.to, label: e.when ?? '', type: 'smoothstep', style: 'stroke:#758ca8' }));
  });

  function update() {
    onchange(edges.map(e => ({ from: e.source, to: e.target, when: typeof e.label === 'string' ? e.label : undefined })),
      Object.fromEntries(nodes.map(n => [n.id, n.position])));
  }
  function onConnect(conn: { source: string | null; target: string | null; sourceHandle?: string | null; targetHandle?: string | null }) {
    if (!conn.source || !conn.target) return;
    edges = addEdge({ id: `${conn.source}-${conn.target}-${edges.length}`, source: conn.source, target: conn.target, type: 'smoothstep' }, edges);
    update();
  }
</script>

<div class="flow-wrap">
  <SvelteFlow bind:nodes bind:edges fitView colorMode="dark" onconnect={onConnect}
    onnodeclick={(event: any) => { const n = event?.node ?? event?.detail?.node; if (n?.id) onselect(n.id); }}
    onnodedragstop={() => update()}>
    <Background variant={BackgroundVariant.Dots} gap={22} size={1} />
    <Controls />
    <MiniMap pannable zoomable />
  </SvelteFlow>
</div>
<style>
.flow-wrap { height: 100%; min-height: 430px; width: 100%; background: #172333; border-radius: 10px; overflow: hidden; }
:global(.svelte-flow__node) { cursor: pointer; }
:global(.svelte-flow__controls-button) { background: #202d3d; color: #fff; border-color: #405166; }
</style>
