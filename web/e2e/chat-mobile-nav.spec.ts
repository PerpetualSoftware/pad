import { test, expect } from './fixtures';
import type { APIRequestContext } from '@playwright/test';

/**
 * TASK-4186 PR A: the chat unread badge on the mobile bottom bar's You slot,
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

test('the You slot badge matches the server, opens the sheet, and Chat lands on /messages', async ({
	page,
	request,
	fixture
}, testInfo) => {
	expect(testInfo.project.name, 'this spec is for the real phone project').toBe('mobile-chromium');
	await seedUnread(request, fixture);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}`);
	const you = page.locator('.bottom-nav button.bn-item', { hasText: 'You' });
	await expect(you).toBeVisible();

	// Other specs share this admin's inbox, so the server total is re-read on each poll.
	await expect
		.poll(
			async () => {
				const total = await unreadTotal(request, fixture);
				const badge = you.locator('.unread-badge');
				const shown = (await badge.count()) ? ((await badge.first().textContent()) ?? '').trim() : '';
				return shown === (total > 0 ? label(total) : '');
			},
			{ timeout: 15_000 }
		)
		.toBe(true);
	expect(await unreadTotal(request, fixture)).toBeGreaterThan(0);

	const m = await page.locator('.bottom-nav').evaluate((e) => ({ sw: e.scrollWidth, cw: e.clientWidth }));
	expect(m.sw, `.bottom-nav scrollWidth ${m.sw} > clientWidth ${m.cw}`).toBeLessThanOrEqual(m.cw);

	await you.click();
	await page.locator('.rows a[href="/messages"]').click();
	await page.waitForURL((u) => u.pathname === '/messages');
});
