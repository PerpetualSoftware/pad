import { test, expect } from './fixtures';
import type { APIRequestContext, Page } from '@playwright/test';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	REAL_PDF,
	REAL_PNG,
	TILE,
	VIEWER,
	VIEWER_COUNTER,
	VIEWER_FALLBACK,
	VIEWER_FALLBACK_NAME,
	VIEWER_FALLBACK_NOTE,
	VIEWER_IMAGE,
	authJson,
	createCollection,
	createItem,
	deleteCollection,
	deleteWorkspace,
	dropFileIntoEditor,
	escapeRe,
	itemUrl,
	postComment,
	uploadAttachment,
	viewerClose,
	viewerDelete,
	viewerDialog,
	viewerNext,
	viewerPrev
} from './lib/attachment-viewer';

/**
 * FOUR-SURFACE PARITY + ACTIVATION + HOSTILE NAMES (TASK-2436).
 *
 * "Behaves as before" is the claim phase 3a makes about every surface it
 * touched, and it is not falsifiable without a finite matrix. This file is
 * that matrix: four producers × {open, ←/→, Escape, backdrop click, close},
 * plus the keyboard activation TASK-2432 introduced and the accessible names
 * TASK-2429/2431/2433 introduced.
 *
 * The four producers are genuinely different code paths, which is why one of
 * them passing says nothing about the others:
 *
 *   1. STRIP — `ItemAttachmentStrip` mounts `Lightbox` directly.
 *   2. TIMELINE — `ItemTimeline` mounts `Lightbox` directly, from a DELEGATED
 *      click/keydown on imperatively-annotated `{@html}` thumbnails.
 *   3. ITEM BODY — the editor's `attachment-image` NodeView emits on the
 *      open-viewer BUS; `AttachmentViewerHost` is what mounts the viewer.
 *   4. COMMENT EDITOR — the same NodeView inside a live `CommentEditor`,
 *      nested INSIDE the timeline's delegation scope, which is the collision
 *      the NodeView's unconditional `stopPropagation` exists to prevent.
 *
 * Producers 3 and 4 emit a SINGLE-image set by design ("this NodeView knows
 * about ITS node and nothing else"), so their ←/→ cell is "no nav controls,
 * arrows are no-ops" — asserted rather than skipped, since a future producer
 * quietly assembling a set here would change what ←/→ pages through.
 */

const INLINE_IMG = '.editor-content .ProseMirror img[data-attachment-id]';
const COMMENT_EDITOR = '.comment-editor .ProseMirror';

/**
 * The comment composer lives under the item CONTENT on the Details tab since
 * IDEA-2843; before that it was on the Activity tab and this helper clicked
 * there. Two of the four surfaces live here, which is itself part of what
 * makes them different code paths. Details is the default tab, so the wait is
 * all that remains of the old prelude.
 */
async function openCommentsSurface(page: Page): Promise<void> {
	await expect(page.locator('#item-comments .timeline')).toBeVisible();
}

