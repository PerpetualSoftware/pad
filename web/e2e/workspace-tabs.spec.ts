import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test';
import { quietCrossActorToasts } from './fixtures';

/**
 * TASK-3274 (PLAN-3002 U3): the desktop TopBar is a tab bar over the
 * caller's server-side open set.
 *
 * THE OPEN SET IS PER USER, and every other spec signs in as the shared
 * admin, in parallel. A leg here that closed or reordered the admin's tabs
 * would move the world under whichever spec is reading the bar at that
 * moment, so EACH LEG MINTS ITS OWN ACCOUNT and its own workspaces. Nothing
 * here touches the admin's set.
 */

const DESKTOP = { width: 1280, height: 800 };
const PASSWORD = 'Playwright-Tabs-2026!';

interface Account {
	username: string;
	token: string;
	api: APIRequestContext;
}

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	if (!resp.ok()) throw new Error(`workspace-tabs seed: ${what} failed (${resp.status()}): ${await resp.text()}`);
	return resp;
}

async function mintAccount(fixture: SuiteFixture, tag: string): Promise<Account> {
	const admin = await request.newContext({
		baseURL: fixture.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${fixture.apiToken}` }
	});
	const session = await request.newContext({ baseURL: fixture.baseURL });
	try {
		const username = `tabs${tag}`;
		const email = `${username}@example.com`;
		// Admin-created accounts are verified, so they can log in at once.
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Tabs ${tag}`, password: PASSWORD } }),
			'register'
		);
		await ok(await session.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		const csrf = (await session.storageState()).cookies.find(
			(c) => c.name === 'pad_csrf' || c.name === '__Host-pad_csrf'
		);
		if (!csrf) throw new Error('workspace-tabs seed: login issued no pad_csrf cookie');
		const tokenResp = await ok(
			await session.post('/api/v1/auth/tokens', {
				headers: { 'X-CSRF-Token': csrf.value },
				data: { name: `tabs-${tag}`, expires_in: 1 }
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
		await admin.dispose();
		await session.dispose();
	}
}

async function createWorkspace(account: Account, name: string): Promise<string> {
	const resp = await ok(await account.api.post('/api/v1/workspaces', { data: { name } }), `create ${name}`);
	return ((await resp.json()) as { slug: string }).slug;
}

/** Replace the account's open set with `slugs`, in that order, all kept. */
async function setOpenSet(account: Account, slugs: string[], ephemeral: string[] = []) {
	const current = (await (await ok(await account.api.get('/api/v1/me/workspace-tabs'), 'list')).json()) as {
		tabs: { slug: string }[];
	};
	for (const t of current.tabs) await ok(await account.api.delete(`/api/v1/me/workspace-tabs/${t.slug}`), 'close');
	for (const slug of slugs) {
		await ok(
			await account.api.post('/api/v1/me/workspace-tabs', { data: { slug, ephemeral: ephemeral.includes(slug) } }),
			`open ${slug}`
		);
	}
}

async function serverOrder(account: Account): Promise<string[]> {
	const body = (await (await ok(await account.api.get('/api/v1/me/workspace-tabs'), 'list')).json()) as {
		tabs: { slug: string }[];
	};
	return body.tabs.map((t) => t.slug);
}

async function actAs(context: BrowserContext, account: Account) {
	await context.setExtraHTTPHeaders({ Authorization: `Bearer ${account.token}` });
	await quietCrossActorToasts(context);
}

const tabs = (page: Page) => page.locator('header.topbar .workspace-tab');
const tab = (page: Page, slug: string) => page.locator(`header.topbar .workspace-tab[data-ws-slug="${slug}"]`);

async function barOrder(page: Page): Promise<string[]> {
	return tabs(page).evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.wsSlug ?? ''));
}

async function closeTab(page: Page, slug: string) {
	await tab(page, slug).hover();
	await tab(page, slug).locator('.workspace-tab-close').click();
}

interface World {
	account: Account;
	slugs: string[];
}

async function seed(fixture: SuiteFixture, names: string[]): Promise<World> {
	const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
	const account = await mintAccount(fixture, tag);
	const slugs: string[] = [];
	for (const n of names) slugs.push(await createWorkspace(account, `${n} ${tag}`));
	return { account, slugs };
}

async function teardown(world: World | undefined) {
	if (!world) return;
	for (const slug of world.slugs) await world.account.api.delete(`/api/v1/workspaces/${slug}`).catch(() => {});
	await world.account.api.dispose();
}

