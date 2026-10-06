import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext } from '@playwright/test';
import { quietCrossActorToasts } from './fixtures';

/**
 * BUG-3447: the first-run launchpad follows an onboarding agent LIVE.
 *
 * One tab sits on a brand-new workspace's launchpad while an "agent" drives
 * the API from OUTSIDE the tab (a bearer-token client, which the server
 * records as source=cli, the agent sources). Each outcome is asserted within
 * a bound well under the dashboard's 30 s poll:
 *   1. a collection the agent creates appears in the sidebar, and, being
 *      agent activity, ticks the launchpad's "Agent connected" step and
 *      removes the "Connect an AI agent" banner, before any item exists;
 *   2. the agent's first item swaps the launchpad for the dashboard.
 *
 * Each run mints its own account and workspace: the launchpad is per
 * workspace, and the shared admin's workspaces are not new.
 */

const DESKTOP = { width: 1280, height: 800 };
const PASSWORD = 'Playwright-Launchpad-2026!';
/** Well under the 30 s poll the old code waited on; the debounce is 400 ms. */
const LIVE = 2_500;

interface Account {
	username: string;
	token: string;
	api: APIRequestContext;
}

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`launchpad seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	return resp;
}

async function mintAccount(fixture: SuiteFixture, tag: string): Promise<Account> {
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.adminSessionToken}` }
	});
	try {
		const username = `lp${tag}`;
		const email = `${username}@example.com`;
		// Admin-created accounts are verified, so they can log in at once.
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Launchpad ${tag}`, password: PASSWORD } }),
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
		if (!csrf) throw new Error('launchpad seed: login issued no pad_csrf cookie');
		const tokenResp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `lp-${tag}`, expires_in: 1 }
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

/**
 * Creates the workspace over a COOKIE session, as the web UI does: a
 * workspace created by a bearer client (`pad init`) records source=cli and is
 * agent-connected from the start (BUG-1557), which is not the first run this
 * spec is about.
 */
async function createWebWorkspace(fixture: SuiteFixture, email: string, name: string): Promise<string> {
	const session = await request.newContext({ baseURL: fixture.baseURL });
	try {
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		const csrf = (await session.storageState()).cookies.find(
			(c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf'
		);
		if (!csrf) throw new Error('launchpad seed: login issued no pad_csrf cookie');
		const resp = await ok(
			await session.post('/api/v1/workspaces', { headers: { 'X-CSRF-Token': csrf.value }, data: { name } }),
			`create ${name}`
		);
		const ws = (await resp.json()) as { slug: string; source?: string };
		if (ws.source && ws.source !== 'web') throw new Error(`launchpad seed: workspace source ${ws.source}, want web`);
		return ws.slug;
	} finally {
		await session.dispose();
	}
}

async function actAs(context: BrowserContext, account: Account) {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
}

test.describe('BUG-3447: the launchpad follows an onboarding agent live', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the sidebar is a desktop surface');
	});

	test('a collection, the dashboard swap and the banner all land within seconds, without a reload', async ({
		page,
		context,
		fixture
	}) => {
		const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
		const account = await mintAccount(fixture, tag);
		try {
			const ws = await createWebWorkspace(fixture, `${account.username}@example.com`, `Launchpad ${tag}`);
			await actAs(context, account);
			await page.setViewportSize(DESKTOP);
			await page.goto(`/${account.username}/${ws}`);

			// Preconditions: a brand-new workspace shows the launchpad, not yet
			// agent-connected, and the banner.
			await expect(page.locator('.launchpad')).toBeVisible();
			const agentStep = page.locator('.lp-progress-item', { hasText: 'Agent connected' });
			await expect(agentStep).not.toHaveClass(/\bdone\b/);
			const banner = page.locator('.banner').filter({ hasText: /connect/i });
			await expect(banner).toBeVisible();
			const collectionName = `Bugs ${tag}`;
			const sidebarCollection = page.locator('a.nav-item .nav-label', { hasText: collectionName });
			await expect(sidebarCollection).toHaveCount(0);

			// 1. The agent creates a collection, from outside the tab.
			const created = await ok(
				await account.api.post(`/api/v1/workspaces/${ws}/collections`, { data: { name: collectionName } }),
				'create collection'
			);
			const coll = (await created.json()) as { slug: string };
			await expect(sidebarCollection, 'the new collection did not reach the sidebar live').toBeVisible({ timeout: LIVE });
			// An agent's collection is agent activity (lead ruling): the step
			// ticks and the banner goes, before any item exists.
			await expect(agentStep, 'the Agent connected step did not tick on the agent\'s collection').toHaveClass(/\bdone\b/, {
				timeout: LIVE
			});
			await expect(banner, 'the connect banner did not see the agent\'s collection').toHaveCount(0, { timeout: LIVE });
			await expect(page.locator('.launchpad'), 'control: no item yet, the launchpad stays').toBeVisible();

			// 2 and 3. The agent creates its first item.
			await ok(
				await account.api.post(`/api/v1/workspaces/${ws}/collections/${coll.slug}/items`, {
					data: { title: 'First thing the agent found' }
				}),
				'create item'
			);
			await expect(page.locator('.launchpad'), 'the launchpad did not give way to the dashboard live').toHaveCount(0, {
				timeout: LIVE
			});
			await expect(page.locator('.dash-header')).toBeVisible({ timeout: LIVE });
		} finally {
			await account.api.dispose();
		}
	});
});