/** open → ←/→ → Escape → reopen → backdrop click → reopen → Close button. */
async function runParityMatrix(
	page: Page,
	open: () => Promise<void>,
	opts: { multiple: boolean }
): Promise<void> {
	// OPEN
	await open();
	await expect(page.locator(VIEWER)).toHaveCount(1);
	await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

	// ←/→
	if (opts.multiple) {
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('1 / 2');
		await page.keyboard.press('ArrowRight');
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('2 / 2');
		await page.keyboard.press('ArrowLeft');
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('1 / 2');
		// The same two steps through the CONTROLS, addressed by their accessible
		// names — the keys and the buttons must not drift apart.
		await viewerNext(page).click();
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('2 / 2');
		await viewerPrev(page).click();
		await expect(page.locator(VIEWER_COUNTER)).toHaveText('1 / 2');
	} else {
		await expect(page.locator(VIEWER_COUNTER)).toHaveCount(0);
		await expect(viewerNext(page)).toHaveCount(0);
		await expect(viewerPrev(page)).toHaveCount(0);
		const src = await page.locator(VIEWER_IMAGE).getAttribute('src');
		await page.keyboard.press('ArrowRight');
		await page.keyboard.press('ArrowLeft');
		await expect(page.locator(VIEWER_IMAGE)).toHaveAttribute('src', src!);
		await expect(page.locator(VIEWER)).toHaveCount(1);
	}

	// ESCAPE
	await page.keyboard.press('Escape');
	await expect(page.locator(VIEWER)).toHaveCount(0);

	// BACKDROP CLICK — the backdrop itself, not the image (which must NOT close).
	await open();
	await expect(page.locator(VIEWER)).toHaveCount(1);
	await page.locator(VIEWER_IMAGE).click();
	await expect(page.locator(VIEWER), 'clicking the image must not dismiss').toHaveCount(1);
	await page.locator(VIEWER).click({ position: { x: 4, y: 4 } });
	await expect(page.locator(VIEWER)).toHaveCount(0);

	// CLOSE BUTTON
	await open();
	await expect(page.locator(VIEWER)).toHaveCount(1);
	await viewerClose(page).click();
	await expect(page.locator(VIEWER)).toHaveCount(0);
}

