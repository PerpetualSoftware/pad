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
 * Permission walk, the item writes on the system-collection pages (BUG-3266):
 * the Conventions row toggle, a group's "Enable all" / "Disable all", and an
 * expanded convention's Edit and Delete; a playbook's "Mark as Active" and
 * Delete. Each is an UPDATE or DELETE of one item, so each renders iff the
 * account may edit THAT item, which is not the create question BUG-3264's
 * walk asks (a guest granted one convention may edit it in a collection they
 * cannot create in).
 *
 * Every leg first asks the SERVER, by sending a status PATCH to the item the
 * control would write, and the control must match that answer. The server
 * checks update and delete with one predicate (requireEditPermission), so the
 * PATCH answers for Delete too. The PATCH writes the value the item already
 * holds, so the probe does not move the world the legs then read.
 *
 * editorSpecific is restricted to Tasks, so since TASK-3376 it does not see
 * the system collections at all (they are ordinary collections for a
 * restricted member, reached only when listed). Its probe answers 404, the
 * item legs below do not apply to it, and a leg of their own pins that
 * contract instead.
 *
 * Its own spec, with its own world, because granting the guests a convention
 * and a playbook would take the playbooks empty state away from the guests in
 * permission-walk-system.spec.ts, which walks that state's create door.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

type Sys = 'conventions' | 'playbooks';

const TITLE: Record<Sys, string> = {
	conventions: 'Walk status convention',
	playbooks: 'Walk status playbook'
};
// An ACTIVE sibling in the draft convention's group, granted the same way, so
// the group offers "Disable all" as well as "Enable all" to a guest who may
// write both rows.
const ACTIVE_SIBLING = 'Walk status convention on';

