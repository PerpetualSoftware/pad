import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-4315 PR 3b: a page-owned URL write under an open Chat. Chat is a shallow
 * navigation, so a board filter/search/view write made while it is open would
 * rewrite Chat's own history entry (and, with the search debounce, close Chat
 * under the reader's hands). The ruling: while Chat is open the filter applies
 * at once and keeps focus, the URL write waits and lands on EVERY way Chat
 * closes, a real navigation away drops it, and Chat's entries stay out of
 * history. jsdom cannot run shallow routing, so this is where it is proven.
 * Desktop only: a phone keeps the standalone /messages route.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };
const hdr = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

async function dm(request: APIRequestContext, f: Fixture, tag: string) {
	const agent = `e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
	const res = await request.post('/api/v1/me/conversations', { headers: hdr(f), data: { agent_key: agent } });
	expect(res.ok(), await res.text()).toBe(true);
	const n = ((await res.json()) as { conversation: { number: number } }).conversation.number;
	const say = await request.post(`/api/v1/me/conversations/${n}/messages`, {
		headers: hdr(f, agent),
		data: { body: 'hello from the agent' }
	});
	expect(say.status(), await say.text()).toBe(201);
	return n;
}

async function seedTasks(request: APIRequestContext, f: Fixture, ws = f.workspaceSlug) {
	for (const st of ['open', 'in-progress', 'done']) {
		const res = await request.post(`/api/v1/workspaces/${ws}/collections/tasks/items`, {
			headers: hdr(f),
			data: { title: `Url writes ${st} ${Date.now()}`, fields: JSON.stringify({ status: st }) }
		});
		expect(res.ok(), await res.text()).toBe(true);
	}
}

const pathname = (page: Page) => new URL(page.url()).pathname;
const chatLink = (page: Page) => page.getByRole('navigation', { name: 'You', exact: true }).getByRole('link', { name: /^Chat/ });
const histLen = (page: Page) => page.evaluate(() => history.length);

type Setup = { prev: string; board: string; n: number };

/** previous page, then the board; filters open; returns the paths. */
async function openBoard(page: Page, request: APIRequestContext, f: Fixture, tag: string): Promise<Setup> {
	const n = await dm(request, f, tag);
	await seedTasks(request, f);
	await page.setViewportSize({ width: 1600, height: 900 });
	await browserLogin(page);
	const prev = `/${f.adminUsername}/${f.workspaceSlug}`;
	const board = `/${f.adminUsername}/${f.workspaceSlug}/tasks`;
	await page.goto(prev);
	await page.goto(`${board}?view=board`);
	await page.waitForLoadState('networkidle');
	const box = page.getByLabel('Search Tasks');
	if (!(await box.isVisible())) await page.getByRole('button', { name: /filter/i }).first().click();
	await expect(box).toBeVisible();
	return { prev, board, n };
}

async function openChat(page: Page, depth: 1 | 2, n: number) {
	const host = page.locator('[data-chat-host]');
	await chatLink(page).click();
	await expect(host).toBeVisible();
	if (depth === 2) {
		await host.locator(`a[href="/messages/${n}"]`).click();
		await expect(host.getByText('hello from the agent')).toBeVisible();
	}
	await page.waitForTimeout(400);
	return host;
}

/** Type a search and click a status filter, both with Chat open. */
async function editBoard(page: Page) {
	const box = page.getByLabel('Search Tasks');
	await box.click();
	await page.keyboard.type('zy', { delay: 120 });
	// Longer than the 250ms search-URL debounce: it fires with Chat open.
	await page.waitForTimeout(900);
	const btn = page.locator('.status-filters .filter-btn:not(.active)').first();
	await btn.click();
	await page.waitForTimeout(600);
}

const searchOf = (page: Page) => new URL(page.url()).searchParams;

for (const depth of [1, 2] as const) {
	test.describe(`depth ${depth}`, () => {
		test.beforeEach(async ({}, ti) => {
			test.skip(ti.project.name !== 'desktop-chromium', 'desktop only');
			test.setTimeout(120_000);
		});

		test('typing keeps Chat open and keeps focus in the search box; a filter click applies at once', async ({ page, request, fixture }) => {
			const { n } = await openBoard(page, request, fixture, `d${depth}-typing`);
			const host = await openChat(page, depth, n);
			const chatPath = pathname(page);
			const len0 = await histLen(page);
			const box = page.getByLabel('Search Tasks');
			await box.click();
			await page.keyboard.type('zy', { delay: 120 });
			await page.waitForTimeout(900);
			await expect(host).toBeVisible();
			expect(pathname(page)).toBe(chatPath);
			await expect(box).toBeFocused();
			await expect(box).toHaveValue('zy');
			expect(await histLen(page)).toBe(len0);

			const btn = page.locator('.status-filters .filter-btn:not(.active)').first();
			const label = ((await btn.textContent()) ?? '').trim();
			await btn.click();
			// The same button, found again by name (the first inactive one is now a different button).
			const clicked = page.locator('.status-filters').getByRole('button', { name: label, exact: true });
			await expect(clicked).toHaveClass(/active/);
			await expect(host).toBeVisible();
			expect(pathname(page)).toBe(chatPath);
			expect(await histLen(page)).toBe(len0);
			await expect(clicked).toBeFocused();
		});

		test('Close Chat: the URL carries the filter, and Back goes to the page before the board, never into Chat', async ({ page, request, fixture }) => {
			const { n, board, prev } = await openBoard(page, request, fixture, `d${depth}-close`);
			const host = await openChat(page, depth, n);
			await editBoard(page);
			await host.getByRole('button', { name: 'Close Chat' }).click();
			await expect(host).toBeHidden();
			await expect.poll(() => pathname(page)).toBe(board);
			await expect.poll(() => searchOf(page).get('q')).toBe('zy');
			expect([...searchOf(page).keys()].length).toBeGreaterThanOrEqual(3); // view, q and the status filter
			await page.goBack();
			await expect.poll(() => pathname(page)).toBe(prev);
			await expect(host).toBeHidden();
		});

		test('browser Back: lands the held write; at depth 2 the first Back leaves it pending', async ({ page, request, fixture }) => {
			const { n, board, prev } = await openBoard(page, request, fixture, `d${depth}-back`);
			const host = await openChat(page, depth, n);
			await editBoard(page);
			await page.goBack();
			if (depth === 2) {
				// Back one entry: the inbox, Chat still open, nothing written yet.
				await expect.poll(() => pathname(page)).toBe('/messages');
				await expect(host).toBeVisible();
				await page.goBack();
			}
			await expect(host).toBeHidden();
			await expect.poll(() => pathname(page)).toBe(board);
			await expect.poll(() => searchOf(page).get('q')).toBe('zy');
			await page.goBack();
			await expect.poll(() => pathname(page)).toBe(prev);
			await expect(host).toBeHidden();
		});

		for (const closer of ['Escape', 'the backdrop'] as const) {
			test(`a drawer (the dock narrowed mid-session): ${closer} lands the write`, async ({ page, request, fixture }) => {
				const { n, board, prev } = await openBoard(page, request, fixture, `d${depth}-drawer`);
				const host = await openChat(page, depth, n);
				await editBoard(page);
				await page.setViewportSize({ width: 1100, height: 900 });
				await expect(host).toHaveClass(/drawer/);
				if (closer === 'Escape') await page.keyboard.press('Escape');
				else await page.locator('.chat-backdrop').click({ position: { x: 10, y: 300 } });
				await expect(host).toBeHidden();
				await expect.poll(() => pathname(page)).toBe(board);
				await expect.poll(() => searchOf(page).get('q')).toBe('zy');
				await page.goBack();
				await expect.poll(() => pathname(page)).toBe(prev);
				await expect(host).toBeHidden();
			});
		}

		test('a sidebar navigation mid-defer drops the write: it never lands in the new page', async ({ page, request, fixture }) => {
			const { n } = await openBoard(page, request, fixture, `d${depth}-nav`);
			const host = await openChat(page, depth, n);
			await editBoard(page);
			await page.locator('aside.sidebar a[href$="/docs"]').first().click();
			await expect.poll(() => pathname(page)).toMatch(/\/docs$/);
			await expect(host).toBeHidden();
			await page.waitForTimeout(800);
			expect(page.url()).not.toContain('zy');
			expect(searchOf(page).get('q')).toBeNull();
		});
	});
}

test('a rename heal under Chat with a filter pending lands on the new slug with the filter, and Back does not reopen Chat', async ({ page, request, fixture }, ti) => {
	test.skip(ti.project.name !== 'desktop-chromium', 'desktop only');
	test.setTimeout(120_000);
	const slug = `urlw-${ti.workerIndex}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
	const auth = { Authorization: `Bearer ${fixture.apiToken}` };
	const ws = await request.post('/api/v1/workspaces', { headers: auth, data: { name: 'Url writes heal', slug, template: 'startup' } });
	expect(ws.ok(), await ws.text()).toBe(true);
	try {
		const n = await dm(request, fixture, 'heal');
		await seedTasks(request, fixture, slug);
		await page.setViewportSize({ width: 1600, height: 900 });
		await browserLogin(page);
		const prev = `/${fixture.adminUsername}/${slug}`;
		await page.goto(prev);
		await page.goto(`${prev}/tasks?view=board`);
		await page.waitForLoadState('networkidle');
		const box = page.getByLabel('Search Tasks');
		if (!(await box.isVisible())) await page.getByRole('button', { name: /filter/i }).first().click();
		await expect(box).toBeVisible();
		const visited: string[] = [];
		page.on('framenavigated', (fr) => {
			if (fr === page.mainFrame()) visited.push(page.url());
		});
		const host = await openChat(page, 2, n);
		await editBoard(page);
		const renamed = await request.patch(`/api/v1/workspaces/${slug}/collections/tasks`, { headers: auth, data: { name: 'Chores' } });
		expect(renamed.ok(), await renamed.text()).toBe(true);
		const newSlug = ((await renamed.json()) as { slug: string }).slug;
		await expect(host).toBeHidden({ timeout: 15_000 });
		await expect.poll(() => pathname(page), { timeout: 15_000 }).toBe(`${prev}/${newSlug}`);
		expect(searchOf(page).get('q')).toBe('zy');
		await page.goBack();
		expect(pathname(page)).not.toMatch(/^\/messages/);
		await expect(host).toBeHidden();
	} finally {
		await request.delete(`/api/v1/workspaces/${slug}`, { headers: auth }).catch(() => {});
	}
});

test('printing with Chat open names the page, not /messages/N', async ({ page, request, fixture }, ti) => {
	test.skip(ti.project.name !== 'desktop-chromium', 'desktop only');
	test.setTimeout(90_000);
	const n = await dm(request, fixture, 'print');
	const doc = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: hdr(fixture),
		data: { title: `Print url ${Date.now()}`, content: 'body' }
	});
	expect(doc.ok(), await doc.text()).toBe(true);
	const { slug } = (await doc.json()) as { slug: string };
	await page.setViewportSize({ width: 1600, height: 900 });
	await browserLogin(page);
	const itemPath = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
	await page.goto(itemPath);
	await openChat(page, 1, n);
	expect(pathname(page)).toBe('/messages');
	await page.evaluate(() => window.dispatchEvent(new Event('beforeprint')));
	const printed = await page.locator('.print-footer-url').first().textContent();
	expect(printed).toContain(itemPath);
	expect(printed).not.toContain('/messages');
});
