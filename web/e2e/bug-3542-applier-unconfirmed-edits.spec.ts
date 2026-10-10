// BUG-3542: a content write routed to an open tab (the designated applier)
// replaced typing the server had not stored yet. The pending-edits check runs
// when the PATCH arrives and reads the op-log, so typing still on the wire is
// invisible to it; the write passed, and the tab's setContent replaced that
// typing. The tab now refuses such an apply, the write answers 409
// content_not_applied (apply_reason unconfirmed_edits), and the typing lands.
// Dave's ruling: tokenless writes are refused too; overwrite_pending_edits is
// the only way past.
//
// Typing on the wire is made deterministic with routeWebSocket: while `hold` is
// set, every frame the page sends is queued, in order (its updates and the
// barrier that would confirm them), so the server's check sees nothing pending
// and the tab cannot learn its typing was stored. The queue is released just
// before the tab's answer to the applier request, which is the order one socket
// gives in real life: the typing was sent before the answer.
import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';
import type { Page, APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

const API_BODY = 'API replaced body.';

type Item = { content: string; seq: number; content_state?: string };

async function openHeld(page: Page, fixture: SuiteFixture, request: APIRequestContext, label: string) {
	const { slug } = await seedDoc(fixture, request, label);
	const url = `/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`;
	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const get = async () => (await (await request.get(url, { headers })).json()) as Item;

	const state = { hold: false, heldUpdates: 0, applierAnswers: [] as string[] };
	const held: (string | Buffer)[] = [];
	await page.routeWebSocket(/\/api\/v1\/collab\//, (ws) => {
		const server = ws.connectToServer();
		server.onMessage((m) => ws.send(m));
		ws.onMessage((m) => {
			if (typeof m === 'string' && /"type":"applier_/.test(m)) {
				state.applierAnswers.push((JSON.parse(m) as { type: string }).type);
				for (const h of held.splice(0)) server.send(h);
				state.hold = false;
			}
			if (state.hold) {
				// y-protocols sync update: message type 0, subtype 2.
				if (typeof m !== 'string' && m[0] === 0 && m[1] === 2) state.heldUpdates++;
				held.push(m);
				return;
			}
			server.send(m);
		});
	});
	const release = () => {
		state.hold = false;
		// Released by the test only when no applier answer did it.
		return held.length;
	};

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	// Past the open-time watermark stamp, so the row starts with nothing pending.
	await expect.poll(async () => (await get()).content_state ?? '', { timeout: 20_000 }).toBe('');
	const before = await get();

	state.hold = true;
	const marker = `typed-${label}-${Date.now()}`;
	await editor.click();
	await page.keyboard.press('End');
	await page.keyboard.type(` ${marker}`);
	await expect(editor).toContainText(marker);
	await expect.poll(() => state.heldUpdates, { message: 'premise: the typing went out and is held' }).toBeGreaterThan(0);
	// Premise: the server's check cannot see the typing.
	expect((await get()).content_state ?? '', 'premise: nothing pending at write time').toBe('');

	return { url, headers, get, before, marker, editor, state, release };
}

for (const leg of [
	{ name: 'with a version token', token: true },
	{ name: 'without a token', token: false },
]) {
	test(`BUG-3542: a write ${leg.name} does not replace typing the server has not stored`, async ({ page, fixture, request }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
		test.setTimeout(120_000);
		const h = await openHeld(page, fixture, request, `BUG-3542-${leg.token ? 'token' : 'tokenless'}`);

		const res = await request.patch(h.url, {
			headers: h.headers,
			data: { content: API_BODY, ...(leg.token ? { expected_seq: h.before.seq } : {}) },
		});
		const body = await res.text();
		expect(h.state.applierAnswers.length, 'premise: the write was routed to this tab').toBeGreaterThan(0);
		expect(res.status(), body).toBe(409);
		expect(h.state.applierAnswers, 'the tab refused rather than applied').toEqual(['applier_refuse']);
		const err = (JSON.parse(body) as { error: { code: string; details: Record<string, unknown> } }).error;
		expect(err.code).toBe('content_not_applied');
		expect(err.details.apply_reason).toBe('unconfirmed_edits');
		expect(err.details.content_landed).toBe(false);

		// The typing survives in the tab and reaches the row.
		await expect(h.editor).toContainText(h.marker);
		await expect(h.editor).not.toContainText(API_BODY);
		await expect.poll(async () => (await h.get()).content, { timeout: 20_000 }).toContain(h.marker);

		// Re-read and resend, as the refusal says: once the tab's typing is
		// confirmed, the write applies.
		await expect.poll(async () => (await h.get()).content_state ?? '', { timeout: 20_000 }).toBe('');
		const fresh = await h.get();
		await expect
			.poll(
				async () =>
					(
						await request.patch(h.url, {
							headers: h.headers,
							data: { content: API_BODY, ...(leg.token ? { expected_seq: (await h.get()).seq } : {}) },
						})
					).status(),
				{ timeout: 20_000, message: `retry after ${fresh.seq} converges` },
			)
			.toBe(200);
		await expect(h.editor).toContainText(API_BODY);
		await expect(h.editor).not.toContainText(h.marker);
	});
}

// Control: overwrite_pending_edits is the escape, and it still replaces.
test('BUG-3542: overwrite_pending_edits still replaces unconfirmed typing', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough');
	test.setTimeout(120_000);
	const h = await openHeld(page, fixture, request, 'BUG-3542-overwrite');

	const res = await request.patch(h.url, {
		headers: h.headers,
		data: { content: API_BODY, expected_seq: h.before.seq, overwrite_pending_edits: true },
	});
	expect(res.status(), await res.text()).toBe(200);
	expect(h.state.applierAnswers, 'premise: the write was routed to this tab').toContain('applier_apply_start');
	await expect(h.editor).toContainText(API_BODY);
	await expect(h.editor).not.toContainText(h.marker);
	expect(h.release()).toBe(0);
});
