// BUG-3571 part 2 (ruling R1): when a session expires under an open item, the
// text the server does not have is kept in this browser under its author's id
// and offered back on their next open. Rich mode is the leg nothing else
// covers: the raw editor's unload save keeps its text before it sends, but
// the collab teardown flush only PATCHes, and on an expired session that
// PATCH is refused and the text is gone.
//
// The expiry is the client's view of one: the flush PATCH is answered 401
// unauthorized, which is what the API client redirects on. The session itself
// stays valid, so the same browser can come back and be offered the text.
// The collab socket is cut first (and reconnects refused), so the typing is
// unsent local edits, the state a sign-out guard calls at risk.
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

test('rich text typed under an expiring session is kept and offered back', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a client-side keep');
	test.setTimeout(120_000);
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title: `BUG-3571 expiry ${Date.now()}`, content: 'Original body.' },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const doc = (await created.json()) as { id: string; ref: string };
	const me = (await (await request.get('/api/v1/auth/me', { headers: authJson(fixture) })).json()) as { id: string };

	const ctx = await browser.newContext();
	const page = await ctx.newPage();
	let cut = false;
	let sockets = 0;
	await page.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
		sockets++;
		if (cut) {
			ws.close();
			return;
		}
		const server = ws.connectToServer();
		server.onMessage((m) => ws.send(m));
		ws.onMessage((m) => {
			if (cut) return;
			server.send(m);
		});
		ws.onClose(() => server.close());
		server.onClose(() => ws.close());
		const timer = setInterval(() => {
			if (!cut) return;
			clearInterval(timer);
			server.close();
		}, 50);
	});
	let expired = 0;
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*`, async (route) => {
		const req = route.request();
		if (!cut || req.method() !== 'PATCH') return route.continue();
		expired++;
		return route.fulfill({
			status: 401,
			contentType: 'application/json',
			body: JSON.stringify({ error: { code: 'unauthorized', message: 'Not logged in' } }),
		});
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	cut = true;
	await expect(page.locator(SYNCED_BADGE_SELECTOR), 'premise: the socket is gone').toBeHidden({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' Typed after the socket died.');

	// The idle flush (5s) PATCHes, meets the 401, and the client sends the
	// tab to /login.
	await page.waitForURL(/\/login/, { timeout: SYNC_TIMEOUT });
	expect(expired, 'premise: the redirect came from the expired PATCH').toBeGreaterThan(0);
	const kept = await page.evaluate((key) => localStorage.getItem(key), `pad:refused-raw-draft:${me.id}:${doc.id}`);
	expect(kept, 'the text must be kept under its author').not.toBeNull();
	expect((JSON.parse(kept!) as { markdown: string }).markdown).toContain('Typed after the socket died.');
	const stored = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { content: string };
	expect(stored.content, 'premise: the server never got it').not.toContain('Typed after the socket died.');

	// The author comes back (the session is still valid here) and is offered it.
	cut = false;
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	const notice = page.getByTestId('refused-raw-draft');
	await expect(notice).toBeVisible({ timeout: SYNC_TIMEOUT });
	expect(sockets).toBeGreaterThan(0);
	await ctx.close();
});
