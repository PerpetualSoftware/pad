import { test, expect, type Page, type Response } from '@playwright/test';
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

// Accounts whose role may edit every collection, so the dashboard also offers
// the first non-Tasks collection (BUG-3258).
const EDITS_ALL: ReadonlySet<AccountKey> = new Set(['owner', 'editor']);

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
				// The sidebar's "+ New {collection}" under the nav (BUG-3258,
				// codex round 1).
				await expect(page.locator('button.new-item-btn'), `${key} sidebar on ${coll}`).toHaveCount(
					EDITS[key].includes(coll) ? 1 : 0
				);
			}
		});

		test('sidebar: quick-add renders iff the account may edit the collection', async ({ page }) => {
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
			await open(page, key, '');
			await expect(page.locator('.dash-header h1')).toBeVisible();
			await expect(page.getByRole('button', { name: '+ New Task', exact: true })).toHaveCount(
				EDITS[key].includes('tasks') ? 1 : 0
			);
			// The secondary button offers the first non-Tasks collection the
			// account may edit. Only a whole-workspace editor has one here.
			await expect(page.locator('.dash-header-actions button')).toHaveCount(
				(EDITS[key].includes('tasks') ? 1 : 0) + (EDITS_ALL.has(key) ? 1 : 0)
			);
		});

		test('Cmd-N: quick-add opens on, and offers, only collections the account may edit', async ({ page }) => {
			// From Ideas where the account sees it, so the fallback to the
			// ACTIVE collection is exercised (BUG-3258): an account that
			// cannot edit Ideas must not get a New Idea dialog there.
			const onIdeas = SEES[key].includes('ideas');
			await open(page, key, onIdeas ? '/ideas' : '');
			if (onIdeas) {
				await expect(page.getByText(anchorTitle('ideas'), { exact: true }).first()).toBeVisible();
			} else {
				await expect(page.locator('.dash-header h1')).toBeVisible();
			}
			await page.keyboard.press('ControlOrMeta+n');
			const modal = page.locator('.quick-add-modal');
			if (EDITS[key].length === 0) {
				// Absence after a keypress: the owner and editor legs prove the
				// same press opens the dialog within this wait.
				await page.waitForTimeout(500);
				await expect(modal).toHaveCount(0);
				return;
			}
			await expect(modal).toBeVisible();
			const expectLabel = onIdeas && EDITS[key].includes('ideas') ? 'New Idea' : 'New Task';
			await expect(modal.locator('.quick-add-label')).toHaveText(expectLabel);
			const pill = modal.locator('.quick-add-pill');
			if (EDITS_ALL.has(key)) {
				await pill.click();
				await expect(modal.locator('.quick-add-picker-option', { hasText: 'New Idea' })).toHaveCount(1);
				await expect(modal.locator('.quick-add-picker-option', { hasText: 'New Task' })).toHaveCount(1);
			} else {
				// One creatable collection: the picker cannot switch to another.
				await expect(pill).toBeDisabled();
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
// a status write the account does not hold. Measured two ways: whether the
// drag STARTED (svelte-dnd-action's dragged element), and which writes it
// sent, each recorded as `<status> <item id> <body>` so a failure names them.
interface DragResult {
	/** svelte-dnd-action mounted its dragged element: the drag started. */
	engaged: boolean;
	writes: string[];
}

async function dragToLane(page: Page, title: string, lane: string): Promise<DragResult> {
	const writes: string[] = [];
	const onResponse = (r: Response) => {
		const path = new URL(r.url()).pathname;
		if (r.request().method() === 'PATCH' && /\/items\/[^/]+$/.test(path)) {
			writes.push(`${r.status()} ${path.split('/').pop()} ${r.request().postData() ?? ''}`);
		}
	};
	page.on('response', onResponse);
	try {
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
		const engaged = (await page.locator('#dnd-action-dragged-el').count()) > 0;
		await page.mouse.up();
		if (engaged) {
			// A started drag writes on drop; wait for it rather than a fixed sleep.
			await expect
				.poll(() => writes.length, { timeout: 5_000 })
				.toBeGreaterThan(0)
				.catch(() => {});
		}
		return { engaged, writes: [...writes] };
	} finally {
		page.off('response', onResponse);
	}
}

// A gesture can fail to start under load: one CI attempt sent nothing, which
// read as "not draggable" (TASK-2866). Retry until the drag engages. A card
// that never engages in three tries counts as not draggable, and each test
// proves the gesture works in the same run with a control drag.
// The lane to drag a card INTO: whichever of Open / In-Progress it is not in
// now. A drop into its own lane is a reorder with no status write, and the
// world is shared by every test (and every --repeat-each) in a worker.
async function otherLane(page: Page, title: string): Promise<string> {
	const inProgress = page
		.getByRole('group', { name: 'In-Progress column', exact: true })
		.locator('.item-card', { hasText: title });
	return (await inProgress.count()) > 0 ? 'Open' : 'In-Progress';
}

async function dragUntilEngaged(page: Page, title: string, lane: string): Promise<DragResult> {
	let result: DragResult = { engaged: false, writes: [] };
	for (let attempt = 0; attempt < 3 && !result.engaged; attempt++) {
		result = await dragToLane(page, title, lane);
	}
	return result;
}

test('owner: dragging an editable card to another lane sends its write (control)', async ({ page }) => {
	await open(page, 'owner', '/tasks', 'board');
	const title = walk.tasks[1].title;
	const drag = await dragUntilEngaged(page, title, await otherLane(page, title));
	expect(drag.engaged).toBe(true);
	expect(drag.writes.some((w) => w.startsWith('200 '))).toBe(true);
});

// BUG-3259 PIN. This asserts today's DEFECT, not the goal: the view-only card
// engages, and its status write is refused 403. It is a pin rather than
// test.fail because test.fail would also swallow a failing CONTROL below,
// and a broken gesture must fail loudly. When BUG-3259 is fixed, flip the
// two pinned assertions to: engaged false, and no writes.
test('guestPrecedence: a view-only card is draggable today (BUG-3259 pin)', async ({ page }) => {
	await open(page, 'guestPrecedence', '/tasks', 'board');
	// In-test control: this guest may edit task C through its collection
	// grant, so the same gesture on C must start and write.
	const controlTitle = walk.tasks[2].title;
	const control = await dragUntilEngaged(page, controlTitle, await otherLane(page, controlTitle));
	expect(control.engaged, 'control drag did not start: the gesture is broken').toBe(true);
	expect(control.writes.some((w) => w.startsWith('200 ') && w.includes('"status"'))).toBe(true);

	const viewOnly = await dragUntilEngaged(page, walk.grantedTask.title, await otherLane(page, walk.grantedTask.title));
	expect(viewOnly.engaged, 'BUG-3259 fixed? Flip this pin').toBe(true);
	expect(viewOnly.writes.some((w) => w.startsWith('403 ') && w.includes('"status"'))).toBe(true);
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
