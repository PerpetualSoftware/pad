import { test, expect, type SuiteFixture } from './fixtures';
import { request, type APIRequestContext, type BrowserContext, type Locator, type Page } from '@playwright/test';
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
	try {
		const username = `tabs${tag}`;
		const email = `${username}@example.com`;
		// Admin-created accounts are verified, so they can log in at once.
		await ok(
			await admin.post('/api/v1/auth/register', { data: { email, username, name: `Tabs ${tag}`, password: PASSWORD } }),
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

/** A locator's box once two reads 100 ms apart agree (an animation has settled). */
async function stableBox(locator: Locator) {
	let prev = await locator.boundingBox();
	for (let i = 0; i < 30; i++) {
		await locator.page().waitForTimeout(100);
		const next = await locator.boundingBox();
		if (prev && next && prev.x === next.x && prev.y === next.y && prev.width === next.width) return next;
		prev = next;
	}
	throw new Error('box never settled');
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

			// Drag B in front of A, with a real mouse. The close just reflowed
			// the zone with a 150 ms FLIP animation, so the boxes are read only
			// once they hold still: a box read mid-animation aims the drag at
			// where a tab was, and the drop lands nowhere.
			const from = await stableBox(tab(page, b).locator('a'));
			const to = await stableBox(tab(page, a).locator('a'));
			await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
			await page.mouse.down();
			await page.mouse.move(from.x + from.width / 2 - 10, from.y + from.height / 2, { steps: 4 });
			// The drag has engaged once svelte-dnd-action mounts its clone.
			await expect(page.locator('#dnd-action-dragged-el')).toHaveCount(1);
			await page.mouse.move(to.x + 4, to.y + to.height / 2, { steps: 10 });
			// svelte-dnd-action samples the pointer on an interval, so a
			// release straight after the last move can drop before it has seen
			// where the pointer is. Hold over the target until the shadow slot
			// has moved in front of A, then release.
			await expect.poll(() => barOrder(page)).toEqual([b, a]);
			await page.mouse.up();
			await expect(page.locator('#dnd-action-dragged-el')).toHaveCount(0);
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
			await page.getByTitle('Find or create a workspace').click();
			await page.getByRole('option', { name: /new workspace/i }).click();
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

/**
 * TASK-3276 (PLAN-3002 U4): the "+" discovery surface. Since U3 the bar shows
 * only the open set, so "+" is how a workspace outside it is reached. The
 * create path is covered above ('a workspace created with "+" …').
 */
test.describe('"+" discovery surface (TASK-3276)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop-only (PLAN-3002 Q11)');
		await page.setViewportSize(DESKTOP);
	});

	test('a user with seven workspaces and six tabs reaches the seventh from "+", as a kept tab', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['One', 'Two', 'Three', 'Four', 'Five', 'Six', 'Seventh']);
			const { account, slugs } = world;
			const seventh = slugs[6];
			await setOpenSet(account, slugs.slice(0, 6));
			await actAs(context, account);

			await page.goto(`/${account.username}/${slugs[0]}`);
			await expect(tabs(page)).toHaveCount(6);
			await expect(tab(page, seventh)).toHaveCount(0);

			await page.getByTitle('Find or create a workspace').click();
			const search = page.getByRole('combobox', { name: 'Find a workspace' });
			await expect(search).toBeFocused();
			// The six open workspaces are not offered; the seventh is.
			await expect(page.locator('.discovery-option[data-ws-slug]')).toHaveCount(1);
			await search.fill('Seventh');
			await page.locator(`.discovery-option[data-ws-slug="${seventh}"]`).click();

			await expect(page).toHaveURL(new RegExp(`/${account.username}/${seventh}$`));
			await expect(tabs(page)).toHaveCount(7);
			await expect(tab(page, seventh)).not.toHaveClass(/ephemeral/);
			await expect.poll(() => serverOrder(account)).toContain(seventh);

			await page.reload();
			await expect(tab(page, seventh)).toHaveCount(1);
			await expect(tab(page, seventh)).not.toHaveClass(/ephemeral/);
		} finally {
			await teardown(world);
		}
	});

	test('keyboard only: open "+", search, arrow, Enter; and Escape returns focus to "+"', async ({
		page,
		context,
		fixture
	}) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Home', 'Kb Alpha', 'Kb Beta']);
			const { account, slugs } = world;
			const [home, , beta] = slugs;
			await setOpenSet(account, [home]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${home}`);
			await expect(tabs(page)).toHaveCount(1);
			const plus = page.getByTitle('Find or create a workspace');
			const search = page.getByRole('combobox', { name: 'Find a workspace' });

			// Escape closes and hands focus back to "+".
			await plus.focus();
			await page.keyboard.press('Enter');
			await expect(search).toBeFocused();
			await page.keyboard.press('Escape');
			await expect(search).toHaveCount(0);
			await expect(plus).toBeFocused();

			// Open again from the keyboard, narrow to the two "Kb" workspaces,
			// step to the second, and take it.
			await page.keyboard.press('Enter');
			await expect(search).toBeFocused();
			await page.keyboard.type('Kb');
			await expect(page.locator('.discovery-option[data-ws-slug]')).toHaveCount(2);
			await page.keyboard.press('ArrowDown');
			await expect(page.locator('.discovery-option[aria-selected="true"]')).toHaveAttribute('data-ws-slug', beta);
			await page.keyboard.press('Enter');

			await expect(page).toHaveURL(new RegExp(`/${account.username}/${beta}$`));
			await expect(tab(page, beta)).toHaveCount(1);
			await expect(tab(page, beta)).not.toHaveClass(/ephemeral/);
		} finally {
			await teardown(world);
		}
	});

	// TASK-3277 (U4b): a pending invitation is listed in "+" and accepting it
	// lands in the workspace on an EPHEMERAL tab (Q5).
	test('an invitation in "+" is accepted and lands on an ephemeral tab', async ({ page, context, fixture }) => {
		let inviter: World | undefined;
		let invitee: World | undefined;
		try {
			inviter = await seed(fixture, ['Invited To']);
			const shared = inviter.slugs[0];
			// Invite an address with no account yet, so the invite stays a
			// pending invitation rather than a direct add, then create the
			// account (admin-created, hence verified).
			const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
			await ok(
				await inviter.account.api.post(`/api/v1/workspaces/${shared}/members/invite`, {
					data: { email: `tabs${tag}@example.com`, role: 'editor' }
				}),
				'invite'
			);
			const account = await mintAccount(fixture, tag);
			invitee = { account, slugs: [await createWorkspace(account, `Home ${tag}`)] };
			const home = invitee.slugs[0];
			await setOpenSet(account, [home]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${home}`);
			await expect(tabs(page)).toHaveCount(1);
			await page.getByTitle('Find or create a workspace').click();
			const section = page.getByRole('region', { name: 'Invitations' });
			await expect(section).toBeVisible();
			await expect(section.locator('.invitation-item')).toHaveCount(1);
			await section.getByRole('button', { name: /^Accept the invitation to Invited To/ }).click();

			await expect(page).toHaveURL(new RegExp(`/${inviter.account.username}/${shared}$`));
			await expect(tab(page, shared)).toHaveCount(1);
			await expect(tab(page, shared)).toHaveClass(/ephemeral/);
			await expect.poll(() => serverOrder(account)).toContain(shared);

			// Accepted: the server no longer lists it.
			const mine = (await (await ok(await account.api.get('/api/v1/me/invitations'), 'list invitations')).json()) as {
				invitations: unknown[];
			};
			expect(mine.invitations).toHaveLength(0);
		} finally {
			await teardown(invitee);
			await teardown(inviter);
		}
	});
});

