import { test, expect, request, type Page } from '@playwright/test';
import {
	ACCOUNT_KEYS,
	actAs,
	seedPermissionWalk,
	waitForAccessSettled,
	type AccountKey,
	type PermissionWalk
} from './lib/permission-walk';

/**
 * Permission walk, HT-1157's item rows (TASK-2866 U3, the gate for
 * BUG-984). On the item page, every write affordance renders iff
 * canEditItem holds for that account and that item:
 * - the title (a button that opens the editor, or a read-only heading);
 * - the body editor (contenteditable or not);
 * - the comment composer, and reply / react / delete on an existing comment
 *   (edit is its author's alone);
 * - the Move, Copy and Delete rows of the item menu.
 *
 * The precedence case is the point of this file. The guest with
 * collection-EDIT on Tasks and item-VIEW on task A must get the read-only
 * render of A, because the item grant wins, while getting the editable
 * render of task B from the collection grant. B is that leg's positive
 * control on the same page type, and the owner is the control for every
 * affordance. Plain @playwright/test and desktop only, for the reasons in
 * permission-walk-sidebar.spec.ts.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

type Which = 'A' | 'B';

// canEditItem per account for task A (the granted one) and task B (reached
// through the collection only). null means the account cannot see the item.
const EDITS_ITEM: Record<AccountKey, Record<Which, boolean | null>> = {
	owner: { A: true, B: true },
	editor: { A: true, B: true },
	viewer: { A: false, B: false },
	viewerTasksEdit: { A: true, B: true },
	guestItemEdit: { A: true, B: null },
	guestPrecedence: { A: false, B: true },
	editorSpecific: { A: true, B: true }
};

let walk: PermissionWalk;

const COMMENT = 'Walk comment from the owner';

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
	// One owner comment with one reaction on each task, so the comment rows
	// have something to act on. Seeded here rather than in the shared
	// fixture: only this file walks comments.
	const owner = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts.owner.token}` }
	});
	try {
		for (const task of [walk.grantedTask, walk.tasks[1]]) {
			const c = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/items/${task.slug}/comments`, {
				data: { body: COMMENT }
			});
			if (!c.ok()) throw new Error(`seed comment failed (${c.status()}): ${await c.text()}`);
			const { id } = (await c.json()) as { id: string };
			const r = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/comments/${id}/reactions`, {
				data: { emoji: '👍' }
			});
			if (!r.ok()) throw new Error(`seed reaction failed (${r.status()}): ${await r.text()}`);
		}
	} finally {
		await owner.dispose();
	}
});

function taskFor(which: Which) {
	return which === 'A' ? walk.grantedTask : walk.tasks[1];
}

async function openItem(page: Page, key: AccountKey, which: Which) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(`${walk.workspacePath}/tasks/${taskFor(which).slug}`);
	// Chrome is absent for every account until access settles (BUG-3267).
	await waitForAccessSettled(page);
	// Presence half: the item itself rendered, editable or not.
	await expect(
		page.locator('.title-row').getByText(taskFor(which).title, { exact: true })
	).toBeVisible();
}

for (const key of ACCOUNT_KEYS) {
	for (const which of ['A', 'B'] as const) {
		const canEdit = EDITS_ITEM[key][which];
		if (canEdit === null) continue;
		const label = `${key} on task ${which} (${canEdit ? 'editable' : 'view-only'})`;

		test(`${label}: title, body and comment composer`, async ({ page }) => {
			await openItem(page, key, which);
			await expect(page.locator('.title-row button.title')).toHaveCount(canEdit ? 1 : 0);
			await expect(page.locator('.title-row h1.title-readonly')).toHaveCount(canEdit ? 0 : 1);
			// The body mounts as a ProseMirror either way; only its editability differs.
			const body = page.locator('.ProseMirror').first();
			await expect(body).toBeAttached();
			await expect(body).toHaveAttribute('contenteditable', canEdit ? 'true' : 'false');
			// The composer is a rich editor; its submit button is the stable handle.
			await expect(page.getByRole('heading', { name: 'Comments' })).toBeVisible();
			await expect(page.getByRole('button', { name: 'Comment', exact: true })).toHaveCount(canEdit ? 1 : 0);
		});

		test(`${label}: comment reply, react and delete iff editable; edit only for its author`, async ({ page }) => {
			await openItem(page, key, which);
			const card = page.locator('.comment-card, [class*="comment"]', { hasText: COMMENT }).first();
			// Presence half: the owner's comment and its reaction rendered.
			await expect(card).toBeVisible();
			const chip = card.locator('button.reaction-chip', { hasText: '👍' });
			await expect(chip).toBeVisible();
			if (canEdit) await expect(chip).toBeEnabled();
			else await expect(chip).toBeDisabled();
			await expect(card.locator('button[title="Add reaction"]')).toHaveCount(canEdit ? 1 : 0);
			await expect(card.getByRole('button', { name: 'Reply', exact: true })).toHaveCount(canEdit ? 1 : 0);
			await expect(card.locator('button[title="Delete comment"]')).toHaveCount(canEdit ? 1 : 0);
			// Editing a comment is its author's alone (server: author-only).
			await expect(card.locator('button[title="Edit comment"]')).toHaveCount(key === 'owner' ? 1 : 0);
		});

		test(`${label}: item menu offers Move, Copy and Delete iff editable`, async ({ page }) => {
			await openItem(page, key, which);
			await page.getByRole('button', { name: 'More item actions' }).first().click();
			const menu = page.getByRole('menu').last();
			await expect(menu).toBeVisible();
			await expect(menu.getByRole('menuitem', { name: /Move to collection/ })).toHaveCount(canEdit ? 1 : 0);
			await expect(menu.getByRole('menuitem', { name: /^Copy or move to workspace/ })).toHaveCount(canEdit ? 1 : 0);
			await expect(menu.getByRole('menuitem', { name: /^Delete/ })).toHaveCount(canEdit ? 1 : 0);
		});
	}
}
