import { readFileSync } from 'node:fs';
import { describe, it, expect } from 'vitest';
const css = readFileSync(new URL('../app.css', import.meta.url), 'utf8');
const agenda = readFileSync(new URL('./(console)/agenda/+page.svelte', import.meta.url), 'utf8');
const consoleLayout = readFileSync(new URL('./(console)/+layout.svelte', import.meta.url), 'utf8');
it('fills the phone console background to the viewport below the You bar while allowing the desktop shell to shrink', () => {
	// The You bar sits above <main> in flow (border-box, so its border is inside its min-height).
	expect(consoleLayout).toMatch(/\.console-layout\s*\{[^}]*min-height: calc\(100dvh - var\(--topbar-height\) - env\(safe-area-inset-top, 0px\)\)/);
	// The bottom-bar reserve pads <main>, so the layout gives that back while the bar is shown.
	expect(consoleLayout).toMatch(/:global\(body\.has-bottom-nav\) \.console-layout:not\(\.desktop\)\s*\{[^}]*min-height: calc\(100dvh - var\(--topbar-height\) - env\(safe-area-inset-top, 0px\) - var\(--bottom-nav-height\) - env\(safe-area-inset-bottom, 0px\)\)/);
	expect(consoleLayout).not.toMatch(/100vh/);
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
