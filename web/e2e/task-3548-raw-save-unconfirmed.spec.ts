import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNC_TIMEOUT } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * TASK-3548: since BUG-3542 a content write to an item open in another tab is
 * refused with 409 content_not_applied, apply_reason unconfirmed_edits, while
 * that tab holds typing the server has not stored. The pane's raw-markdown save
 * did not recognise it: it showed "Failed to save content" and left the text
 * pending. It now asks the same question as the pending-flush refusal, and
 * Overwrite resends once with overwrite_pending_edits.
 *
 * The refusal is answered by a route here: what is under test is the pane's
 * reading of it. The server producing it, from a real tab with typing on the
 * wire, is bug-3542-applier-unconfirmed-edits.spec.ts.
 */
test('a raw save refused for an open tab\'s unconfirmed typing asks, and Overwrite resends with overwrite_pending_edits', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one browser is enough for a client-side branch');
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`, {
		headers: authJson(fixture),
		data: { title: `Raw unconfirmed ${Date.now()}`, content: 'Original body.', fields: JSON.stringify({}) },
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	const doc = (await resp.json()) as { slug: string; ref: string; id: string };

	const sent: Record<string, unknown>[] = [];
	let refused = false;
	await page.route(`**/api/v1/workspaces/${fixture.workspaceSlug}/items/${doc.id}`, async (route) => {
		const req = route.request();
		const body = req.method() === 'PATCH' ? (req.postDataJSON() as Record<string, unknown>) : null;
		if (!body || !('content' in body)) return route.continue();
		sent.push(body);
		if (!refused) {
			refused = true;
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
		}
		return route.continue();
	});

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${doc.ref}`);
	await expect(page.locator(EDITOR_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await page.getByTitle('Raw markdown editor').click();
	const raw = page.locator('.raw-textarea');
	await expect(raw).toBeVisible();
	await raw.click();
	await page.keyboard.press('Control+End');
	await page.keyboard.type(' raw-typed');

	const dialog = page.getByRole('dialog', { name: 'Unsaved edits in an open tab' });
	await expect(dialog, 'the refusal is asked about, not reported as a failed save').toBeVisible({ timeout: 10_000 });
	await expect(page.getByText('Failed to save content')).toHaveCount(0);
	expect(sent).toHaveLength(1);
	expect(sent[0]!.overwrite_pending_edits).toBeUndefined();

	await dialog.getByRole('button', { name: 'Overwrite them' }).click();
	await expect.poll(() => sent.length, { timeout: 10_000 }).toBe(2);
	expect(sent[1]!.overwrite_pending_edits).toBe(true);
	expect(String(sent[1]!.content)).toContain('raw-typed');
	await expect(raw).toHaveValue(/raw-typed/);
});
