import type { Page } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import type { APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2232 (audit C94): every keystroke in a collection's search box ran a
 * same-page `goto` with replaceState. Safari rate-limits replaceState (about
 * 100 per 30s) and then throws SecurityError, so a long query broke search
 * there. The address now follows the box on a debounce; the results still
 * follow every key.
 */

async function countReplaceState(page: Page) {
	await page.addInitScript(() => {
		const w = window as unknown as { __replaceStates: number };
		w.__replaceStates = 0;
		const orig = history.replaceState.bind(history);
		history.replaceState = (...args: Parameters<History['replaceState']>) => {
			w.__replaceStates++;
			return orig(...args);
		};
	});
}

/** One task, so the collection renders its filter bar, then the search box
 *  (behind the filter toggle when the bar starts collapsed). */
async function openSearch(page: Page, fixture: SuiteFixture, request: APIRequestContext) {
	const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/tasks/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `Search seed ${Date.now()}`, fields: JSON.stringify({ status: 'open' }) },
	});
	if (!r.ok()) throw new Error(`seed: ${r.status()} ${await r.text()}`);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks`);
	const box = page.getByLabel('Search Tasks');
	await page.waitForLoadState('networkidle');
	if (!(await box.isVisible())) await page.getByRole('button', { name: /filter/i }).first().click();
	await expect(box).toBeVisible();
	return box;
}

const replaceStates = (page: Page) => page.evaluate(() => (window as unknown as { __replaceStates: number }).__replaceStates);

test('typing a query writes the address once it settles, not per key', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a call count');
	await countReplaceState(page);
	await browserLogin(page);
	const box = await openSearch(page, fixture, request);

	const before = await replaceStates(page);
	await box.pressSequentially('releaseplan', { delay: 30 });
	await expect(page).toHaveURL(/[?&]q=releaseplan(&|$)/);
	const writes = (await replaceStates(page)) - before;
	testInfo.annotations.push({ type: 'replaceState-calls', description: `${writes} for 11 keys` });
	// Per key this was 11 or more; settled, it is one (allow a second for a
	// navigation of the app's own landing at the same moment).
	expect(writes).toBeLessThanOrEqual(2);
});

test('a sync still pending when the user leaves does not pull them back', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop layout');
	await browserLogin(page);
	const base = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;
	const box = await openSearch(page, fixture, request);

	await box.pressSequentially('zz', { delay: 10 });
	// Leave inside the debounce window, by an in-app navigation.
	await page.evaluate((href) => {
		const a = document.createElement('a');
		a.href = href;
		document.body.appendChild(a);
		a.click();
		a.remove();
	}, `${base}/ideas`);
	await expect(page).toHaveURL(new RegExp(`${base}/ideas`));
	await page.waitForTimeout(600);
	await expect(page).toHaveURL(new RegExp(`${base}/ideas`));
	expect(page.url()).not.toContain('q=zz');
});
