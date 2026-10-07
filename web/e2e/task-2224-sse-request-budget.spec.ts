import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test';
import { quietCrossActorToasts } from './fixtures';

// TASK-2224: what a tab spends on the API when someone else changes the
// workspace. Measured on main (05c039f1) before the fix, one tab on the tasks
// page: one external update = 3 requests; a 20-update agent burst = 40 (one
// /collections and one /items-changes per update, nothing coalesced); 5
// comments on ANOTHER item with a pane open = 7, two of them refetching the
// open item's timeline. The bounds below are what the fix must hold, with
// room for a slower CI box; each is far below the measured value it replaces.

const PASSWORD = 'Playwright-Tabs-2026!';

interface Account {
	username: string;
	token: string;
	api: APIRequestContext;
}

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`measure seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	return resp;
}

async function mintAccount(fixture: SuiteFixture, tag: string): Promise<Account> {
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	try {
		const username = `ms${tag}`;
		const email = `${username}@example.com`;
		// Admin-created accounts are verified, so they can log in at once.
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Measure ${tag}`, password: PASSWORD } }),
			'register'
		);
		return await accountFor(fixture, username, email, tag);
	} finally {
		await admin.dispose();
	}
}

/** An API handle for an account that already exists, by signing in as it. */
async function accountFor(fixture: SuiteFixture, username: string, email: string, tag: string): Promise<Account> {
	const session = await request.newContext({ baseURL: fixture.baseURL });
	try {
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		const csrf = (await session.storageState()).cookies.find(
			(c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf'
		);
		if (!csrf) throw new Error('measure seed: login issued no pad_csrf cookie');
		const tokenResp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `ms-${tag}`, expires_in: 1 }
			}),
			'token'
		);
		const { token } = (await tokenResp.json()) as { token: string };
		const api = await request.newContext({
			baseURL: fixture.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${token}` }
		});
		return { username, token, api };
	} finally {
		await session.dispose();
	}
}

async function createWorkspace(account: Account, name: string): Promise<string> {
	const resp = await ok(await account.api.post('/api/v1/workspaces', { data: { name } }), `create ${name}`);
	return ((await resp.json()) as { slug: string }).slug;
}

async function actAs(context: BrowserContext, account: Account) {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
	// Timestamp every SSE event the PAGE receives, by type (codex r5): the
	// budget's windows are decided by when events arrive in the browser, not
	// by when the REST calls that caused them returned.
	await context.addInitScript(() => {
		const w = window as unknown as { __sseArrivals: Array<[string, number]> };
		w.__sseArrivals = [];
		const add = EventSource.prototype.addEventListener;
		EventSource.prototype.addEventListener = function (this: EventSource, type: string, listener: unknown, opts?: unknown) {
			if (typeof listener === 'function') {
				const inner = listener as (e: Event) => void;
				const wrapped = function (this: EventSource, e: Event) {
					w.__sseArrivals.push([type, Date.now()]);
					return inner.call(this, e);
				};
				return add.call(this, type, wrapped as EventListener, opts as AddEventListenerOptions);
			}
			return add.call(this, type, listener as EventListener, opts as AddEventListenerOptions);
		} as typeof EventSource.prototype.addEventListener;
	});
}

/** Arrival times of `type` events delivered to the page since `since` (page clock). */
async function arrivals(page: Page, type: string, since: number): Promise<number[]> {
	return page.evaluate(
		([t, s]) => (window as unknown as { __sseArrivals: Array<[string, number]> }).__sseArrivals
			.filter(([k, at]) => k === t && at >= s)
			.map(([, at]) => at),
		[type, since] as [string, number],
	);
}

function recorder(page: Page) {
	const log: string[] = [];
	const itemsChangesAt: number[] = [];
	let lastAt = Date.now();
	page.on('request', (r) => {
		const u = new URL(r.url());
		if (!u.pathname.startsWith('/api/v1/') || u.pathname.startsWith('/api/v1/events')) return;
		const shape = u.pathname.replace(/\/items\/[^/]+/, '/items/:ref').replace(/\/workspaces\/[^/]+/, '/workspaces/:ws');
		log.push(`${r.method()} ${shape}${u.search.includes('since') ? '?since' : ''}`);
		if (u.pathname.endsWith('/items-changes')) itemsChangesAt.push(Date.now());
		lastAt = Date.now();
	});
	return {
		/** Wait until the tab has issued no API request for `quietMs` (codex r4: late events must count). */
		async quiet(quietMs: number, maxMs: number) {
			const start = Date.now();
			while (Date.now() - lastAt < quietMs && Date.now() - start < maxMs) await sleep(200);
		},
		/** Was an /items-changes read issued at or after `t`? */
		readSince(t: number) {
			return itemsChangesAt.some((at) => at >= t);
		},
		take() {
			const counts: Record<string, number> = {};
			for (const l of log.splice(0)) counts[l] = (counts[l] ?? 0) + 1;
			return counts;
		}
	};
}

const total = (c: Record<string, number>) => Object.values(c).reduce((a, b) => a + b, 0);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

test('TASK-2224: requests per external update on the collection page, and under a 20-update burst', async ({ page, context, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium');
	test.setTimeout(120_000);
	const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
	const account = await mintAccount(fixture, tag);
	const ws = ((await (await ok(await account.api.post('/api/v1/workspaces', { data: { name: `M ${tag}`, template: 'startup' } }), 'ws')).json()) as { slug: string }).slug;
	const slugs: string[] = [];
	for (let i = 0; i < 21; i++) {
		const it = (await (await ok(await account.api.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title: `T${i}`, fields: '{"status":"open"}' } }), 'item')).json()) as { slug: string };
		slugs.push(it.slug);
	}
	await actAs(context, account);
	await page.setViewportSize({ width: 1280, height: 800 });
	const rec = recorder(page);
	await page.goto(`/${account.username}/${ws}/tasks`);
	await page.getByText('T0', { exact: true }).first().waitFor();
	await sleep(4000);
	console.log('MEASURE baseline (load + 4s idle):', total(rec.take()));

	await sleep(3000);
	const idle = rec.take();
	console.log('MEASURE idle 3s:', total(idle), JSON.stringify(idle));

	await ok(await account.api.patch(`/api/v1/workspaces/${ws}/items/${slugs[0]}`, { data: { fields_patch: { status: 'in-progress' } } }), 'update');
	await sleep(4000);
	const one = rec.take();
	console.log('MEASURE one external update:', total(one), JSON.stringify(one));
	expect(total(one), 'one external update').toBeLessThanOrEqual(3);
	expect(total(one), 'control: the tab heard the update at all').toBeGreaterThan(0);

	// Concurrently. The server and the browser may still spread the events on
	// a slow box, and every max-wait window (1 s) is a legitimate run, so the
	// bounds below scale with how the 20 events actually ARRIVED (codex r5).
	const pageNow = () => page.evaluate(() => Date.now());
	const burstStartAt = await pageNow();
	await Promise.all(
		slugs.slice(1, 21).map(async (slug) =>
			ok(await account.api.patch(`/api/v1/workspaces/${ws}/items/${slug}`, { data: { fields_patch: { status: 'in-progress' } } }), 'burst')
		)
	);
	// All 20 delivered to the page, then the windows from first to last arrival.
	let arrived: number[] = [];
	await expect
		.poll(async () => (arrived = await arrivals(page, 'item_updated', burstStartAt)).length, { timeout: 20_000 })
		.toBeGreaterThanOrEqual(20);
	const spanMs = Math.max(...arrived) - Math.min(...arrived);
	const windows = Math.ceil(spanMs / 1000) + 1;
	const burstDoneAt = Math.max(...arrived);
	// The gate only discriminates if the burst is short against the 20 events
	// it would otherwise cost; on a box too slow for that, it says so.
	// The budget discriminates only for events delivered within 3 s (windows
	// <= 4, so every allowance stays under main's value: collections 5 < 20,
	// items-changes 10 < 20, total 15 < 40; codex r3). A box too slow for that
	// is not a failure of the code under test, so it records why and skips
	// only the budget; the catch-up check below still runs (codex r4).
	const discriminates = spanMs < 3_000;
	if (!discriminates) {
		testInfo.annotations.push({ type: 'skipped-budget', description: `events arrived over ${spanMs} ms` });
	}
	// The tab must catch up with the LAST update: a reconcile read issued
	// after the burst finished (codex r1: a title still on screen proved nothing).
	// 20 s: a reconcile may queue behind one already in flight for up to 15 s.
	await expect.poll(() => rec.readSince(burstDoneAt), { timeout: 20_000 }).toBe(true);
	// Every event has arrived; let the last coalescing window (1 s max) fire
	// and its requests finish before counting.
	await sleep(1500);
	await rec.quiet(1000, 20_000);
	const burst = rec.take();
	console.log('MEASURE 20-update burst:', total(burst), JSON.stringify(burst));
	const kind = (k: string) => Object.entries(burst).filter(([p]) => p.includes(k)).reduce((a, [, n]) => a + n, 0);
	// One reload and one two-read reconcile per max-wait window the burst
	// spanned, plus one of each trailing it. Measured on a quiet box: 1 and 2.
	if (discriminates) expect.soft(kind('/collections'), `collection reloads for a 20-update burst over ${windows} window(s) (main: 20)`).toBeLessThanOrEqual(windows + 1);
	if (discriminates) expect.soft(kind('/items-changes'), `items-changes reads for a 20-update burst over ${windows} window(s) (main: 20)`).toBeLessThanOrEqual(2 * (windows + 1));
	if (discriminates) expect.soft(total(burst), `requests for a 20-update burst over ${windows} window(s) (main: 40)`).toBeLessThanOrEqual(3 * (windows + 1));


	// Item pane open on T0; comments land on a DIFFERENT item.
	await page.goto(`/${account.username}/${ws}/tasks/${slugs[0]}`);
	await sleep(6000);
	console.log('MEASURE pane load:', total(rec.take()));
	for (let i = 0; i < 5; i++) {
		await ok(await account.api.post(`/api/v1/workspaces/${ws}/items/${slugs[5]}/comments`, { data: { body: `c${i}` } }), 'comment');
	}
	await sleep(5000);
	const comments = rec.take();
	console.log('MEASURE 5 comments on another item, pane open:', total(comments), JSON.stringify(comments));
	const timelineFetches = Object.entries(comments).filter(([p]) => p.includes('/timeline')).reduce((a, [, n]) => a + n, 0);
	expect.soft(timelineFetches, 'the open item refetched its timeline for comments on another item (main: 2)').toBe(0);
	await account.api.dispose();
});
