import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * BUG-2848 — j/k must survive items arriving in a live list.
 *
 * The pane-follow is debounced by `PANE_FOLLOW_DEBOUNCE_MS` (140). It used to
 * re-read `filteredItems[focusedIndex]` when the timer fired, which made a
 * keypress depend on the list holding still for those 140 ms. This list is
 * SSE-live and does not: an item arriving during the debounce shifts every
 * index below it, the paned item slides down onto the advanced index, and the
 * callback then finds "the focused row is already the paned item" and returns.
 * The keystroke is discarded silently — no cursor move, no re-target, nothing
 * to see.
 *
 * WHY THIS SPEC EXISTS SEPARATELY from pane-controller.spec.ts: that file's
 * regression test asserts the same behaviour but cannot cause the race, so it
 * only catches this when a SIBLING test's seed happens to land in the window —
 * which is how it was found (an intermittent failure blamed on shared-workspace
 * pollution, BUG-2848 as originally filed). This one CAUSES the insert, so the
 * property is tested rather than waited for.
 *
 * THE THREE ASSERTIONS ARE NOT REDUNDANT, and the third is the reason the
 * original diagnosis was wrong:
 *
 *   retargeted   the pane moved off the row that was open
 *   cursorMoved  the list cursor actually moved across the keypress
 *   intended     the pane landed on the row that was BELOW the cursor at
 *                keypress time, identified by ref rather than by position
 *
 * `retargeted` alone passes if the pane wanders anywhere. `cursorMoved` alone
 * passes if the cursor moves and the pane ignores it. A tempting fourth —
 * "the pane agrees with the focused row" — passes VACUOUSLY on the bug, because
 * cursor and pane are then both stuck on the opened row; it was measured doing
 * exactly that and is deliberately not used.
 */

const DESKTOP = { width: 1200, height: 900 };

// Three rounds. One round caught the unfixed build 11 times in 12; three make a
// false green (~1 in 1700) not worth reasoning about, and the test still runs
// in a couple of seconds.
const ROUNDS = 3;

function docsUrl(fixture: SuiteFixture, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

/** Refs of the rendered rows, in order. */
async function rowRefs(page: Page): Promise<string[]> {
	return page.evaluate(() =>
		[...document.querySelectorAll('.item-card')].map(
			(a) => ((a as HTMLAnchorElement).getAttribute('href') ?? '').split('/').pop() ?? '',
		),
	);
}

/** Ref of the row carrying the list cursor. `.focused` is a documented hook. */
async function focusedRef(page: Page): Promise<string | null> {
	return page.evaluate(() => {
		const el = document.querySelector('.item-card.focused') as HTMLAnchorElement | null;
		if (!el) return null;
		return (el.getAttribute('href') ?? '').split('/').pop() ?? null;
	});
}

test.describe('pane-follow over a live list (BUG-2848)', () => {
	test.beforeEach(({}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'pane-follow is viewport-agnostic; the desktop split project is enough',
		);
	});

	test('j re-targets the pane even when rows arrive during the follow debounce', async ({
		page,
		fixture,
		request,
	}) => {
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		await seedDoc(fixture, request, 'Follow race base one');
		await seedDoc(fixture, request, 'Follow race base two');
		await page.goto(docsUrl(fixture, '?view=list'));

		const firstRow = page.locator('.item-card').first();
		await expect(firstRow).toBeVisible();
		await firstRow.click();
		await expect(page.locator('.item-pane')).toBeVisible();

		for (let round = 1; round <= ROUNDS; round++) {
			const openedBefore = openItemParam(page);
			expect(openedBefore, `round ${round}: pane should be open`).not.toBeNull();

			const rowCount = (await rowRefs(page)).length;

			// Put a row above the cursor and wait for it to actually render, so
			// the keypress starts from the shifted list rather than racing the
			// first insert.
			await seedDoc(fixture, request, `Follow race intruder ${round}`);
			await expect
				.poll(async () => (await rowRefs(page)).length, { timeout: 15000 })
				.toBeGreaterThan(rowCount);

			// The intended target, captured BY REF at keypress time — the row
			// below wherever the cursor is now.
			const rowsAtKey = await rowRefs(page);
			const cursorBefore = await focusedRef(page);
			expect(cursorBefore, `round ${round}: the cursor should be on a row`).not.toBeNull();
			const cursorIdx = rowsAtKey.indexOf(cursorBefore as string);
			const intended = rowsAtKey[cursorIdx + 1] ?? null;
			expect(intended, `round ${round}: need a row below the cursor to move onto`).not.toBeNull();

			// THE RACE: a second insert lands across the keypress, inside the
			// 140 ms debounce. Not awaited before the key — that is the point.
			const churn = seedDoc(fixture, request, `Follow race churn ${round}`);
			await page.keyboard.press('j');
			await churn;

			await expect
				.poll(() => openItemParam(page), { timeout: 5000 })
				.toBe(intended as string);

			const cursorAfter = await focusedRef(page);
			expect(cursorAfter, `round ${round}: the cursor moved across the keypress`).not.toBe(
				cursorBefore,
			);
			expect(openItemParam(page), `round ${round}: the pane left the opened row`).not.toBe(
				openedBefore,
			);
		}

		// The pane is still a pane: the race must not have detached or closed it.
		await expect(page.locator('.item-pane')).toBeVisible();
	});
});
