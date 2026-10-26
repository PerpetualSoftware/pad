import { test, expect } from './fixtures';
import type { APIRequestContext, Browser, BrowserContext, Page } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';

/**
 * TASK-4044 PR 3a (rulings 4cf4d4b9, 2de240d9): the read-only /messages/[n]
 * thread against the REAL server. Every value below is one the server sends:
 * a bearer POST with NO X-Pad-Agent is authored AS THE HUMAN (mine true,
 * author_agent_key "", source "cli"); with X-Pad-Agent it is that agent
 * (mine false). Unit fixtures cannot see a wrong literal, which is why this
 * runs on a binary built from this tree (CONVE-34).
 *
 * The browser contexts here carry a Bearer header, and the server ignores
 * `chat_inbox` on a bearer stream (docs/api.md), so the realtime specs open a
 * COOKIE session in their own context. PAD_DISABLE_RATE_LIMITS is on for the
 * e2e server, so no spec here can observe a 429: the request-count spec pins
 * reads only, and the burst was checked against real limits once, out of band
 * (TASK-4044 ruling 488a43af).
 */

type Fixture = { apiToken: string; baseURL: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) =>
	`e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

async function createDM(request: APIRequestContext, f: Fixture, agent: string): Promise<number> {
	const res = await request.post('/api/v1/me/conversations', {
		headers: headers(f),
		data: { agent_key: agent }
	});
	expect(res.ok(), await res.text()).toBe(true);
	return ((await res.json()) as { conversation: { number: number } }).conversation.number;
}

async function send(
	request: APIRequestContext,
	f: Fixture,
	n: number,
	body: string,
	agent?: string
): Promise<{ seq: number; mine: boolean }> {
	const res = await request.post(`/api/v1/me/conversations/${n}/messages`, {
		headers: headers(f, agent),
		data: { body }
	});
	expect(res.status(), await res.text()).toBe(201);
	return ((await res.json()) as { message: { seq: number; mine: boolean } }).message;
}

async function summary(request: APIRequestContext, f: Fixture, n: number) {
	const res = await request.get(`/api/v1/me/conversations/${n}`, { headers: headers(f) });
	expect(res.ok(), await res.text()).toBe(true);
	return (await res.json()) as { read_seq: number; last_seq: number; unread: number };
}

/** A context with a cookie session (the stream only relays chat_inbox hints to one). */
async function cookieContext(browser: Browser, f: Fixture): Promise<BrowserContext> {
	const ctx = await browser.newContext({ baseURL: f.baseURL });
	const login = await ctx.request.post('/api/v1/auth/login', {
		data: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD }
	});
	expect(login.ok(), await login.text()).toBe(true);
	return ctx;
}

const rows = (page: Page) => page.locator('article.bubble');

test('E1: orders by seq, puts the divider before the first unread, and counts like the server', async ({
	page,
	request,
	fixture
}) => {
	const agent = uniqueAgent('read');
	const n = await createDM(request, fixture, agent);
	await send(request, fixture, n, 'one from the agent', agent);
	await send(request, fixture, n, 'two from the human');
	await send(request, fixture, n, 'three from the agent', agent);
	await send(request, fixture, n, 'four from the agent', agent);
	const mark = await request.post(`/api/v1/me/conversations/${n}/read`, {
		headers: headers(fixture),
		data: { seq: 1 }
	});
	expect(mark.ok(), await mark.text()).toBe(true);
	// Read BEFORE opening: opening marks the conversation read.
	const before = await summary(request, fixture, n);
	expect(before.unread).toBe(2);

	await page.goto(`/messages/${n}`);
	// With unread the window opens AFTER the marker (seq 1 is above it), so the
	// divider is always inside the window; the read row is one tap away.
	await expect(rows(page)).toHaveCount(3);
	expect(await rows(page).evaluateAll((els) => els.map((e) => e.getAttribute('data-seq')))).toEqual(['2', '3', '4']);
	const divider = page.locator('#unread-divider');
	await expect(divider).toContainText(`${before.unread} new`);
	await expect(divider.locator('xpath=following-sibling::article[1]')).toHaveAttribute('data-seq', '3');
	await expect(rows(page).nth(0)).toHaveClass(/mine/);
	await expect(rows(page).nth(0).locator('.author')).toHaveText('You');
	await expect(rows(page).nth(1).locator('.author')).toHaveText(agent);
	await expect(rows(page).nth(1)).not.toHaveClass(/mine/);

	await page.getByRole('button', { name: 'Load earlier' }).click();
	await expect(rows(page)).toHaveCount(4);
	expect(await rows(page).evaluateAll((els) => els.map((e) => e.getAttribute('data-seq')))).toEqual(['1', '2', '3', '4']);
	await expect(rows(page).nth(0).locator('.author')).toHaveText(agent);
	await expect(page.getByRole('button', { name: 'Load earlier' })).toHaveCount(0);
});

test('E3: opening marks the displayed seq read, the badge clears, the divider stays', async ({
	page,
	request,
	fixture
}) => {
	const agent = uniqueAgent('mark');
	const n = await createDM(request, fixture, agent);
	await send(request, fixture, n, 'a', agent);
	await send(request, fixture, n, 'b', agent);
	expect((await summary(request, fixture, n)).unread).toBe(2);

	await page.goto(`/messages/${n}`);
	await expect(rows(page)).toHaveCount(2);
	await expect
		.poll(async () => (await summary(request, fixture, n)).read_seq, { timeout: 10_000 })
		.toBe(2);
	const unread = await request.get('/api/v1/me/chat/unread', { headers: headers(fixture) });
	const body = (await unread.json()) as { conversations: Array<{ number: number }> };
	expect(body.conversations.find((c) => c.number === n)).toBeUndefined();
	await expect(page.locator('#unread-divider')).toContainText('2 new');
});

test('E4: a failed load shows Try again, and Try again loads the thread', async ({
	page,
	request,
	fixture
}) => {
	const agent = uniqueAgent('retry');
	const n = await createDM(request, fixture, agent);
	await send(request, fixture, n, 'still here', agent);
	const detail = new RegExp(`/api/v1/me/conversations/${n}$`);
	await page.route(detail, (route) => route.fulfill({ status: 500, body: '{}' }));
	await page.goto(`/messages/${n}`);
	await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible();
	await expect(rows(page)).toHaveCount(0);
	await page.unroute(detail);
	await page.getByRole('button', { name: 'Try again' }).click();
	await expect(rows(page)).toHaveCount(1);
	await expect(rows(page).first()).toContainText('still here');
});

test('E2: a new message and a retraction reach the open thread without a reload', async ({
	browser,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	const agent = uniqueAgent('live');
	const n = await createDM(request, fixture, agent);
	await send(request, fixture, n, 'first', agent);
	const ctx = await cookieContext(browser, fixture);
	try {
		const page = await ctx.newPage();
		await page.goto(`/messages/${n}`);
		await expect(rows(page)).toHaveCount(1);

		const second = await send(request, fixture, n, 'arrived live', agent);
		await expect(rows(page)).toHaveCount(2, { timeout: 15_000 });
		await expect(page.locator(`article[data-seq="${second.seq}"]`)).toContainText('arrived live');

		const del = await request.delete(`/api/v1/me/conversations/${n}/messages/${second.seq}`, {
			headers: headers(fixture, agent)
		});
		expect(del.ok(), await del.text()).toBe(true);
		const row = page.locator(`article[data-seq="${second.seq}"]`);
		await expect(row).toContainText('Message deleted', { timeout: 15_000 });
		await expect(row).not.toContainText('arrived live');
		await expect(rows(page)).toHaveCount(2);
	} finally {
		await ctx.close();
	}
});

test('E8: a ten-message burst costs at most four reads per message per tab', async ({
	browser,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(60_000);
	const agent = uniqueAgent('burst');
	const n = await createDM(request, fixture, agent);
	await send(request, fixture, n, 'seed', agent);
	const ctx = await cookieContext(browser, fixture);
	try {
		const a = await ctx.newPage();
		const b = await ctx.newPage();
		await a.goto(`/messages/${n}`);
		await b.goto(`/messages/${n}`);
		await expect(rows(a)).toHaveCount(1);
		await expect(rows(b)).toHaveCount(1);

		const reads = { a: 0, b: 0 };
		for (const [tab, page] of [['a', a], ['b', b]] as const) {
			page.on('response', (r) => {
				const u = new URL(r.url());
				if (r.request().method() === 'GET' && u.pathname === `/api/v1/me/conversations/${n}/messages`) reads[tab]++;
			});
		}
		for (let i = 1; i <= 10; i++) {
			await send(request, fixture, n, `burst ${i}`, agent);
			await a.waitForTimeout(450);
		}
		await expect(rows(a)).toHaveCount(11, { timeout: 15_000 });
		await expect(rows(b)).toHaveCount(11, { timeout: 15_000 });
		// Measured on this server: 36 reads for 10 messages in one tab. A hint costs two
		// reads (the newest-anchored page and the tombstone page), and the mark-read it
		// triggers resyncs the badge, whose unread fetch bumps the version and costs two
		// more (partly coalesced). So the bound is 4 per message, plus 4 of slack for one
		// coalesced refresh, in EACH tab; the agent send bucket (20/min) keeps a sustained
		// burst under 80 reads/min per tab against the 600/min API bucket.
		console.log(`E8 reads: tab a ${reads.a}, tab b ${reads.b}`);
		expect(reads.a).toBeLessThanOrEqual(10 * 4 + 4);
		expect(reads.b).toBeLessThanOrEqual(10 * 4 + 4);
	} finally {
		await ctx.close();
	}
});
