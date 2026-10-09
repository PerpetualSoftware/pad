import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2218 (audit C40 + C41): the editor's insert surfaces.
 *  - C40: the slash menu opened at `caret.bottom + 4` with no viewport check,
 *    so at the end of a long document (the normal caret position) it opened
 *    almost entirely below the window. It now flips above the caret and stays
 *    inside the viewport.
 *  - C41: the [[ link picker needed both brackets inside 300ms and had no
 *    other way in. It now opens on any `[` typed after a `[`, and the slash
 *    menu offers "Link to item".
 */

async function seedLongDoc(fixture: SuiteFixture, request: APIRequestContext): Promise<string> {
	const body = Array.from({ length: 80 }, (_, i) => `Paragraph ${i + 1} of a long document.`).join('\n\n');
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `Insert menus ${Date.now()}`, fields: '{}', content: body }
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return ((await resp.json()) as { slug: string }).slug;
}

async function openAtEnd(page: Page, fixture: SuiteFixture, slug: string) {
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR).first()).toBeVisible({ timeout: 20_000 });
	const editor = page.locator(EDITOR_SELECTOR).first();
	await editor.click();
	await page.keyboard.press('Control+End');
	// Scroll the caret to the bottom of the window, where appending happens.
	await page.evaluate(() => window.getSelection()?.focusNode?.parentElement?.scrollIntoView({ block: 'end' }));
	await page.keyboard.press('Enter');
	return editor;
}

async function inViewport(page: Page, selector: string) {
	const box = await page.locator(selector).first().boundingBox();
	const vp = page.viewportSize()!;
	expect(box, `${selector} has a box`).not.toBeNull();
	expect(box!.y, `${selector} top`).toBeGreaterThanOrEqual(0);
	expect(box!.y + box!.height, `${selector} bottom`).toBeLessThanOrEqual(vp.height);
	expect(box!.x + box!.width, `${selector} right`).toBeLessThanOrEqual(vp.width);
}

test.describe('TASK-2218: the editor insert menus', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'caret geometry; one desktop browser is enough');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
	});

	test('the slash menu at the end of a long document opens inside the window', async ({ page, fixture, request }) => {
		const slug = await seedLongDoc(fixture, request);
		await openAtEnd(page, fixture, slug);
		await page.keyboard.type('/');
		const menu = page.locator('.slash-menu').first();
		await expect(menu).toBeVisible();
		await inViewport(page, '.slash-menu');
		await page.keyboard.press('Escape');
	});

	test('[[ opens the link picker however slowly it is typed', async ({ page, fixture, request }) => {
		const slug = await seedLongDoc(fixture, request);
		await openAtEnd(page, fixture, slug);
		await page.keyboard.type('[');
		await page.waitForTimeout(700);
		await page.keyboard.type('[');
		const picker = page.locator('.slash-menu').filter({ has: page.locator('.slash-item') }).first();
		await expect(picker).toBeVisible();
		await inViewport(page, '.slash-menu');
		await page.keyboard.press('Escape');
	});

	test('the slash menu offers "Link to item", which opens the picker', async ({ page, fixture, request }) => {
		const slug = await seedLongDoc(fixture, request);
		await openAtEnd(page, fixture, slug);
		await page.keyboard.type('/link');
		const entry = page.locator('.slash-menu .slash-item', { hasText: 'Link to item' });
		await expect(entry).toBeVisible();
		await page.keyboard.press('Enter');
		// The picker lists items (the seeded doc at least) and filters as typed.
		await expect(page.locator('.slash-menu .slash-ref').first()).toBeVisible();
		await page.keyboard.type('Insert menus');
		await expect(page.locator('.slash-menu .slash-item', { hasText: 'Insert menus' }).first()).toBeVisible();
		await page.keyboard.press('Enter');
		await expect(page.locator(`${EDITOR_SELECTOR} a`, { hasText: 'Insert menus' }).last()).toBeVisible();
	});
});
