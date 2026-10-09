// BUG-3526: a tab whose edit went OUT on a socket that died before the server
// stored it, and that then reconnects to a pruned op-log, is force_refreshed.
// TASK-2199 hands a discarded version back (a notice with Copy), but it keyed
// only on edits made while NO socket was open, so this edit was dropped
// without a word. The tab's text is now handed back here too.
//
// The lost frames are made deterministic with routeWebSocket: once `drop` is
// set, nothing the page sends reaches the server, which is what a socket dying
// with frames in flight looks like to the server. The prune is a direct API
// write with no tab connected, which prunes the op-log (the same
// force_refresh a dormancy prune causes, without waiting for the sweep).
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';

test('an edit lost with its socket is handed back after a force_refresh', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(120_000);
	const { slug } = await seedDoc(fixture, request, 'BUG-3526');
	const ctx = await browser.newContext();
	const page = await ctx.newPage();
	let drop = false;
	let sockets = 0;
	await page.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
		sockets++;
		const server = ws.connectToServer();
		server.onMessage((m) => ws.send(m));
		ws.onMessage((m) => {
			if (!drop) server.send(m);
		});
	});
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	// A stored edit first, so the tab holds a cursor above 0: a reconnect with
	// a cursor onto an empty op-log is what the server force_refreshes.
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.type('Stored first. ');
	await page.waitForTimeout(3_000);

	// The edit goes out on the socket and never arrives.
	drop = true;
	await page.keyboard.type('Sent but never stored.');
	// The socket dies (the tab goes offline) before any barrier could confirm it.
	await ctx.setOffline(true);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeHidden({ timeout: SYNC_TIMEOUT });

	// While the tab is away, a direct write replaces the body and prunes the
	// op-log, so the tab's cursor names rows that no longer exist.
	const res = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { content: 'Server body written while the tab was away.' },
	});
	expect(res.ok(), `the direct write failed: ${res.status()}`).toBeTruthy();

	drop = false;
	const before = sockets;
	await ctx.setOffline(false);
	await expect.poll(() => sockets, { timeout: SYNC_TIMEOUT, message: 'premise: the tab reconnected' }).toBeGreaterThan(before);

	// The editor is rebuilt from the server's body, and the tab's version is
	// handed back rather than dropped.
	await expect(page.locator(EDITOR_SELECTOR)).toContainText('Server body written while the tab was away.', { timeout: SYNC_TIMEOUT });
	const notice = page.locator('.offline-recovery');
	await expect(notice, 'the lost edit must be handed back').toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(notice.locator('.offline-recovery-text')).toContainText('Sent but never stored.');
	await ctx.close();
});
