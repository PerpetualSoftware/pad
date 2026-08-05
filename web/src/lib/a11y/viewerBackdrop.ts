/**
 * Viewer backdrop manager (PLAN-2392 phase 3a / TASK-2427).
 *
 * A refcounted, lease-stacked owner of `inert` on `document.body.children` for
 * body-portaled VIEWER surfaces (the attachment viewer and friends), plus the
 * modal-arbitration helpers that let unrelated guards ask "is a viewer in
 * front of me right now?".
 *
 * Scope — this is a VIEWER backdrop, not a modal registry. `Modal`,
 * `BottomSheet` and `DockedSheet` are deliberately NOT registered here:
 * inventing promotion/ordering semantics for every overlay in the tree is a
 * different job. {@link isBlockedByModal} answers "is a viewer lease
 * frontmost", plus a native `dialog:modal` branch so narrowing the manager
 * doesn't narrow the arbitration.
 *
 * Why body children only: `body` has ONE child in practice — the SvelteKit
 * `display: contents` wrapper (`app.html:16`) — plus whatever is portaled
 * alongside it. `inert` cascades, so that single write inerts the entire app,
 * including the collection pane. There is NO ownership conflict with the
 * pane's own `inert`: the pane writes NESTED wrappers
 * (`[username]/[workspace]/+layout.svelte`), never body children. Different
 * elements, so no attribute observer or re-application logic is needed here.
 */

import { paneFocusables } from '$lib/collections/paneFocus';

/** A held backdrop lease. Release is idempotent and lease-identity-bound. */
export interface ViewerBackdropLease {
	/** The portaled body child that stays interactive while this lease is frontmost. */
	readonly exemptRoot: HTMLElement;
	/**
	 * Drop the lease. `released` is false on a repeat call (no-op). `stackEmpty`
	 * reports whether any lease remains AFTER this release — the caller uses it
	 * to decide between letting the manager hand focus to the next viewer and
	 * restoring its own invoker.
	 */
	release(): { released: boolean; stackEmpty: boolean };
}

interface LeaseRecord {
	root: HTMLElement;
	released: boolean;
}

/** Front of the stack is the LAST entry — the topmost viewer. */
const stack: LeaseRecord[] = [];

/**
 * Elements this module set `inert` on. Anything already inert when we looked
 * (someone else's write, or authored markup) is never recorded and never
 * cleared — we only undo our own writes.
 */
const owned = new Set<Element>();

/** ONE shared observer for all leases; disconnected when the stack empties. */
let observer: MutationObserver | null = null;
/** The `body` the live observer is attached to, so a replaced body re-arms it. */
let observedBody: HTMLElement | null = null;

function hasDocument(): boolean {
	return typeof document !== 'undefined' && !!document.body;
}

function frontRoot(): HTMLElement | null {
	return stack.length > 0 ? stack[stack.length - 1].root : null;
}

/**
 * The exact set of body children that SHOULD be inert right now: everything
 * except the frontmost lease's exempt root — and except an open modal
 * `<dialog>`, which lives in the TOP LAYER above the viewer. A modal dialog
 * escapes an inert ANCESTOR, but an `inert` attribute on the dialog itself
 * still disables it, so writing one here would kill a dialog opened over the
 * viewer. Open-state changes are watched (see {@link startObserver}) because
 * they are not childList mutations: a dialog mounted CLOSED before the lease
 * is inerted, and would STAY inert through a later `showModal()` — a dead
 * modal — without that.
 */
function desiredInertSet(): Set<Element> {
	const front = frontRoot();
	if (!front || !hasDocument()) return new Set();
	const desired = new Set<Element>();
	for (const child of Array.from(document.body.children)) {
		if (child === front || keepInteractiveAsDialog(child)) continue;
		desired.add(child);
	}
	return desired;
}

/**
 * Recompute and apply the desired inert set from scratch. This is the ONLY
 * mutator: release doesn't undo its own writes, it re-derives the truth from
 * the current stack, which is what makes out-of-order release correct
 * (A→B→release-A must leave A's root inert, since B is still frontmost).
 */
