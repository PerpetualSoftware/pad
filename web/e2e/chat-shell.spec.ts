import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { browserLogin, seedDoc, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { createChatUser, loginViaForm } from './lib/chat-users';

/**
 * TASK-4315 PR 3b: Chat opens INSIDE the Pad shell on a desktop, over the page
 * that opened it. jsdom cannot run shallow routing, so this is where it is
 * proven: the editor under an open Chat keeps its node, its text, its caret and
 * its scroll through open and close (as a dock at 1400, narrow enough that the text re-wraps, and as a drawer at 1100);
 * browser Back and Forward walk the entries Chat pushed; a reload or a bookmark
 * of /messages/N is the same thread as a route in Pad chrome; and a number that
 * is not yours reads exactly like a number that cannot exist.
 * Desktop only: a phone keeps the standalone /messages route in this PR.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) =>
	`e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

async function createAdminDM(request: import('@playwright/test').APIRequestContext, f: Fixture, agent: string) {
	const res = await request.post('/api/v1/me/conversations', { headers: headers(f), data: { agent_key: agent } });
	expect(res.ok(), await res.text()).toBe(true);
	const n = ((await res.json()) as { conversation: { number: number } }).conversation.number;
	const say = await request.post(`/api/v1/me/conversations/${n}/messages`, {
		headers: headers(f, agent),
		data: { body: 'hello from the agent' }
	});
	expect(say.status(), await say.text()).toBe(201);
	return n;
}

// A latch that rode its 3s fallback would miss this by a wide margin.
const FAST_MS = 1500;

const LONG = Array.from({ length: 120 }, (_, i) => `Paragraph ${i + 1}. ${'lorem ipsum '.repeat(8)}`).join('\n\n');

async function seedLongDoc(request: import('@playwright/test').APIRequestContext, f: Fixture) {
	const res = await request.post(`/api/v1/workspaces/${f.workspaceSlug}/collections/docs/items`, {
		headers: headers(f),
		data: { title: `Chat shell ${Date.now()}`, content: LONG }
	});
	expect(res.ok(), await res.text()).toBe(true);
	return (await res.json()) as { slug: string; ref: string };
}

/** The nearest scrollable ancestor of the editor, tagged so it can be found again. */
async function tagScroller(page: Page) {
	await page.evaluate((sel) => {
		const ed = document.querySelector(sel) as HTMLElement;
		(window as unknown as { __editor: Element }).__editor = ed;
		let el: HTMLElement | null = ed.parentElement;
		while (el) {
			const oy = getComputedStyle(el).overflowY;
			if ((oy === 'auto' || oy === 'scroll') && el.scrollHeight > el.clientHeight + 50) break;
			el = el.parentElement;
		}
		el?.setAttribute('data-e2e-scroller', '1');
	}, EDITOR_SELECTOR);
}

const scrollTop = (page: Page) =>
	page.evaluate(() => (document.querySelector('[data-e2e-scroller]') as HTMLElement | null)?.scrollTop ?? -1);

/**
 * The reader's place is the content at the top of the viewport, not a raw
 * scrollTop: the dock narrows the page by 440px, the text re-wraps (differently
 * per font), and the browser's scroll anchoring moves scrollTop so the same
 * paragraph stays where it was. markAnchor remembers the first paragraph in
 * view; anchorOffset is where that paragraph sits below the scroller's top.
 */
const markAnchor = (page: Page) =>
	page.evaluate((sel) => {
		const s = document.querySelector('[data-e2e-scroller]') as HTMLElement;
		const top = s.getBoundingClientRect().top;
		const p = Array.from(document.querySelector(sel)!.querySelectorAll('p')).find(
			(el) => el.getBoundingClientRect().bottom > top
		);
		(window as unknown as { __anchor: Element | undefined }).__anchor = p;
		return p?.textContent?.slice(0, 20) ?? null;
	}, EDITOR_SELECTOR);

const anchorOffset = (page: Page) =>
	page.evaluate(() => {
		const a = (window as unknown as { __anchor?: Element }).__anchor;
		const s = document.querySelector('[data-e2e-scroller]') as HTMLElement | null;
		return a && a.isConnected && s ? a.getBoundingClientRect().top - s.getBoundingClientRect().top : null;
	});

const caret = (page: Page) =>
	page.evaluate(() => {
		const sel = window.getSelection();
		return sel && sel.anchorNode
			? { text: sel.anchorNode.textContent?.slice(0, 40) ?? '', offset: sel.anchorOffset, collapsed: sel.isCollapsed }
			: null;
	});

const sameEditorNode = (page: Page) =>
	page.evaluate(
		(sel) => (window as unknown as { __editor: Element }).__editor === document.querySelector(sel),
		EDITOR_SELECTOR
	);

const chatButton = (page: Page) => page.locator('header.topbar a.chat-button');
const pathname = (page: Page) => new URL(page.url()).pathname;

for (const [name, width, role] of [
	['dock', 1400, 'complementary'],
	['drawer', 1100, 'dialog']
] as const) {
	test(`the editor keeps its node, text, caret and scroll through opening and closing Chat as a ${name}`, async ({
		page,
		request,
		fixture
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
		test.setTimeout(90_000);
		await createAdminDM(request, fixture, uniqueAgent(`keep-${name}`));
		const doc = await seedLongDoc(request, fixture);
		await page.setViewportSize({ width, height: 900 });
		await browserLogin(page);
		const itemUrl = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${doc.slug}`;
		await page.goto(itemUrl);
		await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await tagScroller(page);

		await page.locator(EDITOR_SELECTOR).click();
		await page.keyboard.press('Control+Home');
		await page.keyboard.type('TYPED-BEFORE-CHAT ');
		await page.evaluate(() => {
			const s = document.querySelector('[data-e2e-scroller]') as HTMLElement | null;
			if (s) s.scrollTop = 600;
		});
		const scrolled = await scrollTop(page);
		expect(scrolled, 'premise: the page scrolls').toBeGreaterThan(100);
		expect(await markAnchor(page), 'premise: a paragraph is in view').toContain('Paragraph');
		const anchorBefore = (await anchorOffset(page))!;
		const caretBefore = await caret(page);
		expect(caretBefore?.text, 'premise: the caret is in the typed text').toContain('TYPED-BEFORE-CHAT');
		const sidebarBefore = await page.locator('aside.sidebar').boundingBox();

		await chatButton(page).click();
		const region = page.getByRole(role, { name: 'Chat' });
		await expect(region).toBeVisible();
		expect(pathname(page)).toBe('/messages');
		await expect(region.getByRole('button', { name: 'Close Chat' })).toBeVisible();
		// Opening Chat neither collapses the sidebar nor remounts the page.
		await expect(page.locator('aside.sidebar')).toBeVisible();
		expect((await page.locator('aside.sidebar').boundingBox())!.width).toBe(sidebarBefore!.width);
		expect(await sameEditorNode(page)).toBe(true);
		// Same paragraph, same place on screen (a re-wrap may move scrollTop itself).
		expect(Math.abs((await anchorOffset(page))! - anchorBefore)).toBeLessThan(4);

		await region.getByRole('button', { name: 'Close Chat' }).click();
		await expect(region).toBeHidden();
		await expect.poll(() => pathname(page)).toBe(itemUrl);
		expect(await sameEditorNode(page)).toBe(true);
		expect(Math.abs((await anchorOffset(page))! - anchorBefore)).toBeLessThan(4);
		await expect(page.locator(EDITOR_SELECTOR)).toContainText('TYPED-BEFORE-CHAT');

		// The caret is where it was (focus went back to the Chat button, as it should);
		// putting focus in the editor again continues typing in place.
		expect(await caret(page)).toEqual(caretBefore);
		await page.locator(EDITOR_SELECTOR).focus();
		await page.keyboard.type('AFTER');
		await expect(page.locator(EDITOR_SELECTOR)).toContainText('TYPED-BEFORE-CHAT AFTER');
	});
}

test('Back and Forward walk the entries Chat pushed', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const agent = uniqueAgent('history');
	const n = await createAdminDM(request, fixture, agent);
	await page.setViewportSize({ width: 1600, height: 900 });
	const home = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;
	await page.goto(home);
	await expect(chatButton(page)).toBeVisible();
	const homePath = pathname(page);

	await chatButton(page).click();
	const region = page.getByRole('complementary', { name: 'Chat' });
	await expect(region).toBeVisible();
	expect(pathname(page)).toBe('/messages');

	await region.locator(`a[href="/messages/${n}"]`).click();
	await expect.poll(() => pathname(page)).toBe(`/messages/${n}`);
	await expect(region.getByText('hello from the agent')).toBeVisible();

	await page.goBack();
	await expect.poll(() => pathname(page)).toBe('/messages');
	await expect(region).toBeVisible();
	await expect(region.locator(`a[href="/messages/${n}"]`)).toBeVisible();

	await page.goBack();
	await expect.poll(() => pathname(page)).toBe(homePath);
	await expect(region).toBeHidden();

	await page.goForward();
	await expect.poll(() => pathname(page)).toBe('/messages');
	await expect(region).toBeVisible();

	// The thread's own back control returns to the inbox in one entry, and Close pops all of them.
	await region.locator(`a[href="/messages/${n}"]`).click();
	await expect.poll(() => pathname(page)).toBe(`/messages/${n}`);
	await region.getByRole('link', { name: 'All conversations' }).click();
	await expect.poll(() => pathname(page)).toBe('/messages');
	await region.locator(`a[href="/messages/${n}"]`).click();
	await expect.poll(() => pathname(page)).toBe(`/messages/${n}`);
	await region.getByRole('button', { name: 'Close Chat' }).click();
	await expect.poll(() => pathname(page)).toBe(homePath);
	await expect(region).toBeHidden();
});

test('a reload or bookmark of /messages/N is the same thread as a route inside Pad chrome', async ({
	page,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const n = await createAdminDM(request, fixture, uniqueAgent('direct'));
	await page.setViewportSize({ width: 1600, height: 900 });
	const home = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;
	await page.goto(home);
	await chatButton(page).click();
	await page.getByRole('complementary', { name: 'Chat' }).locator('a[href^="/messages/"]').first().click();
	await expect.poll(() => pathname(page)).toMatch(/^\/messages\/\d+$/);

	for (const how of ['reload', 'bookmark'] as const) {
		if (how === 'reload') await page.reload();
		else await page.goto(`/messages/${n}`);
		await expect(page.locator('aside.sidebar')).toBeVisible();
		await expect(page.locator('header.topbar')).toBeVisible();
		await expect(page.locator('main')).toHaveCount(1);
		await expect(page.locator('main.main-content').getByText('hello from the agent')).toBeVisible();
		// A route, not the surface: no dock, no drawer, no second thread.
		await expect(page.locator('[data-chat-host]')).toHaveCount(0);
		await expect(page.locator('.scroller')).toHaveCount(1);
	}
});

test('a number that is invalid reads like a number that is not yours', async ({ page, browser, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const n = await createAdminDM(request, fixture, uniqueAgent('private'));
	await page.setViewportSize({ width: 1600, height: 900 });
	const seen = new Map<string, string>();
	for (const path of ['/messages/abc', '/messages/0', '/messages/007', '/messages/987654']) {
		await page.goto(path);
		const main = page.locator('main.main-content');
		await expect(main.getByRole('heading', { name: 'Conversation not found' })).toBeVisible();
		await expect(main.getByRole('textbox', { name: 'Message' })).toHaveCount(0);
		seen.set(path, (await main.locator('.messages-main').innerText()).trim());
	}
	expect(new Set(seen.values()).size, JSON.stringify([...seen])).toBe(1);

	// Same-account access: another account asking for admin's real conversation number.
	const other = await createChatUser('shellOther');
	const ctx = await browser.newContext({ baseURL: fixture.baseURL });
	try {
		const p = await ctx.newPage();
		await p.setViewportSize({ width: 1600, height: 900 });
		await loginViaForm(p, other.email);
		await p.goto(`/messages/${n}`);
		const main = p.locator('main.main-content');
		await expect(main.getByRole('heading', { name: 'Conversation not found' })).toBeVisible();
		expect((await main.locator('.messages-main').innerText()).trim()).toBe([...seen.values()][0]);
	} finally {
		await ctx.close();
	}
});

test('Close and Back are usable again at once after a completed one', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	const n = await createAdminDM(request, fixture, uniqueAgent('again'));
	await page.setViewportSize({ width: 1600, height: 900 });
	const home = `/${fixture.adminUsername}/${fixture.workspaceSlug}`;
	await page.goto(home);
	await expect(chatButton(page)).toBeVisible();
	const homePath = pathname(page);
	const region = page.getByRole('complementary', { name: 'Chat' });

	for (let round = 0; round < 2; round++) {
		await chatButton(page).click();
		await expect(region).toBeVisible();
		await region.locator(`a[href="/messages/${n}"]`).click();
		await expect.poll(() => pathname(page)).toBe(`/messages/${n}`);
		// Back to the inbox, then Close straight after: neither waits on a fallback.
		await region.getByRole('link', { name: 'All conversations' }).click();
		await expect.poll(() => pathname(page), { timeout: FAST_MS }).toBe('/messages');
		await region.getByRole('button', { name: 'Close Chat' }).click();
		await expect.poll(() => pathname(page), { timeout: FAST_MS }).toBe(homePath);
		await expect(region).toBeHidden({ timeout: FAST_MS });
	}
});

test('closing the item pane while Chat is open pops Chat and the pane together', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	test.setTimeout(60_000);
	await createAdminDM(request, fixture, uniqueAgent('pane'));
	await seedDoc(fixture, request, 'Chat over pane');
	await page.setViewportSize({ width: 1600, height: 900 });
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?view=list`);
	const prePaneUrl = page.url();
	const row = page.locator('.item-card').first();
	await expect(row).toBeVisible();
	await row.click();
	const pane = page.locator('.item-pane');
	await expect(pane).toBeVisible();

	await chatButton(page).click();
	const region = page.getByRole('complementary', { name: 'Chat' });
	await expect(region).toBeVisible();
	await expect(pane).toBeVisible();

	// The pane's close counts the entries Chat pushed on top of its own, so one
	// click leaves neither the pane nor Chat half open.
	await pane.locator('button[aria-label="Close pane"]').click();
	await expect(pane).toBeHidden();
	await expect(region).toBeHidden();
	await expect.poll(() => page.url()).toBe(prePaneUrl);
});

// The normal docked flow is Chat first, then a pane. Opening a pane is a real
// navigation, so it closes the shallow Chat under it; closing the pane then goes
// back to where the pane was opened from, and must not bring Chat back.
test.describe('a pane opened over Chat closes without reopening Chat', () => {
	const listUrl = (f: Fixture) => `/${f.adminUsername}/${f.workspaceSlug}/docs?view=list`;

	test('Chat first, then a list row, then close the pane (dock)', async ({ page, request, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
		test.setTimeout(60_000);
		await createAdminDM(request, fixture, uniqueAgent('chat-then-pane'));
		await seedDoc(fixture, request, 'Chat then pane');
		await page.setViewportSize({ width: 1600, height: 900 });
		await browserLogin(page);
		await page.goto(listUrl(fixture));
		const prePaneUrl = page.url();
		const region = page.getByRole('complementary', { name: 'Chat' });
		const pane = page.locator('.item-pane');

		await chatButton(page).click();
		await expect(region).toBeVisible();
		await page.locator('.item-card').first().click();
		// The latch released on the traversal's own popstate, not its 3s fallback.
		await expect(pane).toBeVisible({ timeout: FAST_MS });
		// The pane open is a real navigation: Chat is already closed.
		await expect(region).toBeHidden({ timeout: FAST_MS });

		await pane.locator('button[aria-label="Close pane"]').click();
		await expect(pane).toBeHidden({ timeout: FAST_MS });
		await expect(region).toBeHidden();
		await expect.poll(() => page.url(), { timeout: FAST_MS }).toBe(prePaneUrl);
		// And it stays closed.
		await page.waitForTimeout(500);
		await expect(region).toBeHidden();
	});

	test('re-targeting the open pane from another row while Chat is open', async ({ page, request, fixture }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
		test.setTimeout(60_000);
		await createAdminDM(request, fixture, uniqueAgent('retarget'));
		await seedDoc(fixture, request, 'Retarget one');
		await seedDoc(fixture, request, 'Retarget two');
		await page.setViewportSize({ width: 1600, height: 900 });
		await browserLogin(page);
		await page.goto(listUrl(fixture));
		const prePaneUrl = page.url();
		const region = page.getByRole('complementary', { name: 'Chat' });
		const pane = page.locator('.item-pane');

		await page.locator('.item-card').nth(0).click();
		await expect(pane).toBeVisible();
		await chatButton(page).click();
		await expect(region).toBeVisible();
		await page.locator('.item-card').nth(1).click();
		await expect(pane).toBeVisible();
		await expect(region).toBeHidden({ timeout: FAST_MS });

		await pane.locator('button[aria-label="Close pane"]').click();
		await expect(pane).toBeHidden();
		await expect(region).toBeHidden();
		await expect.poll(() => page.url()).toBe(prePaneUrl);
	});

	test('a pane loaded cold closes in place under Chat, and Back does not bring it back', async ({
		page,
		request,
		fixture
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
		test.setTimeout(60_000);
		await createAdminDM(request, fixture, uniqueAgent('cold'));
		await seedDoc(fixture, request, 'Cold pane');
		await page.setViewportSize({ width: 1600, height: 900 });
		await browserLogin(page);
		await page.goto(listUrl(fixture));
		await page.locator('.item-card').first().click();
		await expect(page.locator('.item-pane')).toBeVisible();
		const item = new URL(page.url()).searchParams.get('item');
		expect(item, 'premise: the pane URL names its item').toBeTruthy();
		await page.goto(listUrl(fixture));
		const coldUrl = `${listUrl(fixture)}&item=${encodeURIComponent(item!)}`;
		await page.goto(coldUrl);
		const region = page.getByRole('complementary', { name: 'Chat' });
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		await chatButton(page).click();
		await expect(region).toBeVisible();
		await pane.locator('button[aria-label="Close pane"]').click();
		await expect(pane).toBeHidden();
		await expect(region).toBeHidden();
		expect(new URL(page.url()).searchParams.has('item')).toBe(false);

		// The cold entry below was edited in place, not left behind with the pane on it.
		await page.goBack();
		await expect.poll(() => new URL(page.url()).searchParams.has('item')).toBe(false);
		await expect(pane).toBeHidden();
	});
});

test('Escape pressed inside the docked Chat does not close the item pane beside it', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	test.setTimeout(60_000);
	await createAdminDM(request, fixture, uniqueAgent('dock-escape'));
	await seedDoc(fixture, request, 'Dock escape');
	await page.setViewportSize({ width: 1600, height: 900 });
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?view=list`);
	await page.locator('.item-card').first().click();
	const pane = page.locator('.item-pane');
	await expect(pane).toBeVisible();

	await chatButton(page).click();
	const region = page.getByRole('complementary', { name: 'Chat' });
	await expect(region).toBeVisible();
	await region.focus();
	await page.keyboard.press('Escape');
	await page.waitForTimeout(500);
	// The dock has no Escape, and the key belongs to the dock while focus is in it.
	await expect(region).toBeVisible();
	await expect(pane).toBeVisible();
});

test('a click outside the open user menu leaves focus on the control that was clicked', async ({ page, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	await page.setViewportSize({ width: 1600, height: 900 });
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?view=list`);
	const field = page.getByRole('button', { name: 'Toggle theme' });
	await expect(field).toBeVisible();
	await page.getByRole('button', { name: 'User menu' }).click();
	await expect(page.getByRole('menu', { name: 'User menu' })).toBeVisible();
	await field.click();
	await expect(page.getByRole('menu', { name: 'User menu' })).toBeHidden();
	await expect(field).toBeFocused();
});
