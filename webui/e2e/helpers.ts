// Shared helpers for the visual regression suite.
//
// V-2: dynamic content is masked or frozen — KPI values and any
//      timestamp-derived text vary with the seeded data's relative days, so
//      they are masked in screenshots and frozen via a deterministic Date.
// V-7: document.fonts.ready is awaited before every capture so a
//      self-hosted font swap never lands mid-screenshot.

import { Page, expect, Locator } from '@playwright/test';

// Fixed instant: 2026-09-13T12:00:00Z — before app scripts run, so every
// Date()/Date.now() in the SPA resolves to this. The seeded usage window
// (last 14 days) and log timestamps stay inside the rendered ranges.
export const FROZEN_DATE = '2026-09-13T12:00:00Z';

// Deterministic credentials — fixturesrv seeds exactly this admin.
export const FIXTURE_ADMIN = { username: 'admin', password: 'proxyllm-e2e' };

/** Freezes Date for the page before any app code runs (V-2). */
export async function freezeTime(page: Page): Promise<void> {
  await page.addInitScript(
    ([iso]: [string]) => {
      const fixed = Date.parse(iso);
      const RealDate = Date;
      class MockDate extends RealDate {
        constructor(...args: [] | [string | number | Date]) {
          if (args.length === 0) {
            super(fixed);
          } else {
            super(args[0]);
          }
        }
        static now(): number {
          return fixed;
        }
      }
      MockDate.parse = RealDate.parse;
      MockDate.UTC = RealDate.UTC;
      // window.Date must be replaced wholesale; the SPA imports nothing else.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (window as any).Date = MockDate;
    },
    [FROZEN_DATE] as [string],
  );
}

/** Signs in through the real login form and waits for the shell. */
export async function login(page: Page): Promise<void> {
  await page.goto('/login');
  await page.getByLabel('Username').fill(FIXTURE_ADMIN.username);
  await page.getByLabel('Password').fill(FIXTURE_ADMIN.password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  // The shell's sidebar marks a signed-in session.
  await expect(page.locator('.sidebar-nav')).toBeVisible();
}

/**
 * Ready-for-capture state: fonts loaded (V-7) and no pending route
 * transitions. Called immediately before toHaveScreenshot.
 */
export async function readyForCapture(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  // Route-level loading states render as EmptyState (.empty, title
  // "Loading …"); .loading is the lazy-chunk boundary. Both must be gone.
  await expect(page.locator('.loading, .empty:has-text("Loading")')).toHaveCount(0);
}

/** KPI numbers depend on the seeded day window — mask them (V-2). */
export function kpiMasks(page: Page): Locator[] {
  return [
    page.locator('.kpi-value'),
    page.locator('.bar-val'),
    page.locator('.spark'),
    page.locator('.ring'),
    // Quota usage windows: the remaining percent/value and the live timer
    // are data-derived, so they are masked like the other KPI readings.
    page.locator('.usage-ring-num'),
    page.locator('.usage-left'),
    page.locator('.usage-reset'),
  ];
}