function reconcile(): void {
	if (!hasDocument()) return;
	const desired = desiredInertSet();

	for (const el of Array.from(owned)) {
		if (desired.has(el)) continue;
		el.removeAttribute('inert');
		owned.delete(el);
	}

	for (const el of desired) {
		if (owned.has(el)) continue;
		// Pre-existing `inert` belongs to whoever wrote it — leave it, and don't
		// take ownership, so we never clear it on release.
		if (el.hasAttribute('inert')) continue;
		el.setAttribute('inert', '');
		owned.add(el);
	}
}

function startObserver(): void {
	if (!hasDocument() || typeof MutationObserver === 'undefined') return;
	// Re-arm if `document.body` was replaced under us — the old observer is
	// still attached to a body nobody renders into any more.
	if (observer && observedBody === document.body) return;
	stopObserver();
	observer = new MutationObserver((records) => {
		// `<body>` itself was swapped: this observer is now watching a detached
		// element, so re-arm on the live body before reconciling. Detected here
		// (rather than only on the next `acquire`) because the lease may never
		// see another acquire.
		if (observedBody !== document.body) {
			startObserver();
			reconcile();
			return;
		}
		for (const record of records) {
			// Only two mutations can change the desired set, and both are scoped
			// to `<body>`'s own children:
			//
			//  • childList ON body — a portal arriving mid-lease must be inerted
			//    too. Nested childList records can't change the body-child set,
			//    so they're ignored (subtree:true is only on for the attribute
			//    filter below).
			//  • `open` on a body-child `<dialog>` — `showModal()` on a dialog
			//    mounted closed has to un-inert it, and that is an attribute
			//    change, not a childList one. Deeper `open` toggles and non-dialog
			//    `open` (`<details>`) are irrelevant.
			//
			// This deliberately does NOT observe `inert`: re-applying `inert`
			// against other writers is the design that was tried and reverted.
			const target = record.target as Element;
			const relevant =
				record.type === 'childList'
					? target === document.body
					: target.parentElement === document.body && target.tagName === 'DIALOG';
			if (relevant) {
				reconcile();
				return;
			}
		}
	});
	observer.observe(document.body, {
		childList: true,
		attributes: true,
		attributeFilter: ['open'],
		subtree: true,
	});
	// `<html>`'s child list is what tells us `<body>` itself got replaced —
	// nothing on the old body fires once it is detached.
	observer.observe(document.documentElement, { childList: true });
	observedBody = document.body;
}

function stopObserver(): void {
	observer?.disconnect();
	observer = null;
	observedBody = null;
}

/**
 * Was focus inside `root` (or nowhere in particular)? `<body>` / `null` is the
 * "adrift" state a viewer teardown leaves behind, and counts as ours to move.
 */
function focusWasInside(root: HTMLElement): boolean {
	if (!hasDocument()) return false;
	// ANY open `showModal()` dialog owns the top layer, wherever it sits in the
	// tree — inside the closing viewer, alongside it, anywhere. While one is
	// open the handoff must not run at all: it would pull focus into a viewer
	// that is, by the browser's own layering, behind the dialog. Checked BEFORE
	// the adrift case, since focus resting on `<body>` is not a licence either.
	if (openNativeModals().length > 0) return false;
	const active = document.activeElement;
	if (active === null || active === document.body) return true;
	return root.contains(active);
}

/**
 * Hand focus into `root`'s first tabbable descendant. When the viewer beneath
 * has no tabbable control (still loading, image-only), focus the root itself —
 * made programmatically focusable if it isn't already — rather than letting
 * focus fall to `<body>` once the closing viewer unmounts. Nobody else would
 * catch it: the caller only restores its own invoker on `stackEmpty`.
 */
function focusInto(root: HTMLElement): void {
	const target = paneFocusables(root)[0];
	if (target) {
		target.focus();
		return;
	}
	if (!root.hasAttribute('tabindex')) root.setAttribute('tabindex', '-1');
	root.focus();
}