/**
 * TASK-3279 (PLAN-3002 U5): every landing in a visible workspace outside the
 * open set opens it as an EPHEMERAL tab, and a write there keeps it. Each leg
 * mints its own account, as above.
 */
async function serverTabs(account: Account): Promise<{ slug: string; ephemeral: boolean }[]> {
	const body = (await (await ok(await account.api.get('/api/v1/me/workspace-tabs'), 'list')).json()) as {
		tabs: { slug: string; ephemeral: boolean }[];
	};
	return body.tabs.map((t) => ({ slug: t.slug, ephemeral: t.ephemeral }));
}

async function createTask(account: Account, ws: string, title: string): Promise<{ slug: string; ref: string }> {
	const resp = await ok(
		await account.api.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title } }),
		`create task in ${ws}`
	);
	const item = (await resp.json()) as { slug: string; collection_prefix: string; item_number: number };
	return { slug: item.slug, ref: `${item.collection_prefix}-${item.item_number}` };
}

async function userId(account: Account): Promise<string> {
	const me = (await (await ok(await account.api.get('/api/v1/auth/me'), 'me')).json()) as { id?: string; user?: { id: string } };
	const id = me.id ?? me.user?.id;
	if (!id) throw new Error('workspace-tabs seed: /auth/me carried no id');
	return id;
}

