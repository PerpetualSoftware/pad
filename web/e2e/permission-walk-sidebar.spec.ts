import { test, expect, type Page } from '@playwright/test';
import {
	ACCOUNT_KEYS,
	actAs,
	seedPermissionWalk,
	type AccountKey,
	type PermissionWalk
} from './lib/permission-walk';

/**
 * Permission walk, row 1 of HT-1157: the sidebar lists exactly the
 * collections each account may see (TASK-2866 U1, the gate for BUG-984).
 *
 * The owner leg is the positive control. It proves the workspace rendered
 * with Tasks AND Ideas present, so a restricted account missing Ideas is
 * missing something that exists, not a page that never loaded. Every
 * restricted leg also asserts its own list is non-empty (the Tasks it is
 * granted), for the same reason: an absence is only evidence beside a
 * presence on the same surface.
 *
 * This file uses plain @playwright/test, not ./fixtures: that fixture
 * authenticates every context as the suite admin, and here each test must be
 * exactly one walk account. Desktop only, because the mobile project hides
 * the sidebar behind a drawer. Viewport coverage is not what this walk
 * measures.
 */

test.skip(({ isMobile }) => isMobile, 'the sidebar walk runs on the desktop project');

let walk: PermissionWalk;

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
});

async function sidebarCollections(page: Page): Promise<string[]> {
	return page.locator('nav.collection-nav .nav-section a.nav-item .nav-label').allInnerTexts();
}

async function openWorkspace(page: Page, key: AccountKey) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(walk.workspacePath);
	// Every account in the walk sees at least Tasks. Waiting on it is also
	// the presence half of each leg's absence assertions.
	await expect(
		page.locator('nav.collection-nav .nav-section a.nav-item .nav-label', { hasText: /^Tasks$/ })
	).toBeVisible();
}

// What each account's regular Collections section must list, exactly.
// 'ALL' means the owner's own list: every member with collection_access=all
// sees what the owner sees.
const EXPECTED: Record<AccountKey, 'ALL' | string[]> = {
	owner: 'ALL',
	editor: 'ALL',
	viewer: 'ALL',
	viewerTasksEdit: 'ALL',
	guestItemEdit: ['Tasks'],
	guestPrecedence: ['Tasks'],
	editorSpecific: ['Tasks']
};

const GUESTS: ReadonlySet<AccountKey> = new Set(['guestItemEdit', 'guestPrecedence']);

let ownerCollections: string[] | undefined;

async function ownerList(page: Page): Promise<string[]> {
	if (ownerCollections) return ownerCollections;
	const ownerPage = await page.context().browser()!.newPage();
	try {
		await openWorkspace(ownerPage, 'owner');
		ownerCollections = await sidebarCollections(ownerPage);
	} finally {
		await ownerPage.context().close();
	}
	return ownerCollections;
}

for (const key of ACCOUNT_KEYS) {
	test(`sidebar lists exactly the allowed collections: ${key}`, async ({ page }) => {
		await openWorkspace(page, key);
		const seen = await sidebarCollections(page);

		const owner = await ownerList(page);
		// Positive control: both walk collections exist and render.
		expect(owner).toEqual(expect.arrayContaining(['Tasks', 'Ideas']));

		const expected = EXPECTED[key];
		expect(seen.slice().sort()).toEqual((expected === 'ALL' ? owner : expected).slice().sort());
	});

	// A guest's list is framed as shared with them, and a member's is not.
	// The presence half is the guest legs, the absence half is the members.
	test(`sidebar frames the list as shared only for a guest: ${key}`, async ({ page }) => {
		// BUG-3254: GET /workspaces/{ws} omits is_guest, and a fresh load
		// resolves the workspace through it, so a guest renders as a member.
		// Expected to fail until that fix lands; the flip to passing fails
		// this test, which is the prompt to delete this line.
		test.fail(GUESTS.has(key), 'BUG-3254');
		await openWorkspace(page, key);
		await expect(page.getByText('Shared with you', { exact: true })).toHaveCount(GUESTS.has(key) ? 1 : 0);
	});
}
