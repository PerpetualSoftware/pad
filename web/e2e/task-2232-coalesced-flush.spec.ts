import type { Page, Request } from '@playwright/test';
import { test, expect } from './fixtures';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';

/**
 * TASK-2232 (audit C78): the editor's markdown reaches the host COALESCED,
 * 150ms after the last change, instead of on every keystroke (a whole-document
 * serialization measured at 48-73 ms p95 per key on 86-101 KB plans). A
 * keystroke typed inside that window must still persist on every path that
 * ends the editor: closing the pane, switching items, and the page going away.
 *
 * DETERMINISTIC, not a race: the page clock is frozen before typing, so the
 * coalesce timer CANNOT deliver. A close or switch is then given at most 100ms
 * of fake time (under the 150ms window) to tear the editor down, so the only
 * way the typed text can reach the server is the Editor's drain on destroy
 * (or, for pagehide, the handler's live read). Without the drain the
 * collab-snapshot PATCH carries the text from before the typing, and the
 * marker assertion below fails, which is the counterfactual this spec was run
 * against.
 */

const COLLAB_SNAPSHOT_RE = /source=collab-snapshot/;

/** Records whether a collab-snapshot PATCH carrying `marker` has been sent. */
function watchFlush(page: Page, marker: string) {
	const seen = { sent: false };
	page.on('request', (r: Request) => {
		if (COLLAB_SNAPSHOT_RE.test(r.url()) && r.method() === 'PATCH' && (r.postData()?.includes(marker) ?? false)) seen.sent = true;
	});
	return seen;
}

/** Step the frozen clock in 10ms increments, at most `budgetMs` in all,
 *  until `done()`; real time passes between steps for the network. */
async function stepUntil(page: Page, done: () => Promise<boolean> | boolean, budgetMs = 100) {
	for (let t = 0; t < budgetMs; t += 10) {
		if (await done()) return true;
		await page.clock.runFor(10);
		await page.waitForTimeout(150);
	}
	return done();
}

async function openSynced(page: Page, url: string) {
	await page.goto(url);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return editor;
}

async function freezeAndType(page: Page, editor: ReturnType<Page['locator']>, marker: string) {
	await editor.click();
	await page.clock.install();
	await page.clock.pauseAt(Date.now() + 1_000);
	await page.keyboard.type(marker);
	// The keystrokes are in the document (ProseMirror renders synchronously),
	// and the coalesce timer is frozen.
	await expect(editor).toContainText(marker);
}

async function readContent(request: import('@playwright/test').APIRequestContext, fixture: { workspaceSlug: string; apiToken: string }, slug: string) {
	const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}` },
	});
	expect(r.ok()).toBe(true);
	return r.text();
}

test.describe('a keystroke inside the coalesce window persists (TASK-2232)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'collab teardown is viewport-agnostic; one project is enough');
		test.setTimeout(60_000);
	});

	test('closing the pane', async ({ page, fixture, request }) => {
		const { slug } = await seedDoc(fixture, request, 'Coalesce close');
		const marker = `coalesce-close-${Date.now()}`;
		await browserLogin(page);
		const editor = await openSynced(page, `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
		const flush = watchFlush(page, marker);

		await freezeAndType(page, editor, marker);
		// PREMISE: nothing has carried the marker yet; the window is holding it.
		expect(flush.sent).toBe(false);

		await page.locator('[aria-label="Close pane"]').first().click();
		const sentInWindow = await stepUntil(page, () => flush.sent);
		expect(sentInWindow, 'the close flushed the typed text inside the coalesce window').toBe(true);

		await page.clock.resume();
		await expect(editor).toHaveCount(0);
		await expect.poll(() => readContent(request, fixture, slug), { timeout: 15_000 }).toContain(marker);
	});

	test('switching to another item', async ({ page, fixture, request }) => {
		const a = await seedDoc(fixture, request, 'Coalesce switch A');
		await seedDoc(fixture, request, 'Coalesce switch B');
		const marker = `coalesce-switch-${Date.now()}`;
		await browserLogin(page);
		const editor = await openSynced(page, `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${a.slug}`);
		const flush = watchFlush(page, marker);

		await freezeAndType(page, editor, marker);
		expect(flush.sent).toBe(false);

		await page.locator('.item-card .card-link', { hasText: 'Coalesce switch B' }).first().click();
		const sentInWindow = await stepUntil(page, () => flush.sent);
		expect(sentInWindow, 'the switch flushed the outgoing typed text inside the coalesce window').toBe(true);

		await page.clock.resume();
		await expect.poll(() => readContent(request, fixture, a.slug), { timeout: 15_000 }).toContain(marker);
	});

	test('the page going away', async ({ page, fixture, request }) => {
		const { slug } = await seedDoc(fixture, request, 'Coalesce leave');
		const marker = `coalesce-leave-${Date.now()}`;
		await browserLogin(page);
		const editor = await openSynced(page, `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
		const flush = watchFlush(page, marker);

		await freezeAndType(page, editor, marker);
		expect(flush.sent).toBe(false);

		// A real departure: the dirty pane asks before unloading, and leaving
		// is the answer. pagehide / beforeunload flush with keepalive, reading
		// the live editor (one serialization, as before this change), so the
		// write outlives the page.
		page.on('dialog', (d) => void d.accept());
		await page.goto('about:blank');
		await expect.poll(() => readContent(request, fixture, slug), { timeout: 15_000 }).toContain(marker);
	});
});
