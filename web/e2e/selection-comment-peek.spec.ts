// The selection toolbar's Comment action, end to end (IDEA-2843 / GitHub #1228).
//
// Why these cannot be unit tests. A jsdom test drives selection with
// `editor.commands.setTextSelection(...)` — a programmatic transaction that
// emits `selectionUpdate` whether or not a browser would, and that never
// touches focus. Both questions here are about what a REAL mouse drag does:
// whether it raises the toolbar, and what it does to a peeking master's
// editable state. Only a browser can answer either, so these drag with the
// mouse and never call the editor's command API.
//
// The second test is a NEGATIVE result kept deliberately. The Comment action
// was briefly gated peek-independently on the theory that a peeking master can
// comment but cannot act on a selection; this measured that state and found it
// unreachable, and the gate was simplified because of it. The test stays so
// that a future change making selections survive the freeze turns red here
// instead of silently reopening a question everyone has forgotten.
import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

const DESKTOP = { width: 1200, height: 900 };

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

async function seedDocWithContent(
	fixture: SuiteFixture,
	request: APIRequestContext,
	titlePrefix: string,
	content: string,
): Promise<{ id: string; slug: string }> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/collections/docs/items`,
		{ headers: authHeaders(fixture), data: { title: `${titlePrefix} ${Date.now()}`, fields: '{}', content } },
	);
	if (!resp.ok()) throw new Error(`doc create failed (${resp.status()}): ${await resp.text()}`);
	return (await resp.json()) as { id: string; slug: string };
}

async function seedRelatedLink(
	fixture: SuiteFixture,
	request: APIRequestContext,
	sourceSlug: string,
	targetId: string,
): Promise<void> {
	const resp = await request.post(
		`/api/v1/workspaces/${fixture.workspaceSlug}/items/${sourceSlug}/links`,
		{ headers: authHeaders(fixture), data: { target_id: targetId, link_type: 'related' } },
	);
	if (!resp.ok()) throw new Error(`link create failed (${resp.status()}): ${await resp.text()}`);
}

async function itemRef(fixture: SuiteFixture, request: APIRequestContext, slug: string): Promise<string> {
	const resp = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, {
		headers: authHeaders(fixture),
	});
	if (!resp.ok()) throw new Error(`item get failed (${resp.status()}): ${await resp.text()}`);
	const item = (await resp.json()) as { collection_prefix?: string; item_number?: number };
	if (!item.collection_prefix || !item.item_number) throw new Error('item has no ref');
	return `${item.collection_prefix}-${item.item_number}`;
}

function fullPageUrl(fixture: SuiteFixture, slug: string): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
}

/** Drag-select a paragraph with the MOUSE — the browser path, not the API. */
async function dragSelect(page: Page, paragraph: ReturnType<Page['locator']>) {
	const box = await paragraph.boundingBox();
	if (!box) throw new Error('paragraph has no box');
	await page.mouse.move(box.x + 2, box.y + box.height / 2);
	await page.mouse.down();
	await page.mouse.move(box.x + box.width - 2, box.y + box.height / 2, { steps: 12 });
	await page.mouse.up();
}

test.describe('selection toolbar — Comment (IDEA-2843)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop-split concern');
	});

	test('a drag-selection in a PEEKING master re-activates it — so there is no frozen-selection case', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDocWithContent(
			fixture,
			request,
			'Peek comment master',
			'Alpha paragraph here\n\nBravo paragraph here',
		);
		const related = await seedDoc(fixture, request, 'Peek comment related');
		await seedRelatedLink(fixture, request, master.slug, related.id);
		const relatedRef = await itemRef(fixture, request, related.slug);

		await page.goto(fullPageUrl(fixture, master.slug));
		const masterPage = page.locator('.item-page-host > .item-page');
		const masterMain = masterPage.locator('.editor-wrapper .ProseMirror');
		await expect(masterMain).toHaveAttribute('contenteditable', 'true');
		const firstPara = masterMain.locator('p').first();
		await expect(firstPara).toContainText('Alpha paragraph');

		// PEEK: open the pane, click into it so the master freezes, put the
		// master's Details tab back on screen (which re-activates it), then
		// re-peek — the master is frozen WITH its editor visible.
		const pane = page.locator('.item-pane');
		await masterPage.getByRole('tab', { name: 'Relationships' }).click();
		await page
			.locator('.relationship-group', { hasText: 'Related' })
			.locator('a.link-target', { hasText: 'Peek comment related' })
			.click();
		await expect(pane).toBeVisible();
		await expect.poll(() => openItemParam(page)).toBe(relatedRef);
		await pane.locator('.editor-wrapper .ProseMirror').click();
		await masterPage.getByRole('tab', { name: 'Details' }).click();
		await pane.locator('.editor-wrapper .ProseMirror').click();
		await expect(masterMain).toHaveAttribute('contenteditable', 'false');

		// THE MEASUREMENT. The Comment action was briefly given a
		// peek-INdependent gate, reasoning that a peeking master keeps a live
		// composer (BUG-2263) and so should be able to act on a selection. This
		// asserts the state that argument assumed, and it does not exist: the
		// drag itself re-activates the master (focus-follows-editing,
		// PLAN-2179 DR-2), so a selection and a frozen master never coexist.
		//
		// The gate was collapsed back onto `mutationsEnabled` because of this.
		// If a future change makes a selection survivable in a frozen master,
		// THIS test goes red and the gate question reopens with evidence.
		await dragSelect(page, firstPara);
		await expect(masterMain).toHaveAttribute('contenteditable', 'true');

		// And with the master active again, both actions are available.
		await expect(page.getByRole('button', { name: 'Comment on selection' })).toBeVisible();
		await expect(page.getByRole('button', { name: 'Extract', exact: true })).toBeVisible();
	});

	test('Comment quotes the selection into the composer under the content, keeping a draft', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const doc = await seedDocWithContent(
			fixture,
			request,
			'Quote into composer',
			'Alpha paragraph here\n\nBravo paragraph here',
		);
		await page.goto(fullPageUrl(fixture, doc.slug));

		const masterPage = page.locator('.item-page-host > .item-page');
		const composer = masterPage.locator('#item-comments .compose .ProseMirror');
		// The composer is on the Details tab now, with no tab click needed —
		// that relocation is half of this unit.
		await expect(composer).toBeVisible();

		// A draft the user already typed. The quote must not eat it.
		await composer.click();
		await page.keyboard.type('my own remark');

		const firstPara = masterPage.locator('.editor-wrapper .ProseMirror p').first();
		await dragSelect(page, firstPara);
		await page.getByRole('button', { name: 'Comment on selection' }).click();

		await expect(composer.locator('blockquote')).toContainText('Alpha paragraph here');
		await expect(composer).toContainText('my own remark');
	});
});
