import { test, expect, request, type Page } from '@playwright/test';
import { ACCOUNT_KEYS, actAs, seedPermissionWalk, type AccountKey, type PermissionWalk } from './lib/permission-walk';

/**
 * Permission walk, the system-collection pages (BUG-3264): Conventions and
 * Playbooks "+ New" and "Import artifact", a playbook's Duplicate, and the
 * library's Activate. Each renders iff the account may create in the
 * collection the write addresses. Accounts that see the seeded playbooks are
 * walked on Duplicate; guests, who see none, get the empty state instead, and
 * are walked on its "Create Your First Playbook".
 *
 * The expectation is not a table typed from reading the client: every leg
 * first asks the SERVER, by sending the create the affordance would send, and
 * the affordance must match that answer. So the positive control and the
 * denial are the same leg, and the spec cannot agree with a client rule the
 * server does not share. Desktop only, like the other walk specs.
 */

test.skip(({ isMobile }) => isMobile, 'the permission walk runs on the desktop project');

type Sys = 'conventions' | 'playbooks';

let walk: PermissionWalk;
// What the server answered to a create in each system collection, per account.
const serverAllows: Record<string, Record<Sys, boolean>> = {};

test.beforeAll(async () => {
	walk = await seedPermissionWalk();
	for (const key of ACCOUNT_KEYS) {
		const api = await request.newContext({
			baseURL: walk.baseURL,
			extraHTTPHeaders: { Authorization: `Bearer ${walk.accounts[key].token}` }
		});
		serverAllows[key] = { conventions: false, playbooks: false };
		for (const coll of ['conventions', 'playbooks'] as const) {
			const res = await api.post(`/api/v1/workspaces/${walk.workspaceSlug}/collections/${coll}/items`, {
				data: { title: `walk probe ${key} ${coll}` }
			});
			// 201 or 403 are the two answers this walk can reason about; any
			// other status means the probe itself is broken.
			expect([201, 403], `${key} create in ${coll}: ${res.status()} ${await res.text()}`).toContain(res.status());
			serverAllows[key][coll] = res.status() === 201;
		}
		await api.dispose();
	}
	// Both answers must occur, or a leg cannot tell a gate from a constant.
	for (const coll of ['conventions', 'playbooks'] as const) {
		const answers = new Set(ACCOUNT_KEYS.map((k) => serverAllows[k][coll]));
		expect(answers.size, `server answers for ${coll}: ${JSON.stringify(serverAllows)}`).toBe(2);
	}
});

async function open(page: Page, key: AccountKey, path: string) {
	await actAs(page.context(), walk.accounts[key]);
	await page.goto(`${walk.workspacePath}${path}`);
}

for (const key of ACCOUNT_KEYS) {
	test.describe(key, () => {
		test('Conventions: + New and Import render iff the server lets the account create', async ({ page }) => {
			await open(page, key, '/conventions');
			await expect(page.getByRole('link', { name: 'Browse Library' })).toBeVisible();
			const allows = serverAllows[key];
			await expect(page.getByRole('button', { name: '+ New Convention', exact: true })).toHaveCount(
				allows.conventions ? 1 : 0
			);
			await expect(page.getByRole('button', { name: 'Import artifact' })).toHaveCount(
				allows.conventions || allows.playbooks ? 1 : 0
			);
		});

		test('Playbooks: + New and Import render iff the server lets the account create', async ({ page }) => {
			await open(page, key, '/playbooks');
			await expect(page.getByRole('link', { name: /Browse Library/ })).toBeVisible();
			const allows = serverAllows[key];
			await expect(page.getByRole('button', { name: '+ New Playbook', exact: true })).toHaveCount(
				allows.playbooks ? 1 : 0
			);
			await expect(page.getByRole('button', { name: 'Import artifact' })).toHaveCount(
				allows.conventions || allows.playbooks ? 1 : 0
			);
			// Either the list or the empty state renders; each has its own
			// create door. Its heading or a card is the presence the count is
			// read against.
			const card = page.locator('button.card-header').first();
			const empty = page.getByText('No playbooks yet', { exact: true });
			await expect(card.or(empty)).toBeVisible();
			if (await card.isVisible()) {
				// Duplicate sits in an expanded playbook, beside Edit, which
				// renders for everyone who can open one.
				await card.click();
				await expect(page.getByRole('button', { name: 'Edit', exact: true }).first()).toBeVisible();
				await expect(page.getByRole('button', { name: 'Duplicate', exact: true })).toHaveCount(
					allows.playbooks ? 1 : 0
				);
			} else {
				await expect(
					page.getByRole('button', { name: 'Create Your First Playbook', exact: true })
				).toHaveCount(allows.playbooks ? 1 : 0);
			}
		});

		test('Library: Activate renders iff the server lets the account create in the target', async ({ page }) => {
			for (const [tab, coll] of [['conventions', 'conventions'], ['playbooks', 'playbooks']] as const) {
				await open(page, key, `/library?tab=${tab}`);
				// A library card's body is the presence the count below is read
				// against. Not its action slot: for an account that may not create,
				// an inactive card's slot is empty, so it has no box to be visible.
				await expect(page.locator('.card-content').first()).toBeVisible();
				const activate = page.locator('button.activate-btn');
				if (serverAllows[key][coll]) {
					await expect(activate.first()).toBeVisible();
				} else {
					await expect(activate).toHaveCount(0);
				}
			}
		});
	});
}
