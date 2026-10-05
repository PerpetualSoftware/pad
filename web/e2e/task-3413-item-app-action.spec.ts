import { expect, type APIRequestContext, type Route } from '@playwright/test';
import { test, type SuiteFixture } from './fixtures';

/**
 * TASK-3413 (SPEC-6 U9d over U11): an installed app's item action in the
 * item pane, in a real browser.
 *
 * WHAT IS REAL AND WHAT IS NOT. The item is a real row on the real server.
 * The e2e server is plain http, so apps are unavailable and no install can
 * offer an action; the two U11 routes (the item's action list and the mint)
 * are answered by `page.route`. The server half is covered by #1784's Go
 * tests. The mint answers the server's own /api/v1/health, so the new tab
 * lands somewhere real.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const coll = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name: `App action ${Date.now()}`, prefix: 'ACT' }
	});
	expect(coll.ok(), await coll.text()).toBeTruthy();
	const { slug: collSlug } = await coll.json();
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`, {
		headers: authHeaders(fixture),
		data: { title: `App action item ${Date.now()}`, content: '' }
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	return { collSlug, item: (await created.json()) as { id: string; slug: string } };
}

test('TASK-3413 U9d: an app action opens the app in a new tab with no opener', async ({ page, fixture, request }) => {
	const { collSlug, item } = await seed(fixture, request);
	const target = new URL('/api/v1/health', fixture.baseURL).href;
	const mints: string[] = [];
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*/app-actions**`, async (route: Route) => {
		const req = route.request();
		if (req.method() === 'GET') {
			return route.fulfill({
				status: 200,
				contentType: 'application/json',
				body: JSON.stringify([{ install_id: 'inst-e2e', app_title: 'Support Portal', action_key: 'open-ticket', label: 'Open ticket' }])
			});
		}
		mints.push(new URL(req.url()).pathname);
		return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ url: target }) });
	});

	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
	const button = page.getByRole('button', { name: /Open ticket/ });
	await expect(button).toBeVisible();
	await expect(button).toHaveAttribute('title', 'Open ticket: opens Support Portal in a new tab');

	const [popup] = await Promise.all([page.waitForEvent('popup'), button.click()]);
	await popup.waitForURL(target);
	expect(await popup.evaluate(() => window.opener)).toBeNull();
	expect(mints).toHaveLength(1);
	expect(mints[0]).toMatch(/\/app-actions\/inst-e2e\/open-ticket$/);
});

test('TASK-3413 U9d: a refused mint closes the tab and says it could not open', async ({ page, fixture, request }) => {
	const { collSlug, item } = await seed(fixture, request);
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*/app-actions**`, async (route: Route) => {
		if (route.request().method() === 'GET') {
			return route.fulfill({
				status: 200,
				contentType: 'application/json',
				body: JSON.stringify([{ install_id: 'inst-e2e', app_title: 'Support Portal', action_key: 'open-ticket', label: 'Open ticket' }])
			});
		}
		return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: { code: 'not_found', message: 'Not found' } }) });
	});
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
	const [popup] = await Promise.all([page.waitForEvent('popup'), page.getByRole('button', { name: /Open ticket/ }).click()]);
	// The tab may already be closed by the time a listener could attach.
	await expect.poll(() => popup.isClosed()).toBe(true);
	await expect(page.getByRole('alert').filter({ hasText: "Couldn't open Support Portal." })).toBeVisible();
});

test('TASK-3413 U9d: an item with no app actions shows no button (real server)', async ({ page, fixture, request }) => {
	const { collSlug, item } = await seed(fixture, request);
	const listed = page.waitForResponse((r) => r.url().includes(`/items/`) && r.url().endsWith('/app-actions') && r.request().method() === 'GET');
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${collSlug}/${item.slug}`);
	const res = await listed;
	expect(res.status()).toBe(200);
	expect(await res.json()).toEqual([]);
	await expect(page.locator('.app-action-btn')).toHaveCount(0);
});
