import { test, expect } from '@playwright/test';

const token = process.env.REGATE_E2E_ADMIN_TOKEN || 'regate-e2e-test-token';
const auth = { Authorization: `Bearer ${token}` };

test('real console edits typed port graph, validates HCL and rejects stale draft writes', async ({ page, request }) => {
  const beforeResponse = await request.get('/api/status', { headers: auth });
  expect(beforeResponse.ok()).toBeTruthy();
  const before = await beforeResponse.json();
  const initialConfigurationResponse = await request.get('/api/config', { headers: auth });
  expect(initialConfigurationResponse.ok()).toBeTruthy();
  const originalConfiguration = await initialConfigurationResponse.json();

  await page.goto('/');
  // A server-rendered form is not interactive until Svelte attaches handlers.
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await expect(page.getByRole('heading', { name: 'Connect to Regate' })).toBeVisible();
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();

  // A real node inspector edit declares the target port before creating
  // the corresponding edge. It must not erase the existing control flow.
  await page.locator('.palette .node-item').filter({ hasText: /^mask/ }).first().click();
  const attrsInput = page.locator('#node-attributes');
  const attrs = JSON.parse(await attrsInput.inputValue());
  attrs.input_ports = { customer: { type: 'object' } };
  await attrsInput.fill(JSON.stringify(attrs, null, 2));
  await page.getByRole('button', { name: 'Apply', exact: true }).click();

  await page.getByRole('button', { name: /Bind data port/ }).click();
  await page.locator('#edge-from').selectOption('fetch');
  await page.locator('#edge-to').selectOption('mask');
  await page.locator('#edge-from-port').fill('value');
  await page.locator('#edge-to-port').fill('customer');
  await page.getByRole('button', { name: 'Apply edge' }).click();
  await expect(page.locator('.palette')).toContainText('data: value → customer');

  const graphSaved = page.waitForResponse(response =>
    response.url().endsWith('/api/graph-patch') && response.request().method() === 'POST'
  );
  await page.getByRole('button', { name: 'Save graph to draft' }).click();
  const graphResponse = await graphSaved;
  expect(graphResponse.status(), await graphResponse.text()).toBe(200);
  const draft = await graphResponse.json();
  expect(draft.source_digest).toMatch(/^[0-9a-f]{64}$/);
  expect(draft.source).toContain('to_port = "customer"');
  expect(draft.source).toContain('kind = "data"');
  expect(draft.source).toContain('input_ports');

  await page.locator('.sidebar nav').getByRole('button', { name: 'HCL', exact: true }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(/to_port = "customer"/);

  const validateResponse = page.waitForResponse(response => response.url().endsWith('/api/validate'));
  await page.getByRole('button', { name: 'Validate', exact: true }).click();
  const validation = await validateResponse;
  expect(validation.status(), await validation.text()).toBe(200);
  expect((await validation.json()).valid).toBe(true);

  // Another authenticated operator edits the same draft while the browser
  // still holds its old SHA-256 precondition.
  const concurrentSource = draft.source + '\n# independent administrative edit\n';
  const other = await request.post('/api/draft-update', {
    headers: auth,
    data: { draft_id: draft.id, source: concurrentSource, expected_source_digest: draft.source_digest }
  });
  expect(other.status(), await other.text()).toBe(200);
  expect((await other.json()).source_digest).not.toBe(draft.source_digest);

  const staleSave = page.waitForResponse(response =>
    response.url().endsWith('/api/draft-update') && response.request().method() === 'POST'
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const rejected = await staleSave;
  expect(rejected.status(), await rejected.text()).toBe(400);
  await expect(page.locator('.toast.error')).toContainText('draft source conflict');

  const afterDraft = await request.get(`/api/drafts/${draft.id}`, { headers: auth });
  expect(afterDraft.status()).toBe(200);
  expect((await afterDraft.json()).source).toBe(concurrentSource);
  const afterStatus = await request.get('/api/status', { headers: auth });
  expect((await afterStatus.json()).revision).toBe(before.revision);

  // A stale browser is recoverable without reloading the whole application.
  // Declining the discard prompt must preserve the operator's unsaved notes.
  const localUnsaved = draft.source + '\n# local unsaved review\n';
  await page.locator('#hcl-source').fill(localUnsaved);
  page.once('dialog', dialog => dialog.dismiss());
  await page.getByRole('button', { name: 'Reload draft' }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(localUnsaved);

  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'Reload draft' }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(concurrentSource);

  // The recovered editor has the new source digest: it can safely continue
  // editing from the winner's snapshot without overwriting the other edit.
  const resumedSource = concurrentSource + '\n# resumed browser edit\n';
  await page.locator('#hcl-source').fill(resumedSource);
  const retrySave = page.waitForResponse(response =>
    response.url().endsWith('/api/draft-update') && response.request().method() === 'POST'
  );
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  const accepted = await retrySave;
  expect(accepted.status(), await accepted.text()).toBe(200);
  expect((await accepted.json()).source).toBe(resumedSource);

  const finalDraft = await request.get(`/api/drafts/${draft.id}`, { headers: auth });
  expect((await finalDraft.json()).source).toBe(resumedSource);
  const finalStatus = await request.get('/api/status', { headers: auth });
  expect((await finalStatus.json()).revision).toBe(before.revision);

  // "Load active" is destructive only to the local editor preview and must
  // not silently erase unsaved HCL or the durable concurrent draft.
  const scratch = resumedSource + '\n# temporary browser scratch\n';
  await page.locator('#hcl-source').fill(scratch);
  page.once('dialog', dialog => dialog.dismiss());
  await page.getByRole('button', { name: 'Load active', exact: true }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(scratch);
  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'Load active', exact: true }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(originalConfiguration.source);
  const preserved = await request.get(`/api/drafts/${draft.id}`, { headers: auth });
  expect((await preserved.json()).source).toBe(resumedSource);
  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'Reload draft' }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(resumedSource);

  // Only explicitly disposable CI environments opt into mutating the live
  // revision. A local Playwright run against a user-managed gateway never
  // activates or rolls back by default.
  if (process.env.REGATE_E2E_ALLOW_ACTIVATION !== 'true') return;
  expect(before.listen).toMatch(/^127\.0\.0\.1:/);

  page.once('dialog', dialog => dialog.accept());
  const activateResponse = page.waitForResponse(response =>
    response.url().endsWith('/api/activate') && response.request().method() === 'POST'
  );
  await page.getByRole('button', { name: 'Activate', exact: true }).click();
  const activated = await activateResponse;
  expect(activated.status(), await activated.text()).toBe(200);
  const activatedConfigurationResponse = await request.get('/api/config', { headers: auth });
  const activatedConfiguration = await activatedConfigurationResponse.json();
  expect(activatedConfiguration.revision).not.toBe(before.revision);
  expect(activatedConfiguration.source).toBe(resumedSource);

  // The UI must restore a previous immutable source as a NEW revision,
  // rather than rewinding the revision counter or restoring only the view.
  await page.locator('.sidebar nav').getByRole('button', { name: 'Revisions', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Immutable revisions' })).toBeVisible();
  const originalRow = page.locator('.revision').filter({ hasText: before.revision });
  await expect(originalRow).toBeVisible();
  page.once('dialog', dialog => dialog.accept());
  const rollbackResponse = page.waitForResponse(response =>
    response.url().endsWith('/api/rollback') && response.request().method() === 'POST'
  );
  await originalRow.getByRole('button', { name: 'Restore' }).click();
  const rolledBack = await rollbackResponse;
  expect(rolledBack.status(), await rolledBack.text()).toBe(200);
  const restoredResponse = await request.get('/api/config', { headers: auth });
  const restored = await restoredResponse.json();
  expect(restored.source).toBe(originalConfiguration.source);
  expect(restored.revision).not.toBe(before.revision);
  expect(restored.revision).not.toBe(activatedConfiguration.revision);
});

test('trace filter works while pipeline graph has unsaved changes', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();

  // Keep an intentionally dirty graph to prove that read-only trace
  // filtering never calls the destructive full refresh() path.
  await page.locator('.palette .node-item').filter({ hasText: /^mask/ }).first().click();
  const editor = page.locator('#node-attributes');
  const original = JSON.parse(await editor.inputValue());
  original.unsaved_trace_test_marker = 'do-not-discard';
  await editor.fill(JSON.stringify(original, null, 2));
  await page.getByRole('button', { name: 'Apply', exact: true }).click();

  await page.locator('.sidebar nav').getByRole('button', { name: 'Traces', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution traces' })).toBeVisible();
  await page.locator('#trace-run').fill('non-existent-run');
  await page.locator('#trace-status').selectOption('failure');
  await page.locator('#trace-limit').selectOption('25');
  const filteredResponse = page.waitForResponse(response =>
    response.url().includes('/api/traces?') && response.url().includes('status=failure')
  );
  await page.getByRole('button', { name: 'Filter traces' }).click();
  const filtered = await filteredResponse;
  expect(filtered.status(), await filtered.text()).toBe(200);
  expect(await filtered.json()).toEqual([]);
  await expect(page.getByText('No matching trace events.')).toBeVisible();
  await expect(page.locator('.toast.error')).toHaveCount(0);

  await page.locator('.sidebar nav').getByRole('button', { name: 'Pipelines', exact: true }).click();
  await expect(page.locator('#node-attributes')).toHaveValue(/do-not-discard/);
});

test('execution buttons fail closed on unsaved graph and HCL changes', async ({ page }) => {
  const executionRequests: string[] = [];
  const dialogs: string[] = [];
  page.on('request', r => {
    if (/\/api\/(test|run)$/.test(new URL(r.url()).pathname)) executionRequests.push(r.url());
  });
  page.on('dialog', async dialog => {
    dialogs.push(dialog.message());
    await dialog.accept();
  });
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();

  await page.locator('.palette .node-item').filter({ hasText: /^mask/ }).first().click();
  const nodeInput = page.locator('#node-attributes');
  const attrs = JSON.parse(await nodeInput.inputValue());
  attrs.pending_change = 'not-saved';
  await nodeInput.fill(JSON.stringify(attrs));
  await page.getByRole('button', { name: 'Apply', exact: true }).click();

  await page.getByRole('button', { name: 'Dry-run' }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved graph edits');
  await page.getByRole('button', { name: 'Live run' }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved graph edits');
  expect(executionRequests).toEqual([]);
  expect(dialogs).toEqual([]);

  await page.reload();
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();
  await page.getByRole('button', { name: 'New draft' }).click();
  await page.locator('.sidebar nav').getByRole('button', { name: 'HCL', exact: true }).click();
  await page.locator('#hcl-source').fill((await page.locator('#hcl-source').inputValue()) + '\n# unsaved local text\n');
  await page.locator('.sidebar nav').getByRole('button', { name: 'Pipelines', exact: true }).click();
  await page.getByRole('button', { name: 'Dry-run' }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved HCL edits');
  await page.getByRole('button', { name: 'Live run' }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved HCL edits');
  expect(executionRequests).toEqual([]);
  expect(dialogs).toEqual([]);
});

test('live console execution identifies and pins the active revision', async ({ page, request }) => {
  const statusResponse = await request.get('/api/status', { headers: auth });
  expect(statusResponse.ok()).toBeTruthy();
  const status = await statusResponse.json();

  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();

  let approval = '';
  page.once('dialog', async dialog => {
    approval = dialog.message();
    await dialog.accept();
  });
  const execution = page.waitForResponse(r => r.url().endsWith('/api/run') && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Live run' }).click();
  const response = await execution;
  expect(response.status(), await response.text()).toBe(200);
  expect(approval).toContain('ACTIVE revision');
  expect(approval).toContain('support.summarize_customer');
  expect(approval).toContain(status.revision.slice(0, 12));
  const submitted = response.request().postDataJSON();
  expect(submitted.expected_revision).toBe(status.revision);
  expect(submitted.draft_id).toBeUndefined();
});

test('production console proxy requires authentication and validation targets visible HCL', async ({ page, request }) => {
  const untrusted = await request.get('/api/status');
  expect(untrusted.status()).toBe(401);
  const trusted = await request.get('/api/status', { headers: auth });
  expect(trusted.status()).toBe(200);

  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();

  await page.locator('.sidebar nav').getByRole('button', { name: 'HCL', exact: true }).click();
  const localSource = 'pipeline "incomplete" {';
  await page.locator('#hcl-source').fill(localSource);
  const validationRequest = page.waitForRequest(r =>
    r.url().endsWith('/api/validate') && r.method() === 'POST'
  );
  await page.getByRole('button', { name: 'Validate', exact: true }).click();
  const requestWithSource = await validationRequest;
  expect(requestWithSource.postDataJSON().source).toBe(localSource);
});

test('activation refuses unsaved graphical edits before prompting or writing', async ({ page }) => {
  const writes: string[] = [];
  const dialogs: string[] = [];
  page.on('request', r => {
    if (/\/api\/(activate|rollback)$/.test(new URL(r.url()).pathname)) writes.push(r.url());
  });
  page.on('dialog', async dialog => { dialogs.push(dialog.message()); await dialog.accept(); });

  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();
  await page.getByRole('button', { name: 'New draft' }).click();
  await page.locator('.palette .node-item').filter({ hasText: /^mask/ }).first().click();
  const node = page.locator('#node-attributes');
  const attrs = JSON.parse(await node.inputValue());
  attrs.unsaved_activation_marker = 'must-not-discard';
  await node.fill(JSON.stringify(attrs));
  await page.getByRole('button', { name: 'Apply', exact: true }).click();
  await page.locator('.sidebar nav').getByRole('button', { name: 'HCL', exact: true }).click();
  await page.getByRole('button', { name: 'Activate', exact: true }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved graph edits');
  expect(writes).toEqual([]);
  expect(dialogs).toEqual([]);
  await page.locator('.sidebar nav').getByRole('button', { name: 'Pipelines', exact: true }).click();
  await expect(page.locator('#node-attributes')).toHaveValue(/must-not-discard/);
});

test('diff never creates a draft over unsaved HCL, and cleared HCL is not replaced by old source', async ({ page }) => {
  const draftRequests: string[] = [];
  page.on('request', r => {
    if (/\/api\/drafts$/.test(new URL(r.url()).pathname) && r.method() === 'POST') {
      draftRequests.push(r.url());
    }
  });

  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();
  await page.locator('.sidebar nav').getByRole('button', { name: 'HCL', exact: true }).click();
  await page.locator('#hcl-source').fill('# local unsaved changes\n');
  await page.getByRole('button', { name: 'Diff', exact: true }).click();
  await expect(page.locator('.toast.error')).toContainText('Unsaved HCL edits');
  expect(draftRequests).toHaveLength(0);
  await expect(page.locator('#hcl-source')).toHaveValue('# local unsaved changes\n');

  page.once('dialog', dialog => dialog.accept());
  await page.getByRole('button', { name: 'New draft' }).click();
  await expect(page.locator('#hcl-source')).not.toHaveValue('');
  await page.locator('#hcl-source').fill('');
  const update = page.waitForRequest(r => r.url().endsWith('/api/draft-update') && r.method() === 'POST');
  const result = page.waitForResponse(r => r.url().endsWith('/api/draft-update') && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  expect((await update).postDataJSON().source).toBe('');
  await result;
  await expect(page.locator('#hcl-source')).toHaveValue('');
});
