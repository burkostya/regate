import { test, expect } from '@playwright/test';

const token = process.env.REGATE_E2E_ADMIN_TOKEN || 'regate-e2e-test-token';
const auth = { Authorization: `Bearer ${token}` };

test('real console edits typed port graph, validates HCL and rejects stale draft writes', async ({ page, request }) => {
  const beforeResponse = await request.get('/api/status', { headers: auth });
  expect(beforeResponse.ok()).toBeTruthy();
  const before = await beforeResponse.json();

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
});
