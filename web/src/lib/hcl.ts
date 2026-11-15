import type { Pipeline, EdgeDef } from './api';

function literal(value: unknown): string {
  if (value === null) return 'null';
  if (typeof value === 'string') return JSON.stringify(value);
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  if (Array.isArray(value)) return '[' + value.map(literal).join(', ') + ']';
  if (typeof value === 'object') {
    return '{ ' + Object.entries(value as Record<string, unknown>).map(([key, val]) => `${JSON.stringify(key)} = ${literal(val)}`).join(', ') + ' }';
  }
  throw new Error(`Unsupported HCL value: ${typeof value}`);
}
function safeName(name: string): string {return JSON.stringify(name);}
export function pipelineToHCL(p: Pipeline): string {
  const rows: string[] = [`pipeline ${safeName(p.name)} {`, `  entry = ${literal(p.entry)}`, `  timeout_ms = ${p.timeout_ms || 60000}`, `  concurrency = ${p.concurrency || 8}`];
  for (const node of Object.values(p.nodes)) {
    rows.push('', `  node ${safeName(node.name)} {`);
    for (const [attr, val] of Object.entries(node.attrs)) rows.push(`    ${attr} = ${literal(val)}`);
    rows.push('  }');
  }
  for (const edge of p.edges) {
    rows.push('', '  edge {', `    from = ${literal(edge.from)}`, `    to = ${literal(edge.to)}`);
    if (edge.when) rows.push(`    when = ${literal(edge.when)}`);
    rows.push('  }');
  }
  rows.push('}');
  return rows.join('\n');
}
export function createNode(p: Pipeline, kind: string): Pipeline {
  let i = 1; while (p.nodes[`new_${i}`]) i++;
  const id = `new_${i}`;
  let attrs: Record<string, unknown> = { kind };
  if (kind === 'transform' || kind === 'output' || kind === 'condition') attrs = { kind, value: 'input' };
  return { ...p, nodes: { ...p.nodes, [id]: { name: id, kind, attrs } } };
}
export function connect(p: Pipeline, from: string, to: string): Pipeline {
  if (from === to || p.edges.some(e => e.from === from && e.to === to)) return p;
  const edges: EdgeDef[] = [...p.edges, { from, to }];
  return { ...p, edges };
}
