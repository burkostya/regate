import type { Edge as FlowEdge } from '@xyflow/svelte';
import type { EdgeDef } from './api';

// IDs are stable for one rendered snapshot. Control and data edges can
// have the same endpoints, so endpoint-only identities are not sufficient.
const prefix = 'regate-edge-';

export function toFlowEdges(definitions: readonly EdgeDef[]): FlowEdge[] {
  return definitions.map((edge, index) => ({
    id: prefix + index,
    source: edge.from,
    target: edge.to,
    type: 'smoothstep',
    label: edge.kind === 'data'
      ? `${edge.from_port || 'value'} → ${edge.to_port || '?'}`
      : edge.when || undefined,
    style: edge.kind === 'data' ? 'stroke:#a78bfa;stroke-dasharray:5 4' : 'stroke:#758ca8'
  }));
}

// A layout-only interaction must never reinterpret authored HCL edge
// semantics. Recover original attributes by the snapshot identity, not
// by a displayed port label or an inferred Svelte Flow handle.
export function fromFlowEdges(current: readonly FlowEdge[], originals: readonly EdgeDef[]): EdgeDef[] {
  return current.map(edge => {
    const match = /^regate-edge-(\d+)$/.exec(edge.id);
    const original = match ? originals[Number(match[1])] : undefined;
    if (original) {
      if (edge.source !== original.from || edge.target !== original.to) {
        throw new Error('Cannot reconnect an existing edge in the canvas; edit its HCL declaration');
      }
      return { ...original };
    }
    // The canvas creates control dependencies only. Ports need HCL.
    return { from: edge.source, to: edge.target };
  });
}

export function canAddControlEdge(edges: readonly EdgeDef[], from: string, to: string): boolean {
  return from !== to && !edges.some(edge =>
    edge.from === from && edge.to === to && edge.kind !== 'data');
}
