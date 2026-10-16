import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * BUG-3850: a body written through the API with a trailing newline (as the CLI
 * and MCP write them) is not the editor's serialization of it, and a flush
 * stores the editor's. A crash draft anchored on the raw row then missed the
 * flushed body, and Restore reported a false overlap. The rich draft is now
 * anchored on the editor's own last confirmed snapshot.
 *
 * The crash is the TASK-3606 one: the socket cut, typing at the end of the
 * last paragraph, the tab closed with no unload. Before reopening, the row is
 * rewritten in the editor's form, as the flush that moved seq in the report
 * did.
 */
type Kept = { markdown: string; savedAt: number; baseSeq: number; baseBody: string; rawBaseBody?: string };

const RAW = 'Paragraph one.\n\nParagraph two.\n';
const TYPED = ' Typed before the crash.';

test('a draft typed at the end of a body stored with a trailing newline restores cleanly', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a restore flow');
	test.setTimeout(150_000);
	const headers = authJson(fixture as never);
	const itemUrl = (id: string) => `/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`;
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers,
		data: { title: `BUG-3850 newline ${Date.now()}`, content: RAW },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const doc = (await created.json()) as { id: string; ref: string };
	const read = async () => (await (await request.get(itemUrl(doc.id), { headers })).json()) as { content: string; seq: number };
	expect((await read()).content, 'premise: the row keeps the trailing newline').toBe(RAW);
	const me = (await (await request.get('/api/v1/auth/me', { headers })).json()) as { id: string };
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
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*`, (route) =>
		cut && route.request().method() === 'PATCH' ? route.abort() : route.continue(),
	);

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	// The keeper takes the confirmed snapshot on its tick (every 2s) while
	// nothing is unconfirmed; give it two ticks before the socket goes.
	await page.waitForTimeout(4_500);
	cut = true;
	await expect(page.locator(SYNCED_BADGE_SELECTOR), 'premise: the socket is gone').toBeHidden({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(TYPED);

	let kept: Kept | null = null;
	await expect
		.poll(async () => {
			const raw = await page.evaluate((k) => localStorage.getItem(k), key);
			kept = raw ? (JSON.parse(raw) as Kept) : null;
			return kept?.markdown ?? '';
		}, { timeout: SYNC_TIMEOUT, message: 'the crash draft is kept while the edits are at risk' })
		.toContain(TYPED.trim());
	const k = kept as unknown as Kept;
	// The base is the editor's form, without the typing; the row is kept beside it.
	expect(k.baseBody, 'the base is the editor snapshot, not the raw row').toBe(RAW.replace(/\n$/, ''));
	expect(k.baseBody).not.toContain(TYPED.trim());
	expect(k.rawBaseBody).toBe(RAW);

	const state = await ctx.storageState();
	await ctx.close({ runBeforeUnload: false });

	// The flush that moved seq in the report: the row in the editor's form.
	const res = await request.patch(itemUrl(doc.id), { headers, data: { content: k.baseBody } });
	expect(res.ok(), await res.text()).toBeTruthy();
	const moved = await read();
	expect(moved.content, 'premise: the server never got the typing').not.toContain(TYPED.trim());
	expect(moved.seq, 'premise: seq moved past the base, so restore goes as a patch').toBeGreaterThan(k.baseSeq);

	const again = await (await browser.newContext({ storageState: state })).newPage();
	await browserLogin(again);
	await again.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(again.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	const notice = again.getByTestId('refused-raw-draft');
	await expect(notice).toContainText('restoring applies only your changes', { timeout: SYNC_TIMEOUT });
	await notice.getByRole('button', { name: 'Restore it' }).click();

	await expect(again.getByTestId('restored-draft-result')).toContainText(TYPED.trim());
	await expect
		.poll(async () => (await read()).content, { timeout: SYNC_TIMEOUT, message: 'the row holds the typing once the editor flushes' })
		.toBe(`Paragraph one.\n\nParagraph two.${TYPED}`);
	await expect.poll(() => again.evaluate((key) => localStorage.getItem(key), key)).toBeNull();
	await again.context().close();
});
