import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3606: a tab that dies outright (no unload prompt, no teardown flush)
 * still offers its unconfirmed edits on the next open. While edits are at
 * risk the item's markdown is kept every couple of seconds with its base
 * seq; confirmed edits leave nothing behind.
 */
async function mkDoc(request: import('@playwright/test').APIRequestContext, fixture: { workspaceSlug: string }, title: string) {
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture as never),
		data: { title, content: 'Original body.' },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	return (await created.json()) as { id: string; ref: string };
}

test('a tab killed with unconfirmed edits offers them on the next open', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a client-side keep');
	test.setTimeout(120_000);
	const doc = await mkDoc(request, fixture, `TASK-3606 crash ${Date.now()}`);
	const me = (await (await request.get('/api/v1/auth/me', { headers: authJson(fixture) })).json()) as { id: string };
	const key = `pad:refused-raw-draft:${me.id}:${doc.id}`;

	const ctx = await browser.newContext();
	const page = await ctx.newPage();
	let cut = false;
	await page.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
		if (cut) {
			ws.close();
			return;
		}
		const server = ws.connectToServer();
		server.onMessage((m) => ws.send(m));
		ws.onMessage((m) => {
			if (!cut) server.send(m);
		});
		ws.onClose(() => server.close());
		server.onClose(() => ws.close());
		const timer = setInterval(() => {
			if (!cut) return;
			clearInterval(timer);
			server.close();
		}, 50);
	});
	// No content save reaches the server either: the edits stay at risk.
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*`, (route) =>
		cut && route.request().method() === 'PATCH' ? route.abort() : route.continue(),
	);

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	cut = true;
	await expect(page.locator(SYNCED_BADGE_SELECTOR), 'premise: the socket is gone').toBeHidden({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' Typed before the crash.');

	// The keeper writes within a couple of seconds, with the base it edited from.
	await expect
		.poll(async () => {
			const raw = await page.evaluate((k) => localStorage.getItem(k), key);
			return raw ? (JSON.parse(raw) as { markdown: string; baseSeq?: number }) : null;
		}, { timeout: SYNC_TIMEOUT, message: 'the crash draft is kept while the edits are at risk' })
		.toMatchObject({ markdown: expect.stringContaining('Typed before the crash.'), baseSeq: expect.any(Number) });

	// The tab dies: no beforeunload, no teardown flush. The browser's storage
	// is the profile's, which a fresh context does not share, so carry it over
	// as a real browser restart would keep it.
	const state = await ctx.storageState();
	await ctx.close({ runBeforeUnload: false });
	const stored = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { content: string };
	expect(stored.content, 'premise: the server never got the edits').not.toContain('Typed before the crash.');

	const again = await (await browser.newContext({ storageState: state })).newPage();
	await browserLogin(again);
	await again.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	const notice = again.getByTestId('refused-raw-draft');
	await expect(notice).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(notice).toContainText('did not reach the server');
	await again.context().close();
});

test('confirmed edits leave no crash draft behind', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a client-side keep');
	test.setTimeout(90_000);
	const doc = await mkDoc(request, fixture, `TASK-3606 confirmed ${Date.now()}`);
	const me = (await (await request.get('/api/v1/auth/me', { headers: authJson(fixture) })).json()) as { id: string };
	const key = `pad:refused-raw-draft:${me.id}:${doc.id}`;
	const ctx = await browser.newContext();
	const page = await ctx.newPage();
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' Saved normally.');
	// The server has it, and the keeper, having nothing at risk, has dropped any draft.
	await expect
		.poll(async () => ((await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { content: string }).content, { timeout: SYNC_TIMEOUT })
		.toContain('Saved normally.');
	await expect.poll(() => page.evaluate((k) => localStorage.getItem(k), key), { timeout: 10_000 }).toBeNull();
	await ctx.close();
});
