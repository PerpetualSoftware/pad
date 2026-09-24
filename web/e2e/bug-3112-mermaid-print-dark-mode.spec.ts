import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, expectEditorMounted } from './lib/collab-helpers';

/**
 * BUG-3112: printing from dark mode put dark-palette mermaid diagrams on white
 * paper.
 *
 * The palette is baked into the SVG when the diagram renders, and print is
 * browser-native, so the fix is a print-only CSS flip for diagrams rendered
 * dark (Editor.svelte, `.mermaid-diagram[data-mermaid-theme='dark']`). What is
 * asserted is the PAINTED colour inside a node, read from a screenshot taken
 * under print emulation, because the filter changes paint and nothing in the
 * DOM or in computed fills.
 *
 * Three legs, so the pixel read is shown able to go both ways (CONVE-34):
 *  - dark mode, printed: the node interior is light (the fix);
 *  - dark mode, on screen: the SAME point is dark (the counterfactual: without
 *    the print rule, this is what printed);
 *  - light mode, printed: still light, and the diagram is not tagged dark (an
 *    unscoped filter would invert a light diagram into a dark one).
 */

const DIAGRAM = '.mermaid-wrapper .mermaid-diagram';
const SOURCE = '```mermaid\ngraph TD; Alpha-->Bravo\n```\n';

async function openDoc(
	page: Page,
	fixture: import('./fixtures').SuiteFixture,
	request: import('@playwright/test').APIRequestContext,
	theme: 'dark' | 'light',
) {
	await page.addInitScript((t) => {
		try {
			localStorage.setItem('pad-theme', t);
		} catch {
			/* storage blocked: the attribute check below fails loudly instead */
		}
	}, theme);
	const ws = fixture.workspaceSlug;
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `BUG-3112 ${theme} ${Date.now()}`, fields: '{}', content: `Diagram:\n\n${SOURCE}\nAfter.\n` },
	});
	expect(resp.ok()).toBe(true);
	const { slug } = (await resp.json()) as { slug: string };
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/docs/${slug}`);
	await expectEditorMounted(page);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();
	await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
}

/**
 * Relative luminance (0 black .. 1 white) of the painted pixel inside the first
 * node's FILL.
 *
 * Measured on the node's SHAPE, not on `g.node`, whose box also holds the label
 * (codex round 1): the point is a few px inside the shape's left edge, past the
 * stroke, at mid-height, and the shape must be wide enough that the centred
 * label cannot reach it. The screen leg (dark fill, luminance < 0.3) is what
 * shows the point is fill rather than a light label on a dark node.
 */
async function nodeInteriorLuminance(page: Page): Promise<number> {
	const shape = page
		.locator(`${EDITOR_SELECTOR} ${DIAGRAM} svg g.node`)
		.first()
		.locator(':scope > rect, :scope > path, :scope > polygon, :scope > circle, :scope > g > rect, :scope > g > path')
		.first();
	await shape.scrollIntoViewIfNeeded();
	const box = await shape.boundingBox();
	expect(box, 'the node shape has a box').not.toBeNull();
	expect(box!.width, 'the shape is wide enough to sample clear of its label').toBeGreaterThan(40);
	const x = Math.round(box!.x + 5);
	const y = Math.round(box!.y + box!.height / 2);
	const png = await page.screenshot({ clip: { x, y, width: 1, height: 1 } });
	return page.evaluate(async (b64) => {
		const img = new Image();
		img.src = `data:image/png;base64,${b64}`;
		await img.decode();
		const c = document.createElement('canvas');
		c.width = img.width;
		c.height = img.height;
		const ctx = c.getContext('2d')!;
		ctx.drawImage(img, 0, 0);
		const [r, g, b] = ctx.getImageData(0, 0, 1, 1).data;
		const lin = (v: number) => {
			const s = v / 255;
			return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
		};
		return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
	}, png.toString('base64'));
}

test.describe('BUG-3112: a diagram rendered in dark mode prints light', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'print emulation; one engine is enough');
	});

	test('dark mode: dark on screen, light in print', async ({ page, fixture, request }) => {
		await openDoc(page, fixture, request, 'dark');
		const diagram = page.locator(`${EDITOR_SELECTOR} ${DIAGRAM}`).first();
		await expect(diagram.locator('svg')).toBeVisible({ timeout: 15_000 });
		await expect(diagram).toHaveAttribute('data-mermaid-theme', 'dark');

		// COUNTERFACTUAL: on screen the node is dark, so this point would have
		// printed dark without the print rule.
		const screen = await nodeInteriorLuminance(page);
		expect(screen, `screen luminance ${screen}`).toBeLessThan(0.3);

		await page.emulateMedia({ media: 'print' });
		const printed = await nodeInteriorLuminance(page);
		expect(printed, `print luminance ${printed}`).toBeGreaterThan(0.5);
	});

	test('light mode: light in print, and not inverted', async ({ page, fixture, request }) => {
		await openDoc(page, fixture, request, 'light');
		const diagram = page.locator(`${EDITOR_SELECTOR} ${DIAGRAM}`).first();
		await expect(diagram.locator('svg')).toBeVisible({ timeout: 15_000 });
		await expect(diagram).toHaveAttribute('data-mermaid-theme', 'default');

		await page.emulateMedia({ media: 'print' });
		const printed = await nodeInteriorLuminance(page);
		expect(printed, `print luminance ${printed}`).toBeGreaterThan(0.5);
	});
});
