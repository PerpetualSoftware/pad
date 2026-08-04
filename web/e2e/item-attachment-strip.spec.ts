import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import type { APIRequestContext, Page } from '@playwright/test';
import type { SuiteFixture } from './fixtures';

/**
 * Item attachment strip — host-level coverage (PLAN-2382).
 *
 * The component suite (ItemAttachmentStrip.svelte.test.ts) mounts the strip
 * DIRECTLY, so every one of its assertions still passes if the ItemDetail
 * mount is deleted, moved, or handed an ungated id. Three things can only be
 * proven in a real browser, and all three were flagged by Codex review as
 * structurally invisible to jsdom:
 *
 *   1. the strip is actually mounted in the item page, showing the CURRENT
 *      item's attachments across an A→B switch (TASK-2383);
 *   2. `canDelete` is wired to ItemDetail's `mutationsEnabled` and not raw
 *      `canEdit` — a peeking master must show tiles with NO delete control
 *      (TASK-2384 / DR-6). Both the component tests (which inject the prop)
 *      and the masterFreeze unit test (which checks the bare boolean) pass
 *      even if that wiring regressed;
 *   3. the delete control is genuinely keyboard reachable. jsdom applies no
 *      scoped CSS, so a regression to `visibility: hidden` — which silently
 *      drops it from the tab order — is invisible there.
 *
 * Plus the round trip TASK-2385 exists for: a file dropped into the editor
 * appears in the strip immediately, and deleting it removes the tile.
 */

const DESKTOP = { width: 1200, height: 900 };

// 1x1 transparent PNG — same bytes as internal/server/handlers_attachments_test.go
// realPNG() and workspace-bundle-roundtrip.spec.ts, so the upload walks the
// same MIME-validation path.
const REAL_PNG = Buffer.from([
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82
]);

const STRIP = '.attachment-strip';
const TILE = `${STRIP} .att-tile`;
const DELETE_BTN = `${STRIP} .att-delete`;

function itemUrl(fixture: SuiteFixture, slug: string): string {
	return `/${fixture.adminUsername}/${fixture.workspaceSlug}/docs/${slug}`;
}

/** Upload a PNG bound to `itemId`, so it lands in that item's strip. */
async function uploadTo(
	fixture: SuiteFixture,
	request: APIRequestContext,
	itemId: string,
	filename: string
): Promise<string> {
	const ws = fixture.workspaceSlug;
	const resp = await request.post(
		`/api/v1/workspaces/${ws}/attachments?item_id=${encodeURIComponent(itemId)}`,
		{
			headers: { Authorization: `Bearer ${fixture.apiToken}` },
			multipart: { file: { name: filename, mimeType: 'image/png', buffer: REAL_PNG } }
		}
	);
	if (!resp.ok()) throw new Error(`upload failed (${resp.status()}): ${await resp.text()}`);
	return ((await resp.json()) as { id: string }).id;
}

/**
 * Drop a file onto the live editor, the way a user does. The upload plugin
 * listens for a real `drop` with a DataTransfer, so we build one in the page
 * rather than driving the (nonexistent) file input.
 */
async function dropFileIntoEditor(page: Page, filename: string, base64: string): Promise<void> {
	const target = page.locator('.editor-content .ProseMirror').first();
	await target.waitFor({ state: 'visible' });
	await target.evaluate(
		(el, { filename, base64 }) => {
			const bytes = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
			const file = new File([bytes], filename, { type: 'image/png' });
			const dt = new DataTransfer();
			dt.items.add(file);
			const rect = el.getBoundingClientRect();
			el.dispatchEvent(
				new DragEvent('drop', {
					bubbles: true,
					cancelable: true,
					dataTransfer: dt,
					clientX: rect.left + 8,
					clientY: rect.top + 8
				})
			);
		},
		{ filename, base64 }
	);
}

