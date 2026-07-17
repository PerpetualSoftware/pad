import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { Page } from '@playwright/test';

/**
 * Accessibility & focus management for the detail pane
 * (PLAN-2105 Phase 4 / TASK-2122).
 *
 * The pane is the URL-derived (`?item=`) master-detail surface on the
 * collection page. This spec drives the focus behaviours the task specifies,
 * in a REAL browser (jsdom has no layout/focus model, so the unit tests in
 * paneFocus.svelte.test.ts cover only the pure DOM math — the runtime wiring
 * is verified here).
 *
 * The DESKTOP model (per the TASK-2122 review decision): focus STAYS on the
 * list when the pane opens, so arrow/j-k navigation (with the pane following)
 * is uninterrupted. The user bridges INTO the pane with Tab and back with a
 * two-level ESC — ESC from inside the pane returns focus to the list (pane
 * stays open); a second ESC from the list closes the pane. The pane is a
 * labelled, non-trapping region on desktop.
 *
 * The MOBILE model: the pane is a full-screen modal overlay — focus moves in
 * on open and is TRAPPED (Tab / Shift+Tab cycle within, never reaching the
 * list mounted behind it).
 *
 * Viewport is driven explicitly, so one project is enough.
 */

const DESKTOP = { width: 1200, height: 900 };
const MOBILE = { width: 768, height: 1024 }; // == breakpoint (inclusive) → overlay

