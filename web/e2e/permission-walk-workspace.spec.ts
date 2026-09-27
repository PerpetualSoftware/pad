import { test, expect, type Page } from '@playwright/test';
import {
	ACCOUNT_KEYS,
	actAs,
	seedPermissionWalk,
	type AccountKey,
	type PermissionWalk
} from './lib/permission-walk';

/**
 * Permission walk, HT-1157's workspace rows (TASK-2866 U4, the last unit of
 * the gate for BUG-984): Settings (General, Members, Collections, Danger
 * Zone), the sidebar's New collection button, the Roles board, quick
 * actions, and saved views.
 *
 * Each rule below is the SERVER's rule, cited where it is enforced, because
 * an affordance the server refuses is the leak this walk looks for.
 * - Workspace settings writes, invites, collection create and role writes
 *   are owner-only.
 * - Saving a view needs editor or above (handleCreateView, requireMinRole).
 *
 * The owner leg is the positive control for every affordance, and every
 * absence waits on the page having rendered for that account. Settings and
 * Roles are walked as MEMBERS only: a guest is not a member, and what a
 * guest gets there belongs to BUG-3254's is_guest fix. Plain
 * @playwright/test and desktop only, for the reasons in
 * permission-walk-sidebar.spec.ts.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

const MEMBERS: readonly AccountKey[] = ['owner', 'editor', 'viewer', 'viewerTasksEdit', 'editorSpecific'];
const EDITOR_PLUS: ReadonlySet<AccountKey> = new Set(['owner', 'editor', 'editorSpecific']);

let walk: PermissionWalk;

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
});

async function open(page: Page, key: AccountKey, path: string) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(`${walk.workspacePath}${path}`);
}

async function openSettingsTab(page: Page, key: AccountKey, tab: string) {
	await open(page, key, `/settings#${tab}`);
	// Presence half: the tab bar rendered for this account.
	await expect(page.getByRole('tab', { name: /General/ })).toBeVisible();
}

for (const key of MEMBERS) {
	const owner = key === 'owner';

	test(`${key}: Danger Zone tab renders iff owner`, async ({ page }) => {
		await openSettingsTab(page, key, 'general');
		await expect(page.getByRole('tab', { name: /Members/ })).toBeVisible();
		await expect(page.getByRole('tab', { name: /Danger Zone/ })).toHaveCount(owner ? 1 : 0);
	});

	test(`${key}: General name is editable and savable iff owner`, async ({ page }) => {
		await openSettingsTab(page, key, 'general');
		const name = page.locator('#ws-name');
		await expect(name).toBeVisible();
		if (owner) await expect(name).not.toHaveAttribute('readonly', /.*/);
		else await expect(name).toHaveAttribute('readonly', /.*/);
		await expect(page.getByRole('button', { name: 'Save', exact: true })).toHaveCount(owner ? 1 : 0);
	});

	test(`${key}: General name field holds the workspace name`, async ({ page }) => {
		// BUG-3260: on a direct load the field is empty for every member.
		// Remove once the fix lands.
		test.fail(true, 'BUG-3260');
		await openSettingsTab(page, key, 'general');
		await expect(page.locator('#ws-name')).toHaveValue(/^Walk /);
	});

	test(`${key}: Members invite form renders iff owner`, async ({ page }) => {
		await openSettingsTab(page, key, 'members');
		// Presence half: the owner's row is listed for every member.
		await expect(page.getByText(walk.accounts.owner.email).first()).toBeVisible();
		await expect(page.getByRole('heading', { name: 'Invite Member' })).toHaveCount(owner ? 1 : 0);
	});

	test(`${key}: Collections tab create button renders iff owner`, async ({ page }) => {
		await openSettingsTab(page, key, 'collections');
		await expect(page.getByText('Tasks', { exact: true }).first()).toBeVisible();
		await expect(page.getByRole('button', { name: '+ Create Collection' })).toHaveCount(owner ? 1 : 0);
	});

	test(`${key}: sidebar New collection renders iff owner`, async ({ page }) => {
		// BUG-3261: renders for every non-owner member. Remove once fixed.
		test.fail(!owner, 'BUG-3261');
		await open(page, key, '');
		await expect(page.locator('nav.collection-nav .nav-label', { hasText: /^Tasks$/ })).toBeVisible();
		await expect(page.locator('button[title="New collection"]')).toHaveCount(owner ? 1 : 0);
	});

	test(`${key}: Roles board role writes render iff owner`, async ({ page }) => {
		await open(page, key, '/roles');
		// Presence half: the board or its empty state rendered.
		await expect(page.locator('.lane, .empty-state, [class*="empty"]').first()).toBeVisible();
		await expect(
			page.getByRole('button', { name: /^(\+\s*)?Add Role$|^Create your first role$/ })
		).toHaveCount(owner ? 1 : 0);
		if (!owner) await expect(page.locator('button[title="Edit role"]')).toHaveCount(0);
	});
}

for (const key of ACCOUNT_KEYS) {
	// The template seeds quick actions on Tasks, so the menu mounts for every
	// account that sees Tasks: running one is per item. Authoring one is
	// owner-only (the collection-settings write).
	test(`${key}: quick actions authoring renders iff owner`, async ({ page }) => {
		await open(page, key, '/tasks');
		await expect(page.getByText(walk.grantedTask.title, { exact: true }).first()).toBeVisible();
		await page.locator('button[title="Quick actions"]').first().click();
		const menu = page.getByRole('menu').last();
		// Presence half: the menu opened and lists at least one seeded action.
		await expect(menu.getByRole('menuitem').first()).toBeVisible();
		await expect(menu.getByRole('menuitem', { name: /New quick action/ })).toHaveCount(key === 'owner' ? 1 : 0);
		await expect(menu.getByRole('menuitem', { name: /Manage actions/ })).toHaveCount(key === 'owner' ? 1 : 0);
	});

	test(`${key}: "Save current view" renders iff editor or above`, async ({ page }) => {
		// BUG-3262: renders for every account. Remove once fixed.
		test.fail(!EDITOR_PLUS.has(key), 'BUG-3262');
		await open(page, key, '/tasks');
		await page.getByRole('button', { name: 'Change view' }).click();
		// Presence half: the view menu opened for this account.
		await expect(page.getByRole('menu').last()).toBeVisible();
		await expect(page.getByRole('menuitem', { name: /Save current view/ })).toHaveCount(
			EDITOR_PLUS.has(key) ? 1 : 0
		);
	});
}
