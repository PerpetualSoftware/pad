import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * BUG-3863: a kept draft D the notice is showing is HELD, so the crash keeper
 * keeping newer at-risk text E for the same item cannot replace D's only stored
 * copy. A reload before the user resolves D offers D again; once D is resolved,
 * E is offered.
 */
test('a shown kept draft survives the keeper and a reload, and the newer text is offered after it', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a client-side keep');
	test.setTimeout(150_000);
	const headers = authJson(fixture as never);
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers,
		data: { title: `BUG-3863 held ${Date.now()}`, content: 'Original body.' },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const doc = (await created.json()) as { id: string; ref: string };
	const me = (await (await request.get('/api/v1/auth/me', { headers })).json()) as { id: string };
	const key = `pad:refused-raw-draft:${me.id}:${doc.id}`;
	const read = async () => ((await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers })).json()) as { content: string; seq: number });
	const url = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`;
	const slots = (page: import('@playwright/test').Page) =>
		page.evaluate((k) => {
			const md = (raw: string | null) => (raw ? (JSON.parse(raw) as { markdown: string }).markdown : null);
			return { pending: md(localStorage.getItem(k)), held: md(localStorage.getItem(`${k}:held`)) };
		}, key);

	// D: text kept by an earlier tab, with no base (a refused unload save).
	const D = 'Draft D from an earlier tab.';
	const ctx = await browser.newContext();
	await ctx.addInitScript(([k, v]) => localStorage.setItem(k, v), [key, JSON.stringify({ markdown: D, savedAt: Date.now() })] as const);
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
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*`, (route) =>
		cut && route.request().method() === 'PATCH' ? route.abort() : route.continue(),
	);
	await browserLogin(page);
	await page.goto(url);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.getByTestId('refused-raw-draft')).toBeVisible({ timeout: SYNC_TIMEOUT });
	expect(await slots(page), 'the shown draft is held').toEqual({ pending: null, held: D });

	// E: typing while the socket is cut is at risk, so the keeper keeps it.
	cut = true;
	await expect(page.locator(SYNCED_BADGE_SELECTOR), 'premise: the socket is gone').toBeHidden({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' Typed E while D is shown.');
	await expect
		.poll(async () => (await slots(page)).pending, { timeout: SYNC_TIMEOUT, message: 'the keeper keeps E' })
		.toContain('Typed E while D is shown.');
	expect((await slots(page)).held, 'the keeper left D alone').toBe(D);

	// The tab dies before D is resolved; the profile's storage survives.
	const state = await ctx.storageState();
	await ctx.close({ runBeforeUnload: false });
	expect((await read()).content, 'premise: the server never got E').not.toContain('Typed E');

	const again = await (await browser.newContext({ storageState: state })).newPage();
	await browserLogin(again);
	await again.goto(url);
	await expect(again.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	const notice = again.getByTestId('refused-raw-draft');
	await expect(notice).toBeVisible({ timeout: SYNC_TIMEOUT });
	// Restore writes the SHOWN draft as the body: D, not E. The dead tab never
	// flushed its collab row, so the server asks first (BUG-3230's dialog);
	// restoring deliberately overwrites it.
	await notice.getByRole('button', { name: 'Restore it' }).click();
	const ask = again.getByRole('dialog').filter({ hasText: 'Unsaved edits in an open tab' });
	await expect(ask, 'premise: the dead tab left an unflushed row').toBeVisible({ timeout: SYNC_TIMEOUT });
	await ask.getByRole('button', { name: 'Overwrite them' }).click();
	await expect.poll(async () => (await read()).content, { timeout: SYNC_TIMEOUT, message: 'D is what was offered and restored' }).toContain(D);
	expect((await read()).content).not.toContain('Typed E');
	await expect.poll(() => slots(again)).toMatchObject({ held: null, pending: expect.stringContaining('Typed E while D is shown.') });
	const afterRestore = await again.context().storageState();
	await again.context().close();

	// D resolved: the next open offers E.
	const third = await (await browser.newContext({ storageState: afterRestore })).newPage();
	await browserLogin(third);
	await third.goto(url);
	await expect(third.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(third.getByTestId('refused-raw-draft')).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect.poll(() => slots(third)).toMatchObject({ pending: null, held: expect.stringContaining('Typed E while D is shown.') });
	await third.context().close();
});
