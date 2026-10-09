import { test, expect, quietCrossActorToasts } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, expectEditorMounted } from './lib/collab-helpers';

/**
 * TASK-2206 (audit C88): a peer that goes away without saying so used to leave
 * its caret and name label in everyone else's document until y-protocols'
 * 30-second timeout (the audit measured 23.8s after a hard tab close). The
 * collab room now records whose presence each connection carries and, when
 * the connection closes, tells the room that client is gone.
 *
 * The peer's whole browser context is closed, which runs no beforeunload: the
 * provider gets no chance to announce its own departure, as with a crash or a
 * dropped network. The caret must go in seconds, not at the timeout.
 */
test('a peer that drops without saying goodbye loses its caret at once', async ({
	page,
	fixture,
	request,
	browser,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'two live collab peers; one viewport is enough');
	test.setTimeout(60_000);

	const ws = fixture.workspaceSlug;
	const resp = await request.post(`/api/v1/workspaces/${ws}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title: `TASK-2206 presence ${Date.now()}`, fields: '{}', content: 'A paragraph the peer clicks into.\n' },
	});
	expect(resp.ok()).toBe(true);
	const { slug } = (await resp.json()) as { slug: string };
	const path = `/${fixture.adminUsername}/${ws}/docs/${slug}`;

	await browserLogin(page);
	await page.goto(path);
	await expectEditorMounted(page);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();

	const peerContext = await browser.newContext({ baseURL: fixture.baseURL });
	await quietCrossActorToasts(peerContext);
	const peer = await peerContext.newPage();
	await browserLogin(peer);
	await peer.goto(path);
	await expect(peer.locator(SYNCED_BADGE_SELECTOR)).toBeVisible();
	const para = peer.locator(`${EDITOR_SELECTOR} p`).first();
	const box = await para.boundingBox();
	if (!box) throw new Error('peer paragraph has no bounding box');
	await peer.mouse.click(box.x + box.width / 2, box.y + box.height / 2);

	const caret = page.locator(`${EDITOR_SELECTOR} .collaboration-carets__caret`);
	await expect(caret).toHaveCount(1, { timeout: 10_000 });

	const closedAt = Date.now();
	await peerContext.close();
	// Well inside the 30s timeout the caret used to wait for.
	await expect(caret).toHaveCount(0, { timeout: 5_000 });
	testInfo.annotations.push({ type: 'caret-gone-after-ms', description: String(Date.now() - closedAt) });
});
