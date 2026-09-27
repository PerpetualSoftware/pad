import { test, expect, type Page } from '@playwright/test';
import {
	ACCOUNT_KEYS,
	actAs,
	seedPermissionWalk,
	type AccountKey,
	type PermissionWalk
} from './lib/permission-walk';

/**
 * Permission walk, HT-1157's collection rows (TASK-2866 U2, the gate for
 * BUG-984): the create chrome on the dashboard, the sidebar and each
 * collection page, the empty state, and board drag. Each renders iff the
 * account may edit the collection it writes to.
 *
 * Absence is only evidence beside a presence on the same surface. Every leg
 * waits for content the account can see before asserting anything is
 * missing, and the owner and editor legs are the positive control: they
 * prove each asserted affordance exists at all. Plain @playwright/test and
 * desktop only, for the reasons in permission-walk-sidebar.spec.ts.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

type Coll = 'tasks' | 'ideas';

// Which collections each account may see, and which it may edit, from the
// seed in lib/permission-walk.ts (HT-1157's Setup list).
const SEES: Record<AccountKey, Coll[]> = {
	owner: ['tasks', 'ideas'],
	editor: ['tasks', 'ideas'],
	viewer: ['tasks', 'ideas'],
	viewerTasksEdit: ['tasks', 'ideas'],
	guestItemEdit: ['tasks'],
	guestPrecedence: ['tasks'],
	editorSpecific: ['tasks']
};
const EDITS: Record<AccountKey, Coll[]> = {
	owner: ['tasks', 'ideas'],
	editor: ['tasks', 'ideas'],
	viewer: [],
	viewerTasksEdit: ['tasks'],
	// An item-edit grant is not a collection-edit grant.
	guestItemEdit: [],
	guestPrecedence: ['tasks'],
	editorSpecific: ['tasks']
};
const NAME: Record<Coll, string> = { tasks: 'Tasks', ideas: 'Ideas' };

// Accounts whose leg currently fails on a filed leak. Each set is exactly the
// accounts that see a create affordance for a collection they cannot edit.
const QUICK_ADD_LEAKS: ReadonlySet<AccountKey> = new Set(['viewer', 'viewerTasksEdit', 'guestItemEdit']);
const DASHBOARD_LEAKS: ReadonlySet<AccountKey> = new Set(['viewer', 'guestItemEdit']);

let walk: PermissionWalk;

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
});

async function open(page: Page, key: AccountKey, path: string, view?: 'list' | 'board') {
	await actAs(page.context(), walk.accounts[key]);
	if (view) {
		await page.context().addInitScript((v) => {
			try {
				localStorage.setItem('pad-view-tasks', v);
				localStorage.setItem('pad-view-ideas', v);
			} catch {
				// storage unavailable: the collection's default view applies
			}
		}, view);
	}
	await page.goto(`${walk.workspacePath}${path}`);
}

function sidebarEntry(page: Page, coll: Coll) {
	return page.locator('nav.collection-nav .nav-section a.nav-item', {
		has: page.locator('.nav-label', { hasText: new RegExp(`^${NAME[coll]}$`) })
	});
}

// The one item of `coll` every account that sees `coll` can see: the granted
// task (guests see only it, or all of Tasks), or the first idea.
function anchorTitle(coll: Coll): string {
	return coll === 'tasks' ? walk.grantedTask.title : walk.ideas[0].title;
}

for (const key of ACCOUNT_KEYS) {
	test.describe(key, () => {
		test('collection page: + New renders iff the account may edit the collection', async ({ page }) => {
			for (const coll of SEES[key]) {
				await open(page, key, `/${coll}`, 'list');
				await expect(page.getByText(anchorTitle(coll), { exact: true }).first()).toBeVisible();
				await expect(page.locator('button.new-btn'), `${key} on ${coll}`).toHaveCount(
					EDITS[key].includes(coll) ? 1 : 0
				);
			}
		});

		test('sidebar: quick-add renders iff the account may edit the collection', async ({ page }) => {
			// BUG-3258: quick-add renders on every visible collection. Remove
			// once the fix lands; the flip to passing fails this test.
			test.fail(QUICK_ADD_LEAKS.has(key), 'BUG-3258');
			await open(page, key, '');
			for (const coll of SEES[key]) {
				const entry = sidebarEntry(page, coll);
				await expect(entry).toBeVisible();
				await expect(entry.locator('.nav-quick-add'), `${key} on ${coll}`).toHaveCount(
					EDITS[key].includes(coll) ? 1 : 0
				);
			}
		});

		test('dashboard: create buttons render only for a collection the account may edit', async ({ page }) => {
			// BUG-3258: the dashboard header's create buttons are ungated.
			test.fail(DASHBOARD_LEAKS.has(key), 'BUG-3258');
			await open(page, key, '');
			await expect(page.locator('.dash-header h1')).toBeVisible();
			await expect(page.getByRole('button', { name: '+ New Task', exact: true })).toHaveCount(
				EDITS[key].includes('tasks') ? 1 : 0
			);
			if (EDITS[key].length === 0) {
				await expect(page.locator('.dash-header-actions button')).toHaveCount(0);
			}
		});

		test('board: lane drag renders iff the account may edit the collection', async ({ page }) => {
			await open(page, key, '/tasks', 'board');
			await expect(page.locator('.item-card', { hasText: walk.grantedTask.title })).toBeVisible();
			const handles = page.locator('.column-drag-handle');
			if (EDITS[key].includes('tasks')) {
				await expect(handles.first()).toBeAttached();
			} else {
				await expect(handles).toHaveCount(0);
			}
		});
	});
}

// Empty state: Docs is seeded by the template and never given an item. Only
// the accounts with collection_access=all see it.
for (const key of ['owner', 'editor', 'viewer', 'viewerTasksEdit'] as const) {
	test(`${key}: empty-state create CTA renders iff the account may edit the collection`, async ({ page }) => {
		await open(page, key, '/docs');
		await expect(page.getByRole('heading', { name: 'No docs yet' })).toBeVisible();
		await expect(page.locator('button.empty-cta')).toHaveCount(
			key === 'owner' || key === 'editor' ? 1 : 0
		);
	});
}

// Board drag is gated per ZONE on canEditCollection, not per item. The
// precedence guest may edit Tasks but only VIEW the granted task, because an
// item grant beats a collection grant. Dragging that card to another lane is
// a status write the account does not hold. Measured as "was a write sent",
// not "is the card draggable", because the drag library's DOM does not say.
// Each entry is `<status> <item id> <body>`, so a failure names what was sent.
// The owner leg is the control: the same gesture on an editable card sends
// its write, so a silent precedence leg means refused, not a broken gesture.
async function dragToLane(page: Page, title: string, lane: string): Promise<string[]> {
	const writes: string[] = [];
	page.on('response', (r) => {
		const path = new URL(r.url()).pathname;
		if (r.request().method() === 'PATCH' && /\/items\/[^/]+$/.test(path)) {
			writes.push(`${r.status()} ${path.split('/').pop()} ${r.request().postData() ?? ''}`);
		}
	});
	const card = page.locator('.item-card', { hasText: title });
	const target = page.getByRole('group', { name: `${lane} column`, exact: true });
	await expect(card).toBeVisible();
	await expect(target).toBeVisible();
	const from = (await card.boundingBox())!;
	const to = (await target.boundingBox())!;
	await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
	await page.mouse.down();
	await page.mouse.move(from.x + from.width / 2 + 10, from.y + from.height / 2 + 10, { steps: 4 });
	await page.mouse.move(to.x + to.width / 2, to.y + 80, { steps: 15 });
	await page.mouse.up();
	await page.waitForTimeout(1500);
	return writes;
}

test('owner: dragging an editable card to another lane sends its write (control)', async ({ page }) => {
	await open(page, 'owner', '/tasks', 'board');
	const writes = await dragToLane(page, walk.tasks[1].title, 'In-Progress');
	expect(writes.length).toBeGreaterThan(0);
});

test('guestPrecedence: a view-only card cannot be dragged to another lane', async ({ page }) => {
	// BUG-3259: drag is gated per zone, so this card drags and its status
	// write is refused 403. Remove once the fix lands.
	test.fail(true, 'BUG-3259');
	await open(page, 'guestPrecedence', '/tasks', 'board');
	const writes = await dragToLane(page, walk.grantedTask.title, 'In-Progress');
	expect(writes).toHaveLength(0);
});

// Lane bulk actions (archive, move, tag, priority, assign) sit behind each
// lane's ⋯ menu. The menu itself is for everyone, because lane sort lives
// there too (TASK-1673). The bulk endpoint gates on workspace ROLE, not on
// grants (TASK-1672), so the bulk verbs render iff the account is an owner
// or editor. A collection-edit grant alone does not unlock them, which
// under-grants rather than leaks.
const BULK_ROLE: ReadonlySet<AccountKey> = new Set(['owner', 'editor', 'editorSpecific']);
for (const key of ACCOUNT_KEYS) {
	test(`${key}: lane bulk verbs render iff the account's role may bulk-edit`, async ({ page }) => {
		await open(page, key, '/tasks', 'board');
		await page.getByRole('button', { name: 'Open lane actions', exact: true }).click();
		const menu = page.locator('.lane-menu-wrap');
		// Presence half: the menu opened for this account.
		await expect(menu.getByText(/sort/i).first()).toBeVisible();
		for (const verb of [/Archive all/, /Move all to/, /Tag all/]) {
			await expect(menu.getByText(verb), `${key}: ${verb}`).toHaveCount(BULK_ROLE.has(key) ? 1 : 0);
		}
	});
}
