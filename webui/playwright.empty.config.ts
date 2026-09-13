// Empty-state variant of the main config: the same V-1 capture rules, but
// the fixture server runs with -seed=false, so every route renders its
// honest empty state (plan §9: never fabricated metrics). Desktop only —
// empty states are content tests, not layout tests.

import { defineConfig, devices } from '@playwright/test';

const PORT = Number(process.env.PROXYLLM_E2E_PORT || 4174);

export default defineConfig({
  testDir: './e2e/empty',
  outputDir: './e2e/.artifacts-empty',
  timeout: 30_000,
  expect: {
    toHaveScreenshot: {
      maxDiffPixelRatio: 0.001,
      animations: 'disabled',
      caret: 'hide',
      scale: 'css',
    },
  },
  snapshotPathTemplate:
    '{snapshotDir}/{testFileDir}/__screenshots__/{testName}/{projectName}-{arg}{ext}',
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },
  projects: [
    {
      name: 'desktop-chrome',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 720 } },
    },
  ],
  webServer: {
    command: `./e2e/fixturesrv-bin${process.platform === 'win32' ? '.exe' : ''} -addr 127.0.0.1:${PORT} -seed=false`,
    url: `http://127.0.0.1:${PORT}/healthz`,
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'ignore',
    stderr: 'pipe',
  },
});