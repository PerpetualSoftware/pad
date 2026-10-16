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
type Kept = { markdown: string; savedAt: number; baseSeq: number; baseBody: string };

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
	// The base is the editor's form, without the typing.
	expect(k.baseBody, 'the base is the editor snapshot, not the raw row').toBe(RAW.replace(/\n$/, ''));
	expect(k.baseBody).not.toContain(TYPED.trim());

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

// Lead ruling 0337bf77: a rich draft's anchors are in the editor's form, and
// only an open editor applies a patch to the document in that form (with none,
// the server matches the stored row, where a raw-only form like underscore
// emphasis makes them miss). So the patch restore waits until this tab's
// editor is mounted and its CURRENT connection is synced, says why while it
// waits, and always offers Copy. Disconnect disables it again; resync enables.
const RAW_EM = 'Paragraph one.\n\nAn _emphasised_ word here.\n';
const EM_TYPED = ' Typed after the emphasis.';

test('a patch restore waits for the editor: disabled with the reason while not synced, enabled on (re)sync, and lands on a raw-only line', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a restore flow');
	test.setTimeout(180_000);
	const headers = authJson(fixture as never);
	const itemUrl = (id: string) => `/api/v1/workspaces/${fixture.workspaceSlug}/items/${id}`;
	const created = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers,
		data: { title: `BUG-3850 restore gate ${Date.now()}`, content: RAW_EM },
	});
	expect(created.ok(), await created.text()).toBeTruthy();
	const doc = (await created.json()) as { id: string; ref: string };
	const read = async () => (await (await request.get(itemUrl(doc.id), { headers })).json()) as { content: string; seq: number };
	expect((await read()).content, 'premise: the row keeps the underscores').toBe(RAW_EM);
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
	await page.waitForTimeout(4_500);
	cut = true;
	await expect(page.locator(SYNCED_BADGE_SELECTOR), 'premise: the socket is gone').toBeHidden({ timeout: SYNC_TIMEOUT });
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(EM_TYPED);

	let kept: Kept | null = null;
	await expect
		.poll(async () => {
			const raw = await page.evaluate((k) => localStorage.getItem(k), key);
			kept = raw ? (JSON.parse(raw) as Kept) : null;
			return kept?.markdown ?? '';
		}, { timeout: SYNC_TIMEOUT, message: 'the crash draft is kept while the edits are at risk' })
		.toContain(EM_TYPED.trim());
	const k = kept as unknown as Kept;
	expect(k.baseBody, 'premise: the snapshot writes the emphasis with asterisks').toContain('*emphasised*');
	expect(k.baseBody).not.toContain('_emphasised_');

	const state = await ctx.storageState();
	await ctx.close({ runBeforeUnload: false });

	// Seq moves without touching the body, so restore goes as a patch onto a
	// row that still holds the raw-only form.
	const res = await request.patch(itemUrl(doc.id), { headers, data: { title: `BUG-3850 restore gate moved ${Date.now()}` } });
	expect(res.ok(), await res.text()).toBeTruthy();
	const moved = await read();
	expect(moved.content, 'premise: the stored body is still the raw row').toBe(RAW_EM);
	expect(moved.seq, 'premise: seq moved past the base, so restore goes as a patch').toBeGreaterThan(k.baseSeq);

	const again = await (await browser.newContext({ storageState: state })).newPage();
	let blocked = true;
	const open: Array<() => void> = [];
	await again.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
		if (blocked) {
			ws.close();
			return;
		}
		const server = ws.connectToServer();
		server.onMessage((m) => ws.send(m));
		ws.onMessage((m) => server.send(m));
		ws.onClose(() => server.close());
		server.onClose(() => ws.close());
		open.push(() => server.close());
	});
	const patches: string[] = [];
	again.on('request', (r) => {
		if (r.method() === 'POST' && r.url().endsWith(`/items/${doc.id}/patch`)) patches.push(r.url());
	});
	await browserLogin(again);
	await again.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	const notice = again.getByTestId('refused-raw-draft');
	const restore = notice.getByRole('button', { name: 'Restore it' });
	const copy = notice.getByRole('button', { name: 'Copy it' });
	const waiting = again.getByTestId('refused-raw-draft-waiting');

	// No connection yet: Restore waits and says why; Copy is offered.
	await expect(notice).toContainText('restoring applies only your changes', { timeout: SYNC_TIMEOUT });
	await expect(waiting).toContainText('once the editor has connected');
	await expect(restore).toBeDisabled();
	await expect(copy).toBeEnabled();

	// Synced: enabled.
	blocked = false;
	await expect(again.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: 60_000 });
	await expect(restore).toBeEnabled();
	await expect(waiting).toBeHidden();

	// This connection drops: disabled again, though the provider has synced before.
	blocked = true;
	for (const close of open.splice(0)) close();
	await expect(again.locator(SYNCED_BADGE_SELECTOR)).toBeHidden({ timeout: SYNC_TIMEOUT });
	await expect(restore).toBeDisabled();
	await expect(waiting).toBeVisible();
	await expect(copy).toBeEnabled();

	// Resynced: enabled, and the restore lands through the open editor on the raw-only line.
	blocked = false;
	await expect(again.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: 60_000 });
	await expect(restore).toBeEnabled();
	expect(patches, 'nothing was sent while Restore waited').toHaveLength(0);
	await restore.click();

	await expect(again.getByTestId('restored-draft-result')).toContainText(EM_TYPED.trim());
	expect(patches).toHaveLength(1);
	await expect
		.poll(async () => (await read()).content, { timeout: SYNC_TIMEOUT, message: 'the row holds the typing once the editor flushes' })
		.toContain(`*emphasised* word here.${EM_TYPED}`);
	expect((await read()).content).toContain('Paragraph one.');
	await expect.poll(() => again.evaluate((key) => localStorage.getItem(key), key)).toBeNull();
	await again.context().close();
});