/**
 * Take a backdrop lease for `exemptRoot`, inerting every OTHER body child.
 *
 * `exemptRoot` MUST be a direct child of `<body>` (the portal target). A nested
 * element would leave its containing body child inerted, which inerts the
 * "exempt" subtree along with everything else.
 */
export function acquire(exemptRoot: HTMLElement): ViewerBackdropLease {
	if (import.meta.env?.DEV && hasDocument() && exemptRoot.parentElement !== document.body) {
		console.warn(
			'[viewerBackdrop] exemptRoot is not a direct child of <body>; ' +
				'its containing body child will be inerted, including the exempt subtree.',
			exemptRoot,
		);
	}

	const record: LeaseRecord = { root: exemptRoot, released: false };
	stack.push(record);
	startObserver();
	reconcile();

	return {
		exemptRoot,
		release(): { released: boolean; stackEmpty: boolean } {
			if (record.released) return { released: false, stackEmpty: stack.length === 0 };
			record.released = true;

			const index = stack.indexOf(record);
			const wasFrontmost = index === stack.length - 1;
			if (index !== -1) stack.splice(index, 1);

			reconcile();

			const next = frontRoot();
			// Releasing a BACKGROUND lease changes nothing visible and must not
			// steal focus; only the frontmost hands off to the viewer beneath it.
			// And only when focus was actually IN the closing viewer (or already
			// adrift on `<body>`): if it sits somewhere else — a native modal
			// opened over the viewer, say — that surface owns it, and yanking it
			// into the viewer beneath would be the theft this handoff exists to
			// prevent.
			try {
				if (wasFrontmost && next && focusWasInside(exemptRoot)) focusInto(next);
			} finally {
				// Belt and braces: the handoff only runs when a lease REMAINS, so
				// today a throwing focus handler can't reach the empty-stack
				// teardown anyway. The `finally` keeps that true if the ordering
				// is ever changed.
				if (stack.length === 0) stopObserver();
			}

			return { released: true, stackEmpty: stack.length === 0 };
		},
	};
}

/** True when `root` is the frontmost viewer's exempt root. */
export function isViewerFrontmost(root: Element): boolean {
	return stack.length > 0 && stack[stack.length - 1].root === root;
}

/**
 * `:modal` matches ONLY dialogs opened with `showModal()` — unlike `[open]`,
 * which also matches non-modal `show()` dialogs and declarative
 * `<dialog open>`. Not every engine (or jsdom) supports it, so probe once and
 * fall back to "no native modal" rather than to a wrong selector.
 */
let modalSelectorSupported: boolean | null = null;

/** Every `showModal()` dialog currently open, or `[]` where `:modal` is unsupported. */
function openNativeModals(): Element[] {
	if (!hasDocument() || modalSelectorSupported === false) return [];
	try {
		const found = Array.from(document.querySelectorAll('dialog:modal'));
		modalSelectorSupported = true;
		return found;
	} catch {
		// Unsupported pseudo-class: answer "no native modal" rather than fall
		// back to a selector that would be wrong.
		modalSelectorSupported = false;
		return [];
	}
}

/**
 * Should `el` be kept OUT of the inert set as a top-layer dialog?
 *
 * The fallback differs from {@link openNativeModals} on purpose, because the
 * two failure modes are not symmetric. For arbitration, guessing "modal" where
 * there is none would block real surfaces, so an unsupported `:modal` answers
 * "no modal". For the MUTATOR, guessing "not modal" writes `inert` ONTO a real
 * `showModal()` dialog and kills it — the user is left with a modal they cannot
 * operate or dismiss. So where `:modal` is unsupported we fall back to `open`,
 * accepting that a non-modal open `<dialog>` also escapes the backdrop: a
 * slightly leaky backdrop beats a dead modal. (Every engine that ships
 * `<dialog>` today supports `:modal`; this is the jsdom / legacy path.)
 */
function keepInteractiveAsDialog(el: Element): boolean {
	if (modalSelectorSupported === false) return el.matches('dialog[open]');
	try {
		const match = el.matches('dialog:modal');
		modalSelectorSupported = true;
		return match;
	} catch {
		modalSelectorSupported = false;
		return el.matches('dialog[open]');
	}
}

