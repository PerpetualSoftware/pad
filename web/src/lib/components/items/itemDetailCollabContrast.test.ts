import { readFileSync } from 'node:fs';
import { expect, it } from 'vitest';
import { contrast } from '../../../routes/[username]/[workspace]/graph/graphTheme';

const source = readFileSync(new URL('./ItemDetail.svelte', import.meta.url), 'utf8');
const css = source.split('<style>')[1].replace(/\/\*[\s\S]*?\*\//g, '');
const appCss = readFileSync(new URL('../../../app.css', import.meta.url), 'utf8');
const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)];
function rule(selector: string) {
	const match = rules.find(([, selectors]) => selectors.split(',').some((s) => s.trim() === selector));
	expect(match, selector).toBeDefined();
	return match![2];
}

it('BUG-4337: all collaboration labels use secondary text with AA contrast and retain state dots', () => {
	const colors = {
		synced: 'var(--accent-green)',
		connecting: 'var(--accent-yellow, #d4a017)',
		reconnecting: 'var(--accent-yellow, #d4a017)',
		offline: 'var(--accent-red)',
	};
	expect.soft(rule('.collab-state')).toContain('color: var(--text-secondary)');
	for (const [state, color] of Object.entries(colors)) {
		const label = rule(`.collab-state-${state}`);
		expect.soft([...label.matchAll(/\bcolor:\s*([^;]+);/g)].map((m) => m[1])).toEqual(['var(--text-secondary)']);
		expect(rule(`.collab-state-${state} .collab-state-dot`)).toContain(`background: ${color};`);
	}
	// The badge and metadata are transparent on the item page's primary surface.
	for (const selector of ['.collab-state', '.meta-info', '.item-page']) {
		expect(rule(selector)).not.toMatch(/background(?:-color)?:/);
	}
	for (const theme of [':root', '[data-theme="light"]', ':root:not([data-theme="dark"])']) {
		const declarations = appCss.slice(appCss.indexOf(`${theme} {`)).split('}')[0];
		const foreground = declarations.match(/--text-secondary:\s*(#[\da-f]{6});/i)![1];
		const background = declarations.match(/--bg-primary:\s*(#[\da-f]{6});/i)![1];
		expect(contrast(foreground, background), theme).toBeGreaterThanOrEqual(4.5);
	}
});
