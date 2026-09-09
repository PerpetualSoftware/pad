import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';

/**
 * BUG-2978 — deep-linking `/{user}/{ws}/settings#danger` landed on General for
 * a workspace OWNER: 0/10 loads before the fix, while `#storage` and
 * `#members` were 10/10.
 *
 * Mechanism, measured rather than guessed (the trail carries the probe). The
 * hash-restoration effect works: it applied `danger` correctly ~219ms in. What
 * broke it is that `workspaceStore.setCurrent` clears `currentMembership` to
 * null before `/me` resolves, and the permission helpers treat unknown as
 * no-access by design — and this route calls `setCurrent` TWICE per load, once
 * from the workspace layout and once from the page's own `load()`. So
 * `canEditWorkspace` reads true -> false -> true, the owner-only tab leaves the
 * tab set during the false window, the effect's snap-back branch moves
 * `activeTab` off the now-invalid `danger`, and `pendingHash` has already been
 * consumed — so nothing restores it when the permission comes back.
 *
 * Only the owner-only tab could hit this, which is why `#storage` never did:
 * an always-valid tab is never snapped away from.
 *
 * WHAT THIS SPEC IS AND IS NOT. It is a smoke leg: it proves that deep-linking
 * the two tabs it covers — the owner-only one and an always-visible control —
 * works in a real browser. It is NOT the regression guard for
 * BUG-2978, because it does not discriminate — RUN AGAINST THE UNFIXED BUILD IT
 * PASSES. On the e2e fixture the layout's `setCurrent` and the page's own land
 * inside a single unresolved `/me` window, so membership never goes
 * known -> unknown -> known and the flicker the bug needs never happens. That
 * ordering is a property of a small, fast fixture workspace; the real workspace
 * produces it readily (0/10 deep links before the fix, 40/40 after, measured on
 * the trail).
 *
 * The discriminating test is
 * `src/routes/[username]/[workspace]/settings/settingsPermissionFlicker.svelte.test.ts`,
 * which drives the two `/me` resolutions by hand and therefore fails without
 * the fix.
 *
 * TIMING STILL MATTERS HERE. The pre-fix failure is "correct, then reverted",
 * so an auto-retrying assertion (`toPass`, or a bare `toHaveAttribute` with its
 * default timeout) can observe the CORRECT intermediate state and pass even
 * where the ordering does occur. Both legs therefore settle first, then assert
 * once, and the owner leg asserts again after a further wait.
 */

const SETTLE_MS = 2500;

async function openSettings(page: Page, username: string, workspace: string, hash: string) {
	await page.goto(`/${username}/${workspace}/settings${hash}`);
	// The owner-only tab arrives with `/me`; waiting for the full set is what
	// makes this a measurement of the settled tab bar rather than of the
	// pre-permission one.
	await expect(page.locator('.tab-bar .tab')).toHaveCount(5);
	await page.waitForTimeout(SETTLE_MS);
}

function activeTabLabel(page: Page) {
	return page.evaluate(() => {
		const active = [...document.querySelectorAll('.tab')].find((t) =>
			t.classList.contains('active'),
		);
		return (active?.textContent ?? '').trim();
	});
}

test('BUG-2978 smoke: deep-linking the owner-only settings tab lands on it and stays', async ({
	page,
	fixture,
}) => {
	await openSettings(page, fixture.adminUsername, fixture.workspaceSlug, '#danger');

	// Non-vacuous: the fixture user must actually be an owner, or "Danger Zone
	// is not selected" would be the correct answer and the leg would pass for
	// the wrong reason.
	await expect(page.locator('.tab', { hasText: 'Danger Zone' })).toHaveCount(1);

	expect(await activeTabLabel(page), 'deep-linked owner-only tab not selected').toContain(
		'Danger Zone',
	);

	// The pre-fix build SELECTED it and then reverted, so hold and look again.
	await page.waitForTimeout(1500);
	expect(await activeTabLabel(page), 'owner-only tab was selected and then lost').toContain(
		'Danger Zone',
	);
});

test('BUG-2978 control: a tab valid without /me deep-links on both builds', async ({
	page,
	fixture,
}) => {
	// Storage is visible to every member, so it is never removed from the tab
	// set by the permission window. This leg passed before the fix too — it is
	// here to show the failure was specific to the owner-only tab rather than
	// to hash restoration in general.
	await openSettings(page, fixture.adminUsername, fixture.workspaceSlug, '#storage');
	expect(await activeTabLabel(page)).toContain('Storage');
});