function docsUrl(fixture: { adminUsername: string; workspaceSlug: string }, query = ''): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs${query}`;
}

function openItemParam(page: Page): string | null {
	return new URL(page.url()).searchParams.get('item');
}

/** Is the document's active element inside the detail pane? */
function activeInPane(page: Page): Promise<boolean> {
	return page.evaluate(() => !!document.activeElement?.closest('.item-pane'));
}

test.describe('pane accessibility & focus management (PLAN-2105 / TASK-2122)', () => {
	test('desktop: focus stays on the list on open; Tab bridges into the pane; two-level ESC returns then closes', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		await seedDoc(fixture, request, 'A11y focus alpha');
		await seedDoc(fixture, request, 'A11y focus bravo');
		await page.goto(docsUrl(fixture));

		const row = page.locator('.item-card', { hasText: 'A11y focus alpha' }).first();
		await expect(row).toBeVisible();
		await row.click();

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		// The region carries the landmark semantics the task requires.
		await expect(pane).toHaveAttribute('aria-label', 'Item detail');
		await expect(pane).toHaveAttribute('tabindex', '-1');

		// Focus STAYS on the list — the pane did NOT steal it on the desktop split.
		expect(await activeInPane(page)).toBe(false);

		// (Tab bridge) Tab from the list moves focus INTO the pane region.
		await page.keyboard.press('Tab');
		await expect.poll(() => activeInPane(page)).toBe(true);

		// (Two-level ESC, step 1) ESC from inside the pane returns focus to the
		// list — the pane STAYS open.
		await page.keyboard.press('Escape');
		await expect.poll(() => activeInPane(page)).toBe(false);
		expect(openItemParam(page)).not.toBeNull();
		// Focus landed on the paned row (the `.focused` list card).
		expect(
			await page.evaluate(() => document.activeElement?.classList.contains('item-card')),
		).toBe(true);

		// (Two-level ESC, step 2) A second ESC from the list closes the pane.
		await page.keyboard.press('Escape');
		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
	});

	test('desktop: j/k keeps driving the list with the pane open (focus never leaves the list)', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		await seedDoc(fixture, request, 'A11y split alpha');
		await seedDoc(fixture, request, 'A11y split bravo');
		await page.goto(docsUrl(fixture));

		await page.locator('.item-card').first().click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const opened = openItemParam(page);
		expect(opened).not.toBeNull();
		// Focus is on the list, not the pane — so j/k list nav is unhijacked.
		expect(await activeInPane(page)).toBe(false);

		// j moves the list cursor and the open pane follows the selection
		// (?item= re-targets after the follow debounce).
		await page.keyboard.press('j');
		await expect.poll(() => openItemParam(page)).not.toBe(opened);
		// Still on the list, pane still open — the browse flow is intact.
		expect(await activeInPane(page)).toBe(false);
		await expect(pane).toBeVisible();

		// Tab still bridges into the pane even though the click left DOM focus on
		// the ORIGINAL row while j moved the `.focused` marker to another (the
		// bridge keys off the row anchor, not the marker — Codex P1).
		await page.keyboard.press('Tab');
		await expect.poll(() => activeInPane(page)).toBe(true);
	});

	test('desktop: browser Back closes the pane and returns focus to the originating row', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		await seedDoc(fixture, request, 'A11y back alpha');
		await seedDoc(fixture, request, 'A11y back bravo');
		await page.goto(docsUrl(fixture));

		// First open PUSHES a history entry, so Back closes the pane.
		const row = page.locator('.item-card', { hasText: 'A11y back alpha' }).first();
		await expect(row).toBeVisible();
		const rowHref = await row.getAttribute('href');
		await row.click();
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		// Move focus into the pane, then close via browser Back (which drops
		// ?item= straight from the URL — closeItemPane never runs).
		await page.keyboard.press('Tab');
		await expect.poll(() => activeInPane(page)).toBe(true);
		await page.goBack();

		await expect.poll(() => openItemParam(page)).toBeNull();
		await expect(pane).toBeHidden();
		// Focus was returned to the originating row (not dropped to <body>).
		await expect
			.poll(() => page.evaluate(() => document.activeElement?.getAttribute('href')))
			.toBe(rowHref);
	});

	test('desktop: closing a pane whose item is filtered out of the list lands focus on the list, not <body>', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'A11y orphan');
		// Deep-link the item open while a search excludes it from the list, so
		// there's NO `.focused` row and no captured trigger to return to.
		await page.goto(docsUrl(fixture, `?item=${slug}&q=zzz-no-match-zzz`));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await expect(page.locator('.item-card.focused, .table-row.focused')).toHaveCount(0);

		// Close (focus is on <body>; ESC falls through to the escape stack).
		await page.keyboard.press('Escape');
		await expect.poll(() => openItemParam(page)).toBeNull();
		// Focus landed on the stable list-column landmark, not dropped to <body>.
		await expect
			.poll(() => page.evaluate(() => document.activeElement?.classList.contains('list-column')))
			.toBe(true);
	});

	test('desktop: navigating to another collection with the pane open does NOT steal focus (route reuse)', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'A11y route reuse');
		await page.goto(docsUrl(fixture, `?item=${slug}`));
		await expect(page.locator('.item-pane')).toBeVisible();

		// Client-side nav to a DIFFERENT collection (SvelteKit reuses this
		// [collection] page, so openItemRef flips truthy→null without a close).
		// The focus-return effect must treat this as navigation, not a pane close
		// — it must NOT yank focus into the newly-loaded list (Codex P2).
		const tasksLink = page.locator(`a.nav-item[href$="/${fixture.workspaceSlug}/tasks"]`).first();
		await expect(tasksLink).toBeVisible();
		await tasksLink.click();

		await expect(page).toHaveURL(new RegExp(`/${fixture.workspaceSlug}/tasks$`));
		await expect(page.locator('.item-pane')).toBeHidden();
		// Focus was NOT stolen into the new collection's list-column.
		expect(await page.evaluate(() => !!document.activeElement?.closest('.list-column'))).toBe(false);
	});

	test('mobile overlay traps focus — Tab and Shift+Tab cycle within the pane', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough',
		);
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'A11y trap');
		await page.goto(docsUrl(fixture, `?item=${slug}`));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		// Full-screen overlay on mobile (the trap only applies here).
		await expect.poll(() => pane.evaluate((el) => getComputedStyle(el).position)).toBe('fixed');
		// Modal: focus moved INTO the overlay on open.
		await expect.poll(() => activeInPane(page)).toBe(true);
		// The hidden list behind it is `inert` — isolated from both the focus
		// order and the screen-reader tree (a JS trap can't constrain an SR
		// virtual cursor).
		expect(await page.locator('.list-column').evaluate((el) => (el as HTMLElement).inert)).toBe(
			true,
		);

		// Forward Tab many times: focus must NEVER escape the pane into the list
		// column mounted behind the overlay. Without the trap it would leak out
		// after cycling past the pane's last focusable.
		for (let i = 0; i < 15; i++) {
			await page.keyboard.press('Tab');
			expect(await activeInPane(page)).toBe(true);
		}
		// Backward Tab is trapped too.
		for (let i = 0; i < 5; i++) {
			await page.keyboard.press('Shift+Tab');
			expect(await activeInPane(page)).toBe(true);
		}

		// Containment backstop: a PROGRAMMATIC focus escape (not via Tab — e.g.
		// Cmd+F focusing the search box behind the overlay) is yanked back into
		// the pane by the focusin handler (Codex P1). Synthesize it with an
		// injected outside button.
		await page.evaluate(() => {
			const b = document.createElement('button');
			b.id = '__t2122_outside';
			document.body.insertBefore(b, document.body.firstChild);
			b.focus();
		});
		expect(await activeInPane(page)).toBe(true);
		await page.evaluate(() => document.getElementById('__t2122_outside')?.remove());

		// A modal <dialog> stacked over the overlay owns its OWN focus cycle —
		// the window-level pane trap must defer to it and NOT drag Tab back into
		// the inert pane behind it (Codex P1). Inject one, focus inside, Tab.
		await page.evaluate(() => {
			const dlg = document.createElement('dialog');
			dlg.id = '__t2122_dialog';
			dlg.innerHTML = '<button id="__t2122_a">A</button><button id="__t2122_b">B</button>';
			document.body.appendChild(dlg);
			dlg.showModal();
			(document.getElementById('__t2122_a') as HTMLElement).focus();
		});
		expect(await page.evaluate(() => document.activeElement?.id)).toBe('__t2122_a');
		await page.keyboard.press('Tab');
		// Focus advanced WITHIN the dialog (native cycle) — not hijacked to the pane.
		expect(await activeInPane(page)).toBe(false);
		expect(await page.evaluate(() => !!document.activeElement?.closest('dialog'))).toBe(true);
	});
});
