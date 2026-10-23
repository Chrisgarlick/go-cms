import { defineConfig } from '@playwright/test';
export default defineConfig({
  testDir: './tests',
  timeout: 60_000,
  workers: 1, // tests share one dev site; zz-passkeys runs last because it turns on 2FA
  retries: process.env.CI ? 1 : 0,
  use: { baseURL: process.env.CMS_BASE_URL || 'http://localhost:8080', trace: 'retain-on-failure' },
});
