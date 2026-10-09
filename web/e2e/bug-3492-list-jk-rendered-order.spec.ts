import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createCollection, deleteCollection } from './lib/attachment-viewer';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3492: j/k in the LIST view step the rows on screen.
 *
 * The page used to step `filteredItems`, which is in UPDATED order, while the
 * list renders in its sort mode's order (default manual: stored sort_order, then
 * created_at). After an edit to an older item the two disagree, and j landed on
 * the edited item instead of the row below. Found under 6-worker e2e load
 * (BUG-3492's trace), where another worker's edit put an old item next to the
 * cursor in updated order.
 *
 * Own collection, so no other spec's rows move this list. Timestamps are
 * one-second resolution, so writes are spaced by a second:
 *   create old (t0), b (t1), c (t2); edit old (t3); create d (t4)
 *   on screen (created ASC, BUG-3527):  old, b, c, d
 *   updated DESC (old order):           d, old, c, b
 * From no focus j lands on old; the next j must land on b, not c.
 */

const DESKTOP = { width: 1200, height: 900 };

function authJson(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function createItem(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string) {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: '' },
	});
	if (!resp.ok()) throw new Error(`create ${title}: ${resp.status()} ${await resp.text()}`);
	return (await resp.json()) as { slug: string };
}

async function focusedTitle(page: Page): Promise<string | null> {
	return page.evaluate(
		() => document.querySelector('.item-card.focused .card-title')?.textContent?.trim() ?? null,
	);
}

async function screenTitles(page: Page): Promise<string[]> {
	return page.evaluate(() =>
		[...document.querySelectorAll('.item-card .card-title')].map((e) => e.textContent?.trim() ?? ''),
	);
}

test.describe('list j/k follow the rows on screen (BUG-3492)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'keyboard list nav; one desktop browser is enough');
	});

	test('after an edit to an older item, j lands on the row below on screen, not on the edited item', async ({
		page,
		fixture,
		request,
	}) => {
		const stamp = Date.now();
		const { slug: coll } = await createCollection(fixture, request, `JK order ${stamp}`);
		try {
			const wait = () => new Promise((r) => setTimeout(r, 1100));
			const old = await createItem(fixture, request, coll, 'old');
			await wait();
			await createItem(fixture, request, coll, 'b');
			await wait();
			await createItem(fixture, request, coll, 'c');
			await wait();
			const edit = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${old.slug}`, {
				headers: authJson(fixture),
				data: { content: 'edited after b and c were created' },
			});
			expect(edit.ok()).toBe(true);
			await wait();
			await createItem(fixture, request, coll, 'd');

			await page.setViewportSize(DESKTOP);
			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll}?view=list`);
			// Manual order breaks the all-zero sort_order tie oldest first
			// (BUG-3527, the server's order). The page holds updated DESC
			// (d, old, c, b), so from `old` the edit order says `c` and the
			// screen says `b`.
			await expect.poll(() => screenTitles(page)).toEqual(['old', 'b', 'c', 'd']);

			await page.locator('body').click({ position: { x: 5, y: 5 } });
			await page.keyboard.press('j');
			await expect.poll(() => focusedTitle(page)).toBe('old');
			await page.keyboard.press('j');
			await expect.poll(() => focusedTitle(page)).toBe('b');
			await page.keyboard.press('j');
			await expect.poll(() => focusedTitle(page)).toBe('c');
			await page.keyboard.press('k');
			await expect.poll(() => focusedTitle(page)).toBe('b');

			// Every group collapsed: nothing is on screen, so j focuses nothing.
			// An empty rendered order must not fall back to the unrendered list
			// (codex r1).
			await page.keyboard.press('Escape');
			await expect.poll(() => focusedTitle(page)).toBeNull();
			for (const h of await page.locator('.group-header').all()) await h.click();
			await expect.poll(() => screenTitles(page)).toEqual([]);
			await page.locator('body').click({ position: { x: 5, y: 5 } });
			await page.keyboard.press('j');
			await page.waitForTimeout(300);
			expect(await page.evaluate(() => document.querySelectorAll('.item-card.focused').length)).toBe(0);
		} finally {
			await deleteCollection(fixture, request, coll);
		}
	});
});
