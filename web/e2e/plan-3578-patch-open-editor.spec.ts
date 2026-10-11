import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * PLAN-3578 step 2 (and TASK-3580's demo): an agent's patch to an item open in
 * the browser lands as a narrowed replace of the live document. The typist's
 * caret stays in the paragraph it was in, and what they type next lands
 * there, not at the start or end of a document a whole-body replace rebuilt.
 */
test('an agent patch lands in the open editor without moving the caret', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for the applier path');
	test.setTimeout(90_000);
	const doc = await seedDoc(fixture, request, 'P3578 patch');
	const base = `/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.slug}`;
	const seeded = await request.patch(base, {
		headers: authJson(fixture),
		data: { content: 'alpha paragraph\n\nbeta paragraph\n\ngamma paragraph', replace: true },
	});
	expect(seeded.ok(), await seeded.text()).toBeTruthy();

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toContainText('gamma paragraph');

	// Caret after "gamma" in the third paragraph.
	await editor.locator('p', { hasText: 'gamma paragraph' }).click();
	await page.evaluate(() => {
		const p = Array.from(document.querySelectorAll('.editor-content .ProseMirror p')).find((n) => n.textContent === 'gamma paragraph')!;
		const r = document.createRange();
		r.setStart(p.firstChild!, 'gamma'.length);
		r.collapse(true);
		const s = window.getSelection()!;
		s.removeAllRanges();
		s.addRange(r);
	});

	// The agent patches the FIRST paragraph while the caret sits in the third.
	const resp = await request.post(`${base}/patch`, {
		headers: authJson(fixture),
		data: { edits: [{ old: 'alpha paragraph', new: 'ALPHA paragraph, patched' }] },
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	const out = (await resp.json()) as { content_state?: string; diff: string };
	expect(out.content_state).toBe('applied_pending_flush');
	expect(out.diff).toContain('+ALPHA paragraph, patched');

	await expect(editor).toContainText('ALPHA paragraph, patched', { timeout: SYNC_TIMEOUT });
	// The caret is still in "gamma": typing lands there.
	await page.keyboard.type('X');
	await expect(editor.locator('p', { hasText: 'gammaX paragraph' })).toBeVisible();
	await expect(editor.locator('p').first()).toHaveText('ALPHA paragraph, patched');
});
