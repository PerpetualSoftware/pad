import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * TASK-2188 + TASK-2187, end to end: removing a select option that items
 * hold is confirmed before the save, with the count, and the name typed at
 * 25 items; the save sends NO migration, so the items keep the removed value
 * instead of being moved onto the next option (which is what the positional
 * rename did), and the server names what it left behind.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function api(request: APIRequestContext, fixture: SuiteFixture, method: 'get' | 'post', path: string, data?: unknown) {
	const resp = await request[method](`/api/v1/workspaces/${fixture.workspaceSlug}${path}`, {
		headers: authHeaders(fixture),
		data,
	});
	if (!resp.ok()) throw new Error(`${method} ${path} failed (${resp.status()}): ${await resp.text()}`);
	return resp.json();
}

async function fieldCardByLabel(page: Page, label: string): Promise<Locator> {
	const cards = page.locator('.field-card');
	const count = await cards.count();
	for (let i = 0; i < count; i++) {
		const card = cards.nth(i);
		if ((await card.locator('.field-label-input').inputValue()) === label) return card;
	}
	throw new Error(`no field card among ${count} has label "${label}"`);
}

async function optionRow(card: Locator, value: string): Promise<Locator> {
	const rows = card.locator('.option-row');
	const count = await rows.count();
	for (let i = 0; i < count; i++) {
		const row = rows.nth(i);
		if ((await row.locator('.option-name-input').inputValue()) === value) return row;
	}
	throw new Error(`no option row among ${count} holds "${value}"`);
}

test('removing a held option asks first, with the count and the name typed, and moves nothing (TASK-2188 / TASK-2187)', async ({
	page,
	fixture,
	request,
}, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the confirm is viewport-agnostic; one project is enough');
	test.setTimeout(90_000);

	const name = `t2188 removal ${Date.now()}`;
	const coll = (await api(request, fixture, 'post', '/collections', {
		name,
		prefix: 'TRMV',
		schema: JSON.stringify({
			fields: [
				{ key: 'status', label: 'Status', type: 'select', options: ['todo', 'doing', 'done'], terminal_options: ['done'], default: 'todo' },
			],
		}),
	})) as { slug: string; name: string };
	const held = 25;
	let first = '';
	for (let i = 0; i < held; i++) {
		const it = (await api(request, fixture, 'post', `/collections/${coll.slug}/items`, {
			title: `t2188 doing ${i}`,
			fields: JSON.stringify({ status: 'doing' }),
		})) as { slug: string };
		if (!first) first = it.slug;
	}

	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${coll.slug}?item=${first}`);
	const pane = page.locator('.item-pane');
	await expect(pane.locator('.title', { hasText: /t2188 doing/ })).toBeVisible();
	await pane.locator('button.trigger-btn[title="Quick actions"]').click();
	await page.getByRole('menuitem', { name: 'Manage actions' }).click();
	await expect(page.locator('#edit-collection-title')).toBeVisible();
	await page.locator('button.tab', { hasText: 'Fields' }).click();

	const status = await fieldCardByLabel(page, 'Status');
	await (await optionRow(status, 'doing')).locator('.option-remove-btn').click();
	await expect(page.locator('.impact-notices')).toContainText(`${held} items have “doing” in “Status”`);

	let patches = 0;
	page.on('request', (r) => {
		if (r.method() === 'PATCH' && r.url().endsWith(`/collections/${coll.slug}`)) patches++;
	});

	// The first Save only asks.
	await page.locator('button.btn-save', { hasText: 'Save Changes' }).click();
	const confirm = page.locator('.impact-confirm');
	await expect(confirm).toBeVisible();
	await expect(confirm).toContainText(`${held} items have “doing” in “Status”`);
	const anyway = confirm.getByRole('button', { name: 'Save anyway' });
	await expect(anyway).toBeDisabled();
	expect(patches, 'nothing is sent before the confirm').toBe(0);

	await confirm.getByRole('textbox').fill(coll.name);
	await expect(anyway).toBeEnabled();
	const patchResp = page.waitForResponse((r) => r.request().method() === 'PATCH' && r.url().endsWith(`/collections/${coll.slug}`));
	await anyway.click();
	const resp = await patchResp;
	const sentBody = resp.request().postDataJSON() as { migrations?: unknown[] };
	expect(sentBody.migrations ?? [], 'a removal is not a rename').toEqual([]);
	const answered = (await resp.json()) as { warnings?: { orphaned?: unknown[] } };
	expect(answered.warnings?.orphaned).toEqual([{ field: 'status', option: 'doing', items: held }]);

	// The items kept their value: nothing moved onto "done".
	const items = (await api(request, fixture, 'get', `/collections/${coll.slug}/items`)) as { fields: string }[];
	const statuses = items.map((i) => JSON.parse(i.fields).status);
	expect(statuses.filter((s) => s === 'doing')).toHaveLength(held);
	expect(statuses).not.toContain('done');
});
