import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';

/**
 * TASK-4315 PR 4: Chat on a phone is a full-height sheet above the bottom bar,
 * opened by a shallow navigation over the page (jsdom cannot run that, so this is
 * where it is proven). Runs only on the real phone project (Pixel 7). Covers the
 * Chat slot and its badge, the sheet above the bar, Back / Forward / reload / Close,
 * the page underneath keeping its place, sibling slots while Chat is open, the
 * standalone route with its compact bar, the soft keyboard (a faked visualViewport),
 * and the long-press menu.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) =>
	`e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

async function seedDM(request: APIRequestContext, f: Fixture, tag: string, bodies = ['hello from the agent']) {
	const agent = uniqueAgent(tag);
	const res = await request.post('/api/v1/me/conversations', { headers: headers(f), data: { agent_key: agent } });
	expect(res.ok(), await res.text()).toBe(true);
	const n = ((await res.json()) as { conversation: { number: number } }).conversation.number;
	for (const body of bodies) {
		const say = await request.post(`/api/v1/me/conversations/${n}/messages`, {
			headers: headers(f, agent),
			data: { body }
		});
		expect(say.status(), await say.text()).toBe(201);
	}
	return { n, agent };
}

const FAST_MS = 1500;
const pathname = (page: Page) => new URL(page.url()).pathname;
const sheet = (page: Page) => page.locator('.ds-panel[aria-label="Chat"]');
const bar = (page: Page) => page.locator('nav.bottom-nav[aria-label="Primary"]');
const chatSlot = (page: Page) => page.locator('nav.bottom-nav [data-chat-slot]');
const home = (f: Fixture) => `/${f.adminUsername}/${f.workspaceSlug}`;

test.beforeEach(async ({}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'this spec targets the real phone project (Pixel 7)');
});

test('the bar has Workspace, Search, plus, Chat, You; the badge is on Chat', async ({ page, request, fixture }) => {
	await seedDM(request, fixture, 'slots');
	await page.goto(home(fixture));
	await expect(bar(page)).toBeVisible();
	const names = await bar(page)
		.locator('.bn-item')
		.evaluateAll((els) => els.map((e) => e.querySelector('.bn-label')?.textContent?.trim() ?? e.getAttribute('aria-label')));
	expect(names).toEqual(['Workspace', 'Search', 'Quick capture', 'Chat', 'You']);
	await expect(bar(page).getByText('Activity', { exact: true })).toHaveCount(0);
	await expect(chatSlot(page).locator('.unread-badge')).toBeVisible({ timeout: 15_000 });
	await expect(bar(page).locator('button.bn-item', { hasText: 'You' }).locator('.unread-badge')).toHaveCount(0);
	// Activity is a tile in the Workspace sheet now.
	await bar(page).locator('button.bn-item', { hasText: 'Workspace' }).click();
	await expect(page.locator('.ds-panel[aria-label="Workspace"]').getByText('Activity', { exact: true })).toBeVisible();
});

test('Chat opens above the bar; Back, Forward, reload and Close behave', async ({ page, request, fixture }) => {
	const { n } = await seedDM(request, fixture, 'sheet');
	await page.goto(home(fixture));
	const homePath = pathname(page);
	await chatSlot(page).click();
	await expect(sheet(page)).toBeVisible();
	await expect.poll(() => pathname(page)).toBe('/messages');
	await expect(chatSlot(page)).toHaveAttribute('aria-expanded', 'true');

	// Above the bar, never over it.
	await expect(bar(page)).toBeVisible();
	const sb = await sheet(page).boundingBox();
	const bb = await bar(page).boundingBox();
	expect(sb!.y + sb!.height).toBeLessThanOrEqual(bb!.y + 1);

	// Back closes it; Forward brings it back.
	await page.goBack();
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
	expect(pathname(page)).toBe(homePath);
	await page.goForward();
	await expect(sheet(page)).toBeVisible();

	// Inbox to thread to inbox, then hardware Back one step at a time.
	await sheet(page).locator(`a[href="/messages/${n}"]`).click();
	await expect.poll(() => pathname(page)).toBe(`/messages/${n}`);
	await page.goBack();
	await expect.poll(() => pathname(page), { timeout: FAST_MS }).toBe('/messages');
	await expect(sheet(page)).toBeVisible();
	await page.goBack();
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
	expect(pathname(page)).toBe(homePath);

	// Close Chat.
	await chatSlot(page).click();
	await expect(sheet(page)).toBeVisible();
	await sheet(page).getByRole('button', { name: 'Close Chat' }).click();
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
	expect(pathname(page)).toBe(homePath);

	// A reload of the shallow URL is the standalone route, with the compact bar.
	await chatSlot(page).click();
	await expect(sheet(page)).toBeVisible();
	await page.reload();
	await expect(page.locator('main.messages-main')).toBeVisible();
	await expect(sheet(page)).toHaveCount(0);
	await expect(bar(page)).toBeVisible();
	await expect(bar(page).locator('.bn-label')).toHaveText(['Workspaces', 'Chat', 'You']);
});

test('the page underneath keeps its text, and You or Workspace while Chat is open leave the draft', async ({
	page,
	request,
	fixture
}) => {
	const { n } = await seedDM(request, fixture, 'draft');
	await page.goto(home(fixture));
	await chatSlot(page).click();
	await sheet(page).locator(`a[href="/messages/${n}"]`).click();
	const box = sheet(page).getByRole('textbox', { name: 'Message' });
	await expect(box).toBeVisible();
	await box.fill('half written reply');

	// You: Chat goes away, You opens above the bar.
	await bar(page).locator('button.bn-item', { hasText: 'You' }).click();
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
	await expect(page.locator('.ds-panel[aria-label="Account"]')).toBeVisible();

	// Reopen Chat from the slot, then the thread: the draft is still there.
	await chatSlot(page).click();
	await expect(sheet(page)).toBeVisible();
	await expect(page.locator('.ds-panel[aria-label="Account"]')).toBeHidden();
	await sheet(page).locator(`a[href="/messages/${n}"]`).click();
	await expect(sheet(page).getByRole('textbox', { name: 'Message' })).toHaveValue('half written reply');

	// Workspace while Chat is open.
	await bar(page).locator('button.bn-item', { hasText: 'Workspace' }).click();
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
	await expect(page.locator('.ds-panel[aria-label="Workspace"]')).toBeVisible();
});

test('a soft keyboard: the composer stays inside the visual viewport, no double inset, bar hidden, Close reachable', async ({
	page,
	request,
	fixture
}) => {
	await page.addInitScript(() => {
		const vv = Object.assign(new EventTarget(), {
			height: window.innerHeight,
			width: window.innerWidth,
			offsetTop: 0,
			offsetLeft: 0,
			scale: 1
		});
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		(window as unknown as { __vv: typeof vv }).__vv = vv;
	});
	const { n } = await seedDM(request, fixture, 'kb');
	await page.goto(home(fixture));
	// The init script ran before layout, so its height may not be the settled one. Make the
	// fake the settled height and let the app re-capture its keyboard-closed baseline
	// (it does so on orientationchange), as a real rotation would.
	await page.evaluate(() => {
		const vv = (window as unknown as { __vv: { height: number } & EventTarget }).__vv;
		vv.height = window.innerHeight;
		window.dispatchEvent(new Event('orientationchange'));
	});
	await page.waitForTimeout(500);
	await chatSlot(page).click();
	await sheet(page).locator(`a[href="/messages/${n}"]`).click();
	const box = sheet(page).getByRole('textbox', { name: 'Message' });
	await expect(box).toBeVisible();

	const KEYBOARD = 320;
	const vvBottom = await page.evaluate((k) => {
		const vv = (window as unknown as { __vv: { height: number } & EventTarget }).__vv;
		vv.height = window.innerHeight - k;
		vv.dispatchEvent(new Event('resize'));
		return vv.height;
	}, KEYBOARD);

	await expect(bar(page)).toHaveCount(0);
	await expect
		.poll(async () => {
			const b = await sheet(page).boundingBox();
			return b ? Math.round(b.y + b.height) : -1;
		})
		.toBeLessThanOrEqual(vvBottom + 1);
	const tb = await box.boundingBox();
	expect(tb!.y + tb!.height).toBeLessThanOrEqual(vvBottom + 1);
	// No double inset: the composer sits near the sheet's bottom edge, not a keyboard-height above it.
	const sb = await sheet(page).boundingBox();
	expect(sb!.y + sb!.height - (tb!.y + tb!.height)).toBeLessThan(120);
	const close = sheet(page).getByRole('button', { name: 'Close Chat' });
	await expect(close).toBeInViewport();
	const cb = await close.boundingBox();
	expect(cb!.y).toBeGreaterThanOrEqual(0);

	// Keyboard down: the bar returns and the sheet sits above it again.
	await page.evaluate(() => {
		const vv = (window as unknown as { __vv: { height: number } & EventTarget }).__vv;
		vv.height = window.innerHeight;
		vv.dispatchEvent(new Event('resize'));
	});
	await expect(bar(page)).toBeVisible();
	const sb2 = await sheet(page).boundingBox();
	const bb2 = await bar(page).boundingBox();
	expect(sb2!.y + sb2!.height).toBeLessThanOrEqual(bb2!.y + 1);
});

test('a long press opens the message menu; a 12px move does not; Escape closes only the menu', async ({
	page,
	request,
	fixture
}) => {
	const { n } = await seedDM(request, fixture, 'press', ['long press me, please']);
	await page.goto(home(fixture));
	await chatSlot(page).click();
	await sheet(page).locator(`a[href="/messages/${n}"]`).click();
	const bubble = sheet(page).locator('article.bubble').first();
	await expect(bubble).toBeVisible();
	const r = (await bubble.locator('.body, p').last().boundingBox())!;
	const x = r.x + 20;
	const y = r.y + r.height / 2;
	const cdp = await page.context().newCDPSession(page);
	const touch = (type: 'touchStart' | 'touchMove' | 'touchEnd', dx = 0) =>
		cdp.send('Input.dispatchTouchEvent', {
			type,
			touchPoints: type === 'touchEnd' ? [] : [{ x: x + dx, y }]
		});

	// A 12px move before the timer cancels it.
	await touch('touchStart');
	await touch('touchMove', 12);
	await page.waitForTimeout(650);
	await touch('touchEnd');
	await expect(page.getByRole('menuitem', { name: 'Reply' })).toHaveCount(0);

	// A held press opens it.
	await touch('touchStart');
	await page.waitForTimeout(650);
	await touch('touchEnd');
	await expect(page.getByRole('menuitem', { name: 'Reply' })).toBeVisible();
	await expect(page.getByRole('menuitem', { name: 'Copy' })).toBeVisible();
	await expect(page.getByRole('menuitem', { name: 'Delete' })).toBeVisible();

	// Escape closes the menu, not the sheet.
	await page.keyboard.press('Escape');
	await expect(page.getByRole('menuitem', { name: 'Reply' })).toHaveCount(0);
	await expect(sheet(page)).toBeVisible();
	expect(pathname(page)).toBe(`/messages/${n}`);
	await page.keyboard.press('Escape');
	await expect(sheet(page)).toBeHidden({ timeout: FAST_MS });
});

test('/ has the compact bar; login and a share page have none', async ({ page, fixture, browser }) => {
	await page.goto('/');
	await expect(bar(page)).toBeVisible();
	await expect(bar(page).locator('.bn-label')).toHaveText(['Workspaces', 'Chat', 'You']);
	await chatSlot(page).click();
	await expect(sheet(page)).toBeVisible();
	await expect(bar(page)).toBeVisible();

	const anon = await browser.newContext({ baseURL: fixture.baseURL });
	try {
		const p = await anon.newPage();
		await p.goto('/login');
		await expect(p.locator('nav.bottom-nav')).toHaveCount(0);
		await p.goto('/s/not-a-real-token');
		await expect(p.locator('nav.bottom-nav')).toHaveCount(0);
	} finally {
		await anon.close();
	}
});
