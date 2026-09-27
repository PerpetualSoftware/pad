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

const VIEW_NAME = 'Walk saved view';
let viewId: string;
let laneRoleId: string;

// How many of the role's four items (two tasks, two ideas) each account may
// see. editorSpecific reads Tasks only; guestItemEdit holds one item grant;
// guestPrecedence's collection grant on Tasks shows it both tasks.
const LANE_VISIBLE: Record<AccountKey, number> = {
	owner: 4,
	editor: 4,
	viewer: 4,
	viewerTasksEdit: 4,
	guestItemEdit: 1,
	guestPrecedence: 2,
	editorSpecific: 2
};

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
	const owner = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts.owner.token}` }
	});
	try {
		const r = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/collections/tasks/views`, {
			data: { name: VIEW_NAME, view_type: 'list', config: '{}' }
		});
		if (!r.ok()) throw new Error(`seed view failed (${r.status()}): ${await r.text()}`);
		viewId = ((await r.json()) as { id: string }).id;

		// A role on two tasks and both ideas, for the role-count leg (BUG-3257).
		const role = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/agent-roles`, {
			data: { name: 'Walk lane' }
		});
		if (!role.ok()) throw new Error(`seed role failed (${role.status()}): ${await role.text()}`);
		laneRoleId = ((await role.json()) as { id: string }).id;
		for (const item of [walk.grantedTask, walk.tasks[1], ...walk.ideas]) {
			const u = await owner.patch(`/api/v1/workspaces/${walk.workspaceSlug}/items/${item.slug}`, {
				data: { agent_role_id: laneRoleId }
			});
			if (!u.ok()) throw new Error(`assign role to ${item.ref} failed (${u.status()}): ${await u.text()}`);
		}
	} finally {
		await owner.dispose();
	}
});

// The server's own answer to the write an affordance performs, as this
// account. An affordance must render iff this is 2xx: a render the server
// refuses is a leak, and a hide the server would accept is an over-hide
// (BUG-3261 / BUG-3262). Each write is harmless to the rest of the file.
async function serverAccepts(
	key: AccountKey,
	method: 'post' | 'patch',
	path: string,
	data: unknown
): Promise<boolean> {
	const api = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts[key].token}` }
	});
	try {
		const r = await api[method](`/api/v1/workspaces/${walk.workspaceSlug}${path}`, { data });
		if (r.status() !== 403 && !r.ok()) {
			throw new Error(`${key} ${method} ${path}: unexpected ${r.status()} ${await r.text()}`);
		}
		return r.ok();
	} finally {
		await api.dispose();
	}
}

async function open(page: Page, key: AccountKey, path: string) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(`${walk.workspacePath}${path}`);
	// Chrome is absent for every account until access settles (BUG-3267).
	await waitForAccessSettled(page);
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
		// BUG-3260: a direct load used to leave this empty for every member.
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

	// BUG-3261. Rendered for every member before; collection create is
	// owner-only (handleCreateCollection: requireMinRole "owner").
	test(`${key}: sidebar New collection renders iff the server accepts collection create`, async ({ page }) => {
		const accepts = await serverAccepts(key, 'post', '/collections', { name: `Walk probe ${key}` });
		expect(accepts, 'the server rule this leg encodes').toBe(owner);
		await open(page, key, '');
		await expect(page.locator('nav.collection-nav .nav-label', { hasText: /^Tasks$/ })).toBeVisible();
		await expect(page.locator('button[title="New collection"]')).toHaveCount(accepts ? 1 : 0);
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
	// BUG-3257. Every role is listed to every reader (roles are workspace
	// metadata), but a role's item_count describes items, so it counts only
	// what this account may see: the lane's own filtered items. It used to be
	// the workspace-wide count (store.ListAgentRoles) on the board.
	test(`${key}: roles board lane count is the items this account can see`, async () => {
		const api = await request.newContext({
			baseURL: walk.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts[key].token}` }
		});
		try {
			const r = await api.get(`/api/v1/workspaces/${walk.workspaceSlug}/roles/board`);
			expect(r.status(), await r.text()).toBe(200);
			const { lanes } = (await r.json()) as {
				lanes: { role: { id: string; item_count?: number } | null; items: unknown[] }[];
			};
			const lane = lanes.find((l) => l.role?.id === laneRoleId);
			expect(lane, 'the role is listed to every reader').toBeTruthy();
			expect(lane!.items).toHaveLength(LANE_VISIBLE[key]);
			expect(lane!.role!.item_count ?? 0, 'role.item_count').toBe(LANE_VISIBLE[key]);
		} finally {
			await api.dispose();
		}
	});

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

	// BUG-3262. Saving a view is ROLE-gated (handleCreateView: requireMinRole
	// "editor"), so no grant admits it. Deleting one goes through
	// requireViewEditable, the grant-aware collection edit check, so the
	// collection-edit grant holders keep the delete. Probed with a same-name
	// PATCH, which passes the identical gate and changes nothing.
	test(`${key}: saved views: save and delete render iff the server accepts them`, async ({ page }) => {
		const canSave = await serverAccepts(key, 'post', '/collections/tasks/views', {
			name: `Walk probe ${key}`,
			view_type: 'list',
			config: '{}'
		});
		expect(canSave, 'the server rule this leg encodes').toBe(EDITOR_PLUS.has(key));
		const canDelete = await serverAccepts(key, 'patch', `/collections/tasks/views/${viewId}`, {
			name: VIEW_NAME
		});

		await open(page, key, '/tasks');
		await page.getByRole('button', { name: 'Change view' }).click();
		const menu = page.getByRole('menu').last();
		// Presence half: the menu opened and lists the seeded view.
		await expect(menu.getByText(VIEW_NAME, { exact: true })).toBeVisible();
		await expect(menu.getByRole('menuitem', { name: /Save current view/ })).toHaveCount(canSave ? 1 : 0);
		// The × is visually hidden until hover, which takes it out of the a11y tree, so
		// count the element itself.
		await expect(menu.locator(`button[aria-label="Delete view ${VIEW_NAME}"]`)).toHaveCount(canDelete ? 1 : 0);
	});
}
