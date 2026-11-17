import type { EdgeDef, Pipeline } from './api';

const portIdentifier = /^[a-zA-Z_][a-zA-Z0-9_]*$/;

function declaredPort(pipeline: Pipeline, node: string, field: 'input_ports' | 'output_ports', port: string): boolean {
  const raw = pipeline.nodes[node]?.attrs[field];
  return !!raw && typeof raw === 'object' && !Array.isArray(raw) &&
    Object.prototype.hasOwnProperty.call(raw, port);
}

/**
 * Edit a single edge without reserializing any other edge. index === -1
 * means a newly declared data binding; existing edges cannot change kind.
 * Go's HCL compiler remains the authority for the full DAG/port contract.
 */
export function editGraphEdge(pipeline: Pipeline, index: number, proposed: EdgeDef): Pipeline {
  if (!pipeline.nodes[proposed.from] || !pipeline.nodes[proposed.to] ||
      proposed.from === proposed.to) {
    throw new Error('An edge needs two different existing nodes.');
  }
  const prior = index === -1 ? undefined : pipeline.edges[index];
  if (index < -1 || (index !== -1 && !prior)) {
    throw new Error('Selected edge no longer exists; reload the graph.');
  }
  const kind = proposed.kind ?? 'control';
  if (kind !== 'data' && kind !== 'control') throw new Error('Unknown edge kind.');
  if (prior && (prior.kind ?? 'control') !== kind) {
    throw new Error('Changing an existing edge kind is not supported; declare a separate binding.');
  }
  let updated: EdgeDef;
  if (kind === 'data') {
    const fromPort = proposed.from_port || 'value';
    const toPort = proposed.to_port || '';
    if (!portIdentifier.test(fromPort) || !portIdentifier.test(toPort) || proposed.when) {
      throw new Error('Data edges require valid port names and cannot carry a control guard.');
    }
    if (!declaredPort(pipeline, proposed.to, 'input_ports', toPort)) {
      throw new Error(`Declare input_ports.${toPort} on node ${proposed.to} before binding data.`);
    }
    if (fromPort !== 'value') {
      const producer = pipeline.nodes[proposed.from];
      if (producer.kind !== 'pipeline.call' &&
          !(producer.kind === 'mcp.call' && (fromPort === 'structured' || fromPort === 'content'))) {
        throw new Error('Only a declared MCP or child-pipeline output may use a named source port.');
      }
      if (!declaredPort(pipeline, proposed.from, 'output_ports', fromPort)) {
        throw new Error(`Declare output_ports.${fromPort} on node ${proposed.from} first.`);
      }
    }
    if (pipeline.edges.some((edge, i) => i !== index && edge.kind === 'data' &&
        edge.to === proposed.to && edge.to_port === toPort)) {
      throw new Error(`Data port ${proposed.to}.${toPort} already has a producer.`);
    }
    updated = { from: proposed.from, to: proposed.to, kind: 'data',
      ...(proposed.from_port ? { from_port: proposed.from_port } : {}),
      to_port: toPort };
  } else {
    if (proposed.from_port || proposed.to_port) {
      throw new Error('Control edges cannot bind data ports.');
    }
    if (pipeline.edges.some((edge, i) => i !== index && edge.from === proposed.from &&
        edge.to === proposed.to && edge.kind !== 'data')) {
      throw new Error('A control dependency between these nodes already exists.');
    }
    updated = { from: proposed.from, to: proposed.to,
      ...(prior?.kind === 'control' ? { kind: 'control' as const } : {}),
      ...(proposed.when ? { when: proposed.when } : {}) };
  }
  const edges = [...pipeline.edges];
  if (index === -1) edges.push(updated);
  else edges[index] = updated;
  return { ...pipeline, edges };
}

export function removeGraphEdge(pipeline: Pipeline, index: number): Pipeline {
  if (index < 0 || index >= pipeline.edges.length) {
    throw new Error('Selected edge no longer exists.');
  }
  return { ...pipeline, edges: pipeline.edges.filter((_, i) => i !== index) };
}
