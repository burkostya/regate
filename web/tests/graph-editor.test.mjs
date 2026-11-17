import { test } from 'node:test';
import assert from 'node:assert/strict';
import { editGraphEdge, removeGraphEdge } from '../src/lib/graphEditor.ts';
import { pipelineToHCL } from '../src/lib/hcl.ts';

const fixture = () => ({
  name: 'typed', entry: 'start', timeout_ms: 1000, concurrency: 1,
  nodes: {
    start: { name: 'start', kind: 'input', attrs: {kind:'input'} },
    fetch: { name: 'fetch', kind: 'mcp.call', attrs: {
      kind:'mcp.call', output_ports:{structured:{type:'object'},content:{type:'array'}}
    } },
    consumer: { name:'consumer', kind:'transform', attrs: {
      kind:'transform', value:'input', input_ports:{
        customer:{type:'object'}, order:{type:'object'}
      }
    } }
  },
  edges: [{from:'start',to:'fetch'},{from:'fetch',to:'consumer',when:'always'}]
});

test('create typed data edge beside existing control dependency and serialize HCL', () => {
  const original = fixture();
  const next = editGraphEdge(original,-1,{
    from:'fetch',to:'consumer',kind:'data',from_port:'structured',to_port:'customer'
  });
  assert.deepEqual(original.edges,fixture().edges);
  assert.equal(next.edges.length,3);
  assert.deepEqual(next.edges[1],original.edges[1]);
  assert.equal(next.edges[2].kind,'data');
  assert.match(pipelineToHCL(next),/to_port = "customer"/);
  assert.match(pipelineToHCL(next),/from_port = "structured"/);
});

test('editing one typed edge leaves all other edge semantics intact', () => {
  const orig = editGraphEdge(fixture(),-1,{
    from:'fetch',to:'consumer',kind:'data',from_port:'structured',to_port:'customer'
  });
  const next = editGraphEdge(orig,2,{
    from:'fetch',to:'consumer',kind:'data',from_port:'structured',to_port:'order'
  });
  assert.deepEqual(next.edges.slice(0,2),orig.edges.slice(0,2));
  assert.equal(next.edges[2].to_port,'order');
  assert.equal(orig.edges[2].to_port,'customer');
  assert.deepEqual(removeGraphEdge(next,2).edges,fixture().edges);
});

test('refuse untyped, duplicate and unsupported producer/consumer binding', () => {
  const p = editGraphEdge(fixture(),-1,{
    from:'fetch',to:'consumer',kind:'data',from_port:'structured',to_port:'customer'
  });
  for (const proposed of [
    {from:'fetch',to:'consumer',kind:'data',to_port:'unknown'},
    {from:'fetch',to:'consumer',kind:'data',to_port:'bad-name'},
    {from:'start',to:'consumer',kind:'data',from_port:'structured',to_port:'order'},
    {from:'fetch',to:'consumer',kind:'data',from_port:'nope',to_port:'order'},
    {from:'fetch',to:'consumer',kind:'data',from_port:'structured',to_port:'order',when:'always'},
    {from:'fetch',to:'consumer',kind:'data',to_port:'customer'}
  ]) assert.throws(() => editGraphEdge(p,-1,proposed));
});

test('control edits keep guards; kind conversion and duplicate control are rejected', () => {
  const orig = fixture();
  const guarded = editGraphEdge(orig,1,{from:'fetch',to:'consumer',when:'approved'});
  assert.equal(guarded.edges[1].when,'approved');
  assert.equal(orig.edges[1].when,'always');
  assert.throws(() => editGraphEdge(orig,1,{
    from:'fetch',to:'consumer',kind:'data',to_port:'customer'
  }),/kind/);
  assert.throws(() => editGraphEdge(orig,-1,{from:'fetch',to:'consumer'}),/already exists/);
  assert.throws(() => editGraphEdge(orig,2,{from:'start',to:'consumer'}),/no longer exists/);
});
