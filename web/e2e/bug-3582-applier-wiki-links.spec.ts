import { test, expect } from './fixtures';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';
import { authJson } from './lib/attachment-viewer';

/**
 * BUG-3582: an agent's body write that reaches an open tab through the
 * designated applier must show its `[[REF]]` as a link, as a fresh load does.
 * The applier handed the raw markdown to setContent, skipping the
 * wiki-link → link conversion the load and the seed apply, so every open
 * tab showed literal `[[TASK-5]]` bracket text until it was reloaded.
 */
test('an agent write through the applier shows its wiki-link as a link in the open tab', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a client-side conversion');
	test.setTimeout(90_000);
	const target = await seedDoc(fixture, request, 'B3582 link target');
	const host = await seedDoc(fixture, request, 'B3582 host');
	const t = (await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${target.slug}`, { headers: authJson(fixture) })).json()) as {
		collection_prefix: string;
		item_number: number;
		title: string;
	};
	const ref = `${t.collection_prefix}-${t.item_number}`;

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${host.slug}`);
	await expect(page.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });

	// The agent write: a plain API PATCH while the tab is connected, which the
	// server hands to the tab's applier.
	const patched = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${host.slug}`, {
		headers: authJson(fixture),
		data: { content: `See [[${ref}]] now.` },
	});
	expect(patched.ok(), await patched.text()).toBeTruthy();

	const editor = page.locator(EDITOR_SELECTOR);
	await expect(editor).toContainText('See', { timeout: SYNC_TIMEOUT });
	await expect(editor.locator('a', { hasText: t.title }), 'the wiki-link renders as a link').toBeVisible();
	await expect(editor, 'no literal bracket text').not.toContainText('[[');

	// The stored body keeps the wiki-link (the flush turns the link back).
	await expect
		.poll(async () => ((await (await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${host.slug}`, { headers: authJson(fixture) })).json()) as { content: string }).content, { timeout: 15_000 })
		.toContain(`[[${ref}]]`);
});
