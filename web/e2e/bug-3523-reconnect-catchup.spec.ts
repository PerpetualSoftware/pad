// BUG-3523: a reconnect appended the WHOLE document to the op-log every time
// (a laptop wake, a network blip), and every joining tab's SyncStep2 answer was
// counted as content. Now:
//   - the reconnect catch-up is sent only when this tab's edits can be missing
//     on the server (an offline edit, or a socket that died with sends a
//     barrier_ack had not confirmed);
//   - a SyncStep2 answer is stored but not counted (content_bearing = 0).
//
// The client leg reads the frames the page actually sends. The server leg
// reads item_yjs_updates from the e2e instance's SQLite file, read-only.
import { test, expect } from './fixtures';
import type { BrowserContext, Page, WebSocket } from '@playwright/test';
import { DatabaseSync } from 'node:sqlite';
import { E2E_DB_PATH } from './lib/data-dir';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';

const DB_PATH = E2E_DB_PATH;

/** Every collab socket the page opens, with what it sent and received. */
function recordSockets(page: Page) {
	const sockets: { sent: (Buffer | string)[]; received: string[] }[] = [];
	page.on('websocket', (ws: WebSocket) => {
		if (!ws.url().includes('/api/v1/collab/')) return;
		const rec = { sent: [] as (Buffer | string)[], received: [] as string[] };
		sockets.push(rec);
		ws.on('framesent', (f) => rec.sent.push(f.payload));
		ws.on('framereceived', (f) => {
			if (typeof f.payload === 'string') rec.received.push(f.payload);
		});
	});
	return sockets;
}

/** The y-protocols sync subtypes (0 step1, 1 step2, 2 update) a socket sent. */
function syncSubtypes(sent: (Buffer | string)[]): number[] {
	return sent.flatMap((p) => (typeof p !== 'string' && p.length > 1 && p[0] === 0 ? [p[1]] : []));
}

async function openItem(ctx: BrowserContext, url: string) {
	const page = await ctx.newPage();
	const sockets = recordSockets(page);
	await browserLogin(page);
	await page.goto(url);
	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return { page, sockets };
}

async function typeAtEnd(page: Page, text: string) {
	await page.locator(EDITOR_SELECTOR).click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(text);
}

/** Drop the network and bring it back; resolves once a NEW socket is synced. */
async function blip(ctx: BrowserContext, page: Page, sockets: unknown[], whileOffline?: () => Promise<void>) {
	const before = sockets.length;
	await ctx.setOffline(true);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeHidden({ timeout: SYNC_TIMEOUT });
	if (whileOffline) await whileOffline();
	await ctx.setOffline(false);
	await expect.poll(() => sockets.length, { timeout: SYNC_TIMEOUT, message: 'a new collab socket opened' }).toBeGreaterThan(before);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
}

test('a reconnect sends the catch-up only when an edit can be missing', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(120_000);
	const { slug } = await seedDoc(fixture, request, 'BUG-3523 reconnect');
	const ctx = await browser.newContext();
	const { page, sockets } = await openItem(ctx, `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);

	await typeAtEnd(page, 'Typed while connected.');
	// The barrier goes out once the sends go quiet, and the server confirms it.
	await expect
		.poll(() => sockets.at(-1)!.received.some((m) => JSON.parse(m).type === 'barrier_ack' && JSON.parse(m).ok === true), {
			timeout: 15_000,
			message: 'the server acknowledged the barrier',
		})
		.toBe(true);

	// Nothing of ours can be missing: the new socket asks for state (step1)
	// and sends no update.
	await blip(ctx, page, sockets);
	await page.waitForTimeout(1_000);
	const quiet = syncSubtypes(sockets.at(-1)!.sent);
	expect(quiet, 'premise: the new socket synced').toContain(0);
	expect(quiet.filter((t) => t === 2), 'no catch-up after a confirmed socket').toEqual([]);

	// Control: an edit made offline IS caught up, as one update.
	await blip(ctx, page, sockets, () => typeAtEnd(page, ' Typed offline.'));
	await page.waitForTimeout(1_000);
	expect(syncSubtypes(sockets.at(-1)!.sent).filter((t) => t === 2), 'the offline edit is sent as one catch-up').toHaveLength(1);
	await expect(page.locator(EDITOR_SELECTOR)).toContainText('Typed offline.');
	await ctx.close();
});

test("a joining tab's step2 answer is stored but not counted as content", async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(120_000);
	const { id, slug } = await seedDoc(fixture, request, 'BUG-3523 step2');
	const url = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`;
	const ca = await browser.newContext();
	const cb = await browser.newContext();
	const a = await openItem(ca, url);
	await typeAtEnd(a.page, 'Alpha typed before Bravo joined.');
	const b = await openItem(cb, url);
	await expect(b.page.locator(EDITOR_SELECTOR)).toContainText('Alpha typed before Bravo joined.');

	const db = new DatabaseSync(DB_PATH, { readOnly: true });
	try {
		const step2Rows = () =>
			(db.prepare('SELECT update_data, content_bearing FROM item_yjs_updates WHERE item_id = ? ORDER BY id').all(id) as {
				update_data: Uint8Array;
				content_bearing: number;
			}[]).filter((r) => r.update_data[0] === 0 && r.update_data[1] === 1);
		// PREMISE: A answered B's step1, and the answer was stored.
		await expect.poll(() => step2Rows().length, { timeout: 10_000, message: "premise: A's step2 answer is in the op-log" }).toBeGreaterThan(0);
		expect(step2Rows().map((r) => r.content_bearing), 'every step2 answer is stored as non-content').toEqual(step2Rows().map(() => 0));
	} finally {
		db.close();
	}
	await ca.close();
	await cb.close();
});
