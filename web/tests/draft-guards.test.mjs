import { test } from 'node:test';
import assert from 'node:assert/strict';
import { assertGraphSnapshot, assertSavedHCL, assertNoPendingEdits, assertRunnableEditor } from '../src/lib/draftGuard.ts';

const graph = { revision: 'revision-1', source: 'pipeline "one" {}' };
const draft = { base: 'revision-1', source: graph.source };

test('unaltered draft can save a graph on its own active revision', () => {
  assert.doesNotThrow(() => assertGraphSnapshot(draft, graph, 'revision-1', draft.source));
});

test('a draft edited as HCL is not overwritten by the older graph view', () => {
  assert.throws(() => assertGraphSnapshot(
    { ...draft, source: 'pipeline "one" { entry = "new" }' }, graph, 'revision-1',
    'pipeline "one" { entry = "new" }'
  ), /Draft HCL differs/);
});

test('a new active revision makes the old graph ineligible for editing', () => {
  assert.throws(() => assertGraphSnapshot(draft, graph, 'revision-2', draft.source),
    /revision changed/);
  assert.throws(() => assertGraphSnapshot(
    { ...draft, base: 'revision-0' }, graph, 'revision-1', draft.source
  ), /revision changed/);
});

test('graph changes can be saved twice after advancing the local snapshot', () => {
  const firstSave = 'pipeline "one" { entry = "after_edit" }';
  assert.doesNotThrow(() => assertGraphSnapshot(
    { ...draft, source: firstSave },
    { ...graph, source: firstSave }, 'revision-1', firstSave
  ));
});

test('unsaved HCL text prevents both graph save and activation', () => {
  assert.throws(() => assertGraphSnapshot(draft, graph, 'revision-1', 'unsaved'),
    /Unsaved HCL edits/);
  assert.throws(() => assertSavedHCL(draft, 'unsaved'), /Unsaved HCL edits/);
  assert.doesNotThrow(() => assertSavedHCL(draft, draft.source));
});

test('execution blocks dirty visual graph even if the draft source is saved', () => {
  assert.throws(() => assertRunnableEditor(true, draft, draft.source, graph, graph.revision),
    /Unsaved graph edits/);
});

test('execution blocks locally edited HCL and an out-of-date graph snapshot', () => {
  assert.throws(() => assertRunnableEditor(false, draft, draft.source + '\n# pending', graph, graph.revision),
    /Unsaved HCL edits/);
  assert.throws(() => assertRunnableEditor(false, null, graph.source + '\n# pending', graph, graph.revision),
    /Unsaved HCL edits/);
  assert.throws(() => assertRunnableEditor(false, null, graph.source, graph, 'revision-2'),
    /active revision changed/);
});

test('execution accepts only a clean reviewed graph and saved draft', () => {
  assert.doesNotThrow(() => assertRunnableEditor(false, draft, draft.source, graph, graph.revision));
  assert.doesNotThrow(() => assertRunnableEditor(false, null, '', graph, graph.revision));
});

test('activation and rollback cannot discard dirty graph or unsaved HCL', () => {
  assert.throws(() => assertNoPendingEdits(true, draft, draft.source, graph),
    /Unsaved graph edits/);
  assert.throws(() => assertNoPendingEdits(false, draft, draft.source + '\n# pending', graph),
    /Unsaved HCL edits/);
  assert.throws(() => assertNoPendingEdits(false, null, graph.source + '\n# pending', graph),
    /Unsaved HCL edits/);
  assert.doesNotThrow(() => assertNoPendingEdits(false, draft, draft.source, graph));
  assert.doesNotThrow(() => assertNoPendingEdits(false, null, graph.source, graph));
});
