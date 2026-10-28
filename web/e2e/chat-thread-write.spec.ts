import { test, expect } from './fixtures';
import type { APIRequestContext, Browser, BrowserContext, Page } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD } from './global-setup';

/**
 * TASK-4044 PR 3b (rulings 4cf4d4b9, 2de240d9, d87482de): the write path of
 * /messages/[n] against the REAL server. Every value asserted is one the server
 * sends: a bearer POST with NO X-Pad-Agent is the human (mine true, source "cli"
 * from the CLI, "web" from this page); with X-Pad-Agent it is that agent. E5
 * asserts the response CODE from the network, never UI text equal to the server
 * sentence. E6 is the mobile project: the keyboard is emulated by overriding
 * visualViewport, and a real-phone check is recorded by hand.
 */

type Fixture = { apiToken: string; baseURL: string };

const headers = (f: Fixture, agent?: string) => ({
	Authorization: `Bearer ${f.apiToken}`,
	'Content-Type': 'application/json',
	...(agent ? { 'X-Pad-Agent': agent } : {})
});

const uniqueAgent = (tag: string) =>
	`e2e-${tag}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;

interface ApiMessage {
	seq: number;
	body: string;
	mine: boolean;
	source: string;
	author_agent_key: string;
	author_is_human: boolean;
	deleted: boolean;
	context_workspace?: unknown;
	reply_to?: { seq: number; excerpt: string };
}

async function createDM(request: APIRequestContext, f: Fixture, agent: string): Promise<number> {
	const res = await request.post('/api/v1/me/conversations', {
		headers: headers(f),
		data: { agent_key: agent }
	});
	expect(res.ok(), await res.text()).toBe(true);
	return ((await res.json()) as { conversation: { number: number } }).conversation.number;
}

async function agentSays(request: APIRequestContext, f: Fixture, n: number, agent: string, body: string) {
	const res = await request.post(`/api/v1/me/conversations/${n}/messages`, {
		headers: headers(f, agent),
		data: { body }
	});
	expect(res.status(), await res.text()).toBe(201);
	return ((await res.json()) as { message: ApiMessage }).message;
}

async function listAll(request: APIRequestContext, f: Fixture, n: number): Promise<ApiMessage[]> {
	const res = await request.get(`/api/v1/me/conversations/${n}/messages?limit=200`, { headers: headers(f) });
	expect(res.ok(), await res.text()).toBe(true);
	return ((await res.json()) as { messages: ApiMessage[] }).messages;
}

/**
 * A context with a COOKIE session: the server stamps a send `source: "web"` only
 * for a cookie; a Bearer context (the default here) is `source: "cli"` (V5).
 */
async function cookieContext(browser: Browser, f: Fixture): Promise<BrowserContext> {
	const ctx = await browser.newContext({ baseURL: f.baseURL });
	const login = await ctx.request.post('/api/v1/auth/login', {
		data: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD }
	});
	expect(login.ok(), await login.text()).toBe(true);
	return ctx;
}

const row = (page: Page, seq: number) => page.locator(`article[data-seq="${seq}"]`);

/** Open a row's menu the way a pointer device does, and click one action. */
async function pick(page: Page, seq: number, action: 'Reply' | 'Copy' | 'Delete') {
	await row(page, seq).hover();
	await row(page, seq).getByRole('button', { name: action, exact: true }).click();
}

async function typeAndSend(page: Page, text: string) {
	await page.getByRole('textbox', { name: 'Message' }).fill(text);
	await page.getByRole('button', { name: 'Send', exact: true }).click();
}

test('E5: send, reply, delete own, delete an agent row, and a reply to a retracted row', async ({
	browser,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the mobile project has E6');
	const ctx = await cookieContext(browser, fixture);
	const page = await ctx.newPage();
	try {
		await e5(page, request, fixture);
	} finally {
		await ctx.close();
	}
});

async function e5(page: Page, request: APIRequestContext, fixture: Fixture) {
	const agent = uniqueAgent('write');
	const n = await createDM(request, fixture, agent);
	const first = await agentSays(request, fixture, n, agent, 'agent line one');
	const doomed = await agentSays(request, fixture, n, agent, 'agent line two');
	const retracted = await agentSays(request, fixture, n, agent, 'agent line three');
	await page.goto(`/messages/${n}`);
	await expect(row(page, retracted.seq)).toContainText('agent line three');

	// Send through the UI: the bubble confirms to a real row with the web shape.
	await typeAndSend(page, 'hello from the web');
	await expect(page.locator('article.pending')).toHaveCount(0);
	const sent = (await listAll(request, fixture, n)).find((m) => m.body === 'hello from the web')!;
	expect(sent).toMatchObject({ source: 'web', mine: true, author_agent_key: '', author_is_human: true });
	expect(sent.context_workspace).toBeUndefined();
	await expect(row(page, sent.seq)).toContainText('hello from the web');

	// Reply through the menu: the API's reply_to.seq is the target, and the chip
	// shows the server's excerpt.
	await pick(page, first.seq, 'Reply');
	await expect(page.locator('.reply')).toContainText('agent line one');
	await typeAndSend(page, 'a reply');
	await expect(page.locator('article.pending')).toHaveCount(0); // r6 L2: the POST has been answered before the API read
	const replied = (await listAll(request, fixture, n)).find((m) => m.body === 'a reply')!;
	expect(replied.reply_to!.seq).toBe(first.seq);
	await expect(row(page, replied.seq).locator('.quote-text')).toHaveText(replied.reply_to!.excerpt);
	await expect(page.locator('.reply')).toHaveCount(0);

	// Delete own: the API row is a tombstone.
	await pick(page, sent.seq, 'Delete');
	await expect(row(page, sent.seq)).toContainText('Message deleted');
	const own = (await listAll(request, fixture, n)).find((m) => m.seq === sent.seq)!;
	expect(own).toMatchObject({ deleted: true, body: '' });

	// Delete an AGENT row as the human: the server allows it (V6).
	await pick(page, doomed.seq, 'Delete');
	await expect(row(page, doomed.seq)).toContainText('Message deleted');
	expect((await listAll(request, fixture, n)).find((m) => m.seq === doomed.seq)).toMatchObject({
		deleted: true,
		body: ''
	});

	// Reply to a row the agent retracted after this page loaded: the network code
	// is reply_target_deleted, the draft is kept and the reply bar is gone.
	await pick(page, retracted.seq, 'Reply');
	const del = await request.delete(`/api/v1/me/conversations/${n}/messages/${retracted.seq}`, {
		headers: headers(fixture, agent)
	});
	expect(del.ok(), await del.text()).toBe(true);
	const answered = page.waitForResponse(
		(r) => r.request().method() === 'POST' && r.url().endsWith(`/api/v1/me/conversations/${n}/messages`)
	);
	await typeAndSend(page, 'too late');
	const res = await answered;
	expect(res.status()).toBe(409);
	expect(((await res.json()) as { error: { code: string } }).error.code).toBe('reply_target_deleted');
	await expect(page.getByRole('textbox', { name: 'Message' })).toHaveValue('too late');
	await expect(page.locator('.reply')).toHaveCount(0);
	await expect(page.locator('article.pending')).toHaveCount(0);
}

test('E5b: an own send scrolls to the end even from further up', async ({ page, request, fixture }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the mobile project has E6');
	const agent = uniqueAgent('scroll');
	const n = await createDM(request, fixture, agent);
	for (let i = 1; i <= 40; i++) await agentSays(request, fixture, n, agent, `filler ${i} `.repeat(8));
	await page.goto(`/messages/${n}`);
	const scroller = page.locator('.scroller');
	await expect(row(page, 40)).toBeVisible();
	await scroller.evaluate((el) => (el.scrollTop = 0));
	await typeAndSend(page, 'from the top of the page');
	await expect(page.locator('article.mine', { hasText: 'from the top of the page' })).toBeInViewport();
});

// 3b r4 Q3 (ruling ec6bbe31): with a gap open and the newest page unreachable, the sent
// row is DETACHED: it shows below the 'Show newer messages' marker and the marker stays.
test('E5c: a send past a gap shows below the Show newer marker while the marker stays', async ({
	page,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the mobile project has E6');
	const agent = uniqueAgent('gap');
	const n = await createDM(request, fixture, agent);
	for (let i = 1; i <= 210; i++) await agentSays(request, fixture, n, agent, `gap ${i}`);
	await page.goto(`/messages/${n}`);
	const marker = page.getByRole('button', { name: 'Show newer messages' });
	await expect(marker).toBeVisible();
	// The re-anchor's newest-page read (no after_seq) fails; every other read goes through.
	await page.route(`**/api/v1/me/conversations/${n}/messages?*`, (route) =>
		new URL(route.request().url()).searchParams.has('after_seq') ? route.continue() : route.abort()
	);
	await typeAndSend(page, 'sent past the gap');
	await expect(page.locator('article.pending')).toHaveCount(0);
	const sent = page.locator('article.bubble', { hasText: 'sent past the gap' });
	await expect(sent).toBeVisible();
	await expect(marker).toBeVisible();
	const m = (await marker.boundingBox())!;
	const s = (await sent.boundingBox())!;
	expect(s.y).toBeGreaterThan(m.y);
	// r6 L1: the own detached row is right-aligned like any own row, not left-aligned and full width.
	const agentRow = (await page.locator('article.bubble:not(.mine)').last().boundingBox())!;
	const col = (await page.locator('.scroller').boundingBox())!;
	expect(s.x).toBeGreaterThan(agentRow.x + 100);
	expect(col.x + col.width - (s.x + s.width)).toBeLessThan(40);
	// Under web/test-results/<test dir>/; the gate script copies it to the evidence folder.
	await page.screenshot({ path: testInfo.outputPath('q3-detached.png') });
});

test('E6: the composer rides above the emulated keyboard, and a touch long-press is the only way in', async ({
	page,
	request,
	fixture
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile-chromium', 'desktop has E5');
	const agent = uniqueAgent('mobile');
	const n = await createDM(request, fixture, agent);
	const line = await agentSays(request, fixture, n, agent, 'a line to long-press');
	await page.addInitScript(() => {
		const vv = Object.assign(new EventTarget(), { height: innerHeight, offsetTop: 0, width: innerWidth });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		(window as unknown as { __vv: typeof vv }).__vv = vv;
	});
	await page.goto(`/messages/${n}`);
	const composer = page.locator('form.composer');
	await expect(composer).toBeVisible();
	const bottomOf = async () => (await composer.boundingBox())!.y + (await composer.boundingBox())!.height;
	const viewportH = page.viewportSize()!.height;
	expect(await bottomOf()).toBeLessThanOrEqual(viewportH);
	expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);

	const KEYBOARD = 300;
	await page.evaluate((kb) => {
		const vv = (window as unknown as { __vv: EventTarget & { height: number } }).__vv;
		vv.height = innerHeight - kb;
		vv.dispatchEvent(new Event('resize'));
	}, KEYBOARD);
	await expect.poll(bottomOf).toBeLessThanOrEqual(viewportH - KEYBOARD);
	await expect(page.getByRole('button', { name: 'Send', exact: true })).toBeInViewport();
	await page.evaluate(() => {
		const vv = (window as unknown as { __vv: EventTarget & { height: number } }).__vv;
		vv.height = innerHeight;
		vv.dispatchEvent(new Event('resize'));
	});
	await expect.poll(bottomOf).toBeLessThanOrEqual(viewportH);

	// A REAL tap (touch events, which move focus, unlike the synthetic pointer events
	// below) opens nothing: no menu and none of its buttons (r5 H1, the Q-c instrument).
	// A 500 ms press then opens the menu, and the lift that ends it presses nothing.
	const bubble = row(page, line.seq);
	const menuButtons = bubble.getByRole('button', { name: /^(Reply|Copy|Delete)$/ });
	await bubble.locator('.body').tap();
	await page.waitForTimeout(700);
	await expect(bubble.locator('.menu')).toHaveCount(0);
	await expect(menuButtons).toHaveCount(0);
	const touch = (type: string) =>
		bubble.dispatchEvent(type, { pointerType: 'touch', bubbles: true, clientX: 50, clientY: 50 });
	await touch('pointerdown');
	await page.waitForTimeout(150);
	await touch('pointerup');
	await page.waitForTimeout(700);
	await expect(bubble.locator('.menu')).toHaveCount(0);
	await touch('pointerdown');
	await expect(bubble.locator('.menu')).toBeVisible({ timeout: 2000 });
	await touch('pointerup');
	await page.waitForTimeout(300);
	await expect(bubble.locator('.menu')).toBeVisible();
	expect((await listAll(request, fixture, n)).find((m) => m.seq === line.seq)!.deleted).toBe(false);
	const boxes = await bubble.locator('.menu button').evaluateAll((els) =>
		els.map((e) => {
			const r = e.getBoundingClientRect();
			return { name: e.textContent, left: Math.round(r.left), right: Math.round(r.right), h: Math.round(r.height) };
		})
	);
	console.log(`Q-c menu geometry (touch): ${JSON.stringify(boxes)}`);
	// A real tap on the bubble body, outside the menu, closes it again; the long-press reopens it.
	await bubble.locator('.body').tap();
	await expect(bubble.locator('.menu')).toHaveCount(0);
	await touch('pointerdown');
	await expect(bubble.locator('.menu')).toBeVisible({ timeout: 2000 });
	await touch('pointerup');
	// Delete is its own deliberate tap, set apart from Copy.
	await bubble.getByRole('button', { name: 'Delete', exact: true }).tap();
	await expect(bubble).toContainText('Message deleted');
});
