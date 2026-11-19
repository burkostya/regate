import { defineConfig, devices } from '@playwright/test';

const composedURL = process.env.REGATE_E2E_COMPOSED_URL;
const executablePath = process.env.REGATE_E2E_CHROMIUM_EXECUTABLE;

export default defineConfig({
  testDir: './tests/e2e',
  testIgnore: composedURL ? [] : '**/v1-acceptance.spec.ts',
  timeout: 60_000,
  expect: { timeout: 12_000 },
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: composedURL || 'http://127.0.0.1:5173',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure'
  },
  projects: [{
    name: 'chromium',
    use: { ...devices['Desktop Chrome'], launchOptions: { executablePath } }
  }]
});
