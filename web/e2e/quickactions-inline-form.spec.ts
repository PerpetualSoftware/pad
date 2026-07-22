import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-2281 regression e2e — the inline "New quick action" form must open
 * and stay open.
 *
 * Found during the BUG-2129 switch-safety reconciliation. Clicking the
 * footer "+ New quick action" flips `showCreateForm=true`, which unmounts
 * the footer's {:else} branch (the very button clicked). Svelte 5
 * flushSyncs after a delegated event handler, so by the time that click
 * bubbles on to the <svelte:window> click-outside handler the button is
 * DETACHED — `target.closest('.quick-actions-menu')` returns null and
 * handleWindowClick treats it as an outside click, closing the whole menu
 * and wiping the create form the instant it opens. `handleTriggerClick`
 * already guards this with `e.stopPropagation()`; `handleOpenCreateForm`
 * and the create-form Cancel button did not.
 *
 * jsdom (the unit test) doesn't reproduce the mid-bubble detach, and no
 * e2e ever CLICKED "New quick action" (the capstone only asserts it's
 * visible) — so this closes that gap in a real Chromium event pipeline.
 * Pre-fix: the create form never appears. Post-fix (stopPropagation): it
 * opens and stays, and Cancel returns to the action list instead of
 * closing the whole menu.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

// A plain collection with a minimal valid schema. Letters-only prefix and a
// timestamped name so the server-derived slug stays in sync with the name
// (see pane-collection-migration-race.spec.ts::seedMigratableCollection for
// why numeric-derived prefixes 404 by-ref lookups and out-of-sync name/slug
// reassign the slug on the next save).
async function seedCollection(
	fixture: SuiteFixture,
	request: APIRequestContext,
	namePrefix: string,
	itemPrefix: string,
) {
	const name = `${namePrefix} ${Date.now()}`;
	const schema = JSON.stringify({
		fields: [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['draft', 'published'],
				default: 'draft',
				required: true,
			},
		],
	});
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: itemPrefix, schema },
	});
	if (!resp.ok()) throw new Error(`collection create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string; name: string };
}

async function seedItem(
	fixture: SuiteFixture,
	request: APIRequestContext,
	collSlug: string,
	title: string,
) {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${collSlug}/items`,
		{
			headers: authHeaders(fixture),
			data: { title, fields: JSON.stringify({ status: 'draft' }), content: '' },
		},
	);
	if (!resp.ok()) throw new Error(`item create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

test("the inline 'New quick action' form opens and stays open (BUG-2281)", async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(
		testInfo.project.name !== 'desktop-chromium',
		'event-propagation behavior is viewport-agnostic; one project is enough',
	);
	test.setTimeout(30_000);

	// A brand-new collection has NO quick actions yet, so the dropdown opens
	// straight to the footer (New quick action / Manage actions) — the exact
	// state a user hits when adding their first action.
	const coll = await seedCollection(fixture, request, 'bug2281-form', 'BFRM');
	const it = await seedItem(fixture, request, coll.slug, 'BUG-2281 form item');

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}/${it.slug}`);
	await expect(page.locator('.title', { hasText: 'BUG-2281 form item' })).toBeVisible();

	await page.locator('button.trigger-btn[title="Quick actions"]').click();
	const openFormBtn = page.locator('button.action-item.footer-row', { hasText: 'New quick action' });
	await expect(openFormBtn).toBeVisible();
	await openFormBtn.click();

	// Pre-fix, the click reached the <svelte:window> click-outside handler on a
	// footer button that Svelte's flushSync had already detached, so the menu
	// closed instantly and the create form never appeared. Post-fix
	// (stopPropagation on handleOpenCreateForm), the form opens and stays.
	await expect(page.locator('input.qa-label-input')).toBeVisible();
	await expect(page.locator('textarea.qa-prompt-input')).toBeVisible();
	// A short settle to prove it didn't merely flash open then close.
	await page.waitForTimeout(200);
	await expect(page.locator('input.qa-label-input')).toBeVisible();

	// Cancel returns to the action list (not: closes the whole menu) — same
	// stopPropagation fix on the create-form Cancel button.
	await page.locator('button.qa-btn.qa-btn-cancel', { hasText: 'Cancel' }).click();
	await expect(page.locator('input.qa-label-input')).toHaveCount(0);
	await expect(
		page.locator('button.action-item.footer-row', { hasText: 'New quick action' }),
		'Cancel returns to the action list rather than closing the whole menu',
	).toBeVisible();
});
