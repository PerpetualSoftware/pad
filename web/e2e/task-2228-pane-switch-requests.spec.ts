import type { APIRequestContext, Page, Request } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { createWorkspace } from './lib/attachment-viewer';

/**
 * TASK-2228 (audit C77): switching the split pane from one item to the next
 * (j/k, or a click) paid duplicate round trips: dependent panels fetched once
 * for the ref-derived slug and again when the resolved id landed, the
 * collection schema was refetched for every item, and progress and links ran
 * one after the other. This records the API requests one switch makes.
 *
 * In its OWN workspace: on the shared suite workspace, other specs' writes
 * reach this page over SSE and a panel legitimately refetches inside a
 * switch, which reads as a duplicate (seen at 8 workers: a second /children).
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext, ws: string) {
	const name = `Switch ${Date.now()}`;
	const schema = JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }] });
	const c = await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: 'SW' + String(Date.now()).slice(-4), schema },
	});
	if (!c.ok()) throw new Error(`collection create: ${c.status()} ${await c.text()}`);
	const coll = (await c.json()) as { slug: string };
	for (let i = 0; i < 5; i++) {
		const r = await request.post(`/api/v1/workspaces/${ws}/collections/${coll.slug}/items`, {
			headers: authHeaders(fixture),
			data: { title: `Switch item ${i}`, fields: JSON.stringify({ status: 'open' }), content: `Body ${i}` },
		});
		if (!r.ok()) throw new Error(`item create: ${r.status()} ${await r.text()}`);
	}
	return coll.slug;
}

const isApi = (r: Request) => {
	const u = new URL(r.url());
	// The SSE stream never finishes, so it is neither counted nor waited on.
	return u.pathname.startsWith('/api/v1/') && !u.pathname.startsWith('/api/v1/events');
};

/**
 * Waits until no API request is in flight and none has started for `quietMs`.
 * NOT `waitForLoadState('networkidle')`: after the first load that state is
 * already reached, so on an in-app navigation it returns at once, and a fixed
 * sleep after it let one switch's slow requests (under 8-worker load) land in
 * the next switch's window.
 */
async function settle(page: Page, quietMs = 500, timeoutMs = 15_000) {
	let inflight = 0;
	let last = Date.now();
	const start = (r: Request) => { if (isApi(r)) { inflight++; last = Date.now(); } };
	const end = (r: Request) => { if (isApi(r)) { inflight = Math.max(0, inflight - 1); last = Date.now(); } };
	page.on('request', start);
	page.on('requestfinished', end);
	page.on('requestfailed', end);
	try {
		const deadline = Date.now() + timeoutMs;
		while (Date.now() < deadline) {
			if (inflight === 0 && Date.now() - last >= quietMs) return;
			await page.waitForTimeout(50);
		}
		throw new Error(`the pane did not settle within ${timeoutMs}ms (${inflight} requests in flight)`);
	} finally {
		page.off('request', start);
		page.off('requestfinished', end);
		page.off('requestfailed', end);
	}
}

/** API requests the page makes while `act` runs and the pane settles, each
 *  with its offset in ms from the start of the window. */
async function apiRequestsDuring(page: Page, act: () => Promise<void>): Promise<{ url: string; at: number }[]> {
	const seen: { url: string; at: number }[] = [];
	const t0 = Date.now();
	const onReq = (r: Request) => {
		if (!isApi(r)) return;
		const u = new URL(r.url());
		seen.push({ url: `${r.method()} ${u.pathname}${u.search}`, at: Date.now() - t0 });
	};
	page.on('request', onReq);
	await act();
	await settle(page);
	page.off('request', onReq);
	return seen;
}

test('a split-pane switch makes each request once', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const ws = (await createWorkspace(fixture, request, 'Pane switch requests')).slug;
	const slug = await seed(fixture, request, ws);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${ws}/${slug}?view=list`);
	const first = page.locator('.item-card').first();
	await expect(first).toBeVisible();
	await first.click();
	await expect(page.locator('.item-pane')).toBeVisible();
	await settle(page);

	const itemParam = () => new URL(page.url()).searchParams.get('item');
	const perSwitch: { url: string; at: number }[][] = [];
	for (let i = 0; i < 4; i++) {
		const before = itemParam();
		perSwitch.push(
			await apiRequestsDuring(page, async () => {
				await page.keyboard.press('j');
				// PREMISE: the switch happened, so the window is one switch's.
				await expect.poll(itemParam).not.toBe(before);
			})
		);
	}
	for (const [i, reqs] of perSwitch.entries()) {
		const lines = reqs.map((r) => `+${r.at}ms ${r.url}`);
		testInfo.annotations.push({ type: `switch-${i + 1}`, description: `${reqs.length}: ${lines.join(' | ')}` });
		console.log(`switch ${i + 1}: ${reqs.length} requests`);
		for (const l of lines) console.log('   ' + l);
	}
	for (const [i, reqs] of perSwitch.entries()) {
		// READS only. The defect was a panel fetching the same thing twice; a
		// write to one URL can legitimately repeat with a different body. The
		// tab bar saves each route it lands on (PATCH /me/workspace-tabs/{ws},
		// debounced), and under load the previous switch's save and this one's
		// both fell inside one window (seen at 8 workers).
		const urls = reqs.filter((r) => r.url.startsWith('GET ')).map((r) => r.url);
		const dupes = urls.filter((u, j) => urls.indexOf(u) !== j);
		expect(dupes, `a request made twice in switch ${i + 1}: ${reqs.map((r) => `+${r.at}ms ${r.url}`).join(' | ')}`).toEqual([]);
	}
});
