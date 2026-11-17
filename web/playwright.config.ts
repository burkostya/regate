import { defineConfig, devices } from '@playwright/test';
import { join } from 'node:path';
import { tmpdir } from 'node:os';

const backendPort = 8180;
const webPort = 5174;
const token = process.env.REGATE_E2E_ADMIN_TOKEN ?? 'regate-browser-e2e-admin';
const stateDir = join(tmpdir(), `regate-browser-e2e-${process.pid}`);

// Playwright starts the actual Go gateway and the SvelteKit console.
// No intercepted/mock API calls: the test exercises bearer enforcement,
// real SQLite draft/revision storage, graph execution and visual controls.
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: `http://127.0.0.1:${webPort}`,
    ...devices['Desktop Chrome'],
    trace: 'retain-on-failure'
  },
  webServer: [
    {
      command: `cd .. && go run ./cmd/regate -config web/e2e/fixtures/gateway.hcl -state ${stateDir} -listen 127.0.0.1:${backendPort}`,
      url: `http://127.0.0.1:${backendPort}/healthz`,
      env: { REGATE_ADMIN_TOKEN: token },
      reuseExistingServer: false,
      timeout: 120_000
    },
    {
      command: `npm run dev -- --port ${webPort} --strictPort`,
      url: `http://127.0.0.1:${webPort}`,
      env: { REGATE_PROXY: `http://127.0.0.1:${backendPort}` },
      reuseExistingServer: false,
      timeout: 120_000
    }
  ]
});