/**
 * Should `owner` — the SURFACE asking to act, NOT `event.target` — decline
 * because something is in front of it?
 *
 * Derives from LEASE STATE, never from DOM `inert`, and returns **false on an
 * empty stack** (with no native modal open) so existing guards keep exactly
 * today's behaviour. An owner inside whatever IS in front is not blocked.
 *
 * A `showModal()` dialog is in the TOP LAYER, above any body-portaled viewer,
 * so it wins the precedence question outright: when one is open, only owners
 * inside it may act — even if a viewer lease is also held. With SEVERAL open,
 * ANY of them exempts its own owners: top-layer order is not observable from
 * the DOM (it is not document order), and guessing wrong would block the
 * genuine top dialog. Owners in a lower dialog are already inert by the
 * browser's own top-layer rules, so the permissive answer costs nothing.
 */
export function isBlockedByModal(owner?: Element | null): boolean {
	const modals = openNativeModals();
	if (modals.length > 0) return !modals.some((d) => d.contains(owner ?? null));

	const front = frontRoot();
	if (front) return !front.contains(owner ?? null);

	return false;
}

/**
 * Class carried by every body-portaled VIEWER root (TASK-2429). The viewer is a
 * `role="dialog"`, so without a marker it is indistinguishable from the foreign
 * modals the app's ESC guards stand down for — and standing down for it would
 * leave Escape with NO owner, since the viewer's own Escape lives on
 * `escapeStack`. Exported (rather than typed twice) so the markup and the
 * selector below cannot drift apart.
 */
export const VIEWER_ROOT_CLASS = 'attachment-viewer';

/**
 * Is a modal surface open that owns Escape ITSELF, so an escape-stack driver
 * must stand down entirely?
 *
 * This is the shared form of the existence check the two route keydown handlers
 * hand-rolled as `document.querySelector('dialog[open], [role="dialog"]:not(.item-pane)')`.
 * Two deliberate differences from that string:
 *
 *  • The NATIVE branch is feature-detected `dialog:modal`, not `dialog[open]`.
 *    A non-modal `show()` / declarative `<dialog open>` never owned Escape, and
 *    `Modal.svelte` is always mounted — so `[open]` was over-broad. Where the
 *    pseudo-class is unsupported (jsdom, legacy engines) it falls back to
 *    `dialog[open]`, i.e. exactly today's behaviour, never to something wider.
 *  • The ARIA branch additionally excludes {@link VIEWER_ROOT_CLASS}. It is
 *    otherwise UNCHANGED and deliberately kept: `BottomSheet` and `DockedSheet`
 *    are shipped `role="dialog"` Escape owners with no stack registration, and
 *    dropping the branch would regress both. `.item-pane` stays excluded for
 *    the reason it always was (TASK-2131) — it is on the stack too.
 *
 * Existence-based, not target-based, for the reason recorded at the call sites:
 * a sheet that doesn't move focus into itself leaves `document.activeElement`
 * on the trigger underneath, so a `closest()` test would miss it.
 *
 * TASK-2430 folds this into {@link isBlockedByModal}'s three-way precedence
 * across all seven global Escape/key owners; 3a needs only the two route
 * guards to stop swallowing the viewer's Escape.
 */
export function hasForeignEscapeOwner(): boolean {
	if (!hasDocument()) return false;
	const aria = `[role="dialog"]:not(.item-pane):not(.${VIEWER_ROOT_CLASS})`;
	if (modalSelectorSupported !== false) {
		try {
			const found = !!document.querySelector(`dialog:modal, ${aria}`);
			modalSelectorSupported = true;
			return found;
		} catch {
			modalSelectorSupported = false;
		}
	}
	return !!document.querySelector(`dialog[open], ${aria}`);
}

/** Test seam: drop all leases and observers without running focus handoff. */
export function __resetViewerBackdropForTests(): void {
	stack.length = 0;
	reconcile();
	stopObserver();
	owned.clear();
	modalSelectorSupported = null;
}
