import { request as playwrightRequest, type APIRequestContext } from '@playwright/test';
import { test, expect, suiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

// TASK-4902: a ref in the Agents panel on home opens the item in a docked
// pane beside the panel, in the item's own workspace context. A guest
// workspace's item is read-only even though the shell workspace is owned.

const PASSWORD = 'Playwright-Home-Pane-2026!';

async function ok(resp: Awaited<ReturnType<APIRequestContext['get']>>, what: string) {
	expect(resp.ok(), `${what}: ${resp.status()} ${await resp.text()}`).toBe(true);
	return resp;
}

async function bearer(baseURL: string, token: string) {
	return playwrightRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
}

interface Seeded {
	role: string;
	owned: { ws: string; ref: string; title: string; owner: string };
	guest: { ws: string; ref: string; title: string; owner: string };
}

/** Two lanes for one agent: one in a workspace the admin owns, one in another account's workspace where the admin is a viewer. */
async function seed(tag: string): Promise<Seeded> {
	const { baseURL, adminSessionToken, apiToken, adminEmail, adminUsername } = suiteFixture();
	const role = `Pane agent ${tag}`;
	const roleSlug = `pane-agent-${tag}`;

	const admin = await bearer(baseURL, adminSessionToken);
	const email = `e2e-pane-${tag}@example.com`;
	const username = `e2epane${tag}`;
	try {
		await ok(await admin.post('/api/v1/auth/register', { data: { email, username, name: `Pane ${tag}`, password: PASSWORD } }), 'register');
	} finally {
		await admin.dispose();
	}
	const anon = await playwrightRequest.newContext({ baseURL });
	let otherToken: string;
	try {
		const login = await ok(await anon.post('/api/v1/auth/login', { data: { email, password: PASSWORD } }), 'login');
		otherToken = ((await login.json()) as { token: string }).token;
	} finally {
		await anon.dispose();
	}

	async function laneIn(client: APIRequestContext, ws: string, title: string) {
		await ok(await client.post(`/api/v1/workspaces/${ws}/agent-roles`, { data: { name: role, slug: roleSlug } }), 'role');
		const made = (await (await ok(await client.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, { data: { title } }), 'item')).json()) as {
			slug: string;
			collection_prefix: string;
			item_number: number;
		};
		const roles = (await (await ok(await client.get(`/api/v1/workspaces/${ws}/agent-roles`), 'roles')).json()) as Array<{ id: string; slug: string }>;
		const roleId = roles.find((r) => r.slug === roleSlug)!.id;
		await ok(await client.patch(`/api/v1/workspaces/${ws}/items/${made.slug}`, { data: { agent_role_id: roleId } }), 'assign');
		return `${made.collection_prefix}-${made.item_number}`;
	}

	const mine = await bearer(baseURL, apiToken);
	const other = await bearer(baseURL, otherToken);
	try {
		const ownedWs = ((await (await ok(await mine.post('/api/v1/workspaces', { data: { name: `Pane owned ${tag}`, template: 'startup' } }), 'ws')).json()) as { slug: string }).slug;
		const guestWs = ((await (await ok(await other.post('/api/v1/workspaces', { data: { name: `Pane guest ${tag}`, template: 'startup' } }), 'ws')).json()) as { slug: string }).slug;
		const ownedTitle = `Owned pane item ${tag}`;
		const guestTitle = `Guest pane item ${tag}`;
		const ownedRef = await laneIn(mine, ownedWs, ownedTitle);
		const guestRef = await laneIn(other, guestWs, guestTitle);
		await ok(await other.post(`/api/v1/workspaces/${guestWs}/members/invite`, { data: { email: adminEmail, role: 'viewer' } }), 'invite');
		const list = (await (await ok(await mine.get('/api/v1/me/invitations'), 'invitations')).json()) as { invitations: { id: string; workspace_slug: string }[] };
		const inv = list.invitations.find((i) => i.workspace_slug === guestWs)!;
		await ok(await mine.post(`/api/v1/me/invitations/${inv.id}/accept`), 'accept');
		return {
			role,
			owned: { ws: ownedWs, ref: ownedRef, title: ownedTitle, owner: adminUsername },
			guest: { ws: guestWs, ref: guestRef, title: guestTitle, owner: username },
		};
	} finally {
		await mine.dispose();
		await other.dispose();
	}
}

