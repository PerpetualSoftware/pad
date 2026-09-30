import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test';
import { quietCrossActorToasts } from './fixtures';

/**
 * TASK-3275 (PLAN-3002 U7b): losing a workspace closes its tab LIVE, with no
 * reload, through the /events/stream?access=true subscription (one per
 * browser since BUG-3318, relayed to every tab).
 *
 * Like workspace-tabs.spec.ts, every leg mints its OWN accounts, because the
 * open set is per user and the shared admin's bar is read by other specs in
 * parallel. The account helpers are a local copy of that spec's, kept here
 * rather than extracted so this unit does not edit a sibling's spec.
 */

const DESKTOP = { width: 1280, height: 800 };
const PASSWORD = 'Playwright-Access-2026!';

interface Account {
	id: string;
	username: string;
	email: string;
	token: string;
	api: APIRequestContext;
}

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`workspace-access seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	return resp;
}

async function mintAccount(fixture: SuiteFixture, tag: string): Promise<Account> {
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.apiToken}` }
	});
	const session = await request.newContext({ baseURL: fixture.baseURL });
	try {
		const username = `access${tag}`;
		const email = `${username}@example.com`;
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Access ${tag}`, password: PASSWORD } }),
			'register'
		);
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		const csrf = (await session.storageState()).cookies.find(
			(c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf'
		);
		if (!csrf) throw new Error('workspace-access seed: login issued no pad_csrf cookie');
		const tokenResp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `access-${tag}`, expires_in: 1 }
			}),
			'token'
		);
		const { token } = (await tokenResp.json()) as { token: string };
		const api = await request.newContext({
			baseURL: fixture.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${token}` }
		});
		const me = (await (await ok(await api.get('/api/v1/auth/me'), 'me')).json()) as { id?: string; user?: { id: string } };
		const id = me.user?.id ?? me.id ?? '';
		if (!id) throw new Error('workspace-access seed: /auth/me carried no user id');
		return { id, username, email, token, api };
	} finally {
		await admin.dispose();
		await session.dispose();
	}
}

async function createWorkspace(account: Account, name: string): Promise<string> {
	const resp = await ok(await account.api.post('/api/v1/workspaces', { data: { name } }), `create ${name}`);
	return ((await resp.json()) as { slug: string }).slug;
}

async function setOpenSet(account: Account, slugs: string[]) {
	const current = (await (await ok(await account.api.get('/api/v1/me/workspace-tabs'), 'list')).json()) as {
		tabs: { slug: string }[];
	};
	for (const t of current.tabs) await ok(await account.api.delete(`/api/v1/me/workspace-tabs/${t.slug}`), 'close');
	for (const slug of slugs) {
		await ok(await account.api.post('/api/v1/me/workspace-tabs', { data: { slug, ephemeral: false } }), `open ${slug}`);
	}
}

async function actAs(context: BrowserContext, account: Account) {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
}

const tabs = (page: Page) => page.locator('header.topbar .workspace-tab');

async function barOrder(page: Page): Promise<string[]> {
	return tabs(page).evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.wsSlug ?? ''));
}

interface World {
	owner: Account;
	member: Account;
	own: string; // the member's own workspace
	shared: string; // the owner's workspace the member is invited into
}

async function seed(fixture: SuiteFixture): Promise<World> {
	const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
	const owner = await mintAccount(fixture, `o${tag}`);
	const member = await mintAccount(fixture, `m${tag}`);
	const own = await createWorkspace(member, `Own ${tag}`);
	const shared = await createWorkspace(owner, `Shared ${tag}`);
	await ok(
		await owner.api.post(`/api/v1/workspaces/${shared}/members/invite`, { data: { email: member.email, role: 'editor' } }),
		'invite'
	);
	await setOpenSet(member, [own, shared]);
	return { owner, member, own, shared };
}

async function teardown(world: World | undefined) {
	if (!world) return;
	await world.owner.api.delete(`/api/v1/workspaces/${world.shared}`).catch(() => {});
	await world.member.api.delete(`/api/v1/workspaces/${world.own}`).catch(() => {});
	await world.owner.api.dispose();
	await world.member.api.dispose();
}

/** Marks the document; a reload drops the mark. */
async function markDocument(page: Page) {
	await page.evaluate(() => ((window as unknown as { __accessMark: number }).__accessMark = 3275));
}
async function stillSameDocument(page: Page) {
	return page.evaluate(() => (window as unknown as { __accessMark?: number }).__accessMark === 3275);
}

/**
 * Arms a wait for the tab's access stream request. It must be armed BEFORE
 * the navigation that opens the stream. Resource Timing cannot serve here:
 * it records an entry when a response COMPLETES, and an open event stream
 * never does.
 */
function accessStreamOpened(page: Page) {
	return page.waitForResponse(
		(r) => r.url().includes('/api/v1/events/stream') && r.url().includes('access=true') && r.status() === 200
	);
}

test.describe('workspace access is live (TASK-3275)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop-only (PLAN-3002 Q11)');
		await page.setViewportSize(DESKTOP);
	});

	test("an owner removes the member, and the member's tab disappears with no reload", async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture);
			const { owner, member, own, shared } = world;
			await actAs(context, member);
			const streamOpen = accessStreamOpened(page);
			await page.goto(`/${member.username}/${own}`);
			await expect.poll(() => barOrder(page)).toEqual([own, shared]);
			await streamOpen;
			await markDocument(page);

			await ok(await owner.api.delete(`/api/v1/workspaces/${shared}/members/${member.id}`), 'remove member');

			await expect.poll(() => barOrder(page), { timeout: 15_000 }).toEqual([own]);
			// Losing a tab that is not active moves nothing.
			await expect(page).toHaveURL(new RegExp(`/${member.username}/${own}$`));
			expect(await stillSameDocument(page)).toBe(true);
		} finally {
			await teardown(world);
		}
	});

	test('losing the ACTIVE workspace lands on its left neighbour (Q3), with no reload', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture);
			const { owner, member, own, shared } = world;
			await actAs(context, member);
			const streamOpen = accessStreamOpened(page);
			await page.goto(`/${owner.username}/${shared}`);
			await expect.poll(() => barOrder(page)).toEqual([own, shared]);
			await streamOpen;
			await markDocument(page);

			await ok(await owner.api.delete(`/api/v1/workspaces/${shared}/members/${member.id}`), 'remove member');

			await expect(page).toHaveURL(new RegExp(`/${member.username}/${own}(/|$)`), { timeout: 15_000 });
			await expect.poll(() => barOrder(page)).toEqual([own]);
			expect(await stillSameDocument(page)).toBe(true);
		} finally {
			await teardown(world);
		}
	});
});
