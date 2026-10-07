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
}

function recorder(page: Page) {
	const log: string[] = [];
	const itemsChangesAt: number[] = [];
	page.on('request', (r) => {
		const u = new URL(r.url());
		if (!u.pathname.startsWith('/api/v1/') || u.pathname.startsWith('/api/v1/events')) return;
		const shape = u.pathname.replace(/\/items\/[^/]+/, '/items/:ref').replace(/\/workspaces\/[^/]+/, '/workspaces/:ws');
		log.push(`${r.method()} ${shape}${u.search.includes('since') ? '?since' : ''}`);
		if (u.pathname.endsWith('/items-changes')) itemsChangesAt.push(Date.now());
	});
	return {
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

	// Concurrently, so the burst lands inside one coalescing window however
	// slow the box is (codex r1: a serial loop on a slow CI runner spans
	// several windows, each a legitimate run).
	await Promise.all(
		slugs.slice(1, 21).map(async (slug) =>
			ok(await account.api.patch(`/api/v1/workspaces/${ws}/items/${slug}`, { data: { fields_patch: { status: 'in-progress' } } }), 'burst')
		)
	);
	const burstDoneAt = Date.now();
	// The tab must catch up with the LAST update: a reconcile read issued
	// after the burst finished (codex r1: a title still on screen proved nothing).
	await expect.poll(() => rec.readSince(burstDoneAt), { timeout: 5000 }).toBe(true);
	await sleep(3000);
	const burst = rec.take();
	console.log('MEASURE 20-update burst:', total(burst), JSON.stringify(burst));
	const kind = (k: string) => Object.entries(burst).filter(([p]) => p.includes(k)).reduce((a, [, n]) => a + n, 0);
	expect.soft(kind('/collections'), 'collection reloads for a 20-update burst (main: 20)').toBeLessThanOrEqual(4);
	expect.soft(kind('/items-changes'), 'items-changes reads for a 20-update burst (main: 20)').toBeLessThanOrEqual(10);
	expect.soft(total(burst), 'requests for a 20-update burst (main: 40)').toBeLessThanOrEqual(14);


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
