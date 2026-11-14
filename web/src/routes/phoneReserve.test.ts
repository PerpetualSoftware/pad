import { readFileSync } from 'node:fs';
import { describe, it, expect } from 'vitest';
const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
const agenda = readFileSync(new URL('./(console)/agenda/+page.svelte', import.meta.url), 'utf8');
const consoleLayout = readFileSync(new URL('./(console)/+layout.svelte', import.meta.url), 'utf8');
it('fills the phone console background to the viewport while allowing the desktop shell to shrink', () => {
	expect(consoleLayout).toMatch(/\.console-layout\s*\{[^}]*min-height: 100vh/);
	expect(consoleLayout).toMatch(/\.console-layout\.desktop\s*\{[^}]*min-height: 0/);
});
describe('phone agenda bottom reserve', () => {
	it('pads the main scroller by the bar and safe area only while the phone bar is shown', () => {
		expect(css).toMatch(/@media \(max-width: 768px\)[\s\S]*body\.has-bottom-nav \.main-content\s*\{[^}]*padding-bottom: calc\(var\(--bottom-nav-height\) \+ env\(safe-area-inset-bottom, 0px\)\)/);
	});
	it('keeps the agenda rail in document flow on phone, so the main scroller reserve covers it', () => {
		expect(agenda).toMatch(/@media \(max-width: 1099px\)[\s\S]*\.rail \{[^}]*position: static;[^}]*max-height: none;[^}]*overflow: visible/);
	});
});
