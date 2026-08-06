import { test, expect } from './fixtures';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	DESKTOP,
	MOBILE,
	REAL_PNG,
	TILE,
	VIEWER,
	VIEWER_IMAGE,
	activeInViewer,
	clearProbes,
	collectionUrl,
	dropFileIntoEditor,
	expectBackgroundInert,
	focusAttemptTakes,
	isFocusable,
	itemUrl,
	uploadAttachment,
	viewerClose
} from './lib/attachment-viewer';

/**
 * THE ATTACHMENT VIEWER'S MODAL CONTRACT, IN A REAL BROWSER
 * (PLAN-2392 phase 3a / TASK-2436, DR-9).
 *
 * Phase 3a deleted a native `<dialog>` that `showModal()` had been giving five
 * guarantees for free — top-layer stacking, background inertness, a focus trap,
 * focus restore, and Escape — and replaced each with hand-written code. jsdom's
 * `<dialog>` polyfill (`web/src/test/setup-jsdom.ts`) only toggles attributes,
 * so it can see NONE of them: no inertness, no top layer, no `:modal`, no real
 * Tab traversal, no stacking. Every unit test in the phase would pass against
 * an implementation that got all five wrong.
 *
 * So each test here is written against the question "what mutation would this
 * catch, that a jsdom-equivalent implementation would survive?" — the specific
 * escapes are named at each test. The four-surface parity matrix and the
 * keyboard-activation cases live in `attachment-viewer-parity.spec.ts`; the
 * seven global key/gesture owners in `attachment-viewer-owners.spec.ts`.
 *
 * Viewport is driven explicitly, so one Playwright project is enough (the
 * mobile-viewport tests set MOBILE themselves and still run under
 * `desktop-chromium`).
 */

