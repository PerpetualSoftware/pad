import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';

/**
 * TASK-4044 PR 3c (ruling 687eb810, Option C): the Chat entries on the
 * workspace sidebar and on the /console nav bar, against the REAL server.
 *
 * Layout is the question (the sidebar footer row once overflowed on desktop,
 * comment 89264cdd), and jsdom does no layout, so this runs in a real engine at
 * the 1280 desktop width (255 px sidebar) and in the mobile drawer. The badge is
 * compared with GET /api/v1/me/chat/unread `total`, never with a constant.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

async function seedUnread(request: APIRequestContext, f: Fixture): Promise<void> {
	const agent = `e2e-entry-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
	const dm = await request.post('/api/v1/me/conversations', { headers: headers(f), data: { agent_key: agent } });
	expect(dm.ok(), await dm.text()).toBe(true);
	const n = ((await dm.json()) as { conversation: { number: number } }).conversation.number;
	for (const body of ['one', 'two']) {
		const res = await request.post(`/api/v1/me/conversations/${n}/messages`, {
			headers: headers(f, agent),
			data: { body }
		});
		expect(res.status(), await res.text()).toBe(201);
	}
}

async function unreadTotal(request: APIRequestContext, f: Fixture): Promise<number> {
	const res = await request.get('/api/v1/me/chat/unread', { headers: headers(f) });
	expect(res.ok(), await res.text()).toBe(true);
	return ((await res.json()) as { total: number }).total;
}

const label = (total: number) => (total > 99 ? '99+' : String(total));

async function expectBadgeMatchesServer(page: Page, request: APIRequestContext, f: Fixture, entry: string) {
	// Other specs share this admin's inbox, so the server total is read again on each poll.
	await expect
		.poll(
			async () => {
				const total = await unreadTotal(request, f);
				const badge = page.locator(`${entry} .unread-badge`);
				const shown = (await badge.count()) ? ((await badge.first().textContent()) ?? '').trim() : '';
				return shown === (total > 0 ? label(total) : '');
			},
			{ timeout: 15_000 }
		)
		.toBe(true);
}

async function noHorizontalOverflow(page: Page, selector: string) {
	const m = await page.locator(selector).first().evaluate((e) => ({ sw: e.scrollWidth, cw: e.clientWidth }));
	expect(m.sw, `${selector} scrollWidth ${m.sw} > clientWidth ${m.cw}`).toBeLessThanOrEqual(m.cw);
}

test('E9: the sidebar Chat entry at desktop 1280 (255 px sidebar)', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	await seedUnread(request, fixture);
	await page.setViewportSize({ width: 1280, height: 900 });
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	const entry = 'nav.collection-nav a.nav-item[href="/messages"]';
	await expect(page.locator(entry)).toBeVisible();
	await expect(page.locator(entry)).toHaveAttribute('href', '/messages');
	const sidebar = await page.locator('aside.sidebar').evaluate((e) => e.getBoundingClientRect().width);
	// Measured 260 px outer (255 px inside its 1 px border and scrollbar allowance).
	expect(Math.round(sidebar)).toBeGreaterThanOrEqual(255);
	expect(Math.round(sidebar)).toBeLessThanOrEqual(260);
	expect(await unreadTotal(request, fixture)).toBeGreaterThan(0);
	await expectBadgeMatchesServer(page, request, fixture, entry);
	await noHorizontalOverflow(page, 'nav.collection-nav');
	await noHorizontalOverflow(page, 'aside.sidebar');
	await noHorizontalOverflow(page, 'aside.sidebar .sidebar-inner');
	await page.locator(entry).click();
	await page.waitForURL((u) => u.pathname === '/messages');
});

test('E9: the sidebar Chat entry in the mobile drawer', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project drives both widths');
	await seedUnread(request, fixture);
	await page.setViewportSize({ width: 700, height: 900 });
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	// The drawer starts closed; open it as sidebar-footer-row.spec.ts does.
	const toggle = page.locator('button[aria-label*="idebar" i]').first();
	if (await toggle.count()) await toggle.click().catch(() => {});
	const entry = 'nav.collection-nav a.nav-item[href="/messages"]';
	await expect(page.locator(entry)).toBeVisible();
	await expect(page.locator(entry)).toHaveAttribute('href', '/messages');
	await expectBadgeMatchesServer(page, request, fixture, entry);
	await noHorizontalOverflow(page, 'nav.collection-nav');
	await noHorizontalOverflow(page, 'aside.sidebar');
});

test('E9: the /console nav shows the Chat link with the same badge', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop only');
	await seedUnread(request, fixture);
	await page.setViewportSize({ width: 1280, height: 900 });
	await page.goto('/console');
	const entry = '.console-nav .nav-links a[href="/messages"]';
	await expect(page.locator(entry)).toBeVisible();
	await expect(page.locator(entry)).toHaveText(/Chat/);
	await expectBadgeMatchesServer(page, request, fixture, entry);
	await noHorizontalOverflow(page, '.console-nav');
	await noHorizontalOverflow(page, '.console-nav .nav-links');
	// Sits right after "Needs you".
	const hrefs = await page.locator('.console-nav .nav-links a').evaluateAll((els) => els.map((e) => e.getAttribute('href')));
	expect(hrefs.indexOf('/messages')).toBe(hrefs.indexOf('/console/needs') + 1);
});

test('E9: the /console Chat link works from the mobile menu', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project drives both widths');
	await seedUnread(request, fixture);
	// The console menu collapses at max-width 640px; the sidebar's drawer at 768px.
	await page.setViewportSize({ width: 600, height: 900 });
	await page.goto('/console');
	await page.locator('.mobile-hamburger').click();
	const entry = '.console-nav .nav-links.open a[href="/messages"]';
	await expect(page.locator(entry)).toBeVisible();
	await expectBadgeMatchesServer(page, request, fixture, entry);
	await noHorizontalOverflow(page, '.console-nav .nav-links.open');
	await page.locator(entry).click();
	await page.waitForURL((u) => u.pathname === '/messages');
});
