import { expect, test } from '@playwright/test';

test('publishes and rolls back the composed graph while retaining unsupported HCL', async ({ page, request }) => {
  const token = process.env.REGATE_E2E_ADMIN_TOKEN;
  expect(token).toBeTruthy();
  const headers = { Authorization: `Bearer ${token}` };
  const configuration = async () => {
    const response = await request.get('/api/config', { headers });
    expect(response.status()).toBe(200);
    return response.json();
  };
  const postFrom = async (button: string, endpoint: string) => {
    const response = page.waitForResponse(r => r.url().endsWith(`/api/${endpoint}`) && r.request().method() === 'POST');
    await page.getByRole('button', { name: button, exact: true }).click();
    const result = await response;
    expect(result.status(), await result.text()).toBe(200);
    return result.json();
  };
  const tab = (name: string) => page.locator('.sidebar nav').getByRole('button', { name, exact: true }).click();
  const editLabel = async (value: string) => {
    await page.locator('.palette .node-item').filter({ hasText: /^labeltransform$/ }).click();
    const attrs = page.locator('#node-attributes');
    await attrs.fill(JSON.stringify({ ...JSON.parse(await attrs.inputValue()), value }, null, 2));
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
  };

  const baseline = await configuration();
  expect(baseline.source).toContain('pipeline "publish_summary"');
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-regate-hydrated', 'true');
  await page.getByLabel('Admin bearer token').fill(token!);
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Execution graph' })).toBeVisible();
  const label = 'literal:Browser summary for ';
  await editLabel(label);
  const draft = await postFrom('Save graph to draft', 'graph-patch');
  expect(draft.source).toContain(label);
  expect(draft.source).toContain('to_port = "agent"');

  await page.locator('.split-pane textarea').fill(JSON.stringify({ record_id: 'preview', lose_reply: false }));
  const dry = await postFrom('Dry-run', 'test');
  expect(dry.status).toBe('success');
  expect(dry.value).toEqual({ record_id: 'preview' });
  await tab('HCL');
  expect((await postFrom('Validate', 'validate')).valid).toBe(true);
  const diff = await postFrom('Diff', 'diff');
  expect(diff.conflict).toBe(false);
  expect(diff.semantic_changes).toContainEqual(expect.objectContaining({
    path: 'pipelines.publish_summary.nodes.label.attrs.value', after: label
  }));

  page.once('dialog', dialog => dialog.accept());
  await postFrom('Activate', 'activate');
  const activated = await configuration();
  expect(activated.source).toBe(draft.source);
  expect(activated.revision).not.toBe(baseline.revision);
  await tab('Revisions');
  const original = page.locator('.revision').filter({ hasText: baseline.revision });
  await expect(original).toBeVisible();
  page.once('dialog', dialog => dialog.accept());
  const rollback = page.waitForResponse(r => r.url().endsWith('/api/rollback'));
  await original.getByRole('button', { name: 'Restore' }).click();
  expect((await rollback).status()).toBe(200);
  const restored = await configuration();
  expect(restored.source).toBe(baseline.source);
  expect(restored.revision).not.toBe(activated.revision);

  await tab('HCL');
  await page.getByRole('button', { name: 'Load active', exact: true }).click();
  const opaque = baseline.source.replace('node "label" {', 'node "label" {\n    vendor_extension = acceptance.extra');
  expect(opaque).not.toBe(baseline.source);
  await page.locator('#hcl-source').fill(opaque);
  const opaqueDraft = await postFrom('Save draft', 'draft-update');
  expect(opaqueDraft.source).toBe(opaque);
  expect((await postFrom('Validate', 'validate')).valid).toBe(false);
  // Activate itself revalidates and leaves the saved source and revision alone.
  expect((await postFrom('Activate', 'validate')).valid).toBe(false);
  await tab('Pipelines');
  await editLabel(label);
  await page.getByRole('button', { name: 'Save graph to draft' }).click();
  await expect(page.locator('.toast.error')).toContainText('Draft HCL differs from the graph snapshot');
  const persisted = await request.get(`/api/drafts/${opaqueDraft.id}`, { headers });
  expect((await persisted.json()).source).toBe(opaque);
  expect(await configuration()).toEqual(restored);
});
