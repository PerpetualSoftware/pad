import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3548 (codex, lead ruling B): an unconfirmed-edits refusal (409
 * content_not_applied, BUG-3542) still commits its row write, so the row's seq
 * moves under the caller's token. An Overwrite resent with that token met
 * update_conflict: the user said Overwrite and the save did not land.
 *
 * Each leg answers the FIRST content save the way the server does: it makes a
 * real, non-content change to the row (seq moves; the body does not), then
 * returns the refusal. Every later save goes to the real server, which accepts
 * the overwrite only with the fresh token. "Lands" is read back from the server.
 */

type Doc = { slug: string; ref: string; id: string; title: string };

async function create(fixture: SuiteFixture, request: APIRequestContext, coll: string, title: string, body: string, fields: Record<string, unknown> = {}): Promise<Doc> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: authJson(fixture),
		data: { title, content: body, fields: JSON.stringify(fields) },
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return { ...((await resp.json()) as Doc), title };
}

async function stored(fixture: SuiteFixture, request: APIRequestContext, doc: Doc) {
	const r = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) });
	return (await r.json()) as { content: string; seq: number };
}

/** Refuse the first content PATCH as an unconfirmed-edits refusal does, moving seq first. */
type Bump = 'title' | 'neutral';

/**
 * The refused write's row half. 'title' renames the item (so the slug moves too,
 * which the by-id re-read must survive); 'neutral' writes a field no save here
 * sends, for a page whose save sends the title itself.
 */
