// Empty-state screenshots: every data route against an unseeded store —
// the honest empty states from plan §9 ("never fabricated metrics").
// Desktop only (content contract, not layout).

import { test } from '@playwright/test';
import { freezeTime, login, readyForCapture } from '../helpers';

test.beforeEach(async ({ page }) => {
  await freezeTime(page);
  await login(page);
});

for (const [route, name] of [
  ['/', 'overview'],
  ['/quota', 'quota'],
  ['/providers', 'providers'],
  ['/models', 'models'],
  ['/routing', 'routing'],
  ['/logs', 'logs'],
  ['/keys', 'keys'],
  ['/setup', 'setup'],
] as const) {
  test(`empty ${name}`, async ({ page }) => {
    await page.goto(route);
    await readyForCapture(page);
    await test.expect(page).toHaveScreenshot(`empty-${name}.png`);
  });
}