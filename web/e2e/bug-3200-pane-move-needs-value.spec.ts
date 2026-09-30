import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-3200: the pane menu's "Move to collection…" was fire-and-forget. A move
 * the server refused for a value only the user can supply ended as a toast,
 * with no way to supply it. It now hands the move to the copy dialog, preset
 * to the chosen collection, whose same-workspace path (DR-18) shows the
 * needs_value picker and completes the move.
 *
 * Each leg asserts the pane-menu move WAS refused with the code in question,
 * so a pass cannot come from a move that simply succeeded.
 */

const DESKTOP = { width: 1280, height: 800 };

function headers(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seedCollection(request: APIRequestContext, fixture: SuiteFixture, name: string, fields: unknown[]) {
	const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: headers(fixture),
		data: { name, schema: JSON.stringify({ fields }) },
	});
	expect(r.ok(), await r.text()).toBeTruthy();
	return (await r.json()) as { slug: string; name: string };
}

async function moveFromPaneMenu(page: Page, collectionName: string) {
	const more = page.getByRole('button', { name: 'More item actions' }).first();
	await expect(more).toBeVisible();
	await more.click();
	await page.getByRole('menuitem', { name: 'Move to collection…' }).click();
	const refused = page.waitForResponse((r) => /\/move(\?|$)/.test(r.url()) && r.request().method() === 'POST');
	await page.getByRole('menuitem', { name: collectionName, exact: true }).click();
	return refused;
}

test.describe('BUG-3200: a pane-menu move that needs a value opens the picker', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
	});

	test('a required destination field with no value', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const coll = await seedCollection(request, fixture, `NV Req ${stamp}`, [
			{ key: 'severity', label: 'Severity', type: 'select', options: ['low', 'high'], required: true },
		]);
		const { id, slug } = await seedDoc(fixture, request, 'Pane move needs value');
		const before = (await (
			await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, { headers: headers(fixture) })
		).json()) as { id: string; item_number?: number };
		expect(typeof before.item_number, 'precondition: the item has a ref number to compare').toBe('number');
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`);
		// The handoff must complete as a MOVE (DR-18), never a copy that mints
		// a new item and ref.
		const copies: string[] = [];
		page.on('request', (r) => {
			if (r.method() === 'POST' && /\/copy(\?|$)/.test(r.url())) copies.push(r.url());
		});

		const refused = await (await moveFromPaneMenu(page, coll.name));
		expect(refused.status(), 'precondition: the pane-menu move was refused').toBe(400);
		expect(((await refused.json()) as { error?: { code?: string } }).error?.code).toBe('missing_required_fields');

		const dialog = page.getByRole('dialog', { name: /^Copy or move / });
		await expect(dialog).toBeVisible();
		await expect(dialog.getByLabel('Collection', { exact: true })).toHaveValue(coll.slug);
		await expect(dialog.getByText('Needs a value')).toBeVisible();
		const confirm = dialog.getByRole('button', { name: 'Move', exact: true });
		await expect(confirm).toBeDisabled();

		const preflight = page.waitForResponse((r) => r.url().includes('/copy/preflight') && r.status() === 200);
		await dialog.locator('.select-trigger').click();
		await dialog.getByRole('option', { name: 'High' }).click();
		await preflight;
		await expect(confirm).toBeEnabled();

		const moved = page.waitForResponse((r) => /\/move(\?|$)/.test(r.url()) && r.request().method() === 'POST');
		await confirm.click();
		expect((await moved).status()).toBe(200);
		await expect(dialog).toBeHidden();

		const item = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
			headers: headers(fixture),
		});
		const body = (await item.json()) as {
			id: string;
			item_number?: number;
			collection_slug?: string;
			fields: string | Record<string, unknown>;
		};
		expect(body.collection_slug).toBe(coll.slug);
		expect(body.id, 'the same item moved, not a copy').toBe(id);
		expect(body.item_number, 'the ref number is unchanged').toBe(before.item_number);
		expect(copies, 'no copy request was sent').toEqual([]);
		const fields = typeof body.fields === 'string' ? JSON.parse(body.fields) : body.fields;
		expect(fields.severity).toBe('high');
	});

	test('a move that would reopen a done item', async ({ page, fixture, request }) => {
		const stamp = Date.now();
		const coll = await seedCollection(request, fixture, `NV State ${stamp}`, [
			{
				key: 'status',
				label: 'Status',
				type: 'select',
				options: ['open', 'shipped'],
				default: 'open',
				terminal_options: ['shipped'],
			},
		]);
		const { slug } = await seedDoc(fixture, request, 'Pane move state change');
		const done = await request.patch(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
			headers: headers(fixture),
			data: { fields_patch: { status: 'archived' } },
		});
		expect(done.ok(), `precondition: mark the doc done: ${await done.text()}`).toBeTruthy();
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`);

		const refused = await (await moveFromPaneMenu(page, coll.name));
		expect(refused.status(), 'precondition: the pane-menu move was refused').toBe(400);
		expect(((await refused.json()) as { error?: { code?: string } }).error?.code).toBe('state_change_requires_value');

		const dialog = page.getByRole('dialog', { name: /^Copy or move / });
		await expect(dialog).toBeVisible();
		await expect(dialog.getByLabel('Collection', { exact: true })).toHaveValue(coll.slug);
		await expect(dialog.getByText('Needs a value')).toBeVisible();
		await expect(dialog.getByRole('button', { name: 'Move', exact: true })).toBeDisabled();
	});
});
