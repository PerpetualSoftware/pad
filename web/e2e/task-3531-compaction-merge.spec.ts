// TASK-3531: a tab that sleeps past a DORMANCY sweep merges its unsent edit
// when it wakes, because the sweep compacted the op-log into one Yjs snapshot
// instead of deleting it (Yjs identity survives, so the tab's update applies).
// Without compaction the same tab is force_refreshed and its text handed back
// (TASK-2199 / BUG-3526), which is the counterfactual this spec also states.
//
// OPT-IN: the sweep must run within the test, so the server needs
// PAD_OPLOG_GC_INTERVAL / PAD_OPLOG_GC_MIN_AGE set short, which would prune
// other specs' items mid-run on the shared CI server. Run it alone:
//
//   PAD_E2E_COMPACTION=on PAD_OPLOG_COMPACT=on PAD_OPLOG_GC_INTERVAL=2s \
//   PAD_OPLOG_GC_MIN_AGE=5s PAD_E2E_PORT=<free> PAD_E2E_DATA_DIR=$(mktemp -d) \
//   npx playwright test e2e/task-3531-compaction-merge.spec.ts --project=desktop-chromium
//
// PAD_E2E_COMPACTION=off (with PAD_OPLOG_COMPACT unset) runs the
// counterfactual: the same steps end in the hand-back notice.
import { test, expect } from './fixtures';
import { DatabaseSync } from 'node:sqlite';
import { E2E_DB_PATH } from './lib/data-dir';
import { browserLogin, EDITOR_SELECTOR, SYNCED_BADGE_SELECTOR, SYNC_TIMEOUT, seedDoc } from './lib/collab-helpers';

const MODE = process.env.PAD_E2E_COMPACTION ?? '';

function opLogState(itemID: string): { rows: number; compacted: boolean } {
	const db = new DatabaseSync(E2E_DB_PATH, { readOnly: true });
	try {
		const rows = (db.prepare('SELECT COUNT(*) AS n FROM item_yjs_updates WHERE item_id = ?').get(itemID) as { n: number }).n;
		const snap = db.prepare('SELECT yjs_snapshot_op_id AS s FROM items WHERE id = ?').get(itemID) as { s: number | null };
		return { rows, compacted: snap.s !== null };
	} finally {
		db.close();
	}
}

test('a tab that slept past the dormancy sweep merges its edit (or, without compaction, is handed it back)', async ({ browser, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium' || (MODE !== 'on' && MODE !== 'off'), 'opt-in: see the header');
	test.setTimeout(240_000);
	const { id, slug } = await seedDoc(fixture, request, 'TASK-3531');
	const url = `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`;

	const sleeperCtx = await browser.newContext();
	const sleeper = await sleeperCtx.newPage();
	await browserLogin(sleeper);
	await sleeper.goto(url);
	await expect(sleeper.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await sleeper.locator(EDITOR_SELECTOR).click();
	await sleeper.keyboard.type('Stored before the sleep. ');
	// Let the edit persist, flush and be confirmed (barrier) before sleeping.
	await sleeper.waitForTimeout(8_000);

	await sleeperCtx.setOffline(true);
	await expect(sleeper.locator(SYNCED_BADGE_SELECTOR)).toBeHidden({ timeout: SYNC_TIMEOUT });
	await sleeper.keyboard.press('Control+End');
	await sleeper.keyboard.type('Typed while asleep.');

	// The room's grace runs out and the sweep reaches the item: compacted into
	// one row (compaction on) or deleted (off).
	await expect
		.poll(() => opLogState(id), {
			timeout: 150_000,
			intervals: [2_000],
			// The spec cannot see the server's environment: a timeout in mode=off
			// usually means the server runs with PAD_OPLOG_COMPACT=on (codex).
			message: `the sweep reached the item (mode=${MODE}; check the server's PAD_OPLOG_COMPACT matches)`
		})
		.toEqual(MODE === 'on' ? { rows: 1, compacted: true } : { rows: 0, compacted: false });

	// A fresh tab edits the item meanwhile.
	const freshCtx = await browser.newContext();
	const fresh = await freshCtx.newPage();
	await browserLogin(fresh);
	await fresh.goto(url);
	await expect(fresh.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
	await expect(fresh.locator(EDITOR_SELECTOR)).toContainText('Stored before the sleep.');
	await fresh.locator(EDITOR_SELECTOR).click();
	await fresh.keyboard.press('Control+End');
	await fresh.keyboard.type(' Typed by a fresh tab.');

	// The sleeper wakes.
	await sleeperCtx.setOffline(false);
	if (MODE === 'on') {
		await expect(sleeper.locator(SYNCED_BADGE_SELECTOR)).toBeVisible({ timeout: SYNC_TIMEOUT });
		await expect(sleeper.locator(EDITOR_SELECTOR)).toContainText('Typed while asleep.', { timeout: SYNC_TIMEOUT });
		await expect(sleeper.locator(EDITOR_SELECTOR)).toContainText('Typed by a fresh tab.', { timeout: SYNC_TIMEOUT });
		await expect(fresh.locator(EDITOR_SELECTOR)).toContainText('Typed while asleep.', { timeout: SYNC_TIMEOUT });
		await expect(sleeper.locator('.offline-recovery'), 'merged, so nothing is handed back').toHaveCount(0);
		// And the stored body holds both, once a tab flushes.
		await expect
			.poll(
				async () => {
					const res = await request.get(`/api/v1/workspaces/${fixture.workspaceSlug}/items/${slug}`, { headers: { Authorization: `Bearer ${fixture.apiToken}` } });
					const body = ((await res.json()) as { content: string }).content;
					return body.includes('Typed while asleep.') && body.includes('Typed by a fresh tab.');
				},
				{ timeout: 30_000, message: 'the stored body holds both edits' }
			)
			.toBe(true);
	} else {
		await expect(sleeper.locator('.offline-recovery'), 'without compaction the edit is handed back').toBeVisible({ timeout: SYNC_TIMEOUT });
	}
	await sleeperCtx.close();
	await freshCtx.close();
});
