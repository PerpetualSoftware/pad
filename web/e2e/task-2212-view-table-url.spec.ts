import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2212 (audit C34): the collection page writes `?view=table` into its URL
 * but its load-time parser accepted only list|board, so a shared table link
 * opened as the BOARD for anyone whose own browser did not already prefer the
 * table (the sharer never saw it: their localStorage carried the preference).
 * Every leg runs in a browser with nothing remembered, as a recipient's is.
 *
 * C95 (the remembered mode is per workspace now) is covered by
 * viewPersistence.svelte.test.ts; the leg below pins only that a preference
 * remembered under the OLD key still applies after the upgrade.
 */

// Docs, with an item seeded: an empty collection shows its empty state
// instead of any view.
function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

test.describe('TASK-2212: a shared ?view= link opens in that view', () => {
	test.beforeEach(async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'URL parsing is width-independent; one project is enough');
		await seedDoc(fixture, request, 'View table link');
		await page.setViewportSize({ width: 1280, height: 800 });
		await browserLogin(page);
		await page.evaluate(() => localStorage.clear());
	});

	test('?view=table opens the table in a browser with no saved preference', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture, '?view=table'));
		await expect(page.locator('.collection-page')).toHaveClass(/\btable-active\b/);
		await expect(page.locator('.table-view[role="table"]')).toBeVisible();
	});

	test('?view=list and ?view=board still open as asked, over a remembered table', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture, '?view=table'));
		await expect(page.locator('.collection-page')).toHaveClass(/\btable-active\b/);
		// Remember the table, as switching to it does.
		await page.evaluate(
			({ ws }) => localStorage.setItem(`pad-view:${ws}:docs`, 'table'),
			{ ws: fixture.workspaceSlug }
		);

		await page.goto(docsUrl(fixture, '?view=board'));
		await expect(page.locator('.collection-page')).toHaveClass(/\bboard-active\b/);
		await page.goto(docsUrl(fixture, '?view=list'));
		await expect(page.locator('.collection-page')).not.toHaveClass(/\b(board|table)-active\b/);
	});

	test('a preference remembered under the pre-upgrade key still applies', async ({ page, fixture }) => {
		await page.goto(docsUrl(fixture));
		await page.evaluate(() => localStorage.setItem('pad-view-docs', 'table'));
		await page.goto(docsUrl(fixture));
		await expect(page.locator('.collection-page')).toHaveClass(/\btable-active\b/);
	});
});