async function refuseFirstContentSave(page: Page, fixture: SuiteFixture, request: APIRequestContext, doc: Doc, bump: Bump = 'title') {
	const sent: Record<string, unknown>[] = [];
	let refused = false;
	const mine = (url: string) => url.endsWith(`/items/${doc.id}`) || url.endsWith(`/items/${doc.slug}`);
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/*`, async (route) => {
		const req = route.request();
		if (req.method() !== 'PATCH' || !mine(new URL(req.url()).pathname)) return route.continue();
		const body = req.postDataJSON() as Record<string, unknown>;
		if (!('content' in body)) return route.continue();
		sent.push(body);
		if (refused) return route.continue();
		refused = true;
		// The refused write's row half: seq moves, the body does not.
		const moved = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, {
			headers: authJson(fixture),
			data: bump === 'title' ? { title: `${doc.title} (row moved)` } : { fields_patch: { e2e_row_moved: Date.now() } },
		});
		expect(moved.ok(), await moved.text()).toBeTruthy();
		return route.fulfill({
			status: 409,
			contentType: 'application/json',
			body: JSON.stringify({
				error: {
					code: 'content_not_applied',
					message: `${doc.ref}: an open editor has edits the server has not stored yet, so the content was not applied (other fields were).`,
					details: { apply_reason: 'unconfirmed_edits', ref: doc.ref },
				},
			}),
		});
	});
	return sent;
}

async function overwrite(page: Page) {
	const dialog = page.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
	await expect(dialog).toBeVisible({ timeout: 10_000 });
	await dialog.getByRole('button', { name: 'Overwrite them' }).click();
}

test.describe('TASK-3548: Overwrite after an unconfirmed-edits refusal lands', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a client-side branch');
	});

	test('raw markdown editor', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'docs', `Raw lands ${Date.now()}`, 'Original body.');
		const sent = await refuseFirstContentSave(page, fixture, request, doc);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
		await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await page.getByTitle('Raw markdown editor').click();
		const raw = page.locator('.raw-textarea');
		await expect(raw).toBeVisible();
		await raw.click();
		await page.keyboard.press('Control+End');
		await page.keyboard.type(' raw-landed');
		await overwrite(page);
		await expect.poll(async () => (await stored(fixture, request, doc)).content, { timeout: 10_000 }).toContain('raw-landed');
		// One question, one overwrite: the answer was not lost to a conflict and re-asked.
		expect(sent).toHaveLength(2);
		expect(sent[1]!.overwrite_pending_edits).toBe(true);
		// The overwrite carried the token the refused write moved to, not the old one.
		expect(Number(sent[1]!.expected_seq)).toBeGreaterThan(Number(sent[0]!.expected_seq));
	});

	test('conventions inline edit', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'conventions', `Conv lands ${Date.now()}`, 'Original convention body.', { status: 'active', trigger: 'on-implement' });
		const sent = await refuseFirstContentSave(page, fixture, request, doc);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/conventions`);
		const row = page.locator('.convention-row', { has: page.locator('.row-title', { hasText: doc.title }) });
		await row.locator('.row-main').click();
		await row.getByRole('button', { name: 'Edit' }).click();
		await row.locator('.edit-textarea').fill('A convention body that lands.');
		await row.getByRole('button', { name: 'Save' }).click();
		await overwrite(page);
		await expect.poll(async () => (await stored(fixture, request, doc)).content, { timeout: 10_000 }).toBe('A convention body that lands.');
		expect(sent).toHaveLength(2);
		expect(sent[1]!.overwrite_pending_edits).toBe(true);
	});

	test('playbook editor', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'playbooks', `PB lands ${Date.now()}`, 'Original playbook body.', { status: 'draft' });
		const sent = await refuseFirstContentSave(page, fixture, request, doc, 'neutral');
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${doc.slug}`);
		await expect(page.locator('.title-input')).toHaveValue(doc.title);
		await page.locator('textarea').first().fill('A playbook body that lands.');
		await page.getByRole('button', { name: /^Save$/ }).click();
		await overwrite(page);
		await expect.poll(async () => (await stored(fixture, request, doc)).content, { timeout: 10_000 }).toBe('A playbook body that lands.');
		expect(sent).toHaveLength(2);
		expect(sent[1]!.overwrite_pending_edits).toBe(true);
	});

	test('raw editor flush on leaving raw mode', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'docs', `Flush lands ${Date.now()}`, 'Original body.');
		const sent = await refuseFirstContentSave(page, fixture, request, doc);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
		await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		const toggle = page.getByTitle('Raw markdown editor');
		await toggle.click();
		const raw = page.locator('.raw-textarea');
		await expect(raw).toBeVisible();
		await raw.click();
		await page.keyboard.press('Control+End');
		await page.keyboard.type(' flush-landed');
		// Leave raw mode inside the 1.2s save debounce, so the flush sends it.
		await toggle.click();
		await overwrite(page);
		await expect.poll(async () => (await stored(fixture, request, doc)).content, { timeout: 10_000 }).toContain('flush-landed');
		expect(sent).toHaveLength(2);
		expect(sent[1]!.overwrite_pending_edits).toBe(true);
		expect(Number(sent[1]!.expected_seq)).toBeGreaterThan(Number(sent[0]!.expected_seq));
	});

	test('restoring markdown kept from a refused unload save', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'docs', `Restore lands ${Date.now()}`, 'Original body.');
		const me = (await (await request.get('/api/v1/auth/me', { headers: authJson(fixture) })).json()) as { id: string };
		await page.addInitScript(([key, value]) => localStorage.setItem(key, value), [
			`pad:refused-raw-draft:${me.id}:${doc.id}`,
			JSON.stringify({ markdown: 'Markdown kept in this browser.', savedAt: Date.now() }),
		] as const);
		const sent = await refuseFirstContentSave(page, fixture, request, doc);
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
		const notice = page.getByTestId('refused-raw-draft');
		await expect(notice, 'premise: the kept draft is offered').toBeVisible({ timeout: SYNC_TIMEOUT });
		await notice.getByRole('button', { name: 'Restore it' }).click();
		await overwrite(page);
		await expect.poll(async () => (await stored(fixture, request, doc)).content, { timeout: 10_000 }).toBe('Markdown kept in this browser.');
		// The restored body then reaches the rich editor, whose own collab flush
		// (op_log_cursor) is not part of this exchange.
		const saves = sent.filter((b) => !('op_log_cursor' in b));
		expect(saves).toHaveLength(2);
		expect(saves[1]!.overwrite_pending_edits).toBe(true);
		expect(Number(saves[1]!.expected_seq)).toBeGreaterThan(Number(saves[0]!.expected_seq));
	});

	// codex P1: the playbook save sends its title. A title someone else set after
	// the refusal must not be put back by the overwrite: no fresh token then, and
	// the save meets the conflict instead.
	test('playbook editor: an overwrite does not restore a title someone else changed', async ({ page, fixture, request }) => {
		const doc = await create(fixture, request, 'playbooks', `PB theirs ${Date.now()}`, 'Original playbook body.', { status: 'draft' });
		await refuseFirstContentSave(page, fixture, request, doc, 'title');
		await browserLogin(page);
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/playbooks/${doc.slug}`);
		await expect(page.locator('.title-input')).toHaveValue(doc.title);
		await page.locator('textarea').first().fill('A playbook body.');
		await page.getByRole('button', { name: /^Save$/ }).click();
		await overwrite(page);
		await page.waitForTimeout(1500);
		const after = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, { headers: authJson(fixture) })).json()) as { title: string };
		expect(after.title, 'their title stands').toBe(`${doc.title} (row moved)`);
	});
});

