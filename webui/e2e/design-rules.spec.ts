// Design-rule conformance suite (research §7 V-6, plan §9 checklist):
// assertions, not screenshots. Tokens-only colors, no emoji, dark-only
// color-scheme — the automatable subset of the DESIGN.md contract.

import { test, expect } from '@playwright/test';
import { login } from './helpers';

// The token palette from design/UpGuard/colors_and_type.css — the ONLY
// legal color values. Anything else in a computed style means a raw hex
// or off-token color crept in.
const TOKEN_COLORS = new Set([
  'rgb(17, 24, 33)', // bg #111821
  'rgb(23, 31, 43)', // surface
  'rgb(40, 50, 63)',
  'rgb(52, 63, 78)',
  'rgb(96, 108, 122)',
  'rgb(146, 156, 168)',
  'rgb(214, 221, 229)',
  'rgb(233, 237, 241)',
  'rgb(59, 214, 113)', // accent #3bd671
  'rgb(255, 99, 99)',
  'rgb(255, 184, 77)',
  'rgb(255, 255, 255)',
  'rgba(0, 0, 0, 0)',
]);

test.beforeEach(async ({ page }) => {
  // No time freeze needed: rules are content-independent.
  await login(page);
});

test('dark-only color-scheme', async ({ page }) => {
  await page.goto('/');
  const scheme = await page.evaluate(() => {
    const html = document.documentElement;
    return html.style.colorScheme || getComputedStyle(html).colorScheme;
  });
  expect(scheme).toBe('dark');
});

test('tokens-only colors on all routes', async ({ page }) => {
  for (const route of ['/', '/providers', '/models', '/routing', '/logs', '/keys', '/settings']) {
    await page.goto(route);
    await page.waitForLoadState('networkidle');
    const violations = await page.evaluate((tokenList: string[]) => {
      const legal = new Set(tokenList);
      const offenders: string[] = [];
      const all = document.querySelectorAll<HTMLElement>('body *');
      for (const el of all) {
        if (!(el instanceof HTMLElement)) continue;
        const cs = getComputedStyle(el);
        for (const prop of ['color', 'backgroundColor', 'borderTopColor']) {
          const v = cs[prop as 'color'];
          if (v && !legal.has(v)) {
            offenders.push(`${route} ${el.tagName}.${el.className} ${prop}=${v}`);
          }
        }
      }
      return offenders;
    }, [...TOKEN_COLORS]);

    expect(violations, `off-token colors on ${route}`).toEqual([]);
  }
});

test('no emoji in rendered output', async ({ page }) => {
  for (const route of ['/', '/providers', '/models', '/routing', '/logs', '/keys', '/settings']) {
    await page.goto(route);
    await page.waitForLoadState('networkidle');
    const emoji = await page.evaluate(() => {
      // Emoji coverage: the main plane blocks + variation selector + flag
      // sequences. Inline SVG icons are the sanctioned alternative.
      // ️ must live in its own regex — combined with ranges in one
      // character class it is a no-misleading-character-class violation.
      const astral = /[\u{1F300}-\u{1FAFF}\u{1F1E6}-\u{1F1FF}]/u;
      const bmp = /[☀-➿]/;
      const variationSelector = /️/;
      const hits: string[] = [];
      for (const el of document.querySelectorAll('body *')) {
        const text = (el.textContent || '').trim();
        if (astral.test(text) || bmp.test(text) || variationSelector.test(text)) {
          hits.push(`${el.tagName}:${text.slice(0, 40)}`);
        }
      }
      return [...new Set(hits)];
    });
    expect(emoji, `emoji on ${route}`).toEqual([]);
  }
});