test.describe('attachment viewer — modal contract (TASK-2436)', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(
			testInfo.project.name !== 'desktop-chromium',
			'viewport is driven explicitly; one project is enough'
		);
		await page.setViewportSize(DESKTOP);
	});

	test('portals to <body> with viewport-fixed geometry, names itself, and takes focus on open', async ({
		page,
		fixture,
		request
	}) => {
		// PORTAL CORRECTNESS is a precondition for every other test in this file:
		// a `position: fixed` overlay is only viewport-fixed while no ancestor
		// establishes a containing block, and `transform` / `filter` /
		// `contain: layout` on ANY ancestor silently does. Trapped in such an
		// ancestor the viewer would still pass every jsdom assertion — same DOM,
		// same attributes — while covering a fraction of the screen and leaving
		// the manager inerting the subtree it lives in.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer portal');
		await uploadAttachment(fixture, request, doc.id, 'portal.png');
		await page.goto(itemUrl(fixture, doc.slug));

		const tile = page.locator(TILE).first();
		await expect(tile).toBeVisible();
		await tile.click();

		const viewer = page.locator(VIEWER);
		await expect(viewer).toBeVisible();
		// Named by the image, never anonymous — an unnamed role="dialog" is
		// announced as nothing at all.
		await expect(page.getByRole('dialog', { name: 'portal.png' })).toBeVisible();
		await expect(viewer).toHaveAttribute('aria-modal', 'true');

		const geometry = await page.evaluate(() => {
			const el = document.querySelector<HTMLElement>('.attachment-viewer')!;
			const cs = getComputedStyle(el);
			const rect = el.getBoundingClientRect();
			return {
				parentIsBody: el.parentElement === document.body,
				position: cs.position,
				zIndex: cs.zIndex,
				rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
				viewport: { width: window.innerWidth, height: window.innerHeight }
			};
		});
		expect(geometry.parentIsBody, 'the viewer must be a DIRECT child of <body>').toBe(true);
		expect(geometry.position).toBe('fixed');
		// FULL VIEWPORT, measured — not `inset: 0` read back from the cascade.
		// A containing-block ancestor changes the RECT while leaving the
		// declaration untouched.
		expect(geometry.rect.x).toBe(0);
		expect(geometry.rect.y).toBe(0);
		expect(geometry.rect.width).toBe(geometry.viewport.width);
		expect(geometry.rect.height).toBe(geometry.viewport.height);

		// FOCUS ENTRY lands on the first tabbable DESCENDANT (the close button),
		// not on the container — jsdom cannot decide this, because
		// `paneFocusables` filters on layout the jsdom DOM does not have.
		await expect(viewerClose(page)).toBeFocused();
	});

	test('every background body child goes inert — proven by injected probes and by the REAL top bar', async ({
		page,
		fixture,
		request
	}) => {
		// THE CENTRAL jsdom BLIND SPOT. The unit suite asserts the `inert`
		// ATTRIBUTE on the elements the manager chose; that passes even if the
		// choice is wrong, because jsdom never enforces inertness. Here the proof
		// is behavioural: an element that cannot take focus.
		//
		// The top bar is asserted SEPARATELY and deliberately: it is NOT a body
		// child, it is a grandchild through the SvelteKit `display: contents`
		// wrapper (`app.html:16`), so it can only go inert by CASCADE. An
		// implementation that inerted a hand-picked list of app-shell elements
		// instead of body children would pass the probe assertions and fail this.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer inert');
		await uploadAttachment(fixture, request, doc.id, 'inert.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE).first()).toBeVisible();

		const topbarToggle = '[aria-label="Hide sidebar"], [aria-label="Show sidebar"]';
		expect(
			await isFocusable(page, topbarToggle),
			'baseline: the top-bar control is reachable with no viewer open'
		).toBe(true);

		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toBeVisible();

		await expectBackgroundInert(page);
		expect(
			await isFocusable(page, topbarToggle),
			'the real top-bar control must go inert THROUGH ITS ANCESTOR'
		).toBe(false);

		// ...and it all comes back. Without this the inert half could be
		// permanent damage rather than a lease.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		expect(await isFocusable(page, topbarToggle)).toBe(true);
		const after = await page.evaluate(() =>
			Array.from(document.body.children).some((c) => c.hasAttribute('inert'))
		);
		expect(after, 'no body child may be left inert once the lease is released').toBe(false);
		await clearProbes(page);
	});

	test('focus returns to the INVOKER, which is only possible if the lease is released first', async ({
		page,
		fixture,
		request
	}) => {
		// THE ORDERING MUTATION (TASK-2429, Codex): moving the focus restore
		// BEFORE the backdrop release passes every jsdom test, because jsdom lets
		// you focus an inert element. In a browser it silently does nothing and
		// the keyboard user is left on `<body>`.
		//
		// So the test asserts BOTH ends: the invoker is genuinely unfocusable
		// while the viewer is up (otherwise "focus went back to it" would prove
		// nothing about ordering), and it HOLDS focus once the viewer is gone.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer restore');
		await uploadAttachment(fixture, request, doc.id, 'restore.png');
		await page.goto(itemUrl(fixture, doc.slug));

		const tile = page.locator(TILE).first();
		await expect(tile).toBeVisible();
		await tile.click();
		await expect(page.locator(VIEWER)).toBeVisible();

		expect(
			await isFocusable(page, `${TILE}`),
			'the invoking tile must be INERT while the viewer is open — this is what makes ' +
				'a restore-before-release a no-op in a real browser'
		).toBe(false);

		await viewerClose(page).click();
		await expect(page.locator(VIEWER)).toHaveCount(0);
		await expect(tile).toBeFocused();
	});

	test('a native showModal() dialog stays interactive above the viewer; show() and [open] do not', async ({
		page,
		fixture,
		request
	}) => {
		// `:modal` vs `[open]` (TASK-2427). jsdom has no `:modal` at all, so
		// "a declaratively-open NON-modal dialog does not block" passes there
		// VACUOUSLY — the fallback branch is what runs. Only a real engine can
		// contrast the two, and getting it wrong in the mutator's direction
		// writes `inert` onto a live top-layer modal: a dialog the user can see,
		// cannot operate, and cannot dismiss.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer native dialog');
		await uploadAttachment(fixture, request, doc.id, 'native.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toBeVisible();

		expect(
			await page.evaluate(() => {
				try {
					document.querySelectorAll('dialog:modal');
					return true;
				} catch {
					return false;
				}
			}),
			'this engine must support :modal, or the contrast below is meaningless'
		).toBe(true);

		await page.evaluate(() => {
			const make = (id: string) => {
				const d = document.createElement('dialog');
				d.id = id;
				const b = document.createElement('button');
				b.dataset.dlg = id;
				b.textContent = id;
				d.appendChild(b);
				document.body.appendChild(d);
				return d;
			};
			make('probe-modal').showModal();
			make('probe-nonmodal').show();
			// A dialog mounted CLOSED, opened modally LATER — the case the
			// original childList-only observer missed, and the reason the manager
			// observes the `open` ATTRIBUTE. Its failure mode is a dead modal.
			make('probe-late');
		});
		// The manager reconciles from a MutationObserver, i.e. on a microtask.
		await expect
			.poll(() => page.evaluate(() => document.getElementById('probe-late')!.hasAttribute('inert')))
			.toBe(true);

		const before = await page.evaluate(() => ({
			modalInert: document.getElementById('probe-modal')!.hasAttribute('inert'),
			nonModalInert: document.getElementById('probe-nonmodal')!.hasAttribute('inert')
		}));
		expect(before.modalInert, 'a showModal() dialog must NOT be inerted').toBe(false);
		expect(
			before.nonModalInert,
			'a show() dialog is NOT in the top layer and must be inerted like any other body child'
		).toBe(true);
		expect(await isFocusable(page, '[data-dlg="probe-modal"]')).toBe(true);
		expect(await isFocusable(page, '[data-dlg="probe-nonmodal"]')).toBe(false);

		// The late modal: inert while closed, released the moment it is shown.
		await page.evaluate(() =>
			(document.getElementById('probe-late') as HTMLDialogElement).showModal()
		);
		await expect
			.poll(() => page.evaluate(() => document.getElementById('probe-late')!.hasAttribute('inert')))
			.toBe(false);
		expect(
			await isFocusable(page, '[data-dlg="probe-late"]'),
			'a dialog mounted closed and shown modally later must be operable, not dead'
		).toBe(true);

		await page.evaluate(() => {
			for (const id of ['probe-modal', 'probe-nonmodal', 'probe-late']) {
				const d = document.getElementById(id) as HTMLDialogElement | null;
				d?.close();
				d?.remove();
			}
		});
	});

	test('the viewer PAINTS above the highest body-portaled overlay in the app (z-index 99999)', async ({
		page,
		fixture,
		request
	}) => {
		// DR-4c. Body-portaled overlays coexist by z-index alone, and the desktop
		// emoji picker's dropdown sits at 99999 — the reason the viewer is
		// 100000 rather than 1000. jsdom cannot see stacking at all, so the only
		// existing evidence is the declaration itself.
		//
		// HIT TESTING, not the declaration. One wrinkle makes the naive version
		// vacuous: Chromium excludes INERT subtrees from hit testing, so a rival
		// injected under a live lease would lose `elementFromPoint` at ANY
		// z-index. The rival's `inert` is therefore removed before measuring (the
		// manager does not re-apply it — it observes childList and dialog `open`,
		// not `inert`), and the raised-z-index control below proves the
		// measurement is genuinely sensitive to stacking rather than to inertness.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer paint order');
		await uploadAttachment(fixture, request, doc.id, 'paint.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER_IMAGE)).toBeVisible();

		await page.evaluate(() => {
			const rival = document.createElement('div');
			rival.id = 'rival-overlay';
			// Stands in for EmojiPickerButton's body-portaled desktop dropdown,
			// at the same z-index it declares.
			rival.style.cssText = 'position:fixed;inset:0;z-index:99999;';
			document.body.appendChild(rival);
		});
		await expect
			.poll(() => page.evaluate(() => document.getElementById('rival-overlay')!.hasAttribute('inert')))
			.toBe(true);

		const hits = await page.evaluate(() => {
			const rival = document.getElementById('rival-overlay')!;
			rival.removeAttribute('inert');
			const at = () => {
				const el = document.elementFromPoint(window.innerWidth / 2, window.innerHeight / 2);
				return el ? `${el.tagName}#${el.id}` : 'none';
			};
			const at99999 = at();
			rival.style.zIndex = '100001';
			const raised = at();
			rival.remove();
			return { at99999, raised };
		});
		expect(
			hits.at99999,
			'the viewer must be the topmost painted surface over a 99999 body-portaled overlay'
		).toBe('IMG#');
		expect(
			hits.raised,
			'CONTROL: the same measurement must flip when the rival is raised above the viewer — ' +
				'otherwise the assertion above proves nothing about stacking'
		).toBe('DIV#rival-overlay');

		// Complementary regression net for "a new overlay above this value is a
		// bug": nothing else in the app's own stylesheets may declare a z-index
		// at or above the viewer's. Declaration-based on purpose — it catches a
		// NEW overlay that no test has thought to hit-test yet.
		const scan = await page.evaluate(() => {
			const found: string[] = [];
			const unreadable: string[] = [];
			for (const sheet of Array.from(document.styleSheets)) {
				let rules: CSSRuleList;
				try {
					rules = sheet.cssRules;
				} catch {
					// FAIL CLOSED: an unreadable sheet could declare anything. The
					// app ships no cross-origin CSS, so this is reported, not skipped.
					unreadable.push(sheet.href ?? '(inline)');
					continue;
				}
				const walk = (list: CSSRuleList) => {
					for (const rule of Array.from(list)) {
						if ('cssRules' in rule) walk((rule as CSSGroupingRule).cssRules);
						const style = (rule as CSSStyleRule).style;
						const selector = (rule as CSSStyleRule).selectorText;
						if (!style || !selector) continue;
						const z = Number(style.getPropertyValue('z-index'));
						if (Number.isFinite(z) && z >= 100000 && !selector.includes('lightbox-backdrop')) {
							found.push(`${selector} { z-index: ${z} }`);
						}
					}
				};
				walk(rules);
			}
			return { found, unreadable };
		});
		expect(
			scan.unreadable,
			'every stylesheet must be readable, or the sweep below is not a sweep'
		).toEqual([]);
		expect(scan.found, 'no app overlay may declare a z-index at or above the viewer').toEqual([]);
	});

	test('Escape closes exactly the viewer on the ITEM route, leaving the page it sits over', async ({
		page,
		fixture,
		request
	}) => {
		// Route guard 1 of 2. The unit coverage for this uses a LOCAL COPY of the
		// escape driver rather than mounting either route (TASK-2429), so "Escape
		// reaches the viewer through the real guard" has never actually been
		// executed. WHICH layer closed is the assertion — a count decreasing
		// would be satisfied by the page navigating away.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer esc item');
		await uploadAttachment(fixture, request, doc.id, 'esc-item.png');
		const target = itemUrl(fixture, doc.slug);
		await page.goto(target);
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		expect(new URL(page.url()).pathname, 'the route must not have navigated').toBe(target);
		await expect(page.locator('.item-page').first()).toBeVisible();
	});

	test('Escape closes exactly the viewer on the COLLECTION route, leaving the pane open beneath it', async ({
		page,
		fixture,
		request
	}) => {
		// Route guard 2 of 2 — the duplicated handler in
		// `[collection]/+page.svelte`. This one has a LAYER UNDER the viewer (the
		// detail pane, itself an escape-stack member), so it can distinguish
		// "the top layer closed" from "an Escape closed something".
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer esc collection');
		await uploadAttachment(fixture, request, doc.id, 'esc-coll.png');
		await page.goto(collectionUrl(fixture, `?item=${encodeURIComponent(doc.slug)}`));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();
		const paneTile = page.locator(`.item-pane ${TILE}`).first();
		await expect(paneTile).toBeVisible();
		await paneTile.click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		// EXACTLY ONE layer: the pane is still open and still in the URL.
		await expect(pane).toBeVisible();
		expect(new URL(page.url()).searchParams.get('item')).toBe(doc.slug);

		// ...and the layer beneath is still live: the NEXT Escapes reach it, one
		// level per press. (From inside the pane: ESC returns focus to the list
		// and the pane STAYS; a second ESC from the list closes it.) The
		// intermediate assertion is what makes this two distinct levels rather
		// than "some number of Escapes eventually closed something".
		await page.keyboard.press('Escape');
		await expect(pane, 'the first press returns focus to the list, it does not close').toBeVisible();
		await page.keyboard.press('Escape');
		await expect(pane).toHaveCount(0);
	});

	test('two viewers: Tab stays in the frontmost, and closing it hands focus DOWN, not into inert chrome', async ({
		page,
		fixture,
		request
	}) => {
		// Two mounted viewers is a real state — the strip mounts `Lightbox`
		// directly while `AttachmentViewerHost` mounts another for inline images
		// (the hosts are deliberately not consolidated until 3c). jsdom cannot
		// isolate them: the trap only means something if Tab actually traverses,
		// and inertness only means something if the browser enforces it.
		//
		// The second viewer is opened with a programmatic `click()` because the
		// first one has (correctly) made the inline image unreachable to a user.
		// The state under test is the one the implementation documents as safe,
		// not a state a user is expected to reach.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer stack');
		await uploadAttachment(fixture, request, doc.id, 'alpha.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await dropFileIntoEditor(page, 'bravo.png', REAL_PNG.toString('base64'));
		const inline = page.locator('.editor-content .ProseMirror img[data-attachment-id]').first();
		await expect(inline).toBeVisible();

		const alphaTile = page.locator(`${TILE}[aria-label*="alpha.png"]`);
		await expect(alphaTile).toBeVisible();
		await alphaTile.click();
		await expect(page.getByRole('dialog', { name: 'alpha.png' })).toBeVisible();

		await inline.evaluate((el) => (el as HTMLElement).click());
		await expect(page.locator(VIEWER)).toHaveCount(2);

		const stacked = await page.evaluate(() =>
			Array.from(document.querySelectorAll('.attachment-viewer')).map((v) => ({
				label: v.getAttribute('aria-label'),
				inert: v.hasAttribute('inert')
			}))
		);
		expect(stacked.map((s) => s.label)).toEqual(['alpha.png', 'bravo.png']);
		expect(stacked[0].inert, 'the BACKGROUND viewer must be inert like any other body child').toBe(
			true
		);
		expect(stacked[1].inert).toBe(false);

		// The trap holds INSIDE the frontmost: several Tabs, focus never leaves.
		for (let i = 0; i < 5; i++) {
			await page.keyboard.press('Tab');
			expect(await activeInViewer(page), `Tab ${i + 1} escaped the frontmost viewer`).toBe(true);
		}
		// ...and the background viewer is unreachable for a reason Tab alone
		// cannot show: a JS trap would keep Tab inside the front viewer even if
		// the layer behind it were fully interactive. So aim focus straight at
		// the BACKGROUND viewer's close button and confirm it refuses.
		expect(
			await focusAttemptTakes(page, '.attachment-viewer:not(:last-of-type) .lightbox-close'),
			"the background viewer's own control must be inert, not merely skipped by the trap"
		).toBe(false);

		// One Escape closes EXACTLY the front one...
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(page.getByRole('dialog', { name: 'alpha.png' })).toBeVisible();
		// ...and the handoff puts focus INSIDE the newly frontmost viewer — not on
		// `<body>`, and not back into the app, which is still inert.
		expect(
			await page.evaluate(() => {
				const v = document.querySelector('.attachment-viewer')!;
				return {
					inert: v.hasAttribute('inert'),
					holdsFocus: !!document.activeElement && v.contains(document.activeElement)
				};
			})
		).toEqual({ inert: false, holdsFocus: true });

		// And the last one out restores the original invoker.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		await expect(alphaTile).toBeFocused();
	});

	test('the trap holds BACKWARD too: Shift+Tab wraps to the last control, and a single-control viewer keeps focus', async ({
		page,
		fixture,
		request
	}) => {
		// THE BACKWARD-WRAP BRANCH. `nextTrapTarget` returns `last` only when
		// Shift is held AND focus is on the FIRST control (paneFocus.ts:108-111);
		// nothing in this suite executed that branch in a browser until now, so a
		// trap that held forward and leaked backward would have shipped green. It
		// cannot be reached in jsdom either — the whole thing rests on `focus()`
		// actually moving and on `preventDefault` actually suppressing the UA's
		// own traversal.
		await browserLogin(page);
		const many = await seedDoc(fixture, request, 'Viewer trap back');
		await uploadAttachment(fixture, request, many.id, 'trap-a.png');
		await uploadAttachment(fixture, request, many.id, 'trap-b.png');
		await page.goto(itemUrl(fixture, many.slug));
		await expect(page.locator(TILE)).toHaveCount(2);
		await page.locator(`${TILE}[aria-label*="trap-a.png"]`).click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		// Two images ⇒ three controls, in DOM order: Close, Previous, Next.
		const focused = () => page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? null);
		expect(await focused(), 'focus entry is the first control').toBe('Close');

		// FIRST → wraps to LAST. This is the branch.
		await page.keyboard.press('Shift+Tab');
		expect(await focused(), 'Shift+Tab from the first control must wrap to the LAST').toBe(
			'Next image'
		);
		// ...and from the middle of the set it simply steps backward, so the
		// wrap above is a wrap and not "Shift+Tab always lands on Next".
		await page.keyboard.press('Shift+Tab');
		expect(await focused()).toBe('Previous image');
		await page.keyboard.press('Shift+Tab');
		expect(await focused()).toBe('Close');
		expect(await activeInViewer(page), 'backward traversal never left the viewer').toBe(true);

		// The FORWARD wrap, asserted by name rather than by containment: from the
		// last control, Tab returns to the first.
		await page.locator(VIEWER).getByRole('button', { name: 'Next image' }).focus();
		await page.keyboard.press('Tab');
		expect(await focused(), 'Tab from the last control must wrap to the FIRST').toBe('Close');

		// SINGLE-CONTROL VIEWER: with one image there are no nav buttons, so
		// `first === last` and BOTH directions resolve to the same element. A
		// trap that only handled the multi-control case would let focus escape
		// here — into a background that is inert, i.e. onto `<body>`.
		const one = await seedDoc(fixture, request, 'Viewer trap single');
		await uploadAttachment(fixture, request, one.id, 'trap-solo.png');
		await page.goto(itemUrl(fixture, one.slug));
		await page.locator(TILE).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expect(page.locator(VIEWER).getByRole('button', { name: 'Next image' })).toHaveCount(0);
		expect(await focused()).toBe('Close');
		for (const key of ['Tab', 'Shift+Tab', 'Tab']) {
			await page.keyboard.press(key);
			expect(await focused(), `${key} in a single-control viewer must stay on it`).toBe('Close');
		}
	});

	test('a native showModal() dialog over the viewer wins Escape AND Tab outright', async ({
		page,
		fixture,
		request
	}) => {
		// THE PRECEDENCE RULE, in an engine. `hasForeignEscapeOwner()`'s native
		// branch says a `dialog:modal` out-ranks a viewer lease, so the route
		// driver stands down and the viewer's Escape (which lives only on
		// `escapeStack`) must NOT run. jsdom throws on `:modal` and has no top
		// layer, so the rule was written deliberately and never executed.
		//
		// Both halves are asserted because they fail differently: getting Escape
		// wrong closes two layers on one press, getting Tab wrong drags focus out
		// of the dialog and into the viewer underneath it.
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer native precedence');
		// TWO images, so the viewer has nav controls and therefore a Tab the trap
		// genuinely OWNS — that is the control leg below.
		await uploadAttachment(fixture, request, doc.id, 'precedence-a.png');
		await uploadAttachment(fixture, request, doc.id, 'precedence-b.png');
		await page.goto(itemUrl(fixture, doc.slug));
		await expect(page.locator(TILE)).toHaveCount(2);
		await page.locator(`${TILE}[aria-label*="precedence-a.png"]`).click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		// INSTRUMENT `defaultPrevented`, because focus alone cannot distinguish the
		// mutation (Codex round 4). With `isBlockedByModal(el)` removed from
		// `Lightbox.onKeydown`, the viewer WOULD act — `preventDefault()` and aim
		// focus at its own Close button — but `showModal()` inerts the rest of the
		// document, so that focus attempt is refused and focus stays in the dialog
		// anyway. The observable difference is whether the viewer consumed the key.
		// The listener is registered AFTER the viewer mounted, so it runs after
		// `Lightbox`'s own handler in the same dispatch.
		await page.evaluate(() => {
			(window as unknown as { __tab: boolean[] }).__tab = [];
			window.addEventListener('keydown', (e) => {
				if (e.key === 'Tab') (window as unknown as { __tab: boolean[] }).__tab.push(e.defaultPrevented);
			});
		});
		const tabPrevented = () => page.evaluate(() => (window as unknown as { __tab: boolean[] }).__tab);

		// CONTROL, viewer alone: a Tab the trap OWNS (from the last control, which
		// wraps to the first) IS consumed. Without this the assertion below would
		// pass against a viewer whose key handler never runs at all.
		await page.locator(VIEWER).getByRole('button', { name: 'Next image' }).focus();
		await page.keyboard.press('Tab');
		expect(
			(await tabPrevented()).at(-1),
			'control: with no dialog, the viewer OWNS a wrapping Tab'
		).toBe(true);

		await page.evaluate(() => {
			(window as unknown as { __tab: boolean[] }).__tab = [];
			const d = document.createElement('dialog');
			d.id = 'over-viewer';
			d.innerHTML =
				'<button data-dlg="a">first</button><button data-dlg="b">second</button>';
			document.body.appendChild(d);
			d.showModal();
		});
		const dialogOpen = () =>
			page.evaluate(() => (document.getElementById('over-viewer') as HTMLDialogElement).open);
		expect(await dialogOpen()).toBe(true);

		// TAB: the viewer's own key handler stands down for the top layer.
		//
		// The assertion is "focus never lands INSIDE THE VIEWER", not "focus is
		// always inside the dialog": the UA cycles a modal dialog's sequential
		// navigation through the document root between passes, so a strict
		// in-dialog check fails on a browser detail rather than on the rule. What
		// the rule forbids is the viewer's trap reaching up into the top layer
		// and dragging focus down into itself, and that is what this measures.
		await page.locator('[data-dlg="a"]').focus();
		const landings: string[] = [];
		for (let i = 0; i < 4; i++) {
			await page.keyboard.press('Tab');
			const where = await page.evaluate(() => {
				const a = document.activeElement;
				if (!a) return 'none';
				if (a.closest('.attachment-viewer')) return 'VIEWER';
				if (a.closest('#over-viewer')) return `dialog:${a.getAttribute('data-dlg')}`;
				return a === document.body ? 'body' : a.tagName;
			});
			landings.push(where);
			expect(where, `Tab ${i + 1} was pulled into the viewer under the native modal`).not.toBe(
				'VIEWER'
			);
		}
		// ...it really is cycling the dialog, so the check above is not passing
		// merely because focus went nowhere at all...
		expect(
			landings.filter((l) => l.startsWith('dialog:')).length,
			`Tab never returned into the native modal (landings: ${landings.join(', ')})`
		).toBeGreaterThan(0);
		// ...and the VIEWER never consumed any of those presses. This is the
		// assertion that fails if the `isBlockedByModal` bail is removed; the
		// containment checks above would survive it.
		expect(
			await tabPrevented(),
			'the viewer must not consume Tab while a top-layer dialog owns the screen'
		).toEqual([false, false, false, false]);

		// ESCAPE #1 closes the DIALOG ONLY — the browser's own cancel — and the
		// viewer beneath it is untouched.
		await page.keyboard.press('Escape');
		await expect.poll(dialogOpen).toBe(false);
		await expect(
			page.locator(VIEWER),
			'the viewer must NOT close while a top-layer dialog owns the screen'
		).toHaveCount(1);

		// ESCAPE #2, with the dialog gone, reaches the viewer — so press #1 was
		// arbitration, not a dead key.
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		await page.evaluate(() => document.getElementById('over-viewer')?.remove());
	});

	test('a DETACHED invoker does not break the teardown: no stranded backdrop, lease released, app usable', async ({
		page,
		fixture,
		request
	}) => {
		// `restoreFocus`'s DECLINE path. The invoker here is an editor NodeView
		// `<img>`, and that NodeView is re-rendered on any document change — so
		// "the element that opened the viewer is no longer in the document by the
		// time it closes" is the ordinary case, not a hypothetical.
		//
		// WHAT THIS TEST DOES AND DOES NOT KILL (Codex round 4, recorded rather
		// than papered over). It does NOT kill removal of the
		// `openInvoker.isConnected` check: in Chromium `focus()` on a detached
		// node is a no-op, so guarded and unguarded end in the same state. Nor
		// does it kill a change to the release/restore/remove ORDER — with a
		// detached invoker there is no focus target either way (that ordering is
		// killed by the 'focus returns to the INVOKER' test above, which uses a
		// CONNECTED invoker that is inert until the lease drops).
		//
		// What it does kill is a THROW in the teardown. `el.remove()` runs AFTER
		// the restore, and the node has been reparented out of its Svelte anchor,
		// so an exception in `restoreFocus` strands the backdrop in the DOM over
		// an app that has already been un-inerted. That is a user-visible wedge,
		// and it is what the assertions below are aimed at.
		// Any uncaught exception in the teardown surfaces here, so the failure
		// names the cause instead of only its symptom.
		const pageErrors: string[] = [];
		page.on('pageerror', (err) => pageErrors.push(String(err)));

		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer detached invoker');
		await page.goto(itemUrl(fixture, doc.slug));
		await dropFileIntoEditor(page, 'detached.png', REAL_PNG.toString('base64'));
		const inline = page.locator('.editor-content .ProseMirror img[data-attachment-id]').first();
		await expect(inline).toBeVisible();
		await inline.click();
		await expect(page.locator(VIEWER)).toHaveCount(1);

		// Replace the invoker with an identical clone — what a NodeView re-render
		// does to it. The captured reference is now detached; the document still
		// shows an image, so nothing about the page looks different.
		const detached = await page.evaluate(() => {
			const img = document.querySelector<HTMLElement>(
				'.editor-content .ProseMirror img[data-attachment-id]'
			)!;
			img.replaceWith(img.cloneNode(true));
			return !img.isConnected;
		});
		expect(detached, 'the captured invoker must actually be out of the document').toBe(true);

		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);

		const after = await page.evaluate(() => ({
			// A throw in the restore would skip `el.remove()` and strand the
			// backdrop — the count above is checked on `.attachment-viewer`, so
			// this is the same element by a different name for clarity.
			strandedBackdrops: document.querySelectorAll('.lightbox-backdrop').length,
			anyInert: Array.from(document.body.children).some((c) => c.hasAttribute('inert')),
			// WEAK, and kept as a sanity check rather than as proof: removing the
			// focused viewer parks focus on `<body>` whether or not the restore
			// ran at all. It is here to catch focus being left somewhere absurd,
			// not to demonstrate the decline path.
			activeIsBodyOrNone:
				document.activeElement === document.body || document.activeElement === null
		}));
		expect(pageErrors, 'the teardown must not throw with a detached invoker').toEqual([]);
		expect(
			after.strandedBackdrops,
			'a throw in the restore would skip `el.remove()` and strand the backdrop'
		).toBe(0);
		expect(after.anyInert, 'the lease must be released even when the restore declines').toBe(false);
		expect(after.activeIsBodyOrNone, 'focus must not be left on a detached element').toBe(true);

		// The app is genuinely usable again — the consequence a user would feel
		// if the teardown had thrown half-way.
		expect(
			await isFocusable(page, '[aria-label="Hide sidebar"], [aria-label="Show sidebar"]')
		).toBe(true);
	});

	test('over the mobile pane: the exact inert set at pane-only, viewer-over-pane, and close', async ({
		page,
		fixture,
		request
	}) => {
		// THE REAL PANE INTEGRATION. Two independent inert writers meet here and
		// neither exists in jsdom: the mobile pane overlay inerts NESTED
		// `display: contents` wrappers in the workspace layout, while the viewer
		// backdrop inerts BODY CHILDREN. The claim in `viewerBackdrop.ts` is that
		// they never touch the same elements — untestable in a unit test, since
		// `paneOverlay` and the layout attributes only exist in a browser.
		await page.setViewportSize(MOBILE);
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'Viewer pane inert');
		await uploadAttachment(fixture, request, doc.id, 'pane.png');
		await page.goto(collectionUrl(fixture, `?item=${encodeURIComponent(doc.slug)}`));

		const pane = page.locator('.item-pane');
		await expect(pane).toBeVisible();

		const snapshot = () =>
			page.evaluate(() => ({
				bodyChildrenInert: Array.from(document.body.children).map((c) => c.hasAttribute('inert')),
				// Everything inert that is NOT a body child — i.e. the pane
				// overlay's own writes, described stably enough to compare sets
				// across transitions.
				nestedInert: Array.from(document.querySelectorAll('[inert]'))
					.filter((el) => el.parentElement !== document.body)
					.map((el) => `${el.tagName}.${String(el.className).split(' ')[0]}`)
					.sort(),
				paneReachable: (() => {
					const el = document.querySelector<HTMLElement>('.item-pane');
					if (!el) return false;
					el.focus();
					return document.activeElement === el;
				})()
			}));

		// PANE ONLY — the mobile overlay owns the chrome, no body child is inert.
		const paneOnly = await snapshot();
		expect(paneOnly.bodyChildrenInert.some(Boolean), 'no viewer lease exists yet').toBe(false);
		expect(
			paneOnly.nestedInert.length,
			'the mobile pane overlay must have inerted the app-shell chrome'
		).toBeGreaterThan(0);
		expect(paneOnly.paneReachable, 'the pane itself stays interactive').toBe(true);

		// VIEWER OVER PANE — the backdrop adds body-child inertness ON TOP,
		// and the pane (a descendant of a body child) goes inert WITH it.
		await page.locator(`.item-pane ${TILE}`).first().click();
		await expect(page.locator(VIEWER)).toHaveCount(1);
		await expectBackgroundInert(page);
		const over = await snapshot();
		expect(
			over.nestedInert,
			"the pane's own inert writes are untouched by the viewer's — different elements, " +
				'which is the claim `viewerBackdrop.ts` makes and cannot prove in jsdom'
		).toEqual(paneOnly.nestedInert);
		expect(over.paneReachable, 'the pane is behind the viewer and must be unreachable').toBe(false);

		// CLOSE — back to exactly the pane-only set, not to "nothing inert".
		await page.keyboard.press('Escape');
		await expect(page.locator(VIEWER)).toHaveCount(0);
		await expect(pane).toBeVisible();
		const closed = await snapshot();
		expect(closed.bodyChildrenInert.some(Boolean), 'the lease released every body child').toBe(
			false
		);
		expect(
			closed.nestedInert,
			'the pane overlay must still own exactly what it owned before'
		).toEqual(paneOnly.nestedInert);
		expect(closed.paneReachable).toBe(true);
		await clearProbes(page);
	});
});
