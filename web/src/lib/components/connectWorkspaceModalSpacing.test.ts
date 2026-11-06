import { readFileSync } from 'node:fs';
import { expect, it } from 'vitest';

const source = readFileSync(new URL('./ConnectWorkspaceModal.svelte', import.meta.url), 'utf8');
const appCss = readFileSync(new URL('../../app.css', import.meta.url), 'utf8');

it('BUG-4337: command boxes use defined spacing tokens and contain the copy control', () => {
	const defined = new Set([...appCss.matchAll(/(--space-\d+)\s*:/g)].map((match) => match[1]));
	const used = [...source.matchAll(/var\((--space-\d+)\)/g)].map((match) => match[1]);
	expect(used.length).toBeGreaterThan(0);
	expect([...new Set(used.filter((token) => !defined.has(token)))]).toEqual([]);
	for (const selector of ['code-block', 'claim-code-row']) {
		const rule = source.match(new RegExp(`\\.${selector}\\s*\\{([^}]+)\\}`))![1];
		expect(rule).toMatch(/padding:\s*var\(--space-\d+\) var\(--space-10\)/);
		expect(rule).toContain('min-height: var(--space-10)');
	}
});

it('BUG-4337: phone copy controls have a contained 44px touch area', () => {
	const phone = source.match(/@media\s*\(max-width:\s*(?:640|480)px\)\s*\{([\s\S]*?)\n\t\}/)?.[1] ?? '';
	const button = phone.match(/\.copy-btn-small\s*\{([^}]+)\}/)?.[1] ?? '';
	expect(button).toMatch(/width:\s*44px;/);
	expect(button).toMatch(/height:\s*44px;/);
	expect(phone).toMatch(/\.code-block,\s*\.claim-code-row\s*\{[^}]*min-height:\s*calc\(44px \+ var\(--space-2\) \* 2\)/);
	expect(phone).toContain('padding-right: calc(var(--space-10) + var(--space-6))');
});
