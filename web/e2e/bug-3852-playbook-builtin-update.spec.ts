import { existsSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';
import { authJson, createWorkspace, deleteWorkspace } from './lib/attachment-viewer';
import { E2E_DB_PATH } from './lib/data-dir';

/**
 * BUG-3852: the Library's "Update available" card for a built-in PLAYBOOK
 * opens the dedicated playbook editor, which had no review dialog, so a person
 * upgrading could not review or accept the newer text from the web UI.
 *
 * A real upgrade is staged the only way one happens: the item holds the text
 * it was given, and that text is no longer the library's. The item's body is
 * changed through the API, then its origin row is pointed at that body as its
 * seed (no API writes a seed: only seeding, activation and an accepted update
 * do). The seed keeps the library's fields, so its hash over them is the
 * item_hash the server reports, and the server derives update_available from
 * the row itself, not from anything this spec tells the page.
 *
 * Its own workspace: the leg rewrites a seeded playbook, and the shared suite
 * workspace's playbooks are read by other specs running in parallel.
 */

type BuiltinState = {
	state: string;
	item_hash: string;
	seq: number;
	library?: { content: string };
};

test.describe('BUG-3852: a built-in playbook update from the Library', () => {
	test.setTimeout(90_000);
	test.beforeEach(({}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'one project is enough for a routing gap');
		test.skip(!existsSync(E2E_DB_PATH), 'stages the origin row in the server SQLite file');
	});

	test('Library card → playbook editor → review → accept takes the library text', async ({ page, fixture, request }) => {
		const { slug: ws } = await createWorkspace(fixture, request, 'bug3852');
		try {
			// A playbook the startup template seeded, with its built-in origin.
			const list = await request.get(`/api/v1/workspaces/${ws}/builtins`, { headers: authJson(fixture) });
			expect(list.ok(), await list.text()).toBe(true);
			const entries = (await list.json()) as Array<{ item_id: string; slug: string; title: string; kind?: string; collection_slug: string; state: string }>;
			const pb = entries.find((e) => e.collection_slug === 'playbooks' && e.state === 'current');
			if (!pb) throw new Error(`no seeded built-in playbook: ${JSON.stringify(entries)}`);

			const builtinUrl = `/api/v1/workspaces/${ws}/items/${pb.item_id}/builtin`;
			const readState = async (): Promise<BuiltinState> => {
				const resp = await request.get(builtinUrl, { headers: authJson(fixture) });
				expect(resp.ok(), await resp.text()).toBe(true);
				return (await resp.json()) as BuiltinState;
			};

			// The text an older Pad gave it.
			const item = await request.get(`/api/v1/workspaces/${ws}/items/${pb.item_id}`, { headers: authJson(fixture) });
			const { content: libraryContent, seq } = (await item.json()) as { content: string; seq: number };
			const oldBody = `${libraryContent}\n\nBUG-3852 older library text.`;
			const patch = await request.patch(`/api/v1/workspaces/${ws}/items/${pb.item_id}`, {
				headers: authJson(fixture),
				data: { content: oldBody, expected_seq: seq },
			});
			expect(patch.ok(), await patch.text()).toBe(true);
			const edited = await readState();
			expect(edited.state).toBe('current'); // an edit alone offers nothing

			const db = new DatabaseSync(E2E_DB_PATH);
			try {
				db.exec('PRAGMA busy_timeout = 5000');
				const r = db
					.prepare('UPDATE item_builtin_origin SET seed_hash = ?, seed_content = ? WHERE item_id = ?')
					.run(edited.item_hash, oldBody, pb.item_id);
				expect(r.changes).toBe(1);
			} finally {
				db.close();
			}
			const staged = await readState();
			expect(staged.state).toBe('update_available');
			expect(staged.library?.content).toBe(libraryContent);

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}/library?tab=playbooks`);
			const card = page.locator('.card').filter({ has: page.locator('.card-title', { hasText: pb.title }) });
			const offer = card.locator('a.builtin-offer');
			await expect(offer).toHaveText('Update available');
			await offer.click();

			// The dedicated editor route, the one the card links to.
			await expect(page).toHaveURL(new RegExp(`/${ws}/playbooks/[^/?]+$`));
			await expect(page.locator('#pbe-body')).toHaveValue(oldBody);
			await page.locator('button.builtin-badge', { hasText: 'Update available' }).click();
			const dialog = page.getByRole('dialog');
			await expect(dialog.getByRole('heading', { level: 2, name: 'Update available' })).toBeVisible();
			await expect(dialog.getByText('Your copy is unedited')).toBeVisible();

			const accept = page.waitForResponse(
				(r) => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith('/builtin/update'),
			);
			await dialog.getByRole('button', { name: "Accept the library's version" }).click();
			const res = await accept;
			expect(res.status(), await res.text()).toBe(200);

			await expect.poll(async () => (await readState()).state).toBe('current');
			// The form reloads the accepted text, so a later Save cannot send the old body back.
			await expect(page.locator('#pbe-body')).toHaveValue(libraryContent);
			await expect(page.locator('button.builtin-badge')).toHaveCount(0);
		} finally {
			await deleteWorkspace(fixture, request, ws);
		}
	});
});