test('a ref opens the item in a pane beside the Agents panel, in its own workspace', async ({ page, context }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop pane');
	test.setTimeout(90_000);
	await page.setViewportSize({ width: 1600, height: 1000 });
	const tag = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
	const s = await seed(tag);

	await browserLogin(page);
	await page.goto('/');
	const agents = page.getByRole('region', { name: 'Agents', exact: true });
	await agents.getByRole('button', { name: new RegExp(s.role) }).click();
	const ownedLink = agents.locator(`a.ref[data-pane-key="${s.owned.owner}/${s.owned.ws}/${s.owned.ref}"]`);
	const guestLink = agents.locator(`a.ref[data-pane-key="${s.guest.owner}/${s.guest.ws}/${s.guest.ref}"]`);
	await expect(ownedLink).toBeVisible();
	await expect(guestLink).toBeVisible();

	// A plain click opens the pane; the Agents panel stays in view beside it.
	await ownedLink.click();
	const pane = page.locator('aside.item-pane');
	await expect(pane).toBeVisible();
	await expect(pane.getByText(s.owned.title)).toBeVisible();
	await expect(agents).toBeInViewport();
	const url = new URL(page.url());
	expect(url.pathname).toBe('/');
	expect(url.searchParams.get('item')).toBe(s.owned.ref);
	expect(url.searchParams.get('item_in')).toBe(`${s.owned.owner}/${s.owned.ws}/tasks`);
	// Owned: the title is editable.
	await expect(pane.locator('button.title')).toBeVisible();
	await testInfo.attach('home-pane-open', { body: await page.screenshot(), contentType: 'image/png' });

	// ESC closes it and focus returns to the ref.
	await pane.focus();
	await page.keyboard.press('Escape');
	await expect(pane).toHaveCount(0);
	await expect(ownedLink).toBeFocused();
	expect(new URL(page.url()).searchParams.has('item')).toBe(false);

	// A modifier click opens the full page in a new tab.
	const popup = context.waitForEvent('page');
	await ownedLink.click({ modifiers: [process.platform === 'darwin' ? 'Meta' : 'Control'] });
	const tab = await popup;
	await tab.waitForLoadState();
	expect(new URL(tab.url()).pathname).toBe(`/${s.owned.owner}/${s.owned.ws}/tasks/${s.owned.ref}`);
	await tab.close();
	await expect(pane).toHaveCount(0);

	// The guest workspace's item is read-only, though the shell workspace is owned.
	await guestLink.click();
	await expect(pane.getByText(s.guest.title)).toBeVisible();
	await expect(pane.locator('.title-row')).toBeVisible();
	await page.waitForLoadState('networkidle');
	await expect(pane.locator('button.title')).toHaveCount(0);
	await expect(pane.getByRole('button', { name: 'More item actions' })).toBeVisible();

	// Reload keeps the pane; Back closes it.
	await page.reload();
	await expect(pane.getByText(s.guest.title)).toBeVisible();
	await page.goBack();
	await expect(pane).toHaveCount(0);
	await expect(agents).toBeVisible();
});

test('on a phone a ref click opens the full page', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'phone path');
	test.setTimeout(90_000);
	const tag = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
	const s = await seed(tag);

	await browserLogin(page);
	await page.goto('/');
	const agents = page.getByRole('region', { name: 'Agents', exact: true });
	await agents.getByRole('button', { name: new RegExp(s.role) }).click();
	// The phone shows one lane open at a time; either lane's ref will do.
	const link = agents.locator(`a.ref[data-pane-key$="/${s.owned.ref}"]:visible, a.ref[data-pane-key$="/${s.guest.ref}"]:visible`).first();
	const href = (await link.getAttribute('href'))!;
	await link.click();
	await expect(page).toHaveURL(new RegExp(`${href.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
	await expect(page.locator('aside.item-pane')).toHaveCount(0);
	await testInfo.attach('phone-full-page', { body: await page.screenshot(), contentType: 'image/png' });
});
