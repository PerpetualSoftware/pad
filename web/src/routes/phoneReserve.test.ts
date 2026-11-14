import { readFileSync } from 'node:fs';
import { describe, it, expect } from 'vitest';
const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
const agenda = readFileSync(new URL('./(console)/agenda/+page.svelte', import.meta.url), 'utf8');
describe('phone agenda bottom reserve', () => {
 it('pads the main scroller by the bar and safe area only while the phone bar is shown', () => {
  expect(css).toMatch(/@media \(max-width: 768px\)[\s\S]*body\.has-bottom-nav \.main-content\s*\{[^}]*padding-bottom: calc\(var\(--bottom-nav-height\) \+ env\(safe-area-inset-bottom, 0px\)\)/);
 });
 it('keeps the agenda rail in document flow on phone and lifts the sticky action bar above the nav', () => {
  expect(agenda).toMatch(/@media \(max-width: 1099px\)[\s\S]*\.rail \{[^}]*position: static;[^}]*max-height: none;[^}]*overflow: visible/);
  expect(agenda).toMatch(/:global\(body\.has-bottom-nav\) \.dragbar\s*\{[^}]*bottom: calc\(var\(--bottom-nav-height\) \+ env\(safe-area-inset-bottom, 0px\) \+ var\(--space-4\)\)/);
 });
});
