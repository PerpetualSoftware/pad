import { test, expect } from './fixtures';
import type { APIRequestContext } from '@playwright/test';

/**
 * TASK-4186 PR A / TASK-4315 PR 4: the chat unread badge on the mobile bottom bar's Chat slot,
 * on the REAL mobile-chromium project (Pixel 7). chat-entries.spec.ts fakes
 * mobile widths inside desktop-chromium; this one runs only on the phone
 * project. The badge is compared with GET /api/v1/me/chat/unread `total`.
 */

type Fixture = { apiToken: string; baseURL: string; workspaceSlug: string; adminUsername: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

async function seedUnread(request: APIRequestContext, f: Fixture): Promise<void> {
	const agent = `e2e-mnav-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
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

test('the Chat slot badge matches the server and opens the Chat sheet; You keeps a Chat row', async ({
	page,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'this spec targets the real phone project (Pixel 7)');
	await seedUnread(request, fixture);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	const chat = page.locator('.bottom-nav [data-chat-slot]');
	const you = page.locator('.bottom-nav button.bn-item', { hasText: 'You' });
	await expect(chat).toBeVisible();
	await expect(you.locator('.unread-badge')).toHaveCount(0);

	// Other specs share this admin's inbox, so the server total is re-read on each poll.
	await expect
		.poll(
			async () => {
				const total = await unreadTotal(request, fixture);
				const badge = chat.locator('.unread-badge');
				const shown = (await badge.count()) ? ((await badge.first().textContent()) ?? '').trim() : '';
				return shown === (total > 0 ? label(total) : '');
			},
			{ timeout: 15_000 }
		)
		.toBe(true);
	expect(await unreadTotal(request, fixture)).toBeGreaterThan(0);

	const m = await page.locator('.bottom-nav').evaluate((e) => ({ sw: e.scrollWidth, cw: e.clientWidth }));
	expect(m.sw, `.bottom-nav scrollWidth ${m.sw} > clientWidth ${m.cw}`).toBeLessThanOrEqual(m.cw);

	// The You sheet keeps its own Chat row, and it opens the same sheet.
	await you.click();
	await page.locator('.rows a[href="/messages"]').click();
	await page.waitForURL((u) => u.pathname === '/messages');
	await expect(page.locator('.ds-panel[aria-label="Chat"]')).toBeVisible();
	await expect(page.locator('.bottom-nav')).toBeVisible();
});