test.describe('item attachment strip', () => {
	test('is mounted in the item page and shows only the CURRENT item across a switch (TASK-2383)', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const docA = await seedDoc(fixture, request, 'Strip host A');
		const docB = await seedDoc(fixture, request, 'Strip host B');
		await uploadTo(fixture, request, docA.id, 'alpha-only.png');
		await uploadTo(fixture, request, docB.id, 'bravo-only.png');

		await page.goto(itemUrl(fixture, docA.slug));
		// The strip is mounted between Properties and the editor — its presence
		// here is what the component suite structurally cannot assert.
		await expect(page.locator(TILE)).toHaveCount(1);
		await expect(page.locator(TILE).first()).toHaveAttribute('aria-label', /alpha-only\.png/);

		// A→B switch: B's strip must not carry A's tile. The strip persists
		// across the switch (it sits outside ItemDetail's {#key itemSlug}), so
		// this exercises the generation fence in the real navigation path.
		await page.goto(itemUrl(fixture, docB.slug));
		await expect(page.locator(TILE)).toHaveCount(1);
		await expect(page.locator(TILE).first()).toHaveAttribute('aria-label', /bravo-only\.png/);
		await expect(page.locator(`${TILE}[aria-label*="alpha-only"]`)).toHaveCount(0);
	});

	test('renders nothing at all for an item with no attachments', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const doc = await seedDoc(fixture, request, 'Strip host empty');
		// Arm BEFORE navigating: absence is only meaningful once the strip's
		// list request has actually answered. Without this the assertion can
		// pass while the fetch is still in flight — and since TASK-2418 a slow
		// fetch deliberately paints a loading row, which would make the timing
		// visible rather than merely unproven.
		const listSettled = page.waitForResponse(
			(r) => r.request().method() === 'GET' && r.url().includes('/attachments?'),
			{ timeout: 15_000 }
		);
		await page.goto(itemUrl(fixture, doc.slug));
		// Wait for the editor so we know the page settled before asserting absence.
		await expect(page.locator('.editor-content .ProseMirror').first()).toBeVisible();
		await listSettled;
		await expect(page.locator(STRIP)).toHaveCount(0);
	});

	test('a dropped file appears immediately, and deleting it removes the tile (TASK-2384 / TASK-2385)', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const doc = await seedDoc(fixture, request, 'Strip drop + delete');

		// Count attachment-LIST requests so "no refetch" is proven rather than
		// asserted in a comment. An implementation that re-listed after the drop
		// would otherwise pass this test identically (Codex review of TASK-2385).
		let listCalls = 0;
		await page.route('**/api/v1/workspaces/*/attachments?*', async (route) => {
			// The upload POST hits the same path with a query string; only count
			// the GET list.
			if (route.request().method() === 'GET') listCalls += 1;
			await route.fallback();
		});

		// `listCalls` counts REQUESTS (the route handler runs at request time),
		// so it can't tell us the load finished — wait on the RESPONSE for
		// that. Armed before navigating, per the empty-item test above.
		const listSettled = page.waitForResponse(
			(r) => r.request().method() === 'GET' && r.url().includes('/attachments?'),
			{ timeout: 15_000 }
		);
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator('.editor-content .ProseMirror').first()).toBeVisible();
		// Let the initial GET settle FIRST, so the count below is a clean
		// baseline, the fetch-vs-upload merge isn't what we're relying on, and
		// the absence assertion isn't racing the in-flight load (which since
		// TASK-2418 paints a loading row when it is slow).
		await listSettled;
		await expect.poll(() => listCalls, { timeout: 10_000 }).toBeGreaterThan(0);
		await expect(page.locator(STRIP)).toHaveCount(0);
		const callsBeforeDrop = listCalls;

		// TASK-2385: the upload announces on the attachment event bus and the
		// strip picks it up — no reload, no refetch.
		await dropFileIntoEditor(page, 'dropped.png', REAL_PNG.toString('base64'));
		await expect(page.locator(TILE)).toHaveCount(1, { timeout: 10_000 });
		expect(
			listCalls,
			'the strip must render the dropped file from the upload event — a new ' +
				'attachment-list GET here means it refetched instead'
		).toBe(callsBeforeDrop);
		await expect(page.locator(TILE).first()).toHaveAttribute('aria-label', /dropped\.png/);

		// The delete control is keyboard reachable: focus it directly (no hover)
		// and confirm it actually took focus. `visibility: hidden` would drop it
		// from the tab order and fail here — the regression jsdom can't see.
		const del = page.locator(DELETE_BTN).first();
		await del.focus();
		await expect(del).toBeFocused();
		// WCAG 2.2 target size (2.5.8), from PLAN-2382. Only a real browser
		// applies the scoped CSS, so this is the only place it can be checked.
		const box = await del.boundingBox();
		expect(box?.width).toBeGreaterThanOrEqual(24);
		expect(box?.height).toBeGreaterThanOrEqual(24);

		// TASK-2425 / DR-18: the confirmation is the app's own drill-down, not
		// a browser dialog. A native `confirm()` would hang this click until
		// Playwright auto-dismissed it, so the absence of a dialog handler is
		// itself part of the assertion.
		await del.click();
		const confirmMenu = page.locator('[role="menu"]');
		await expect(confirmMenu).toBeVisible();
		// The attachment IS embedded in the body (the drop inserted it), so the
		// prompt must say so rather than hedging.
		await expect(confirmMenu.locator('.attachment-delete-prompt')).toContainText(
			"still used in this item's content"
		);
		// Cancel is first, so the focus handoff can never land Enter on Delete.
		await expect(confirmMenu.getByRole('menuitem').first()).toContainText('Cancel');
		await confirmMenu.getByRole('menuitem', { name: 'Delete file' }).click();

		await expect(page.locator(TILE)).toHaveCount(0);
		// ...and the strip disappears entirely once empty.
		await expect(page.locator(STRIP)).toHaveCount(0);

		// The editor's inline image degrades to the missing placeholder without
		// a reload — the deletion bus reaching the live NodeView.
		await expect(page.locator('.editor-content .attachment-missing').first()).toBeVisible();
	});

	test('a peeking master shows tiles but NO delete control (TASK-2384 / DR-6)', async ({
		page,
		fixture,
		request
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'viewport driven explicitly');
		await page.setViewportSize(DESKTOP);
		await browserLogin(page);

		const master = await seedDoc(fixture, request, 'Strip freeze master');
		const related = await seedDoc(fixture, request, 'Strip freeze related');
		await uploadTo(fixture, request, master.id, 'frozen.png');

		await page.goto(itemUrl(fixture, master.slug));
		const masterHost = page.locator('.item-page-host > .item-page');
		const masterStrip = masterHost.locator(DELETE_BTN);

		// Active master: the control exists (canDelete === mutationsEnabled === true).
		await expect(masterHost.locator(TILE)).toHaveCount(1);
		await expect(masterStrip).toHaveCount(1);

		// Open a pane and click INTO it → the master goes peeking (read-only
		// freeze). Its tiles stay (the strip is a read affordance) but the
		// delete control must go — this is the wiring that regresses silently
		// if ItemDetail passes raw canEdit.
		await page.goto(`${itemUrl(fixture, master.slug)}?item=${encodeURIComponent(related.slug)}`);
		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		await pane.locator('.editor-wrapper .ProseMirror').first().click();
		await expect(
			masterHost.locator('.editor-wrapper .ProseMirror').first()
		).toHaveAttribute('contenteditable', 'false');

		await expect(masterHost.locator(TILE)).toHaveCount(1);
		await expect(masterStrip).toHaveCount(0);
	});
});