let walk: PermissionWalk;
const slugs = {} as Record<Sys, string>;
// What the server answered to a status PATCH on each seeded item, per account.
const serverEdits: Record<string, Record<Sys, boolean>> = {};
// Accounts the server hides the seeded items from (404). TASK-3376: exactly
// editorSpecific, whose collection access lists Tasks only.
const HIDDEN: ReadonlySet<AccountKey> = new Set(['editorSpecific']);

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
	const owner = await request.newContext({
		baseURL: walk.baseURL,
		extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts.owner.token}` }
	});
	try {
		// Draft, so the row reads inactive and the group offers "Enable all",
		// and the playbook offers "Mark as Active".
		const seeds: [Sys, string, Record<string, string>][] = [
			['conventions', TITLE.conventions, { status: 'draft', trigger: 'always' }],
			['conventions', ACTIVE_SIBLING, { status: 'active', trigger: 'always' }],
			['playbooks', TITLE.playbooks, { status: 'draft' }]
		];
		for (const [coll, title, fields] of seeds) {
			const res = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/collections/${coll}/items`, {
				data: { title, fields: JSON.stringify(fields) }
			});
			expect(res.status(), `seed ${title}: ${await res.text()}`).toBe(201);
			const slug = ((await res.json()) as { slug: string }).slug;
			if (title === TITLE[coll]) slugs[coll] = slug;
			// The two guests reach each item by grant only: edit for one,
			// view for the other, so the guest legs split on the item grant.
			const grants: [AccountKey, 'edit' | 'view'][] = [
				['guestItemEdit', 'edit'],
				['guestPrecedence', 'view']
			];
			for (const [key, permission] of grants) {
				const g = await owner.post(`/api/v1/workspaces/${walk.workspaceSlug}/items/${slug}/grants`, {
					data: { user_id: walk.accounts[key].userId, permission }
				});
				expect(g.ok(), `grant ${key} ${permission} on ${title}: ${await g.text()}`).toBe(true);
			}
		}
	} finally {
		await owner.dispose();
	}

	for (const key of ACCOUNT_KEYS) {
		const api = await request.newContext({
			baseURL: walk.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts[key].token}` }
		});
		serverEdits[key] = { conventions: false, playbooks: false };
		for (const coll of ['conventions', 'playbooks'] as const) {
			const res = await api.patch(`/api/v1/workspaces/${walk.workspaceSlug}/items/${slugs[coll]}`, {
				data: { fields_patch: { status: 'draft' } }
			});
			// Every account but the HIDDEN ones can see both items (members
			// by role and collection access, guests by grant), so 200 or 403
			// are the answers this walk reasons about; a HIDDEN account must
			// get 404 (TASK-3376). Anything else means the probe is broken.
			const allowed = HIDDEN.has(key) ? [404] : [200, 403];
			expect(allowed, `${key} PATCH ${coll}: ${res.status()} ${await res.text()}`).toContain(res.status());
			serverEdits[key][coll] = res.status() === 200;
		}
		await api.dispose();
	}
	// Both answers must occur among members AND among guests, or a leg cannot
	// tell a per-item gate from a role constant.
	const guests: AccountKey[] = ['guestItemEdit', 'guestPrecedence'];
	for (const coll of ['conventions', 'playbooks'] as const) {
		for (const group of [ACCOUNT_KEYS.filter((k) => !guests.includes(k) && !HIDDEN.has(k)), guests]) {
			const answers = new Set(group.map((k) => serverEdits[k][coll]));
			expect(answers.size, `server answers for ${coll} over ${group}: ${JSON.stringify(serverEdits)}`).toBe(2);
		}
	}
});

async function open(page: Page, key: AccountKey, path: string) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(`${walk.workspacePath}${path}`);
	// Chrome is absent for every account until access settles (BUG-3267).
	await waitForAccessSettled(page);
}

// TASK-3376: a member restricted to Tasks does not see the system collections
// unless they are listed for it. The beforeAll probe already required 404 on
// both seeded items; this leg pins that the pages agree and show it nothing.
test('editorSpecific: the system collections are not in its reach', async ({ page }) => {
	for (const coll of ['conventions', 'playbooks'] as const) {
		expect(serverEdits.editorSpecific[coll], `editorSpecific can edit ${coll}`).toBe(false);
	}
	await open(page, 'editorSpecific', '/conventions');
	await expect(page.getByText(TITLE.conventions, { exact: true })).toHaveCount(0);
	await open(page, 'editorSpecific', '/playbooks');
	await expect(page.getByText(TITLE.playbooks, { exact: true })).toHaveCount(0);
});

for (const key of ACCOUNT_KEYS.filter((k) => !HIDDEN.has(k))) {
	test.describe(key, () => {
		test('Conventions: toggle, Enable all, Edit and Delete render iff the server lets the account edit the item', async ({
			page
		}) => {
			await open(page, key, '/conventions');
			const edits = serverEdits[key].conventions ? 1 : 0;
			// Exact title: the sibling's title contains this one.
			const row = page
				.locator('.convention-row')
				.filter({ has: page.locator('.row-title', { hasText: new RegExp(`^${TITLE.conventions}$`) }) });
			// The title is the presence every count below is read against.
			await expect(row.locator('.row-title')).toBeVisible();
			await expect(row.locator('button.toggle-switch')).toHaveCount(edits);

			// The group holds the draft row and its active sibling, so it
			// would offer both bulk buttons to an account that may write them.
			// Members see the template's always-on conventions too, whose
			// editability is the same collection-level answer; guests see only
			// the two granted rows.
			const group = page.locator('section.trigger-group').filter({ has: row });
			await expect(group.locator('.group-header')).toBeVisible();
			await expect(group.getByRole('button', { name: 'Enable all', exact: true })).toHaveCount(edits);
			await expect(group.getByRole('button', { name: 'Disable all', exact: true })).toHaveCount(edits);

			await row.locator('.row-main').click();
			await expect(row.getByRole('button', { name: 'Export', exact: true })).toBeVisible();
			await expect(row.getByRole('button', { name: 'Edit', exact: true })).toHaveCount(edits);
			await expect(row.getByRole('button', { name: 'Delete', exact: true })).toHaveCount(edits);
		});

		test('Playbooks: Mark as Active and Delete render iff the server lets the account edit the item', async ({
			page
		}) => {
			await open(page, key, '/playbooks');
			const edits = serverEdits[key].playbooks ? 1 : 0;
			const card = page.locator('.card').filter({ has: page.getByText(TITLE.playbooks, { exact: true }) });
			await card.locator('button.card-header').click();
			// Export renders for everyone who can open the playbook; it is the
			// presence the counts are read against.
			await expect(card.getByRole('button', { name: 'Export', exact: true })).toBeVisible();
			await expect(card.getByRole('button', { name: 'Mark as Active', exact: true })).toHaveCount(edits);
			await expect(card.getByRole('button', { name: 'Delete', exact: true })).toHaveCount(edits);
			// The editor link says what it opens (BUG-3270): Edit for an
			// account that may write the playbook, View for one that may not.
			await expect(card.getByRole('button', { name: 'Edit', exact: true })).toHaveCount(edits);
			await expect(card.getByRole('button', { name: 'View', exact: true })).toHaveCount(1 - edits);
		});

		test('Playbook editor: the form is writable and Save renders iff the server lets the account edit the item', async ({
			page
		}) => {
			await open(page, key, `/playbooks/${slugs.playbooks}`);
			const edits = serverEdits[key].playbooks;
			const titleInput = page.getByPlaceholder('Playbook title');
			// Export renders for everyone who can open the playbook, and the
			// title shows the loaded item: the presence the rest is read against.
			await expect(page.getByRole('button', { name: 'Export', exact: true })).toBeVisible();
			await expect(titleInput).toHaveValue(TITLE.playbooks);
			await expect(page.getByRole('button', { name: 'Save', exact: true })).toHaveCount(edits ? 1 : 0);
			const body = page.locator('#pbe-body');
			// By id: the sidebar renders Trigger and Scope selects before it.
			const status = page.locator('#pbff-status');
			// A disabled control still shows the stored value.
			await expect(status).toHaveValue('draft');
			const trigger = page.getByLabel('Trigger', { exact: true });
			for (const control of [titleInput, body, status, trigger]) {
				if (edits) await expect(control).toBeEditable();
				else await expect(control).not.toBeEditable();
			}
		});
	});
}
