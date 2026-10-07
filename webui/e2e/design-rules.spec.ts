// Design-rule conformance suite (research §7 V-6, plan §9 checklist):
// assertions, not screenshots. Tokens-only colors, no emoji, dark-only
// color-scheme — the automatable subset of the DESIGN.md contract.

import { test, expect } from '@playwright/test';
import { login, readyForCapture } from './helpers';

// The design contract in its robust form: every rendered color must be
// built from the token palette's HUE families — neutral blue (250),
// the green accent (143–145), the status tones (25 down / 75 warn /
// 230 info), or achromatic white/black — at any lightness/chroma/alpha.
// Enumerating exact serialized values instead would fight Chromium's
// color-space normalization and the kit's opacity blends (border-collapse
// hairlines, status-ping mid-frames, color-mix hover fills), which are
// all token-derived but serialize as arbitrary sRGB/oklch intermediates.
// The per-family hue windows live inside the tokens-only test below.

test.beforeEach(async ({ page }) => {
  // No time freeze needed: rules are content-independent.
  // - reducedMotion: the app's own spinner stops (app.css).
  // - injected `*{animation:none}`: the kit's status-ping opacity keyframes
  //   (kit.css has no reduced-motion override) freeze at their base values
  //   instead of bleeding sRGB interpolation frames into computed styles —
  //   the same normalization toHaveScreenshot's animations:'disabled' does.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.addInitScript(() => {
    const style = document.createElement('style');
    style.textContent = '*,*::before,*::after{animation:none !important;transition:none !important}';
    document.addEventListener('DOMContentLoaded', () => document.head.append(style));
    // head exists already at init time for SPA document loads.
    document.head?.append(style);
  });
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
  for (const route of ['/', '/quota', '/providers', '/models', '/routing', '/logs', '/keys']) {
    await page.goto(route);
    await readyForCapture(page);
    // `route` is closure-scoped on the Node side and not visible inside the
    // browser callback — pass it in as an argument.
    const violations = await page.evaluate(
      ({ routeLabel }: { routeLabel: string }) => {
        // Token hue families in their native color spaces: oklch hues for
        // oklch() serializations, HSL windows for sRGB composites (oklch
        // hue 250 renders as HSL ~210-217 — the two scales differ, so
        // each serialization is checked in its own space).
        const oklchWindows: [number, number][] = [
          [245, 255], // neutral
          [140, 148], // accent green
          [20, 30], // down red
          [70, 80], // warn amber
          [225, 235], // status info
        ];
        const srgbWindows: [number, number][] = [
          [185, 225], // neutral blue (oklch 250 → HSL ~210) + status-info (oklch 230 → HSL ~199)
          [115, 155], // accent green (oklch 143–145 → HSL ~122–140)
          [10, 45], // down red (oklch 25 → HSL ~15–25)
          [30, 50], // warn amber (oklch 75 → HSL ~38–45)
        ];
        const inWindows = (h: number, ws: [number, number][]): boolean =>
          ws.some(([lo, hi]) => lo <= h && h <= hi);
        const offenders: string[] = [];
        const all = document.querySelectorAll<HTMLElement>('body *');
        for (const el of all) {
          if (!(el instanceof HTMLElement)) continue;
          const cs = getComputedStyle(el);
          for (const prop of ['color', 'backgroundColor', 'borderTopColor']) {
            const v = cs[prop as 'color'];
            if (!v) continue;
            let ok: boolean;
            if (v === 'rgba(0, 0, 0, 0)') ok = true;
            else if (v.startsWith('rgb(') || v.startsWith('rgba(')) {
              const parts = v
                .replace(/rgba?\(|\)/g, '')
                .split(',')
                .map((s) => Number(s.trim()));
              const [r = 0, g = 0, b = 0] = parts;
              const max = Math.max(r, g, b);
              const min = Math.min(r, g, b);
              if (max - min === 0) ok = true; // pure gray
              else {
                let h: number;
                if (max === r) h = ((g - b) / (max - min)) % 6;
                else if (max === g) h = (b - r) / (max - min) + 2;
                else h = (r - g) / (max - min) + 4;
                h = Math.round((h * 60 + 360) % 360);
                ok = inWindows(h, srgbWindows);
              }
            } else if (v.startsWith('oklch(')) {
              const m = v.match(/^oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)/);
              // oklch() serializations are checked in their native hue space.
              ok =
                !!m &&
                (Number(m[2]) === 0 || inWindows(Math.round(Number(m[3])), oklchWindows));
            } else {
              // WebKit serializes color-mix(in oklab, ...) composites as
              // oklab(L a b) — the a/b axes are the same as oklch's, so the
              // hue (atan2(b, a)) checks against the oklch windows directly.
              const m = v.match(/^oklab\(([\d.]+)\s+(-?[\d.]+)\s+(-?[\d.]+)/);
              ok = !!m && (() => {
                const a = Number(m[2]);
                const b = Number(m[3]);
                const chroma = Math.sqrt(a * a + b * b);
                if (chroma === 0) return true; // achromatic
                if (chroma < 0.01) return true; // border-collapse hairline composites
                const hue = Math.round(((Math.atan2(b, a) * 180) / Math.PI + 360) % 360);
                return inWindows(hue, oklchWindows);
              })();
            }
            if (!ok) {
              offenders.push(`${routeLabel} ${el.tagName}.${el.className} ${prop}=${v}`);
            }
          }
        }
        return offenders;
      },
      { routeLabel: route },
    );

    expect(violations, `off-token colors on ${route}`).toEqual([]);
  }
});

test('no emoji in rendered output', async ({ page }) => {
  for (const route of ['/', '/providers', '/models', '/routing', '/logs', '/keys', '/settings']) {
    await page.goto(route);
    await readyForCapture(page);
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