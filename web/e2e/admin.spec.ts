import { expect, test } from '@playwright/test';

// A single real-browser journey keeps the relative sequence of revisions
// deterministic. No remote SaaS credentials and no test-network proxy.
const token = process.env.REGATE_E2E_ADMIN_TOKEN ?? 'regate-browser-e2e-admin';
const headers = { Authorization: `Bearer ${token}` };

test('browser console: authenticated draft, validation, dry-run, activation and rollback', async ({ page, request }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Connect to Regate' })).toBeVisible();

  // Read-only unauthenticated browser must not enumerate the control plane.
  const denied = await request.get('http://127.0.0.1:8180/api/catalog');
  expect(denied.status()).toBe(401);
  await page.getByLabel('Admin bearer token').fill('invalid-e2e-token');
  await page.getByRole('button', { name: 'Connect' }).click();
  await expect(page.getByRole('alert')).toContainText('HTTP 401');
  await expect(page.getByRole('heading', { name: 'Connect to Regate' })).toBeVisible();

  await page.getByLabel('Admin bearer token').fill(token);
  await page.getByRole('button', { name: 'Connect' }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Choose pipeline' })).toHaveValue('echo');
  await expect(page.getByRole('status')).toContainText('Connected to Regate');

  // The bearer remains in tab memory, never browser storage.
  expect(await page.evaluate(() => localStorage.length + sessionStorage.length)).toBe(0);

  await page.getByRole('button', { name: 'Dry-run' }).click();
  await expect(page.getByText('Last execution').locator('..').locator('pre')).toContainText('C123');

  const initialResponse = await request.get('http://127.0.0.1:8180/api/status', { headers });
  expect(initialResponse.ok()).toBeTruthy();
  const initial = await initialResponse.json();

  await page.getByRole('button', { name: 'HCL', exact: true }).click();
  await page.getByRole('button', { name: 'Load active' }).click();
  await expect(page.locator('#hcl-source')).toHaveValue(/pipeline "echo"/);
  const original = await page.locator('#hcl-source').inputValue();

  await page.getByRole('button', { name: 'New draft' }).click();
  await page.locator('#hcl-source').fill(original + '\n# browser-e2e validated revision\n');
  await page.getByRole('button', { name: 'Save draft' }).click();
  await expect(page.getByRole('status')).toContainText('HCL validated and saved in draft');

  await page.getByRole('button', { name: 'Diff' }).click();
  await expect(page.getByText('Validation / Impact')).toBeVisible();
  await page.getByRole('button', { name: 'Validate', exact: true }).click();
  await expect(page.getByRole('status')).toContainText('Configuration validated');

  page.once('dialog', async d => { expect(d.type()).toBe('confirm'); await d.accept(); });
  await page.getByRole('button', { name: 'Activate' }).click();
  await expect(page.getByRole('status')).toContainText('New revision activated');

  const afterActivationResponse = await request.get('http://127.0.0.1:8180/api/status', { headers });
  const activated = await afterActivationResponse.json();
  expect(activated.revision).not.toBe(initial.revision);
  const activatedConfig = await (await request.get('http://127.0.0.1:8180/api/config', { headers })).json();
  expect(activatedConfig.source).toContain('# browser-e2e validated revision');

  await page.getByRole('button', { name: 'Revisions' }).click();
  await expect(page.getByRole('heading', { name: 'Immutable revisions' })).toBeVisible();
  page.once('dialog', async d => { expect(d.type()).toBe('confirm'); await d.accept(); });
  await page.getByRole('button', { name: 'Restore' }).first().click();
  await expect(page.getByRole('status')).toContainText('Rollback activated');

  const rolledBack = await (await request.get('http://127.0.0.1:8180/api/config', { headers })).json();
  expect(rolledBack.source).toBe(original);
  expect(rolledBack.revision).not.toBe(activated.revision);

  // After a valid management cycle the gateway still serves the original
  // executable MCP surface, rather than leaking the editor draft.
  await page.getByRole('button', { name: 'Surfaces' }).click();
  await expect(page.getByRole('heading', { name: 'MCP surfaces' })).toBeVisible();
  await expect(page.getByText('pipeline: echo')).toBeVisible();
});
