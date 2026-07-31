import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Locator, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Cross-workspace copy / move dialog (PLAN-2373 / TASK-2355).
 *
 * Existing pane coverage asserts only that the "Move to collection…" menu ROW
 * is visible (pane-full-page-capstone.spec.ts), and the native-dialog/mobile-
 * pane interaction is exercised through an INJECTED generic dialog
 * (pane-a11y-focus.spec.ts:283) — that generic trap test is deliberately not
 * duplicated here. This suite drives the real product dialog: its destination
 * pickers, the needs_value gate, the same-workspace Move lock, and the three
 * failure presentations that are easy to get backwards.
 *
 * Locator policy: the MOBILE PANE IS ITSELF A DIALOG, so nothing here uses a
 * bare `[role="dialog"]` locator — every reference targets the copy dialog by
 * its accessible name ("Copy or move <REF>"). Everything waits on a
 * deterministic API response, never a timeout, and no click is forced: a forced
 * click would mask exactly the toast-interception regression the pane suite
 * guards against.
 */

const DESKTOP = { width: 1280, height: 900 };
const MOBILE = { width: 768, height: 1024 };

function headers(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

/** The copy dialog, addressed by accessible name (never `[role="dialog"]`). */
function copyDialog(page: Page): Locator {
	return page.getByRole('dialog', { name: /^Copy or move / });
}

function paneMoreBtn(page: Page): Locator {
	return page.getByRole('button', { name: 'More item actions' }).first();
}

/** Create a fresh destination workspace with one collection carrying `schema`. */
async function seedDestination(
	request: APIRequestContext,
	fixture: SuiteFixture,
	label: string,
	collection: { name: string; slug: string; fields: unknown[] },
): Promise<{ wsSlug: string; wsName: string; collSlug: string }> {
	const wsName = `Copy Dest ${label} ${Date.now()}`;
	const wsResp = await request.post('/api/v1/workspaces', {
		headers: headers(fixture),
		data: { name: wsName, template: 'blank' },
	});
	if (!wsResp.ok()) throw new Error(`workspace create failed (${wsResp.status()}): ${await wsResp.text()}`);
	const ws = (await wsResp.json()) as { slug: string };

	const collResp = await request.post(`/api/v1/workspaces/${ws.slug}/collections`, {
		headers: headers(fixture),
		data: {
			name: collection.name,
			slug: collection.slug,
			schema: JSON.stringify({ fields: collection.fields }),
		},
	});
	if (!collResp.ok())
		throw new Error(`collection create failed (${collResp.status()}): ${await collResp.text()}`);

	return { wsSlug: ws.slug, wsName, collSlug: collection.slug };
}

const REQUIRED_SELECT = {
	key: 'severity',
	label: 'Severity',
	type: 'select',
	options: ['low', 'high'],
	required: true,
};

function itemUrl(fixture: SuiteFixture, slug: string): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
}

/** Open the pane ⋯ menu and launch the copy dialog. Returns the ⋯ trigger. */
async function openCopyDialog(page: Page): Promise<Locator> {
	const more = paneMoreBtn(page);
	await expect(more).toBeVisible();
	await more.click();
	await page.getByRole('menuitem', { name: 'Copy or move to workspace…' }).click();
	await expect(copyDialog(page)).toBeVisible();
	return more;
}

