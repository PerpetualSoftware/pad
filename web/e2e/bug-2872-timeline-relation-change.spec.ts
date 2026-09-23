import { test, expect } from './fixtures';
import type { Page, Locator, APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-2872 — an activity change on a `relation` field rendered the item ID the
 * field stores ("owner: → d84fb3b1-…"). It now renders the target's
 * `REF · title` (or "(deleted)" / "Unresolved reference"), on the item's
 * Activity tab and the workspace activity page's Audit view (where, until the
 * workspace index has loaded, it reads "Linked item"). A scalar
 * field's change is unchanged.
 */
const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i;

async function seed(fixture: import('./fixtures').SuiteFixture, request: APIRequestContext) {
	const h = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const ws = fixture.workspaceSlug;
	const stamp = Date.now();
	const tgt = await (await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers: h, data: { name: `B2872 targets ${stamp}`, prefix: `BT${String(stamp).slice(-4)}` },
	})).json();
	const schema = JSON.stringify({ fields: [
		{ key: 'owner', label: 'Owner', type: 'relation', collection: tgt.slug },
		{ key: 'note', label: 'Note', type: 'text' },
	] });
	const res = await request.post(`/api/v1/workspaces/${ws}/collections`, {
		headers: h, data: { name: `B2872 src ${stamp}`, prefix: `BS${String(stamp).slice(-4)}`, schema },
	});
	expect(res.ok(), await res.text()).toBeTruthy();
	const src = await res.json();
	const target = await (await request.post(`/api/v1/workspaces/${ws}/collections/${tgt.slug}/items`, {
		headers: h, data: { title: `B2872 owner ${stamp}` },
	})).json();
	const item = await (await request.post(`/api/v1/workspaces/${ws}/collections/${src.slug}/items`, {
		headers: h, data: { title: `B2872 subject ${stamp}` },
	})).json();
	const up = await request.patch(`/api/v1/workspaces/${ws}/items/${item.slug}`, {
		headers: h, data: { fields_patch: { owner: target.ref, note: 'plain words' } },
	});
	expect(up.ok(), await up.text()).toBeTruthy();
	return { h, ws, tgt, src, target, item };
}

/** The item page's Activity tab: the one item-page surface with change pills
 *  (the Details timeline under the content shows other entry kinds). */
async function openActivityTab(page: Page, fixture: import('./fixtures').SuiteFixture, s: { ws: string; src: { slug: string }; item: { ref: string } }) {
	await page.goto(`/${fixture.adminUsername}/${s.ws}/${s.src.slug}/${s.item.ref}`);
	await page.getByRole('tab', { name: /activity/i }).click();
}

/** The workspace activity page's Audit view: the `owner` pill of the item's entry. */
async function auditPill(page: Page, itemTitle: string): Promise<Locator> {
	await page.getByRole('group', { name: 'View mode' }).getByRole('button', { name: 'Audit' }).click();
	const entry = page.locator('.entry').filter({ hasText: itemTitle }).filter({ has: page.locator('.change-pill') }).first();
	await expect(entry).toBeVisible({ timeout: 15_000 });
	return pill(entry, 'owner');
}

/** The first change pill for `key` inside `scope`. */
function pill(scope: Locator | Page, key: string): Locator {
	return scope.locator('.change-pill').filter({ hasText: new RegExp(`^\\s*${key}:`) }).first();
}

test.describe('BUG-2872: an activity change on a relation field renders the target, never its id', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'rendering check; one project is enough');
		test.setTimeout(90_000);
	});

	test('item Activity tab: REF · title; a scalar change is verbatim', async ({ page, fixture, request }) => {
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			await openActivityTab(page, fixture, s);
			const tab = page.locator('[aria-label="Activity"]');
			const activity = pill(tab, 'owner');
			await expect(activity).toBeVisible({ timeout: 10_000 });
			await expect(activity).toContainText(s.target.ref);
			await expect(activity).toContainText(s.target.title);
			expect(await activity.innerText()).not.toMatch(UUID);
			// A scalar change is untouched.
			await expect(pill(tab, 'note')).toContainText('plain words');
		} finally {
			await deleteCollection(fixture, request, s.src.slug);
			await deleteCollection(fixture, request, s.tgt.slug);
		}
	});

	test('a deleted target reads "(deleted)", not its id', async ({ page, fixture, request }) => {
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			const del = await request.delete(`/api/v1/workspaces/${s.ws}/items/${s.target.slug}`, { headers: s.h });
			expect(del.ok(), await del.text()).toBeTruthy();
			await openActivityTab(page, fixture, s);
			const p = pill(page.locator('[aria-label="Activity"]'), 'owner');
			await expect(p).toBeVisible({ timeout: 15_000 });
			const text = await p.innerText();
			expect(text).not.toMatch(UUID);
			expect(text).toMatch(/\(deleted\)|Unresolved reference/);
		} finally {
			await deleteCollection(fixture, request, s.src.slug);
			await deleteCollection(fixture, request, s.tgt.slug);
		}
	});

	test('workspace activity page, cold load: never the id; "Linked item" until the index loads', async ({ page, fixture, request }) => {
		// /activity does not bootstrap the local index, so on a cold load the
		// target cannot be looked up yet. It must not print the id, and must not
		// claim the value names nothing ("Unresolved reference") either.
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			await page.goto(`/${fixture.adminUsername}/${s.ws}/activity`);
			const p = await auditPill(page, s.item.title);
			await expect(p).toContainText('Linked item');
			const text = await p.innerText();
			expect(text).not.toMatch(UUID);
			expect(text).not.toContain('Unresolved reference');
		} finally {
			await deleteCollection(fixture, request, s.src.slug);
			await deleteCollection(fixture, request, s.tgt.slug);
		}
	});

	test('workspace activity page with the index loaded (in-app navigation): REF · title', async ({ page, fixture, request }) => {
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			// The item page loads the index; the sidebar link keeps it in memory.
			await openActivityTab(page, fixture, s);
			await expect(pill(page.locator('[aria-label="Activity"]'), 'owner')).toContainText(s.target.ref, { timeout: 10_000 });
			await page.locator(`a[href$="/${s.ws}/activity"]`).first().click();
			await page.waitForURL(/\/activity$/);
			const p = await auditPill(page, s.item.title);
			await expect(p).toContainText(s.target.ref);
			await expect(p).toContainText(s.target.title);
			expect(await p.innerText()).not.toMatch(UUID);
		} finally {
			await deleteCollection(fixture, request, s.src.slug);
			await deleteCollection(fixture, request, s.tgt.slug);
		}
	});
});