test.describe('workspace tab bar (TASK-3274)', () => {
	test.beforeEach(async ({ page, context }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop-only (PLAN-3002 Q11)');
		await page.setViewportSize(DESKTOP);
		void context;
	});

	test('close and reorder persist across a reload', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Tabs A', 'Tabs B', 'Tabs C']);
			const [a, b, c] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [a, b, c]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${a}`);
			await expect(tabs(page)).toHaveCount(3);
			expect(await barOrder(page)).toEqual([a, b, c]);

			// Close C, which is not the active tab: nothing navigates.
			await closeTab(page, c);
			await expect(tabs(page)).toHaveCount(2);
			await expect(page).toHaveURL(new RegExp(`/${a}$`));

			// Drag B in front of A, with a real mouse.
			const from = await tab(page, b).locator('a').boundingBox();
			const to = await tab(page, a).locator('a').boundingBox();
			if (!from || !to) throw new Error('tab has no box');
			await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
			await page.mouse.down();
			await page.mouse.move(from.x + from.width / 2 - 10, from.y + from.height / 2, { steps: 4 });
			await page.mouse.move(to.x + 4, to.y + to.height / 2, { steps: 10 });
			await page.mouse.up();
			await expect.poll(() => barOrder(page)).toEqual([b, a]);
			await expect.poll(() => serverOrder(account)).toEqual([b, a]);
			// The drop's synthetic click must not have navigated to B.
			await expect(page).toHaveURL(new RegExp(`/${a}$`));

			await page.reload();
			await expect(tabs(page)).toHaveCount(2);
			expect(await barOrder(page)).toEqual([b, a]);
		} finally {
			await teardown(world);
		}
	});

	test('closing the active tab lands on its left neighbour (Q3)', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Left A', 'Left B', 'Left C']);
			const [a, b, c] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [a, b, c]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${b}`);
			await expect(tabs(page)).toHaveCount(3);
			await closeTab(page, b);
			await expect(page).toHaveURL(new RegExp(`/${account.username}/${a}$`));
			expect(await barOrder(page)).toEqual([a, c]);
		} finally {
			await teardown(world);
		}
	});

	test('closing the active FIRST tab lands on the tab that becomes first (Q3, lead ruling)', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['First A', 'First B', 'First C']);
			const [a, b, c] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [a, b, c]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${a}`);
			await expect(tabs(page)).toHaveCount(3);
			await closeTab(page, a);
			await expect(page).toHaveURL(new RegExp(`/${account.username}/${b}$`));
			expect(await barOrder(page)).toEqual([b, c]);
		} finally {
			await teardown(world);
		}
	});

	test('closing the last tab lands on /console with its create action highlighted (Q2)', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Only']);
			const [only] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [only]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${only}`);
			await expect(tabs(page)).toHaveCount(1);
			await closeTab(page, only);
			await expect(page).toHaveURL(/\/console$/);
			// /console has no TopBar; its Create Workspace button is the "+".
			await expect(page.getByRole('button', { name: 'Create Workspace' }).first()).toHaveClass(/add-highlighted/);
		} finally {
			await teardown(world);
		}
	});

	test('an ephemeral tab reads italic, and a double-click keeps it across a reload (Q9)', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Keep A', 'Keep B']);
			const [a, b] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [a, b], [b]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${a}`);
			const name = tab(page, b).locator('.workspace-name');
			await expect(name).toHaveCSS('font-style', 'italic');

			await tab(page, b).locator('a').dblclick();
			await expect(name).toHaveCSS('font-style', 'normal');
			await page.reload();
			await expect(tab(page, b).locator('.workspace-name')).toHaveCSS('font-style', 'normal');
		} finally {
			await teardown(world);
		}
	});

	test('a workspace created with "+" opens as a kept tab that survives a reload', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		let created: string | undefined;
		try {
			world = await seed(fixture, ['Home']);
			const [home] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [home]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${home}`);
			await expect(tabs(page)).toHaveCount(1);
			await page.getByTitle('New workspace').click();
			const dialog = page.getByRole('dialog', { name: /new workspace/i });
			await expect(dialog).toBeVisible();
			const name = `Made here ${Date.now()}`;
			await dialog.getByPlaceholder('Workspace name').fill(name);
			await dialog.getByRole('button', { name: /create workspace/i }).click();

			await expect(tabs(page)).toHaveCount(2);
			created = (await barOrder(page))[1];
			world.slugs.push(created);
			await expect(page).toHaveURL(new RegExp(`/${created}$`));
			await expect(tab(page, created)).not.toHaveClass(/ephemeral/);

			await page.reload();
			await expect(tabs(page)).toHaveCount(2);
			await expect(tab(page, created)).not.toHaveClass(/ephemeral/);
		} finally {
			await teardown(world);
		}
	});
});
