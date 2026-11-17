import { test } from 'node:test';
import assert from 'node:assert/strict';
import { toFlowEdges, fromFlowEdges, canAddControlEdge } from '../src/lib/flowEdges.ts';
import { pipelineToHCL } from '../src/lib/hcl.ts';

const originalEdges = [
  { from: 'fetch', to: 'consumer', when: 'always', kind: 'control' },
  { from: 'fetch', to: 'consumer', kind: 'data', from_port: 'structured', to_port: 'customer' },
  { from: 'consumer', to: 'finish', when: 'allow' }
];

test('drag round trip retains distinct typed/data/control edges and guards', () => {
  const visual = toFlowEdges(originalEdges);
  assert.equal(new Set(visual.map(edge => edge.id)).size, originalEdges.length);
  assert.match(String(visual[1].label), /structured.*customer/);
  assert.deepEqual(fromFlowEdges(visual, originalEdges), originalEdges);
  assert.deepEqual(fromFlowEdges([...visual].reverse(), originalEdges), [...originalEdges].reverse());
});

test('canvas connections only add control edges without changing existing data ports', () => {
  const visual = toFlowEdges(originalEdges);
  visual.push({ id: 'regate-new-1', source: 'fetch', target: 'finish', type: 'smoothstep' });
  assert.deepEqual(fromFlowEdges(visual, originalEdges).at(-1), { from: 'fetch', to: 'finish' });
  assert.equal(canAddControlEdge(originalEdges, 'fetch', 'consumer'), false);
  assert.equal(canAddControlEdge([originalEdges[1]], 'fetch', 'consumer'), true);
  assert.equal(canAddControlEdge(originalEdges, 'finish', 'finish'), false);
});

test('rerouting an authored edge fails closed instead of silently changing its binding', () => {
  const visual = toFlowEdges(originalEdges);
  visual[1].target = 'finish';
  assert.throws(() => fromFlowEdges(visual, originalEdges), /Cannot reconnect/);
});

test('HCL graph serialization retains typed edge properties and guards', () => {
  const hcl = pipelineToHCL({
    name: 'typed', entry: 'fetch', timeout_ms: 1000, concurrency: 2,
    nodes: {
      fetch: { name: 'fetch', kind: 'input', attrs: { kind: 'input' } },
      consumer: { name: 'consumer', kind: 'transform', attrs: { kind: 'transform', value: 'input' } },
      finish: { name: 'finish', kind: 'output', attrs: { kind: 'output', value: 'input' } }
    },
    edges: originalEdges
  });
  assert.match(hcl, /kind = "data"/);
  assert.match(hcl, /from_port = "structured"/);
  assert.match(hcl, /to_port = "customer"/);
  assert.match(hcl, /kind = "control"/);
  assert.match(hcl, /when = "allow"/);
});
