import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * PLAN-3535 — a child in a REFERENCE collection (tracks_work: false) is not
 * part of its parent's work. On the parent page it is listed in its own
 * References group, outside the status groups, with no done styling, and the
 * header's done/total counts the work children only (the server's /progress,
 * which leaves reference children out of both numbers).
 *
 * Non-vacuity: the same item counts as work when its collection tracks work,
 * so the test first reads the page with the collection as WORK (3 children
 * counted, no References group), then flips the collection THROUGH THE
 * COLLECTION EDITOR's "Track as work" toggle (the control an owner uses to
 * turn an existing Docs collection into reference), and reads it again.
 */

function headers(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function create(
	request: APIRequestContext,
	fixture: SuiteFixture,
	coll: string,
	data: Record<string, unknown>,
): Promise<{ id: string; slug: string; ref?: string }> {
	const resp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll}/items`, {
		headers: headers(fixture),
		data,
	});
	expect(resp.ok(), await resp.text()).toBeTruthy();
	return resp.json();
}

test('PLAN-3535: reference children are listed apart and leave the progress count', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'one layout is enough for a data rule');

	const stamp = Date.now();
	const collResp = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: headers(fixture),
		data: {
			name: `E3535 Refs ${stamp}`,
			tracks_work: true,
			schema: JSON.stringify({
				fields: [{ key: 'status', type: 'select', options: ['draft', 'published'], default: 'draft' }],
			}),
		},
	});
	expect(collResp.ok(), await collResp.text()).toBeTruthy();
	const refColl = (await collResp.json()) as { slug: string };

	const parent = await create(request, fixture, 'tasks', {
		title: `E3535 parent ${stamp}`,
		fields: JSON.stringify({ status: 'open' }),
	});
	await create(request, fixture, 'tasks', {
		title: `E3535 done task ${stamp}`,
		fields: JSON.stringify({ status: 'done', parent: parent.id }),
	});
	await create(request, fixture, 'tasks', {
		title: `E3535 open task ${stamp}`,
		fields: JSON.stringify({ status: 'open', parent: parent.id }),
	});
	const refTitle = `E3535 reference doc ${stamp}`;
	await create(request, fixture, refColl.slug, {
		title: refTitle,
		fields: JSON.stringify({ status: 'draft', parent: parent.id }),
	});

	await page.setViewportSize({ width: 1200, height: 900 });
	await browserLogin(page);

	const open = async () => {
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${parent.slug}`);
		// The parent page opens on Details; children live under Relationships.
		const tab = page.getByRole('tablist', { name: 'Item sections' }).getByRole('tab', { name: 'Relationships' });
		await expect(tab).toBeVisible();
		await tab.click();
		await expect(tab).toHaveAttribute('aria-selected', 'true');
		await expect(page.locator('.child-items .child-count')).toBeVisible();
	};

	// As WORK: the doc is an ordinary open child. 1 of 3 done, no References.
	await open();
	await expect(page.locator('.child-items .child-count')).toHaveText('1/3 done');
	await expect(page.getByTestId('child-references')).toHaveCount(0);

	// Flip it the way an owner does: collection menu → Edit collection →
	// Display → untick "Track as work" → Save.
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${refColl.slug}`);
	await page.getByRole('button', { name: 'Collection menu' }).click();
	await page.getByRole('menuitem', { name: /Edit collection/ }).click();
	const dialog = page.getByRole('dialog', { name: 'Edit Collection' });
	await expect(dialog).toBeVisible();
	await dialog.getByRole('button', { name: 'Display', exact: true }).click();
	const toggle = dialog.getByRole('checkbox', { name: 'Track as work' });
	await expect(toggle).toBeChecked();
	await toggle.uncheck();
	const saved = page.waitForResponse(
		(r) => r.request().method() === 'PATCH' && r.url().endsWith(`/collections/${refColl.slug}`),
	);
	await dialog.getByRole('button', { name: 'Save Changes' }).click();
	expect((await saved).ok()).toBeTruthy();
	// The collection page may leave the modal open after a save (its own
	// contract); what matters is the stored setting, read back below.
	const stored = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${refColl.slug}`, {
		headers: headers(fixture),
	});
	expect(JSON.parse(((await stored.json()) as { settings: string }).settings).tracks_work).toBe(false);

	// As REFERENCE: 1 of 2 done, and the doc is listed apart.
	await open();
	await expect(page.locator('.child-items .child-count')).toHaveText('1/2 done');
	const refs = page.getByTestId('child-references');
	await expect(refs).toContainText('References (1)');
	await expect(refs.locator('.child-row', { hasText: refTitle })).toBeVisible();
	await expect(refs.locator('.child-title.done')).toHaveCount(0);
	await expect(
		page.locator('.child-group:not(.reference-group) .child-row', { hasText: refTitle }),
	).toHaveCount(0);
});
