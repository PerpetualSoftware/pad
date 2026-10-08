import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { Page } from '@playwright/test';

/**
 * TASK-2209: a collection with the `balanced` layout lays its fields out in a
 * two-column grid, and that grid used to drop to one column only on a VIEWPORT
 * query (max-width 768px). In the docked pane (~400px on a 1440px screen) it
 * stayed two columns, and because a bare `1fr` track has an `auto` minimum the
 * select's and tag input's min-content pushed the grid to 476px: the right
 * column ran past the screen edge (measured: Impact and Tags ended at x=1492 on
 * a 1440px viewport). The grid now answers to `.item-body`'s width through a
 * container query.
 *
 * Mutation-tested: swapping the `@container` for a viewport `@media` fails the
 * pane leg (2 tracks); adding `contain: layout` to the container fails the
 * sheet leg (the overlay shrinks to the container). The `minmax(0, 1fr)`
 * tracks are defensive and not pinned here: this item's content fits both
 * columns at 560px either way. jsdom computes no layout, so this is an e2e.
 */

async function seedIdea(fixture: import('./fixtures').SuiteFixture, request: import('@playwright/test').APIRequestContext, title: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/ideas/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title, fields: JSON.stringify({ impact: 'high', category: 'pane width' }), content: '' }
	});
	if (!resp.ok()) throw new Error(`idea create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { slug: string; ref: string };
}

/** The fields grid's tracks and how far any field control reaches past the panel or the viewport. */
async function fieldsGeometry(page: Page) {
	const panel = page.locator('.item-body.layout-balanced .fields-panel').first();
	await expect(panel).toBeVisible();
	await expect(panel.locator('.field-row', { hasText: 'Impact' })).toBeVisible();
	return panel.evaluate((el) => {
		const box = el.getBoundingClientRect();
		const rights = [...el.querySelectorAll('.field-row, .field-row *')].map((e) => e.getBoundingClientRect().right);
		const right = Math.max(...rights);
		return {
			tracks: getComputedStyle(el).gridTemplateColumns.split(' ').length,
			bodyWidth: (el.closest('.item-body') as HTMLElement).getBoundingClientRect().width,
			pastPanel: right - box.right,
			pastViewport: right - window.innerWidth
		};
	});
}

test.describe('TASK-2209: the balanced fields grid follows the item body, not the viewport', () => {
	test('in the docked pane on a wide screen the grid is one column and nothing runs off screen', async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the viewport is set explicitly');
		await page.setViewportSize({ width: 1440, height: 900 });
		const idea = await seedIdea(fixture, request, `t2209 pane ${Date.now()}`);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/ideas?item=${idea.ref}`);
		const g = await fieldsGeometry(page);
		// The premise: this is the narrow pane, not the full page.
		expect(g.bodyWidth).toBeLessThan(560);
		expect(g.tracks).toBe(1);
		expect(g.pastPanel).toBeLessThanOrEqual(0.5);
		expect(g.pastViewport).toBeLessThanOrEqual(0);
	});

	test('CONTROL: on the full item page the same viewport keeps two columns', async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the viewport is set explicitly');
		await page.setViewportSize({ width: 1440, height: 900 });
		const idea = await seedIdea(fixture, request, `t2209 page ${Date.now()}`);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/ideas/${idea.slug}`);
		const g = await fieldsGeometry(page);
		expect(g.bodyWidth).toBeGreaterThanOrEqual(560);
		expect(g.tracks).toBe(2);
		expect(g.pastPanel).toBeLessThanOrEqual(0.5);
	});

	test('the mobile bottom sheet opened from a field is not trapped by the new container', async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'the BottomSheet only replaces the anchored panel at the mobile breakpoint');
		const idea = await seedIdea(fixture, request, `t2209 sheet ${Date.now()}`);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/ideas/${idea.slug}`);
		const body = page.locator('.item-body.layout-balanced').first();
		await expect.poll(() => body.evaluate((el) => getComputedStyle(el).containerType)).toBe('inline-size');
		await body.locator('.field-row', { hasText: 'Impact' }).locator('button.select-trigger').click();
		const overlay = page.locator('.bs-overlay');
		await expect(overlay).toBeVisible();
		// Inside the container, or this proves nothing about containment.
		expect(await overlay.evaluate((el) => !!el.closest('.item-body.layout-balanced'))).toBe(true);
		const box = (await overlay.boundingBox())!;
		const vp = page.viewportSize()!;
		expect(Math.round(box.width)).toBe(vp.width);
		expect(Math.round(box.height)).toBe(vp.height);
	});
});