test.describe('cross-workspace copy dialog (PLAN-2373 / TASK-2355)', () => {
	test('same-workspace destination locks to Move and submits through the MOVE endpoint', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'Copy dialog same-ws');
		await page.goto(itemUrl(fixture, slug));

		// Record which mutating endpoint is hit. Routing a same-workspace
		// destination through /copy would mint a new item id, drop the parent and
		// clone attachments instead of relocating the item — and, with an
		// `archive_source` matching the Move label this destination presents,
		// archive the original too (DR-18). So the assertion is not "it worked"
		// but "it went through /move and never /copy".
		const hits: string[] = [];
		await page.route('**/api/v1/workspaces/**/items/**', async (route) => {
			const url = route.request().url();
			if (route.request().method() === 'POST' && (url.includes('/move') || /\/copy(\?|$)/.test(url))) {
				hits.push(url.includes('/move') ? 'move' : 'copy');
			}
			await route.continue();
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);

		// Destination defaults to the current workspace → Copy is not selectable
		// and Move is the effective action.
		const copyRadio = dialog.getByRole('radio', { name: /^Copy — / });
		await expect(copyRadio).toBeDisabled();
		await expect(dialog.getByRole('radio', { name: /Move to another collection/ })).toBeChecked();

		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption('tasks');
		await preflight;

		const confirm = dialog.getByRole('button', { name: 'Move', exact: true });
		await expect(confirm).toBeEnabled();
		await confirm.click();

		// Post-move navigation: handleMove's navIfStillCurrent lands on the
		// target collection's route for the same slug.
		await page.waitForURL(`**/${fixture.adminUsername}/${fixture.workspaceSlug}/tasks/${slug}`);
		expect(hits).toEqual(['move']);
		await expect(copyDialog(page)).toBeHidden();
		// Focus restoration is asserted on the paths where the page SURVIVES the
		// close (the copy test and the mobile Escape test). A successful move
		// navigates, so there is no stable node left to restore to.
	});

	test('a required destination field gates Confirm until supplied, then the copy commits', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'happy', {
			name: 'Bugs',
			slug: 'bugs',
			fields: [REQUIRED_SELECT],
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog happy');
		await page.goto(itemUrl(fixture, slug));

		const trigger = await openCopyDialog(page);
		const dialog = copyDialog(page);

		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const firstPreflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await firstPreflight;

		// The content-semantics note is permanent, not conditional — wiki-link
		// RETARGETING is the surprising outcome and must always be stated.
		await expect(dialog.getByText(/are not rewritten/)).toBeVisible();

		// Confirm is gated on the server's own answer (needs_value empty).
		const confirm = dialog.getByRole('button', { name: 'Copy', exact: true });
		await expect(confirm).toBeDisabled();
		await expect(dialog.getByText('Needs a value')).toBeVisible();

		// Supply it through the composed FieldEditor select (not a rebuilt control).
		const secondPreflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.locator('.select-trigger').click();
		await dialog.getByRole('option', { name: 'High' }).click();
		await secondPreflight;

		await expect(confirm).toBeEnabled();
		const copyResp = page.waitForResponse(
			(r) => /\/copy(\?|$)/.test(r.url()) && r.request().method() === 'POST',
		);
		await confirm.click();
		expect((await copyResp).status()).toBe(201);

		await expect(copyDialog(page)).toBeHidden();
		await expect(trigger).toBeFocused();

		// The copy really landed in the destination, with the supplied value.
		const listed = await request.get(
			`/api/v1/workspaces/${dest.wsSlug}/collections/${dest.collSlug}/items`,
			{ headers: headers(fixture) },
		);
		expect(listed.ok()).toBeTruthy();
		const items = (await listed.json()) as { title: string; fields: string }[];
		expect(items).toHaveLength(1);
		// `fields` is a JSON *string* on the wire.
		expect(JSON.parse(items[0].fields)).toMatchObject({ severity: 'high' });

		// A plain copy leaves the source alone — no archived banner, and no
		// provenance banner: `moved_to` is absent, and absent must produce
		// nothing (no second lookup, no client-side reconstruction).
		await expect(page.locator('.archived-banner')).toHaveCount(0);
		await expect(page.locator('.moved-banner')).toHaveCount(0);
	});

	test('a required multi_select renders a blocked state, never a text box', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		// multi_select is the dangerous gap: FieldEditor falls it through to the
		// TEXT fallback, which yields a string where the server requires []any —
		// enterable and silently invalid.
		const dest = await seedDestination(request, fixture, 'blocked', {
			name: 'Tagged',
			slug: 'tagged',
			fields: [
				{ key: 'labels', label: 'Labels', type: 'multi_select', options: ['a', 'b'], required: true },
			],
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog blocked');
		await page.goto(itemUrl(fixture, slug));

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await preflight;

		// Asserted with toContainText: the sentence is split across <strong> /
		// <code> children, which getByText's element-text engine does not join.
		const blocked = dialog.locator('.notice-error').first();
		await expect(blocked).toContainText('can’t collect a value for that type safely');
		await expect(blocked).toContainText('multi_select');
		await expect(blocked).toContainText('pad item copy');
		// No control was rendered for it — the whole point is not to accept a
		// value the server would reject or misread.
		await expect(dialog.locator('.needs-row')).toHaveCount(0);
		await expect(dialog.getByRole('button', { name: 'Copy', exact: true })).toBeDisabled();
	});

	test('an in-flight copy resists Escape and backdrop, and is dispatched exactly once', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'inflight', {
			name: 'Inbox',
			slug: 'inbox',
			fields: [],
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog in-flight');
		await page.goto(itemUrl(fixture, slug));

		// HOLD the mutation rather than racing it — that is what makes the
		// dismissal attempts deterministic.
		let release!: () => void;
		const held = new Promise<void>((r) => (release = r));
		let dispatches = 0;
		await page.route(/\/copy(\?|$)/, async (route) => {
			if (route.request().method() !== 'POST') return route.continue();
			dispatches++;
			await held;
			await route.continue();
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await preflight;

		await dialog.getByRole('button', { name: 'Copy', exact: true }).click();
		await expect(dialog.getByText(/don’t close this window/)).toBeVisible();

		// Escape is forwarded by Modal UNCONDITIONALLY and cannot be suppressed
		// by prop — the veto lives in the dialog's onclose handler. Dismissal is
		// not rollback: the copy commits regardless.
		await page.keyboard.press('Escape');
		await expect(dialog).toBeVisible();
		// Backdrop.
		await page.mouse.click(5, 5);
		await expect(dialog).toBeVisible();
		await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeDisabled();

		release();
		await expect(copyDialog(page)).toBeHidden();
		expect(dispatches).toBe(1);
	});

	test('an opaque 502 answered after dispatch presents outcome-unknown with no retry', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'opaque', {
			name: 'Inbox',
			slug: 'inbox',
			fields: [],
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog opaque');
		await page.goto(itemUrl(fixture, slug));

		// A non-JSON 502 from an intermediary carries no `copy_failed` code, but
		// is exactly as ambiguous to the CLIENT: the request was dispatched, and
		// a real intermediary can answer 502 either before or after the server
		// commits. Fulfilling at the route stands in for that intermediary — so
		// what is under test is the client's CLASSIFICATION of an opaque reply
		// to a dispatched mutation, not the server's state, which by definition
		// the client cannot observe here.
		await page.route(/\/copy(\?|$)/, async (route) => {
			if (route.request().method() !== 'POST') return route.continue();
			await route.fulfill({ status: 502, contentType: 'text/html', body: '<html>502</html>' });
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await preflight;
		await dialog.getByRole('button', { name: 'Copy', exact: true }).click();

		await expect(dialog.getByText(/outcome of this copy is unknown/i)).toBeVisible();
		await expect(dialog.getByText(/do\s*not\s*retry/i)).toBeVisible();
		// No retry affordance: Confirm stays locked, and the only way out is Close.
		await expect(dialog.getByRole('button', { name: 'Copy', exact: true })).toBeDisabled();
		await expect(
			dialog.locator('.modal-footer').getByRole('button', { name: 'Close', exact: true }),
		).toBeEnabled();
	});

	test('a destination archived mid-flow returns to destination selection, NOT outcome-unknown', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'gone', {
			name: 'Doomed',
			slug: 'doomed',
			fields: [REQUIRED_SELECT],
		});
		// A second, surviving collection to fall back to. It declares the SAME
		// field, so the value entered for Doomed must survive the switch.
		await request.post(`/api/v1/workspaces/${dest.wsSlug}/collections`, {
			headers: headers(fixture),
			data: {
				name: 'Survivor',
				slug: 'survivor',
				schema: JSON.stringify({ fields: [REQUIRED_SELECT] }),
			},
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog dest-gone');
		await page.goto(itemUrl(fixture, slug));

		let dispatches = 0;
		await page.route(/\/copy(\?|$)/, async (route) => {
			if (route.request().method() === 'POST') dispatches++;
			await route.continue();
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await preflight;

		// Supply the required value for Doomed.
		const afterValue = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.locator('.select-trigger').click();
		await dialog.getByRole('option', { name: 'High' }).click();
		await afterValue;
		await expect(dialog.getByRole('button', { name: 'Copy', exact: true })).toBeEnabled();

		// Archive the chosen destination AFTER the review, BEFORE confirm. The
		// server's sentinel is explicit that this is a PRE-WRITE rejection, so
		// the user must not be sent hunting for an item that does not exist.
		const del = await request.delete(
			`/api/v1/workspaces/${dest.wsSlug}/collections/${dest.collSlug}`,
			{ headers: headers(fixture) },
		);
		expect(del.ok()).toBeTruthy();

		await dialog.getByRole('button', { name: 'Copy', exact: true }).click();
		await expect(dialog.getByText(/no longer available/)).toBeVisible();
		await expect(dialog.getByText(/outcome of this copy is unknown/i)).toHaveCount(0);
		expect(dispatches).toBe(0);
		// Back at destination selection with a replacement offered — and the
		// entered value is retained, so picking the replacement does not make the
		// user type it again.
		await expect(dialog.getByLabel('Collection', { exact: true })).toBeVisible();
		await expect(dialog.getByLabel('Collection', { exact: true }).getByText('Survivor')).toHaveCount(1);
		const replacement = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption('survivor');
		await replacement;
		await expect(dialog.getByRole('button', { name: 'Copy', exact: true })).toBeEnabled();
		await expect(dialog.getByText('your value')).toBeVisible();
		expect(dispatches).toBe(0);
	});

	test('a cross-workspace MOVE archives the source and shows the provenance banner', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'move', {
			name: 'Inbox',
			slug: 'inbox',
			fields: [],
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog move');
		await page.goto(itemUrl(fixture, slug));

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		const preflight = page.waitForResponse(
			(r) => r.url().includes('/copy/preflight') && r.status() === 200,
		);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await preflight;

		// Cross-workspace: Copy is selectable here, and Move adds archive_source.
		await dialog.getByRole('radio', { name: /^Move — / }).check();
		const copyResp = page.waitForResponse(
			(r) => /\/copy(\?|$)/.test(r.url()) && r.request().method() === 'POST',
		);
		await dialog.getByRole('button', { name: 'Move', exact: true }).click();
		expect((await copyResp).status()).toBe(201);
		await expect(copyDialog(page)).toBeHidden();

		// The source is archived in place and carries PROVENANCE — worded as
		// history ("was moved to"), never as a current location (DR-2a).
		await expect(page.locator('.archived-banner')).toBeVisible();
		const moved = page.locator('.moved-banner');
		await expect(moved).toBeVisible();
		await expect(moved).toContainText('was moved to');
		await expect(moved).toContainText(dest.wsName);
		// Linked to the destination, using the slugs the server supplied.
		await expect(moved.getByRole('link')).toHaveAttribute(
			'href',
			new RegExp(`/${dest.wsSlug}/${dest.collSlug}/`),
		);
	});

	test('a stale preflight cannot displace a newer destination', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		const dest = await seedDestination(request, fixture, 'race', {
			name: 'Alpha',
			slug: 'alpha',
			fields: [{ key: 'alpha_field', label: 'Alpha Field', type: 'text', required: true }],
		});
		await request.post(`/api/v1/workspaces/${dest.wsSlug}/collections`, {
			headers: headers(fixture),
			data: {
				name: 'Beta',
				slug: 'beta',
				schema: JSON.stringify({
					fields: [{ key: 'beta_field', label: 'Beta Field', type: 'text', required: true }],
				}),
			},
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog race');
		await page.goto(itemUrl(fixture, slug));

		// HOLD Alpha's preflight, so the destination switch provably happens
		// while it is outstanding. Holding rather than racing is what makes the
		// ordering deterministic.
		let releaseAlpha!: () => void;
		const alphaHeld = new Promise<void>((r) => (releaseAlpha = r));
		let started = 0;
		await page.route('**/copy/preflight', async (route) => {
			started++;
			if (started === 1) await alphaHeld;
			await route.continue();
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		await dialog.getByLabel('Collection', { exact: true }).selectOption('alpha');
		await expect.poll(() => started).toBe(1);

		// Switch destination while Alpha is outstanding, then let Alpha resolve
		// LAST. Its result must never be displayed, and its required-field row
		// must never join Beta's sticky set — entering a value there would ship
		// an override Beta does not declare (400 malformed_override).
		await dialog.getByLabel('Collection', { exact: true }).selectOption('beta');
		releaseAlpha();
		await expect(dialog.getByText('Beta Field')).toBeVisible();
		await expect(dialog.getByText('Alpha Field')).toHaveCount(0);

	});

	test('override edits collapse to one in-flight preflight plus one trailing run', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);
		// THREE required selects. Selects emit immediately (no FieldEditor
		// typing debounce), so each pick is a distinct override change — which
		// is what makes this test the dialog's runner rather than FieldEditor's
		// internal 500ms text debounce.
		const opts = ['low', 'high'];
		const dest = await seedDestination(request, fixture, 'coalesce', {
			name: 'Triple',
			slug: 'triple',
			fields: [1, 2, 3].map((n) => ({
				key: `sev${n}`,
				label: `Sev ${n}`,
				type: 'select',
				options: opts,
				required: true,
			})),
		});
		const { slug } = await seedDoc(fixture, request, 'Copy dialog coalesce');
		await page.goto(itemUrl(fixture, slug));

		let started = 0;
		let releaseSecond!: () => void;
		const secondHeld = new Promise<void>((r) => (releaseSecond = r));
		await page.route('**/copy/preflight', async (route) => {
			started++;
			// #1 is the initial preview; hold #2 (the first override's preview)
			// so the remaining picks land while it is genuinely in flight.
			if (started === 2) await secondHeld;
			await route.continue();
		});

		await openCopyDialog(page);
		const dialog = copyDialog(page);
		await dialog.getByLabel('Workspace', { exact: true }).selectOption(dest.wsSlug);
		await dialog.getByLabel('Collection', { exact: true }).selectOption(dest.collSlug);
		await expect.poll(() => started).toBe(1);
		await expect(dialog.locator('.needs-row')).toHaveCount(3);

		const pick = async (i: number) => {
			await dialog.locator('.select-trigger').nth(i).click();
			await dialog.getByRole('option', { name: 'High' }).click();
		};
		// First pick → one preview, which the route now HOLDS. Waiting for it to
		// start (rather than sleeping past the debounce) is what makes the next
		// two picks land while a preflight is genuinely in flight.
		await pick(0);
		await expect.poll(() => started).toBe(2);

		await pick(1);
		await pick(2);
		// Single-flight: neither pick issued a request of its own.
		expect(started).toBe(2);
		releaseSecond();
		await expect(dialog.getByRole('button', { name: 'Copy', exact: true })).toBeEnabled();
		// …and both were served by ONE trailing run, not one request each.
		expect(started).toBe(3);
	});

	test('mobile: the dialog opens over the active pane and returns focus to ⋯ on Escape', async ({
		page,
		fixture,
		request,
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const { slug } = await seedDoc(fixture, request, 'Copy dialog mobile');
		// The collection page with `?item=` — at ≤768px the pane is a
		// full-screen overlay that runs its OWN focus trap, which defers to a
		// native <dialog>. That deferral is what this asserts on the real
		// product dialog (the generic injected-dialog trap test stays where it
		// is, in pane-a11y-focus.spec.ts).
		await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${slug}`);
		await expect(page.locator('.item-pane')).toBeVisible();

		const more = page.locator('.item-pane').getByRole('button', { name: 'More item actions' });
		await expect(more).toBeVisible();
		await more.click();
		await page.getByRole('menuitem', { name: 'Copy or move to workspace…' }).click();

		const dialog = copyDialog(page);
		await expect(dialog).toBeVisible();
		// Focus is inside the dialog, not left behind in the pane.
		await expect(dialog.getByLabel('Workspace', { exact: true })).toBeFocused();

		await page.keyboard.press('Escape');
		await expect(copyDialog(page)).toBeHidden();
		// The pane itself must still be open — Escape closed the dialog only.
		await expect(page.locator('.item-pane')).toBeVisible();
		await expect(more).toBeFocused();
	});
});