test.describe('attachment viewer — four-surface parity (TASK-2436)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough'
		);
		await page.setViewportSize(DESKTOP);
	});

	test('surface 1 — the attachment strip', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity strip');
		await uploadAttachment(fixture, request, doc.id, 'strip-a.png');
		await uploadAttachment(fixture, request, doc.id, 'strip-b.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);

		await runParityMatrix(
			page,
			async () => {
				await page.locator(`${TILE}[aria-label*="strip-a.png"]`).click();
			},
			{ multiple: true }
		);
	});

	test('surface 2 — a saved comment in the timeline', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity timeline');
		const one = await uploadAttachment(fixture, request, doc.id, 'tl-a.png');
		const two = await uploadAttachment(fixture, request, doc.id, 'tl-b.png');
		await postComment(
			fixture,
			request,
			doc.slug,
			`two shots\n\n![first](pad-attachment:${one})\n\n![second](pad-attachment:${two})\n`
		);
		await page.goto(itemUrl(fixture, doc.slug));
		await openCommentsSurface(page);

		const thumbs = page.locator('.timeline img[data-attachment-id]');
		await expect(thumbs).toHaveCount(2);
		// The imperative semantics pass is what makes these keyboard-reachable
		// controls at all — `{@html}` cannot wrap them in a <button>.
		await expect(thumbs.first()).toHaveAttribute('role', 'button');
		await expect(thumbs.first()).toHaveAttribute('aria-label', 'View image: first');

		await runParityMatrix(
			page,
			async () => {
				await thumbs.first().click();
			},
			{ multiple: true }
		);
	});

	test('surface 3 — an inline image in the item body', async ({ page, fixture, request }) => {
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity body');
		await page.goto(itemUrl(fixture, doc.slug));
		await dropFileIntoEditor(page, 'body-a.png', REAL_PNG.toString('base64'));
		await expect(page.locator(INLINE_IMG)).toHaveCount(1);
		await expect(page.locator(INLINE_IMG).first()).toHaveAttribute('role', 'button');

		await runParityMatrix(
			page,
			async () => {
				await page.locator(INLINE_IMG).first().click();
			},
			{ multiple: false }
		);
	});

	test('surface 4 — a draft image inside the comment editor', async ({ page, fixture, request }) => {
		// The nested case: this NodeView lives INSIDE `ItemTimeline`'s delegation
		// scope, so without the NodeView's unconditional `stopPropagation` one
		// activation would open TWO viewers. `toHaveCount(1)` in the matrix is
		// what catches that, and only a real browser dispatches through both.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity comment editor');
		await page.goto(itemUrl(fixture, doc.slug));
		await openCommentsSurface(page);

		await page.locator(COMMENT_EDITOR).first().click();
		await dropFileIntoEditor(page, 'draft-a.png', REAL_PNG.toString('base64'), 'image/png', COMMENT_EDITOR);
		const draftImg = page.locator(`${COMMENT_EDITOR} img[data-attachment-id]`);
		await expect(draftImg).toHaveCount(1);

		await runParityMatrix(
			page,
			async () => {
				await draftImg.first().click();
			},
			{ multiple: false }
		);
	});

	test('surface 5 — a strip FILE tile (the converged file route)', async ({ page, fixture, request }) => {
		// The producer taxonomy GROWS the file route (PLAN-2392 3c-ii). The four
		// surfaces above are all IMAGE producers reaching the raster arm; convergence
		// adds a fifth path — a non-image tile emitting on the SAME surface channel,
		// through the SAME host, onto the SAME `Lightbox`, drawing the no-bytes
		// fallback arm. A single-item set, so ←/→ are no-ops (no counter, no nav) —
		// asserted, since a future producer assembling a file set here would change
		// what paging does. Escape / backdrop / Close all dismiss, exactly as for an
		// image, because the modal chrome is shared.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity file route');
		await uploadAttachment(fixture, request, doc.id, 'route.pdf', 'application/pdf', REAL_PDF);
		await page.goto(itemUrl(fixture, doc.slug));
		const tile = page.locator(`${TILE}[aria-label*="route.pdf"]`);
		await expect(tile).toBeVisible();

		const open = async () => {
			await tile.click();
			await expect(viewerDialog(page, 'route.pdf')).toHaveCount(1);
			// The fallback arm, not the raster arm: name + honest note, no <img>.
			await expect(page.locator(VIEWER_FALLBACK)).toBeVisible();
			await expect(page.locator(VIEWER_FALLBACK_NAME)).toHaveText('route.pdf');
			await expect(page.locator(VIEWER_FALLBACK_NOTE)).toHaveText('No preview available');
			await expect(page.locator(VIEWER_IMAGE)).toHaveCount(0);
		};

		// OPEN + ←/→ no-ops on the single-item file set.
		await open();
		await expect(page.locator(VIEWER_COUNTER)).toHaveCount(0);
		await expect(viewerNext(page)).toHaveCount(0);
		await expect(viewerPrev(page)).toHaveCount(0);
		await page.keyboard.press('ArrowRight');
		await page.keyboard.press('ArrowLeft');
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(page.locator(VIEWER_FALLBACK)).toBeVisible();

		// ESCAPE
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// BACKDROP CLICK — the stage overlays are pointer-events:none, so a click on
		// the empty area (including over the centred fallback) reaches the backdrop.
		await open();
		await page.locator(VIEWER).click({ position: { x: 4, y: 4 } });
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// CLOSE BUTTON
		await open();
		await viewerClose(page).click();
		await expect(page.locator(VIEWER)).toHaveCount(0);
	});

	test('Enter and Space open an inline image; a MODIFIED Enter is the comment editor\'s submit', async ({
		page,
		fixture,
		request
	}) => {
		// TASK-2432 made inline images keyboard-activatable, and the guard that
		// lets a MODIFIED Enter through is the difference between "Cmd+Enter posts
		// the comment" and "Cmd+Enter opens a viewer and the comment is lost".
		// jsdom cannot prove either half: it does not synthesise activation, and
		// the ProseMirror keymap needs a real focused editor.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity keys');
		await page.goto(itemUrl(fixture, doc.slug));
		await dropFileIntoEditor(page, 'key-a.png', REAL_PNG.toString('base64'));
		const img = page.locator(INLINE_IMG).first();
		await expect(img).toBeVisible();

		for (const key of ['Enter', ' ']) {
			await img.focus();
			await expect(img).toBeFocused();
			await page.keyboard.press(key);
			// EXACTLY ONE viewer per press — two would mean a hand-rolled handler
			// racing something else, which is the shape the timeline delegation
			// would produce without the NodeView's `stopPropagation`.
			await expect(page.locator(VIEWER), `${key} must open exactly one viewer`).toHaveCount(1);
			await page.keyboard.press('Escape');
			await expect(page.locator(VIEWER)).toHaveCount(0);
			await expect(img, 'focus returns to the image it was activated from').toBeFocused();
		}

		// A HELD key repeats, and every repeat is another keydown — the NodeView
		// bails on `event.repeat` so one gesture is one viewer. `keyboard.down()`
		// does NOT auto-repeat (it sends a single keydown), so the repeats are
		// dispatched explicitly with the flag the handler reads.
		await img.focus();
		await page.keyboard.press('Enter');
		await expect(page.locator(VIEWER)).toHaveCount(1);
		// Stamp the mounted viewer so a SECOND activation is detectable. Counting
		// viewers is not enough on its own: a repeat that re-activated would tear
		// the first instance down and mount a new one, leaving the count at 1.
		// The host remounts per open (`{#key request}`), so a surviving stamp is
		// proof that no second activation happened.
		await page.evaluate(() => {
			(document.querySelector('.attachment-viewer') as HTMLElement & { __stamp?: number }).__stamp = 7;
		});
		// One dispatch per task, so nothing can be coalesced into a single event.
		for (let i = 0; i < 3; i++) {
			await page.evaluate(() => {
				const el = document.querySelector<HTMLElement>(
					'.editor-content .ProseMirror img[data-attachment-id]'
				)!;
				el.dispatchEvent(
					new KeyboardEvent('keydown', { key: 'Enter', repeat: true, bubbles: true, cancelable: true })
				);
			});
			await page.waitForTimeout(100);
		}
		await expect(page.locator(VIEWER)).toHaveCount(1);
		expect(
			await page.evaluate(
				() =>
					(document.querySelector('.attachment-viewer') as HTMLElement & { __stamp?: number })
						?.__stamp === 7
			),
			'a HELD Enter is ONE activation: the viewer must be the SAME instance, not a remount'
		).toBe(true);
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// ── The modified-key case, in the surface that owns the shortcut ──
		await openCommentsSurface(page);
		await page.locator(COMMENT_EDITOR).first().click();
		await dropFileIntoEditor(
			page,
			'key-b.png',
			REAL_PNG.toString('base64'),
			'image/png',
			COMMENT_EDITOR
		);
		const draftImg = page.locator(`${COMMENT_EDITOR} img[data-attachment-id]`).first();
		await expect(draftImg).toBeVisible();
		await draftImg.focus();
		await page.keyboard.press('Control+Enter');
		// The comment POSTS (it appears in the timeline) and no viewer opens.
		await expect(page.locator(VIEWER), 'Cmd/Ctrl+Enter is SUBMIT, not activation').toHaveCount(0);
		await expect(page.locator('.timeline img[data-attachment-id]')).toHaveCount(1);
	});

	test('hostile and RTL accessible names survive, and the viewer stays inside the viewport under RTL', async ({
		page,
		fixture,
		request
	}) => {
		// The names 3a INTRODUCES are built from user-controlled filenames: the
		// dialog's own `aria-label` and the strip tile's "View <name>". A long or
		// bidi-control-bearing name must still produce an addressable control and
		// a dialog that names itself — not an empty name, not a truncated one,
		// and not a layout that pushes the page sideways.
		const longName = `${'long-'.repeat(30)}name.png`;
		const bidiName = 'start ‮reversed‬ مرحبا.png';

		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Parity hostile names');
		await uploadAttachment(fixture, request, doc.id, longName);
		await uploadAttachment(fixture, request, doc.id, bidiName);
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);

		for (const name of [longName, bidiName]) {
			// Addressable BY NAME — the accessible name is the whole filename,
			// exactly as stored.
			// ANCHORED at BOTH ends of the filename: on the action verb (the
			// tile's delete control carries the same filename, so an unanchored
			// name matches two controls) and on the comma that follows it in the
			// label, so a truncated or mangled name cannot satisfy the match.
			const tile = page.getByRole('button', { name: new RegExp(`^View ${escapeRe(name)},`) });
			await expect(tile).toHaveCount(1);
			await tile.click();
			// The dialog names itself for the file (3c-ii T2b): the name now LEADS with
			// the filename and carries a `", type · size"` tail (the header), so the
			// hostile/bidi name must survive as the ANCHORED start of the label — not a
			// bare exact-equals, which the T2b header growth broke.
			await expect(viewerDialog(page, name)).toHaveCount(1);
			// ENFORCE the growth (not just tolerate it): the whole accessible name is
			// `"<name>, <type · size>"`, so a regression back to a bare-filename label
			// is caught here — the tolerant addressing matcher above would accept it.
			await expect(page.locator(VIEWER)).toHaveAttribute(
				'aria-label',
				new RegExp(`^${escapeRe(name)}, .+`)
			);
			await page.keyboard.press('Escape');
			await expect(page.locator(VIEWER)).toHaveCount(0);
		}

		// RTL: the viewer is positioned with `inset` (direction-agnostic), so it
		// must still cover exactly the viewport and must not introduce a
		// horizontal overflow — the failure a physical `left`/`right` pair would
		// produce if the layout depended on it.
		await page.evaluate(() => document.documentElement.setAttribute('dir', 'rtl'));
		await page.getByRole('button', { name: new RegExp(`^View ${escapeRe(bidiName)},`) }).click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		const rtl = await page.evaluate(() => {
			const el = document.querySelector<HTMLElement>('.attachment-viewer')!;
			const rect = el.getBoundingClientRect();
			const close = el.querySelector<HTMLElement>('.lightbox-close')!.getBoundingClientRect();
			return {
				rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
				viewport: { width: window.innerWidth, height: window.innerHeight },
				closeInside:
					close.left >= 0 &&
					close.top >= 0 &&
					close.right <= window.innerWidth &&
					close.bottom <= window.innerHeight,
				overflows: document.documentElement.scrollWidth > window.innerWidth
			};
		});
		expect(rtl.rect).toEqual({
			x: 0,
			y: 0,
			width: rtl.viewport.width,
			height: rtl.viewport.height
		});
		expect(rtl.closeInside, 'the close control must stay on screen under RTL').toBe(true);
		expect(rtl.overflows, 'the viewer must not push the page sideways under RTL').toBe(false);
		await page.evaluate(() => document.documentElement.setAttribute('dir', 'ltr'));
	});

	test('the viewer closes on a CLIENT-SIDE resource switch, with the document still mounted', async ({
		page,
		fixture,
		request
	}) => {
		// `AttachmentViewerHost`'s lifecycle rule, and the reason this is driven
		// through the PANE rather than `page.goto()`: a full navigation unloads
		// the document, so the viewer would vanish whatever the host did — the
		// assertion would be about the browser, not about the rule. The pane
		// switches the loaded item with the host still mounted, which is the only
		// place `itemId` / `resourceGen` actually transition.
		//
		// The CONTROL that keeps "the viewer closed" from being satisfied by a
		// host that closes on anything is the separate same-resource refresh test
		// below. A same-item RE-SELECT is deliberately not used for it: it also
		// closes the viewer today (the pane re-targets through a null `itemId`,
		// and the host's documented rule counts a change to null as the start of
		// a switch), so it says nothing either way.
		await browserLogin(page);
		const a = await seedDoc(fixture, request, 'Lifecycle alpha');
		const b = await seedDoc(fixture, request, 'Lifecycle bravo');
		await uploadAttachment(fixture, request, a.id, 'life-a.png');
		await uploadAttachment(fixture, request, b.id, 'life-b.png');

		await page.goto(
			`/${fixture.adminUsername}/${fixture.workspaceSlug}/docs?item=${encodeURIComponent(a.slug)}`
		);
		await expect(page.locator('.item-pane')).toBeVisible();
		await expect(page.locator(`.item-pane ${TILE}`).first()).toBeVisible();
		// Stamp the document AND the pane's own element: a reload would clear the
		// first, and a host remount (which would make the viewer disappear for a
		// reason that has nothing to do with the lifecycle rule) would replace
		// the second.
		await page.evaluate(() => {
			(window as unknown as { __spa?: number }).__spa = 1;
			(document.querySelector('.item-pane') as HTMLElement & { __pane?: number }).__pane = 2;
		});

		const openFromPane = async () => {
			await page
				.locator(`.item-pane ${TILE}`)
				.first()
				.evaluate((el) => (el as HTMLElement).click());
			await expect(page.locator(VIEWER)).toHaveCount(1);
		};
		const stillSameDocument = () =>
			page.evaluate(() => (window as unknown as { __spa?: number }).__spa === 1);
		// The card click has to be programmatic: the open viewer has (correctly)
		// made the list inert, which is the state under test.
		const selectCard = async (title: string) => {
			await page
				.locator('.item-card', { hasText: title })
				.first()
				.evaluate((el) => (el as HTMLElement).click());
		};

		await openFromPane();
		await expect(page.getByRole('dialog', { name: 'life-a.png' })).toHaveCount(1);

		// A → B, client-side: the resource under the host changed, so it closes.
		await selectCard('Lifecycle bravo');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		expect(await stillSameDocument(), 'the switch must not have been a page load').toBe(true);
		expect(
			await page.evaluate(
				() => (document.querySelector('.item-pane') as HTMLElement & { __pane?: number })?.__pane === 2
			),
			'the pane (and with it the viewer host) must have SURVIVED the switch — a remount ' +
				'would remove the viewer for a reason unrelated to the lifecycle rule'
		).toBe(true);
		await expect(page.locator('.item-pane')).toContainText('Lifecycle bravo');
	});

	test('a client-side WORKSPACE switch closes the viewer', async ({ page, fixture, request }) => {
		// The other half of "resource switch": the host addresses by item UUID,
		// and the viewer's URLs are built from a workspace slug captured at open,
		// so a viewer left standing across a workspace change would be pointing
		// at another workspace's endpoint.
		//
		// The TopBar's workspace entries are real `<a href>`s, so this is a
		// SvelteKit client-side navigation — the document survives, which is what
		// makes the assertion about the host rather than about unloading.
		const headers = authJson(fixture);
		const resp = await request.post('/api/v1/workspaces', {
			headers,
			data: { name: `Viewer ws switch ${Date.now()}` }
		});
		expect(resp.ok(), await resp.text()).toBe(true);
		const other = (await resp.json()) as { slug: string };
		try {
			await browserLogin(page);
			const doc = await seedDoc(fixture, request, 'Lifecycle ws');
			await uploadAttachment(fixture, request, doc.id, 'life-ws.png');
			await page.goto(itemUrl(fixture, doc.slug));
			const link = page.locator(`a[href$="/${other.slug}"]`).first();
			await expect(link).toHaveCount(1);

			await page.locator(TILE).first().click();
			await expect(page.locator(VIEWER)).toHaveCount(1);
			await page.evaluate(() => ((window as unknown as { __spa?: number }).__spa = 1));

			await link.evaluate((el) => (el as HTMLElement).click());
			await expect(page).toHaveURL(new RegExp(`/${other.slug}(/|$)`));
			await expect(page.locator(VIEWER)).toHaveCount(0);
			expect(
				await page.evaluate(() => (window as unknown as { __spa?: number }).__spa === 1),
				'this must be a client-side navigation, or the assertion is about unloading'
			).toBe(true);
		} finally {
			await deleteWorkspace(fixture, request, other.slug);
		}
	});

	test('a same-resource REFRESH under the viewer leaves it open', async ({
		page,
		fixture,
		request
	}) => {
		// Lifecycle rule, half two — and the mutation it exists to catch is
		// specific: keying the host on `ItemDetail`'s `loadGeneration` (bumped by
		// EVERY `loadData()`) instead of on `resourceGen` passes every unit test
		// and tears the viewer down whenever something reloads the item under it.
		//
		// The trigger is a real one: another session edits the COLLECTION, which
		// broadcasts `collection_updated` and makes this page re-read its schema
		// for the SAME item. A dedicated collection is created for it so the
		// broadcast cannot disturb specs running in parallel.
		const ws = fixture.workspaceSlug;
		const coll = await createCollection(fixture, request, `Viewer refresh probe ${Date.now()}`);
		try {
			const item = await createItem(fixture, request, coll.slug, 'Refresh under viewer');
			await uploadAttachment(fixture, request, item.id, 'refresh.png');

			await browserLogin(page);
			await page.goto(`/${fixture.adminUsername}/${ws}/${coll.slug}/${item.slug}`);
			await page.locator(TILE).first().click();
			await expect(page.locator(VIEWER)).toHaveCount(1);

			await request.patch(`/api/v1/workspaces/${ws}/collections/${coll.slug}`, {
				headers: authJson(fixture),
				data: { icon: '🧪' }
			});
			// Wait for the refresh to actually ARRIVE — otherwise "still open" is a
			// statement about a reload that never happened.
			// Scoped to the ITEM PAGE's own breadcrumb — that link renders from this
			// page's `collection` snapshot, so the new icon appearing there is the
			// same-item reload actually landing. `body` would also match the icon
			// turning up anywhere else on the page.
			await expect(page.locator('nav.breadcrumb')).toContainText('🧪');
			await expect(
				page.locator(VIEWER),
				'a same-resource reload must not tear the viewer down'
			).toHaveCount(1);
			// Still a working viewer, not a stranded husk.
			await page.keyboard.press('Escape');
			await expect(page.locator(VIEWER)).toHaveCount(0);
		} finally {
			await deleteCollection(fixture, request, coll.slug);
		}
	});

	test('a viewer opened in one host does not also open in the other (two-host isolation)', async ({
		page,
		fixture,
		request
	}) => {
		// Two `ItemDetail` mounts co-exist at runtime — the master and the pane —
		// and each mounts its own `AttachmentViewerHost` with its own token. The
		// addressing rule is the load-bearing half of DR-8, and with both hosts
		// listening on ONE module-global bus a token mix-up opens the viewer in
		// both. Only a real page has two real hosts minting two real tokens.
		await browserLogin(page);
		const master = await seedDoc(fixture, request, 'Isolation master');
		const other = await seedDoc(fixture, request, 'Isolation pane');
		await page.goto(itemUrl(fixture, master.slug));
		await dropFileIntoEditor(page, 'master-inline.png', REAL_PNG.toString('base64'));
		await expect(page.locator(INLINE_IMG)).toHaveCount(1);

		// Open the pane beside the master: two hosts, one page.
		await page.goto(`${itemUrl(fixture, master.slug)}?item=${encodeURIComponent(other.slug)}`);
		await expect(page.locator('.item-pane')).toBeVisible();
		const masterImg = page.locator(`.item-page-host ${INLINE_IMG}`).first();
		await expect(masterImg).toBeVisible();

		await masterImg.evaluate((el) => (el as HTMLElement).click());
		// EXACTLY ONE viewer — a broken token would mount one per host — and it
		// is named for the image that opened it. An `|Attachment viewer` fallback
		// here would let an unnamed viewer satisfy the assertion.
		await expect(page.locator(VIEWER)).toHaveCount(1);
		// Named for the image that opened it (3c-ii T2b: name leads with the file,
		// tail is type · size). An `|Attachment viewer` fallback here would let an
		// unnamed viewer satisfy the assertion; the anchored name rejects that.
		await expect(viewerDialog(page, 'master-inline.png')).toHaveCount(1);
	});

	test('the PEEKED master withholds Delete while the ACTIVE pane opens the surface, and a peeked-side open UN-PEEKS (dual-host addressing)', async ({
		page,
		fixture,
		request
	}) => {
		// The dual-`ItemDetail` peeked case (PLAN-2392 3c-ii T7). Two hosts co-exist —
		// the master and the pane, here on DIFFERENT items — and the module-global
		// surface bus addresses by `{itemId, hostToken}`, so an open FROM THE PANE must
		// mount in exactly ONE host. This proves the pane-side routing (the surviving
		// viewer is the pane's, and the count catches any host that wrongly ALSO
		// mounts — the two hosts differ by item, so the address must resolve to one),
		// the peeked master's withheld Delete, and — asserted, not fought — the
		// invisible-freeze rule that interacting with a peeked control ACTIVATES that
		// side (BUG-2263), so a peeked-side open un-peeks and its viewer is the
		// now-active side's, Delete and all. (The `hostToken` half — two hosts on the
		// SAME item — is exercised by the module-level addressing unit tests; here the
		// items differ, so this leg is the item-identity + no-double-mount proof.)
		await browserLogin(page);
		const master = await seedDoc(fixture, request, 'Peek master');
		const related = await seedDoc(fixture, request, 'Peek related');
		await uploadAttachment(fixture, request, master.id, 'master-peek.png');
		await uploadAttachment(fixture, request, related.id, 'pane-peek.png');

		await page.goto(`${itemUrl(fixture, master.slug)}?item=${encodeURIComponent(related.slug)}`);
		const pane = page.locator('.item-pane');
		const masterHost = page.locator('.item-page-host > .item-page');
		await expect(pane).toBeVisible();
		// Click INTO the pane → the master freezes into the peeking state.
		await pane.locator('.editor-wrapper .ProseMirror').first().click();
		await expect(masterHost.locator('.editor-wrapper .ProseMirror').first()).toHaveAttribute(
			'contenteditable',
			'false'
		);

		// The peeked master's strip still SHOWS its tile (a read affordance) but its
		// delete control is GONE — `mutationsEnabled=false` reached the peeked side.
		await expect(masterHost.locator(TILE)).toHaveCount(1);
		await expect(masterHost.locator('.att-delete')).toHaveCount(0);

		// Open from the ACTIVE pane → EXACTLY ONE viewer, the pane's image, opened by
		// the pane host. A broken address (ignoring the token) would ALSO open the
		// master's host → count 2. The active side owns the item, so Delete is offered.
		await pane.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(viewerDialog(page, 'pane-peek.png')).toHaveCount(1);
		await expect(viewerDelete(page)).toBeVisible();
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		// The un-peek, ASSERTED rather than fought: clicking the peeked master's tile
		// ACTIVATES the master before the surface captures anything, so its viewer is
		// the now-active side's — named for the master's image, WITH Delete — and the
		// master's editor is editable again.
		await masterHost.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(viewerDialog(page, 'master-peek.png')).toHaveCount(1);
		await expect(viewerDelete(page)).toBeVisible();
		await expect(masterHost.locator('.editor-wrapper .ProseMirror').first()).toHaveAttribute(
			'contenteditable',
			'true'
		);
	});
});

/** Upload bound to an item in an ARBITRARY workspace (the lifecycle test). */
async function uploadAttachmentTo(
	fixture: { apiToken: string },
	request: APIRequestContext,
	ws: string,
	itemId: string,
	filename: string
): Promise<void> {
	const resp = await request.post(
		`/api/v1/workspaces/${ws}/attachments?item_id=${encodeURIComponent(itemId)}`,
		{
			headers: { Authorization: `Bearer ${fixture.apiToken}` },
			multipart: { file: { name: filename, mimeType: 'image/png', buffer: REAL_PNG } }
		}
	);
	if (!resp.ok()) throw new Error(`upload failed (${resp.status()}): ${await resp.text()}`);
}
