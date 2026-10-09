import type { APIRequestContext, Page, WebSocketRoute } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';

/**
 * BUG-3522: the browser refuses a keepalive request whose body is over 64 KiB,
 * and the item pane's teardown flushes went keepalive, so on a large body a
 * pane close or item switch never reached the server (measured: an 87,192-byte
 * PATCH dispatched on a switch, never received). The client now sends keepalive
 * only under a 56 KiB budget and an ordinary request otherwise.
 *
 * The close and switch legs read items.content within 4s of the action, inside
 * the 5s idle flush, so only the teardown flush can satisfy them: on main both
 * fail. The websocket-down leg covers the lead's condition: edits the server
 * does not hold must still raise the unsaved-changes prompt, never be dropped
 * quietly, and arrive once the socket is back.
 */

const COLLAB_WS_RE = /\/api\/v1\/collab\//;

/** ~87 KB of ordinary markdown: headings, paragraphs and lists. */
function largeBody(): string {
	const parts: string[] = [];
	for (let i = 0; parts.join('\n\n').length < 87 * 1024; i++) {
		parts.push(`## Section ${i}`);
		parts.push(`Paragraph ${i} of a long plan, with enough words in it to read like one: the work, its reasons, and what it leaves for later.`);
		parts.push(`- point ${i}.a\n- point ${i}.b\n- point ${i}.c`);
	}
	return parts.join('\n\n');
}

async function seed(fixture: SuiteFixture, request: APIRequestContext, title: string) {
	const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' },
		data: { title, fields: '{}', content: largeBody() },
	});
	if (!r.ok()) throw new Error(`seed: ${r.status()} ${await r.text()}`);
	return (await r.json()) as { slug: string };
}

async function stored(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: { Authorization: `Bearer ${fixture.apiToken}` },
	});
	expect(r.ok()).toBe(true);
	return ((await r.json()) as { content: string }).content;
}

async function open(page: Page, fixture: SuiteFixture, slug: string) {
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return editor;
}

async function typeAtEnd(page: Page, editor: ReturnType<Page['locator']>, marker: string) {
	await editor.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(` ${marker}`);
	await expect(editor).toContainText(marker);
}

test.describe('a large body still flushes (BUG-3522)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the keepalive cap is a browser rule; one project is enough');
		test.setTimeout(90_000);
	});

	test('closing the pane lands the edit in items.content at once', async ({ page, fixture, request }) => {
		const { slug } = await seed(fixture, request, `Large close ${Date.now()}`);
		const marker = `large-close-${Date.now()}`;
		await browserLogin(page);
		const editor = await open(page, fixture, slug);
		await typeAtEnd(page, editor, marker);
		const closedAt = Date.now();
		await page.locator('[aria-label="Close pane"]').first().click();
		await expect(editor).toHaveCount(0);
		// Inside the 5s idle flush: only the teardown flush can have written it.
		await expect.poll(() => stored(request, fixture, slug), { timeout: 4_000 }).toContain(marker);
		expect(Date.now() - closedAt).toBeLessThan(5_000);
	});

	test('switching items lands the outgoing edit in items.content at once', async ({ page, fixture, request }) => {
		const a = await seed(fixture, request, `Large switch A ${Date.now()}`);
		await seed(fixture, request, `Large switch B ${Date.now()}`);
		const marker = `large-switch-${Date.now()}`;
		await browserLogin(page);
		const editor = await open(page, fixture, a.slug);
		await typeAtEnd(page, editor, marker);
		const switchedAt = Date.now();
		await page.locator('.item-card .card-link', { hasText: 'Large switch B' }).first().click();
		await expect.poll(() => stored(request, fixture, a.slug), { timeout: 4_000 }).toContain(marker);
		expect(Date.now() - switchedAt).toBeLessThan(5_000);
	});

	test('with the socket down, leaving asks first, and the edit arrives once it is back', async ({ page, fixture, request }) => {
		const { slug } = await seed(fixture, request, `Large offline ${Date.now()}`);
		const marker = `large-offline-${Date.now()}`;
		const live: WebSocketRoute[] = [];
		let blocked = false;
		await page.routeWebSocket(COLLAB_WS_RE, (ws) => {
			if (blocked) {
				void ws.close();
				return;
			}
			live.push(ws);
			ws.connectToServer();
		});
		await browserLogin(page);
		const editor = await open(page, fixture, slug);

		// Drop the socket and keep it down.
		blocked = true;
		for (const ws of live.splice(0)) await ws.close();
		await expect(page.locator(SYNCED_BADGE_SELECTOR)).toHaveCount(0, { timeout: 15_000 });
		await typeAtEnd(page, editor, marker);

		// Leaving: the browser's unsaved-changes prompt, because the server does
		// not hold these edits. Stay.
		const dialog = page.waitForEvent('dialog');
		await page.close({ runBeforeUnload: true });
		const d = await dialog;
		expect(d.type()).toBe('beforeunload');
		await d.dismiss();
		expect(page.isClosed()).toBe(false);

		// Back online: the edits the tab kept reach the server.
		blocked = false;
		await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: 45_000 });
		await expect.poll(() => stored(request, fixture, slug), { timeout: 30_000 }).toContain(marker);
	});
});
