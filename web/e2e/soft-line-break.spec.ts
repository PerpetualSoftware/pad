import { test, expect, type SuiteFixture } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { DatabaseSync } from 'node:sqlite';
import { createHash } from 'node:crypto';
import * as Y from 'yjs';
import { updateFrame } from '../src/lib/collab/materializer/replay';
import { SCHEMA_VERSION } from '../src/lib/collab/schemaVersion';
import { E2E_DB_PATH } from './lib/data-dir';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

const endpoint = (fixture: SuiteFixture, slug: string) => `/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`;
async function stored(request: APIRequestContext, fixture: SuiteFixture, slug: string) {
	const response = await request.get(endpoint(fixture, slug), { headers: authJson(fixture) });
	expect(response.ok()).toBe(true);
	return (await response.json()).content as string;
}
async function open(page: Page, fixture: SuiteFixture, slug: string) {
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	return page.locator(EDITOR_SELECTOR);
}

// These tests inspect the private instance's op-log and must run serially.
test.describe.configure({ mode: 'serial' });
test.beforeEach(({}, info) => {
	test.skip(info.project.name !== 'desktop-chromium', 'one browser covers the editor path');
	test.setTimeout(90_000);
});

test('whole-body PATCH preserves newlines through the open editor flush', async ({ page, fixture, request }) => {
	const body = 'one\ntwo\nthree';
	const doc = await seedDoc(fixture, request, 'BUG-4447');
	const editor = await open(page, fixture, doc.slug);
	const response = await request.patch(endpoint(fixture, doc.slug), {
		headers: authJson(fixture), data: { content: body, replace: true },
	});
	expect(response.ok(), await response.text()).toBe(true);
	expect((await response.json()).warnings?.content_outcome).toBe('applied_pending_flush');
	await expect(editor.locator('br').first()).toBeAttached({ timeout: SYNC_TIMEOUT });
	await expect(editor).toContainText('three');
	await expect.poll(() => stored(request, fixture, doc.slug), { timeout: 30_000 }).toBe(body);
	await page.waitForTimeout(7_000);
	expect(await stored(request, fixture, doc.slug)).toBe(body);
	await page.reload();
	await expect(page.locator(`${EDITOR_SELECTOR} br`).first()).toBeAttached({ timeout: SYNC_TIMEOUT });
});

test('a task-item continuation stores a plain newline', async ({ page, fixture, request }) => {
	const doc = await seedDoc(fixture, request, 'BUG-4447 task');
	const editor = await open(page, fixture, doc.slug);
	await editor.click();
	await page.keyboard.type('[ ] task');
	await expect(editor.getByRole('checkbox')).toHaveCount(1);
	await page.keyboard.press('Shift+Enter');
	await page.keyboard.type('more');
	await expect.poll(() => stored(request, fixture, doc.slug), { timeout: 30_000 }).toBe('- [ ] task\n  more');
});

test('Shift+Enter stores a plain newline', async ({ page, fixture, request }) => {
	const doc = await seedDoc(fixture, request, 'BUG-4447 typing');
	const editor = await open(page, fixture, doc.slug);
	await editor.click();
	await page.keyboard.type('one');
	await page.keyboard.press('Shift+Enter');
	await page.keyboard.type('two');
	await expect.poll(() => stored(request, fixture, doc.slug), { timeout: 30_000 }).toBe('one\ntwo');
});

for (const body of ['one\ntwo', 'one\\\ntwo', 'one  \ntwo', '**one\ntwo**', 'one\r\ntwo', 'one\rtwo']) {
	test(`a no-edit view preserves stored bytes for ${JSON.stringify(body)}`, async ({ page, fixture, request }) => {
		const doc = await seedDoc(fixture, request, 'BUG-4447 view');
		const response = await request.patch(endpoint(fixture, doc.slug), {
			headers: authJson(fixture), data: { content: body, replace: true },
		});
		expect(response.ok()).toBe(true);
		expect(await stored(request, fixture, doc.slug)).toBe(body);
		const editor = await open(page, fixture, doc.slug);
		await expect(editor.locator('br').first()).toBeAttached();
		await page.waitForTimeout(7_000);
		await page.goto('about:blank');
		await page.waitForTimeout(2_000);
		expect(await stored(request, fixture, doc.slug)).toBe(body);
	});
}

test('a no-edit view of a flat legacy op-log preserves the stored newlines', async ({ page, fixture, request }) => {
	const body = 'one\ntwo';
	const doc = await seedDoc(fixture, request, 'BUG-4447 flat replay');
	const response = await request.patch(endpoint(fixture, doc.slug), {
		headers: authJson(fixture), data: { content: body, replace: true },
	});
	expect(response.ok()).toBe(true);
	const legacy = new Y.Doc();
	const paragraph = new Y.XmlElement('paragraph');
	const text = new Y.XmlText();
	legacy.getXmlFragment('default').insert(0, [paragraph]);
	paragraph.insert(0, [text]);
	text.insert(0, 'one two');
	const frame = updateFrame(Y.encodeStateAsUpdate(legacy));
	legacy.destroy();
	const db = new DatabaseSync(E2E_DB_PATH);
	try {
		// The live test server may be committing another worker's write.
		db.exec('PRAGMA busy_timeout = 5000');
		const row = db.prepare(`INSERT INTO item_yjs_updates
			(item_id, update_data, schema_version, created_at, content_hash, content_bearing)
			VALUES (?, ?, ?, ?, ?, 1)`).run(doc.id, frame, SCHEMA_VERSION, new Date().toISOString(), createHash('sha256').update(frame).digest('hex'));
		// A fully flushed legacy log must not take the pending-row recovery path.
		db.prepare('UPDATE items SET content_flushed_op_log_id = ? WHERE id = ?').run(Number(row.lastInsertRowid), doc.id);
	} finally { db.close(); }
	const editor = await open(page, fixture, doc.slug);
	await expect(editor).toHaveText('one two');
	await expect(editor.locator('br')).toHaveCount(0);
	await page.waitForTimeout(7_000);
	await page.goto('about:blank');
	await page.waitForTimeout(2_000);
	expect(await stored(request, fixture, doc.slug)).toBe(body);
});
