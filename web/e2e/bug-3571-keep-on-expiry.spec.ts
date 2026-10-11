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
	// The redirect raises the unsaved-changes prompt (the editor still holds
	// text the server does not have). It is DISMISSED here: the keepers run
	// before the navigation starts, so the keep is read on the page as it
	// stands, and Playwright's answer to a prompt on a leaving page races the
	// navigation. The offer is then read from a fresh page.
	let prompts = 0;
	page.on('dialog', (d) => {
		prompts++;
		d.dismiss().catch(() => {});
	});
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

	// The idle flush (5s) PATCHes, meets the 401, and the client runs the
	// keepers and sends the tab to /login.
	const key = `pad:refused-raw-draft:${me.id}:${doc.id}`;
	await expect.poll(() => expired, { timeout: SYNC_TIMEOUT, message: 'premise: a content PATCH met the expiry' }).toBeGreaterThan(0);
	await expect
		.poll(() => page.evaluate((k) => localStorage.getItem(k), key), { timeout: SYNC_TIMEOUT, message: 'the text must be kept under its author' })
		.not.toBeNull();
	const kept = (await page.evaluate((k) => localStorage.getItem(k), key))!;
	expect((JSON.parse(kept) as { markdown: string }).markdown).toContain('Typed after the socket died.');
	const stored = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { content: string };
	expect(stored.content, 'premise: the server never got it').not.toContain('Typed after the socket died.');

	// Now the session REALLY ends: the old page's teardown flush on close is a
	// keepalive request that page.route does not see, so only the server can
	// refuse it, as it does on a real expiry.
	const loggedOut = await page.evaluate(async () => {
		const m = document.cookie.match(/(?:^|;\s*)(?:__Host-)?pad_csrf=([^;]+)/);
		const r = await fetch('/api/v1/auth/logout', { method: 'POST', headers: m ? { 'X-CSRF-Token': m[1] } : {} });
		return r.status;
	});
	expect(loggedOut, 'premise: the session is revoked').toBeLessThan(300);
	await page.close({ runBeforeUnload: false });

	// The author signs back in, opens the item, and is offered the text.
	const again = await ctx.newPage();
	await browserLogin(again);
	await again.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	const notice = again.getByTestId('refused-raw-draft');
	await expect(notice).toBeVisible({ timeout: SYNC_TIMEOUT });
	const after = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { content: string };
	expect(after.content, 'premise: still not on the server, so the offer is the only copy').not.toContain('Typed after the socket died.');
	expect(sockets).toBeGreaterThan(0);
	await ctx.close();
});
