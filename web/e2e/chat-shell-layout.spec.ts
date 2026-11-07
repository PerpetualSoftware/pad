import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';

/**
 * TASK-4315 PR 3b, real browser: where the Chat host is a docked column and
 * where it is a right drawer (ChatHost: the space after the sidebar must be
 * 440 + 560 = 1000px, so 1260 with the 260px sidebar), what a live resize across
 * that line does to the thread inside, the sidebar toggle re-deciding it, and the
 * keys of the drawer and of the dock. jsdom has no layout, so these live here.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) => `e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

async function createAdminDM(request: APIRequestContext, f: Fixture, agent: string) {
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

const chatButton = (page: Page) => page.locator('header.topbar a.chat-button');
const host = (page: Page) => page.locator('[data-chat-host]');
const SIDEBAR = 260;
const BOUNDARY = SIDEBAR + 440 + 560; // 1260

async function openThread(page: Page, n: number) {
	await chatButton(page).click();
	await expect(host(page)).toBeVisible();
	await host(page).locator(`a[href="/messages/${n}"]`).click();
	await expect(host(page).getByText('hello from the agent')).toBeVisible();
}

async function setup(page: Page, request: APIRequestContext, fixture: Fixture, tag: string, width: number) {
	const n = await createAdminDM(request, fixture, uniqueAgent(tag));
	await page.setViewportSize({ width, height: 900 });
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	await expect(chatButton(page)).toBeVisible();
	return n;
}

const desktopOnly = (name: string) => name !== 'desktop-chromium';

test.describe('Chat dock and drawer layout', () => {
	test.beforeEach(async ({}, testInfo) => {
		test.skip(desktopOnly(testInfo.project.name), 'desktop only');
		test.setTimeout(90_000);
	});

	test('the dock/drawer line is sidebar + 440 + 560, on each side', async ({ page, request, fixture }) => {
		await setup(page, request, fixture, 'boundary', BOUNDARY);
		expect((await page.locator('aside.sidebar').boundingBox())!.width, 'premise: sidebar width').toBe(SIDEBAR);

		await chatButton(page).click();
		await expect(page.getByRole('complementary', { name: 'Chat' })).toBeVisible();
		await expect(page.getByRole('dialog', { name: 'Chat' })).toHaveCount(0);
		await page.getByRole('complementary', { name: 'Chat' }).getByRole('button', { name: 'Close Chat' }).click();
		await expect(host(page)).toHaveCount(0);

		await page.setViewportSize({ width: BOUNDARY - 1, height: 900 });
		await chatButton(page).click();
		await expect(page.getByRole('dialog', { name: 'Chat' })).toBeVisible();
		await expect(page.getByRole('complementary', { name: 'Chat' })).toHaveCount(0);
	});

	test('a live resize across the line keeps the thread element and the draft', async ({ page, request, fixture }) => {
		const n = await setup(page, request, fixture, 'resize', BOUNDARY + 100);
		await openThread(page, n);
		await expect(page.getByRole('complementary', { name: 'Chat' })).toBeVisible();
		const box = host(page).getByRole('textbox', { name: 'Message' });
		await box.fill('a draft that must survive');
		await page.evaluate(() => {
			const w = window as unknown as { __ta: Element; __host: Element };
			w.__ta = document.querySelector('[data-chat-host] textarea')!;
			w.__host = document.querySelector('[data-chat-host]')!;
		});
		const same = () =>
			page.evaluate(() => {
				const w = window as unknown as { __ta: Element; __host: Element };
				return {
					ta: w.__ta === document.querySelector('[data-chat-host] textarea') && w.__ta.isConnected,
					host: w.__host === document.querySelector('[data-chat-host]'),
					value: (document.querySelector('[data-chat-host] textarea') as HTMLTextAreaElement).value
				};
			});

		await page.setViewportSize({ width: BOUNDARY - 1, height: 900 });
		await expect(page.getByRole('dialog', { name: 'Chat' })).toBeVisible();
		expect(await same()).toEqual({ ta: true, host: true, value: 'a draft that must survive' });

		await page.setViewportSize({ width: BOUNDARY, height: 900 });
		await expect(page.getByRole('complementary', { name: 'Chat' })).toBeVisible();
		expect(await same()).toEqual({ ta: true, host: true, value: 'a draft that must survive' });
		await expect(host(page).getByText('hello from the agent')).toBeVisible();
	});

	test('toggling the sidebar with Chat open re-decides dock or drawer', async ({ page, request, fixture }) => {
		// 1100 is a drawer with the sidebar (840 left) and a dock without it (1100 >= 1000).
		await setup(page, request, fixture, 'sidebar', 1100);
		await chatButton(page).click();
		await expect(page.getByRole('dialog', { name: 'Chat' })).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(host(page)).toHaveCount(0);

		// Hide the sidebar first (the drawer would cover it), then open Chat: a dock.
		await page.getByRole('button', { name: 'Hide sidebar' }).click();
		await expect(page.locator('aside.sidebar.collapsed')).toHaveCount(1);
		await chatButton(page).click();
		await expect(page.getByRole('complementary', { name: 'Chat' })).toBeVisible();

		// Show it again with Chat open: back to a drawer.
		await page.getByRole('button', { name: 'Open sidebar' }).click();
		await expect(page.locator('aside.sidebar.collapsed')).toHaveCount(0);
		await expect(page.getByRole('dialog', { name: 'Chat' })).toBeVisible();
		await expect(page.getByRole('complementary', { name: 'Chat' })).toHaveCount(0);
	});

	test('drawer keys: focus stays inside, Escape closes and returns focus, Escape in the menu closes only the menu', async ({
		page,
		request,
		fixture
	}) => {
		const n = await setup(page, request, fixture, 'keys', 1100);
		await openThread(page, n);
		const drawer = page.getByRole('dialog', { name: 'Chat' });
		await expect(drawer).toBeVisible();
		const inside = () =>
			page.evaluate(() => {
				const at = document.activeElement;
				return !!at && !!at.closest('[data-chat-host]');
			});

		await drawer.focus();
		for (let i = 0; i < 25; i++) {
			await page.keyboard.press('Tab');
			expect(await inside(), `Tab #${i + 1} left the drawer`).toBe(true);
		}
		for (let i = 0; i < 25; i++) {
			await page.keyboard.press('Shift+Tab');
			expect(await inside(), `Shift+Tab #${i + 1} left the drawer`).toBe(true);
		}

		// Escape in the open message menu closes the menu only.
		const more = drawer.getByRole('button', { name: /^More actions for/ }).first();
		await more.click();
		const menu = page.getByRole('menu').first();
		await expect(menu).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(menu).toBeHidden();
		await expect(drawer).toBeVisible();

		// Escape then closes the drawer and focus returns to the Chat button.
		await drawer.focus();
		await page.keyboard.press('Escape');
		await expect(host(page)).toHaveCount(0);
		await expect(chatButton(page)).toBeFocused();
	});

	test('docked: Escape inside the dock closes neither the dock nor the item pane', async ({ page, request, fixture }) => {
		await createAdminDM(request, fixture, uniqueAgent('dock-keys'));
		await seedDoc(fixture, request, 'Dock keys');
		await page.setViewportSize({ width: 1600, height: 900 });
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?view=list`);
		await page.locator('.item-card').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await chatButton(page).click();
		const dock = page.getByRole('complementary', { name: 'Chat' });
		await expect(dock).toBeVisible();
		await dock.locator('a[href^="/messages/"]').first().click();
		const box = dock.getByRole('textbox', { name: 'Message' });
		await box.focus();
		await page.keyboard.press('Escape');
		await page.waitForTimeout(500);
		await expect(dock).toBeVisible();
		await expect(pane).toBeVisible();
	});

	test('screenshots of the dock and the drawer', async ({ page, request, fixture }) => {
		const dir = process.env.CHAT_SHOTS_DIR;
		test.skip(!dir, 'CHAT_SHOTS_DIR not set');
		const n = await setup(page, request, fixture, 'shots', 1600);
		await openThread(page, n);
		await expect(page.getByRole('complementary', { name: 'Chat' })).toBeVisible();
		await page.screenshot({ path: `${dir}/dock-1600.png` });
		await page.setViewportSize({ width: 1100, height: 900 });
		await expect(page.getByRole('dialog', { name: 'Chat' })).toBeVisible();
		await page.screenshot({ path: `${dir}/drawer-1100.png` });
	});
});
