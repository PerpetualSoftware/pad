import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3240: an item's body stored TWICE by the collab lazy seed.
 *
 * The seed (TASK-1261) writes items.content into a Y.Doc that is empty once
 * the sync completes. Two routes ran it when they must not:
 *  - the sync "completed" on a 1s timer, so a replay slower than that (a slow
 *    link, a long op-log under load) arrived after the seed and merged the
 *    ORIGINAL seed ops beside it;
 *  - two tabs opening a never-opened doc elected a seeder by awareness, which
 *    had not arrived yet, so both seeded.
 * The sync now completes on the relay's post-replay op_log_cursor frame, and
 * the relay elects one seeder per room. Every leg asserts the STORED body.
 */

const auth = (f: SuiteFixture) => ({ Authorization: `Bearer ${f.apiToken}`, 'Content-Type': 'application/json' });

async function freshDoc(fixture: SuiteFixture, request: APIRequestContext, tag: string) {
	const marker = `${tag}${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
	const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: auth(fixture),
		data: { title: `B3240 ${marker}`, content: `Body ${marker}.` },
	});
	expect(r.ok(), await r.text()).toBeTruthy();
	const item = (await r.json()) as { slug: string };
	return { marker, url: `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${item.slug}`, slug: item.slug };
}

const shownCount = (page: Page, marker: string) =>
	page.evaluate((m) => (document.querySelector('.ProseMirror')?.textContent ?? '').split(m).length - 1, marker);

async function storedCount(fixture: SuiteFixture, request: APIRequestContext, slug: string, marker: string) {
	const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, { headers: auth(fixture) });
	return ((await r.json()) as { content: string }).content.split(marker).length - 1;
}

test.describe('BUG-3240: the lazy seed stores the body once', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'a sync protocol property; one browser is enough');
		test.setTimeout(90_000);
	});

	test('a reopen whose replay arrives late does not seed again', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await freshDoc(fixture, request, 'slow');
		// First open, normal link: this tab seeds the op-log.
		await page.goto(doc.url);
		await expect.poll(() => shownCount(page, doc.marker), { timeout: 15_000 }).toBe(1);
		await page.waitForTimeout(1500);
		await page.goto('about:blank');
		await page.waitForTimeout(800);
		// Reopen over a slow link: every server frame 1.5s late, order kept,
		// so the replay lands after the old 1s grace.
		await page.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
			const server = ws.connectToServer();
			let chain = Promise.resolve();
			server.onMessage((m) => {
				const at = Date.now() + 1500;
				chain = chain.then(async () => {
					await new Promise((x) => setTimeout(x, Math.max(0, at - Date.now())));
					ws.send(m);
				});
			});
			ws.onMessage((m) => server.send(m));
		});
		await page.goto(doc.url);
		await expect.poll(() => shownCount(page, doc.marker), { timeout: 15_000 }).toBeGreaterThan(0);
		await page.waitForTimeout(2500);
		expect(await shownCount(page, doc.marker), 'the editor shows the body twice').toBe(1);
		// Past the collab flush.
		await expect.poll(() => storedCount(fixture, request, doc.slug, doc.marker), { timeout: 10_000, intervals: [7000, 1000] }).toBe(1);
	});

	test('two tabs opening a never-opened doc together seed it once', async ({ browser, fixture, request }) => {
		const doc = await freshDoc(fixture, request, 'two');
		const ctxs = await Promise.all([browser.newContext(), browser.newContext()]);
		try {
			const pages = await Promise.all(ctxs.map((c) => c.newPage()));
			await Promise.all(pages.map((p) => browserLogin(p)));
			await Promise.all(pages.map((p) => p.goto(doc.url)));
			for (const p of pages) await expect.poll(() => shownCount(p, doc.marker), { timeout: 15_000 }).toBeGreaterThan(0);
			await pages[0].waitForTimeout(2500);
			expect(await Promise.all(pages.map((p) => shownCount(p, doc.marker))), 'a tab shows the body twice').toEqual([1, 1]);
			await expect.poll(() => storedCount(fixture, request, doc.slug, doc.marker), { timeout: 10_000, intervals: [7000, 1000] }).toBe(1);
		} finally {
			await Promise.all(ctxs.map((c) => c.close()));
		}
	});

	test('the seed passes on when the elected seeder leaves before its seed reaches the server', async ({ browser, fixture, request }) => {
		const doc = await freshDoc(fixture, request, 'rel');
		// Tab A is elected (first to connect) but nothing it sends reaches the
		// server, so its seed never lands; then it closes.
		const a = await browser.newContext();
		const pa = await a.newPage();
		await browserLogin(pa);
		let granted = false;
		await pa.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
			const server = ws.connectToServer();
			server.onMessage((m) => {
				if (typeof m === 'string' && m.includes('"seed":true')) granted = true;
				ws.send(m);
			});
			ws.onMessage(() => {});
		});
		await pa.goto(doc.url);
		await expect.poll(() => granted, { timeout: 15_000, message: 'premise: tab A was elected' }).toBe(true);
		await a.close();
		// Tab B opens next and must seed, exactly once.
		const b = await browser.newContext();
		try {
			const pb = await b.newPage();
			await browserLogin(pb);
			await pb.goto(doc.url);
			await expect.poll(() => shownCount(pb, doc.marker), { timeout: 15_000 }).toBe(1);
			await expect.poll(() => storedCount(fixture, request, doc.slug, doc.marker), { timeout: 10_000, intervals: [7000, 1000] }).toBe(1);
		} finally {
			await b.close();
		}
	});
});
