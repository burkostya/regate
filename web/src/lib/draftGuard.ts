import type { Draft } from './api';

export type GraphSourceSnapshot = { revision: string; source: string };

// The visual graph represents the active revision, not arbitrary HCL edits
// saved by another editor. Refuse to overwrite a modified draft with an
// outdated visual projection; operators can activate/reload or edit in HCL.
export function assertGraphSnapshot(
  draft: Pick<Draft, 'base' | 'source'>,
  snapshot: GraphSourceSnapshot,
  activeRevision: string,
  editorText: string
): void {
  if (draft.base !== activeRevision || snapshot.revision !== activeRevision) {
    throw new Error('The active revision changed. Reload the graph and start a fresh draft.');
  }
  if (draft.source !== snapshot.source) {
    throw new Error('Draft HCL differs from the graph snapshot. Finish or activate the draft before graphical editing.');
  }
  if (editorText !== draft.source) {
    throw new Error('Unsaved HCL edits exist. Save or discard them before graphical editing.');
  }
}

export function assertSavedHCL(draft: Pick<Draft, 'source'>, editorText: string): void {
  if (draft.source !== editorText) {
    throw new Error('Unsaved HCL edits exist. Save the draft before activation.');
  }
}