async function expectEphemeral(page: Page, account: Account, slug: string) {
	await expect(tab(page, slug)).toHaveClass(/ephemeral/);
	await expect(tab(page, slug).locator('.workspace-name')).toHaveCSS('font-style', 'italic');
	await expect.poll(() => serverTabs(account)).toContainEqual({ slug, ephemeral: true });
}

test.describe('landings open an ephemeral tab (TASK-3279)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'the tab bar is desktop-only (PLAN-3002 Q11)');
		await page.setViewportSize(DESKTOP);
	});

	test('a deep link opens one, and a second deep link replaces it', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Land Home', 'Land Deep', 'Land Next']);
			const [home, deep, next] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [home]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${deep}/tasks`);
			await expectEphemeral(page, account, deep);
			expect(await barOrder(page)).toEqual([home, deep]);

			await page.goto(`/${account.username}/${next}`);
			await expectEphemeral(page, account, next);
			await expect(tab(page, deep)).toHaveCount(0);
			await expect.poll(() => serverOrder(account)).toEqual([home, next]);
		} finally {
			await teardown(world);
		}
	});

	test('a /-/r/ link opens one', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Ref Home', 'Ref Target']);
			const [home, target] = world.slugs;
			const { account } = world;
			const task = await createTask(account, target, 'Resolved by ref');
			await setOpenSet(account, [home]);
			await actAs(context, account);

			await page.goto(`/-/r/${target}/${task.ref}`);
			await expect(page).toHaveURL(new RegExp(`/${account.username}/${target}/tasks/${task.ref}$`));
			await expectEphemeral(page, account, target);
		} finally {
			await teardown(world);
		}
	});

	test('accepting an invitation link lands IN the workspace, on an ephemeral tab (Q5)', async ({
		page,
		context,
		fixture
	}) => {
		let inviter: World | undefined;
		let invitee: World | undefined;
		try {
			inviter = await seed(fixture, ['Join Target']);
			const shared = inviter.slugs[0];
			const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
			const inv = (await (
				await ok(
					await inviter.account.api.post(`/api/v1/workspaces/${shared}/members/invite`, {
						data: { email: `tabs${tag}@example.com`, role: 'editor' }
					}),
					'invite'
				)
			).json()) as { code: string };
			const account = await mintAccount(fixture, tag);
			invitee = { account, slugs: [await createWorkspace(account, `Join Home ${tag}`)] };
			await setOpenSet(account, [invitee.slugs[0]]);
			await actAs(context, account);

			await page.goto(`/join/${inv.code}`);
			await expect(page).toHaveURL(new RegExp(`/${inviter.account.username}/${shared}$`));
			await expectEphemeral(page, account, shared);
		} finally {
			await teardown(invitee);
			await teardown(inviter);
		}
	});

	test('registering from an invitation link lands IN the workspace, on an ephemeral tab (BUG-3284)', async ({
		browser,
		fixture
	}) => {
		let inviter: World | undefined;
		let invitee: Account | undefined;
		// Signed OUT: the suite's default page is the shared admin, and a signed-in
		// join page accepts as that account instead of offering registration.
		const context = await browser.newContext({ baseURL: fixture.baseURL, storageState: { cookies: [], origins: [] } });
		const page = await context.newPage();
		await page.setViewportSize(DESKTOP);
		try {
			inviter = await seed(fixture, ['Signup Target']);
			const shared = inviter.slugs[0];
			const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
			const username = `tabs${tag}`;
			const email = `${username}@example.com`;
			const inv = (await (
				await ok(
					await inviter.account.api.post(`/api/v1/workspaces/${shared}/members/invite`, {
						data: { email, role: 'editor' }
					}),
					'invite'
				)
			).json()) as { code: string };

			// No account yet, and no session: the join page offers registration.
			await page.goto(`/join/${inv.code}`);
			await page.getByPlaceholder('Name', { exact: true }).fill(`Tabs ${tag}`);
			await page.getByPlaceholder('Username', { exact: true }).fill(username);
			await page.getByPlaceholder('Password', { exact: true }).fill(PASSWORD);
			await page.getByPlaceholder('Confirm password').fill(PASSWORD);
			await page.getByRole('button', { name: 'Create account & join' }).click();

			await expect(page).toHaveURL(new RegExp(`/${inviter.account.username}/${shared}$`));
			invitee = await accountFor(fixture, username, email, tag);
			await expectEphemeral(page, invitee, shared);
		} finally {
			await context.close();
			await invitee?.api.dispose();
			await teardown(inviter);
		}
	});

	test('a guest landing opens one, with the guest marker (Q10)', async ({ page, context, fixture }) => {
		let owner: World | undefined;
		let guest: World | undefined;
		try {
			owner = await seed(fixture, ['Guest Target']);
			const shared = owner.slugs[0];
			const task = await createTask(owner.account, shared, 'Shared by grant');
			const tag = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
			const account = await mintAccount(fixture, `g${tag}`);
			guest = { account, slugs: [await createWorkspace(account, `Guest Home ${tag}`)] };
			await ok(
				await owner.account.api.post(`/api/v1/workspaces/${shared}/items/${task.slug}/grants`, {
					data: { user_id: await userId(account), permission: 'view' }
				}),
				'grant'
			);
			await setOpenSet(account, [guest.slugs[0]]);
			await actAs(context, account);

			await page.goto(`/${owner.account.username}/${shared}/tasks/${task.ref}`);
			await expectEphemeral(page, account, shared);
			await expect(tab(page, shared)).toHaveClass(/guest/);
		} finally {
			await teardown(guest);
			await teardown(owner);
		}
	});

	test('restoring a deleted workspace reopens it as an ephemeral tab (Q12)', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Restore Home', 'Restore Me']);
			const [home, gone] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [home, gone]);
			await ok(await account.api.delete(`/api/v1/workspaces/${gone}`), 'delete');
			// Soft delete removes every user's row for it (U1).
			await expect.poll(() => serverOrder(account)).toEqual([home]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${home}`);
			await expect(tabs(page)).toHaveCount(1);
			await page.goto('/console/deleted-workspaces');
			await page.getByRole('button', { name: /Restore/ }).first().click();
			await expect.poll(() => serverTabs(account)).toContainEqual({ slug: gone, ephemeral: true });

			await page.goto(`/${account.username}/${home}`);
			await expect(tab(page, gone)).toHaveClass(/ephemeral/);
		} finally {
			await teardown(world);
		}
	});

	test('a write in the workspace keeps its tab (Q9)', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Write Home', 'Write Here']);
			const [home, here] = world.slugs;
			const { account } = world;
			const task = await createTask(account, here, 'Star me');
			// Seeded ephemeral rather than opened by the landing, so this leg
			// isolates the write from the landing legs above.
			await setOpenSet(account, [home, here], [here]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${here}/tasks/${task.ref}`);
			await expectEphemeral(page, account, here);

			// Reading alone does not keep it.
			await page.waitForTimeout(1500);
			expect(await serverTabs(account)).toContainEqual({ slug: here, ephemeral: true });

			const star = page.getByTitle('Star', { exact: true }).first();
			const starred = page.waitForResponse(
				(r) => r.request().method() === 'POST' && r.url().endsWith(`/items/${task.slug}/star`) && r.ok()
			);
			await star.click();
			await starred;
			await expect(tab(page, here).locator('.workspace-name')).toHaveCSS('font-style', 'normal');
			await expect.poll(() => serverTabs(account)).toContainEqual({ slug: here, ephemeral: false });
		} finally {
			await teardown(world);
		}
	});

	test('"Keep open" keeps an ephemeral tab', async ({ page, context, fixture }) => {
		let world: World | undefined;
		try {
			world = await seed(fixture, ['Pin Home', 'Pin Me']);
			const [home, pin] = world.slugs;
			const { account } = world;
			await setOpenSet(account, [home, pin], [pin]);
			await actAs(context, account);

			await page.goto(`/${account.username}/${home}`);
			await tab(page, pin).hover();
			await page.getByRole('button', { name: `Keep ${await tab(page, pin).locator('.workspace-name').innerText()} open` }).click();
			await expect(tab(page, pin).locator('.workspace-name')).toHaveCSS('font-style', 'normal');
			await expect(page.getByRole('button', { name: /^Keep .* open$/ })).toHaveCount(0);
			await expect.poll(() => serverTabs(account)).toContainEqual({ slug: pin, ephemeral: false });
		} finally {
			await teardown(world);
		}
	});
});
