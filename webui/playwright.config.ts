// Playwright visual regression configuration (Phase 2 research §7).
//
// V-1: toHaveScreenshot with maxDiffPixelRatio 0.001, animations disabled,
//      caret hidden, CSS-pixel scaling.
// V-3: one project per viewport — Desktop Chrome 1280x720 and iPhone 13
//      390x844 (DPR 3) — each with its own baseline directory, so a phone
//      layout break never diffs against a desktop baseline.
// V-4: the fixture server is a real PROxyLLM assembly (webui/e2e/fixturesrv)
//      built by the webServer command; baselines are captured in CI only —
//      never locally (OS rendering differences would poison the baselines).

import { defineConfig, devices } from '@playwright/test';

const PORT = Number(process.env.PROXYLLM_E2E_PORT || 4173);
const BASE_URL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './e2e',
  // The empty-state specs live in e2e/empty but belong to the separate
  // playwright.empty.config.ts run (different port, -seed=false server).
  testIgnore: '**/empty/**',
  outputDir: './e2e/.artifacts',
  timeout: 30_000,
  expect: {
    // V-1: screenshots must match within 0.1% of pixels.
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
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },
  projects: [
    {
      name: 'desktop-chrome',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 720 } },
    },
    {
      name: 'iphone-13',
      use: { ...devices['iPhone 13'], viewport: { width: 390, height: 844 } },
    },
  ],
  webServer: {
    // The pre-built binary (CI builds fixturesrv-bin first; locally
    // `go build -o webui/e2e/fixturesrv-bin ./webui/e2e/fixturesrv`).
    command: `./e2e/fixturesrv-bin${process.platform === 'win32' ? '.exe' : ''} -addr 127.0.0.1:${PORT}`,
    url: `http://127.0.0.1:${PORT}/healthz`,
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'ignore',
    stderr: 'pipe',
  },
});