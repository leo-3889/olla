import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

// Read the source directly rather than importing it - the tailwind vite
// plugin intercepts .css imports (even with ?raw) and runs its own
// pipeline over them, which strips the plain custom-property declarations
// this test needs to see.
const appCss = readFileSync(
  path.join(path.dirname(fileURLToPath(import.meta.url)), 'app.css'),
  'utf8'
);

// Regression coverage: --blue was missing from the explicit dark theme block
// (:root[data-theme='dark']), so with the OS in light mode and the user
// explicitly picking dark, --blue fell through to the light :root value
// instead of the dark one. Everything the media-query dark block defines
// must also be defined in the explicit dark block, or the same class of
// drift creeps back in unnoticed.

/** Extracts the `--name: value;` custom property declarations from a single
 * `selector { ... }` block in a stylesheet's raw source. */
function customPropsInBlock(css: string, selector: string): Set<string> {
  const selectorIndex = css.indexOf(selector);
  if (selectorIndex === -1) {
    throw new Error(`selector not found in app.css: ${selector}`);
  }
  const openBrace = css.indexOf('{', selectorIndex);
  const closeBrace = css.indexOf('}', openBrace);
  const body = css.slice(openBrace + 1, closeBrace);

  const props = new Set<string>();
  for (const match of body.matchAll(/(--[a-zA-Z0-9-]+)\s*:/g)) {
    props.add(match[1]);
  }
  return props;
}

describe('app.css theme block parity', () => {
  it('explicit dark theme defines every custom property the media-query dark theme defines', () => {
    const mediaQueryDark = customPropsInBlock(appCss, '@media (prefers-color-scheme: dark)');
    const explicitDark = customPropsInBlock(appCss, ":root[data-theme='dark']");

    const missing = [...mediaQueryDark].filter((prop) => !explicitDark.has(prop));
    expect(missing).toEqual([]);
  });

  // The base :root also carries non-themed tokens (fonts, spacing, radii)
  // that are deliberately declared once and never overridden per theme, so
  // the three theme-scoped blocks are compared against each other rather
  // than against :root - that is the actual "themed token" set.
  it('the three theme-scoped blocks (media-dark, explicit light, explicit dark) define the same custom properties', () => {
    const mediaQueryDark = customPropsInBlock(appCss, '@media (prefers-color-scheme: dark)');
    const explicitLight = customPropsInBlock(appCss, ":root[data-theme='light']");
    const explicitDark = customPropsInBlock(appCss, ":root[data-theme='dark']");

    expect([...explicitLight].sort()).toEqual([...mediaQueryDark].sort());
    expect([...explicitDark].sort()).toEqual([...mediaQueryDark].sort());
  });
});
