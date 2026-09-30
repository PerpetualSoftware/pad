import { test, expect } from './fixtures';
import type { Page, Locator, APIRequestContext } from '@playwright/test';
import { browserLogin } from './lib/collab-helpers';
import { deleteCollection } from './lib/attachment-viewer';

/**
 * BUG-2872 — an activity change on a `relation` field rendered the item ID the
 * field stores ("owner: → d84fb3b1-…"). It now renders the target's
 * `REF · title` (or "(deleted)" / "Unavailable item"), on the item's
 * History tab (the Activity tab before PLAN-2348 U3) and the workspace
 * activity page's Audit view, which since
 * BUG-3181 brings the workspace index up itself. A scalar
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

/** The item page's History tab: the one item-page surface with field changes
 *  (the Details timeline under the content shows other entry kinds). It
 *  replaced the Activity tab (PLAN-2348 U3). */
async function openHistoryTab(page: Page, fixture: import('./fixtures').SuiteFixture, s: { ws: string; src: { slug: string }; item: { ref: string } }) {
	await page.goto(`/${fixture.adminUsername}/${s.ws}/${s.src.slug}/${s.item.ref}`);
	await page.getByRole('tab', { name: /history/i }).click();
}

/** The History tab's change row for `key` inside `scope`. */
function historyField(scope: Locator | Page, key: string): Locator {
	return scope.locator(`.field-row[data-field="${key}"]`).first();
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

	test('item History tab: REF · title; a scalar change is verbatim', async ({ page, fixture, request }) => {
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			await openHistoryTab(page, fixture, s);
			const tab = page.locator('[aria-label="History"]');
			const activity = historyField(tab, 'owner');
			await expect(activity).toBeVisible({ timeout: 10_000 });
			await expect(activity).toContainText(s.target.ref);
			await expect(activity).toContainText(s.target.title);
			expect(await activity.innerText()).not.toMatch(UUID);
			// A scalar change is untouched.
			await expect(historyField(tab, 'note')).toContainText('plain words');
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
			await openHistoryTab(page, fixture, s);
			const p = historyField(page.locator('[aria-label="History"]'), 'owner');
			await expect(p).toBeVisible({ timeout: 15_000 });
			const text = await p.innerText();
			expect(text).not.toMatch(UUID);
			expect(text).toMatch(/\(deleted\)|Unavailable item/);
		} finally {
			await deleteCollection(fixture, request, s.src.slug);
			await deleteCollection(fixture, request, s.tgt.slug);
		}
	});

	test('workspace activity page, cold load: REF · title (the page brings the index up — BUG-3181)', async ({ page, fixture, request }) => {
		// /activity used to leave the local index cold, so a relation change read
		// "Linked item" until some other page loaded it. It now enters the index
		// itself (enterWorkspaceIndex), so a cold load resolves the target.
		await page.setViewportSize({ width: 1400, height: 900 });
		await browserLogin(page);
		const s = await seed(fixture, request);
		try {
			await page.goto(`/${fixture.adminUsername}/${s.ws}/activity`);
			const p = await auditPill(page, s.item.title);
			await expect(p).toContainText(s.target.ref, { timeout: 10_000 });
			await expect(p).toContainText(s.target.title);
			const text = await p.innerText();
			expect(text).not.toMatch(UUID);
			expect(text).not.toContain('Unavailable item');
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
			await openHistoryTab(page, fixture, s);
			await expect(historyField(page.locator('[aria-label="History"]'), 'owner')).toContainText(s.target.ref, { timeout: 10_000 });
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
