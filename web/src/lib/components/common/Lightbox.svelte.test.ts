import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import Lightbox from './Lightbox.svelte';
import {
	acquire,
	hasForeignEscapeOwner,
	__resetViewerBackdropForTests,
	VIEWER_ROOT_CLASS,
} from '$lib/a11y/viewerBackdrop';
import {
	pushEscapeHandler,
	runTopEscape,
	ESCAPE_PRIORITY,
	_resetEscapeStackForTests,
} from '$lib/stores/escapeStack';

// TASK-2429 — the DR-4b modal contract on the attachment viewer.
//
// WHAT JSDOM CANNOT PROVE, and is therefore TASK-2436's browser suite (DR-9):
//
//  • REAL INERTNESS. jsdom parses the `inert` attribute but does not implement
//    its semantics: a control inside an inert subtree is still clickable and
//    focusable here. So the tests below assert that the manager WAS ASKED (the
//    attribute lands on the right body children and is removed again), never
//    that the background is genuinely unreachable.
//  • LAYOUT AND STACKING. There is no layout engine, so "fixed, covering the
//    viewport, above everything" is unassertable — including the one that
//    actually bites: an ancestor with `transform` / `filter` / `contain` making
//    a `position: fixed` overlay scroll with the page. What IS assertable is
//    the structural precondition, and it is asserted: the root is a DIRECT
//    child of `<body>`, so no ancestor can establish a containing block.
//  • REAL TAB TRAVERSAL. jsdom does not move focus on a Tab keydown at all, so
//    the trap is exercised through the handler's own decision (it preventDefaults
//    and focuses explicitly) rather than through browser behaviour.
//  • VISIBILITY. `offsetParent` / `getClientRects` report everything hidden, so
//    `paneFocusables` would return an empty set for every element. The stub
//    below (the shape `viewerBackdrop.svelte.test.ts` uses) makes the real
//    selection path run instead of always seeing nothing.

const realGetClientRects = HTMLElement.prototype.getClientRects;

const IMG_A = 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa';
const IMG_B = 'bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb';

interface Props {
	images: { id: string; alt: string }[];
	index?: number;
	wsSlug: string;
	onClose: () => void;
	invoker?: HTMLElement | null;
}

// Reactive props for the capture-at-open cases ($state may only initialize a
// declaration, hence top level).
const liveProps = $state<Props>({
	images: [{ id: IMG_A, alt: 'a diagram' }],
	index: 0,
	wsSlug: 'ws-one',
	onClose: () => {},
});

/** The app shell's stand-in: a body child, so the manager has something to inert. */
let appRoot: HTMLElement;
const mounted: ReturnType<typeof mount>[] = [];

function mountViewer(props: Partial<Props> = {}): ReturnType<typeof mount> {
	const app = mount(Lightbox, {
		target: appRoot,
		props: {
			images: [{ id: IMG_A, alt: 'a diagram' }],
			index: 0,
			wsSlug: 'ws-one',
			onClose: () => {},
			...props,
		},
	});
	mounted.push(app);
	flushSync();
	return app;
}

function roots(): HTMLElement[] {
	return Array.from(document.body.querySelectorAll<HTMLElement>('.lightbox-backdrop'));
}

function root(): HTMLElement {
	const found = roots();
	if (found.length === 0) throw new Error('no viewer mounted');
	return found[found.length - 1];
}

function imageSrc(scope: HTMLElement = root()): string {
	return scope.querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('src') ?? '';
}

function closeButton(scope: HTMLElement = root()): HTMLButtonElement {
	return scope.querySelector<HTMLButtonElement>('.lightbox-close')!;
}

/** A cancelable window keydown, returning whether the app consumed it. */
function press(key: string, init: KeyboardEventInit = {}): boolean {
	const event = new KeyboardEvent('keydown', { key, cancelable: true, bubbles: true, ...init });
	window.dispatchEvent(event);
	flushSync();
	return event.defaultPrevented;
}

/** Body children currently carrying `inert` (see the jsdom caveat above). */
function inertBodyChildren(): Element[] {
	return Array.from(document.body.children).filter((el) => el.hasAttribute('inert'));
}

beforeEach(() => {
	HTMLElement.prototype.getClientRects = function () {
		return [{}] as unknown as DOMRectList;
	};
	Object.assign(liveProps, {
		images: [{ id: IMG_A, alt: 'a diagram' }],
		index: 0,
		wsSlug: 'ws-one',
		onClose: () => {},
	});
	appRoot = document.body.appendChild(document.createElement('div'));
	appRoot.id = 'app';
});

afterEach(() => {
	while (mounted.length) unmount(mounted.pop()!);
	document.body.innerHTML = '';
	__resetViewerBackdropForTests();
	_resetEscapeStackForTests();
	HTMLElement.prototype.getClientRects = realGetClientRects;
	vi.restoreAllMocks();
});

describe('Lightbox — dialog semantics', () => {
	it('is an aria-modal dialog named after the image', () => {
		mountViewer();
		expect(root().getAttribute('role')).toBe('dialog');
		expect(root().getAttribute('aria-modal')).toBe('true');
		expect(root().getAttribute('aria-label')).toBe('a diagram');
	});

	it('falls back to a generic name when the image has no alt', () => {
		// An unnamed dialog is announced as nothing at all, so the fallback is
		// part of the contract rather than a nicety.
		mountViewer({ images: [{ id: IMG_A, alt: '' }] });
		expect(root().getAttribute('aria-label')).toBe('Attachment viewer');
	});

	it('gives every control a real accessible name, not a glyph', () => {
		// The button text is "✕" / "‹" / "›", and `title` does not win over
		// element content for the accessible name — so without these the controls
		// are announced as punctuation, and TASK-2436's browser suite (which
		// addresses surfaces BY NAME) would have nothing to target.
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		expect(closeButton().getAttribute('aria-label')).toBe('Close');
		expect(
			root().querySelector('.lightbox-nav.prev')?.getAttribute('aria-label')
		).toBe('Previous image');
		expect(
			root().querySelector('.lightbox-nav.next')?.getAttribute('aria-label')
		).toBe('Next image');
	});

	it('names itself after the image CURRENTLY shown, not the one it opened on', () => {
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		expect(root().getAttribute('aria-label')).toBe('first');
		root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!.click();
		flushSync();
		expect(root().getAttribute('aria-label')).toBe('second');
	});
});

describe('Lightbox — portal', () => {
	it('portals to <body> DIRECTLY, not into its mount container', () => {
		// The structural half of the fixed-overlay contract: with `<body>` as the
		// parent there is no ancestor left to establish a containing block with
		// `transform` / `filter` / `contain`, which is the failure mode that
		// silently traps a `position: fixed` overlay. The geometric half needs a
		// layout engine and belongs to TASK-2436.
		mountViewer();
		expect(root().parentElement).toBe(document.body);
		expect(appRoot.contains(root())).toBe(false);
	});

	it('carries the viewer-root class the app-wide guards key off', () => {
		// `hasForeignEscapeOwner` excludes this class so the route ESC guards
		// look PAST the viewer to the escape stack. A rename that touched only
		// the markup would make Escape dead app-wide, hence the shared constant.
		mountViewer();
		expect(root().classList.contains(VIEWER_ROOT_CLASS)).toBe(true);
	});

	it('takes the portaled root back out of <body> on close', () => {
		const app = mountViewer();
		expect(roots()).toHaveLength(1);
		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(roots()).toHaveLength(0);
	});
});

describe('Lightbox — workspace captured at open', () => {
	it('keeps serving the open-time workspace after the prop changes', () => {
		mounted.push(mount(Lightbox, { target: appRoot, props: liveProps }));
		flushSync();
		expect(imageSrc()).toContain('/workspaces/ws-one/');

		// The pane switches workspace WITHOUT remounting what is above it, so a
		// live read would rebuild already-captured attachment ids against the new
		// workspace — a 404 at best, another workspace's blob at worst.
		liveProps.wsSlug = 'ws-two';
		flushSync();
		expect(imageSrc()).toContain('/workspaces/ws-one/');
		expect(imageSrc()).not.toContain('ws-two');
	});

	it('still rebuilds the src when the SHOWN IMAGE changes', () => {
		// The guard above would also pass against a src that never updates at
		// all, which would be a different bug. This is the counterweight.
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		expect(imageSrc()).toContain(IMG_A);
		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);
		expect(imageSrc()).toContain('/workspaces/ws-one/');
	});
});

describe('Lightbox — focus', () => {
	it('moves focus to the FIRST tabbable descendant, not the root or any other', () => {
		// Multi-image on purpose: with three controls (close, prev, next) this
		// separates "the first tabbable" from "a tabbable" and from "the root".
		// A single-control fixture would pass for an implementation that took the
		// LAST candidate.
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		const controls = Array.from(root().querySelectorAll('button'));
		expect(controls).toHaveLength(3);
		expect(document.activeElement).toBe(controls[0]);
		expect(document.activeElement).toBe(closeButton());
		expect(document.activeElement).not.toBe(root());
	});

	it('returns focus to the invoker on close', () => {
		const invoker = appRoot.appendChild(document.createElement('button'));
		const app = mountViewer({ invoker });
		expect(document.activeElement).toBe(closeButton());

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(invoker);
	});

	it('falls back to <body> when the invoker was detached while the viewer was up', () => {
		// An editor NodeView is re-rendered on any document change, so the
		// element that opened the viewer is routinely gone by close time.
		const invoker = appRoot.appendChild(document.createElement('button'));
		const app = mountViewer({ invoker });
		invoker.remove();
		// `activeElement` alone can't see the `isConnected` check: focusing a
		// DETACHED element is a no-op in jsdom, so focus would land on <body>
		// either way. Asserting the call never happens is what pins the check —
		// and on a real engine an unguarded focus() on a detached node moves
		// focus to <body> on some engines and nowhere on others.
		const attempted = vi.spyOn(invoker, 'focus');

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(attempted).not.toHaveBeenCalled();
		expect(document.activeElement).toBe(document.body);
	});

	it('falls back to <body> when the invoker is connected but not focusable', () => {
		// `isConnected` alone is not enough: deletion can leave the node in the
		// tree but unfocusable (hidden, inerted, or never focusable to begin
		// with). The restore focuses it and VERIFIES, rather than trusting.
		//
		// `activeElement` ALONE cannot see the difference here, and a test that
		// stopped at it would be vacuous: focus sitting inside the root the
		// teardown then removes ends up on `<body>` either way. What separates a
		// verified restore from a trusting one is that the verified path parks
		// focus DELIBERATELY instead of relying on node-removal fallout — so the
		// blur is asserted too. (Whether a real engine refuses focus on an inert
		// or hidden invoker is TASK-2436's; jsdom's `focus()` no-ops on a
		// non-focusable element, which is the same shape.)
		const blurred = vi.spyOn(HTMLElement.prototype, 'blur');
		const invoker = appRoot.appendChild(document.createElement('div'));
		const app = mountViewer({ invoker });

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(document.body);
		expect(blurred).toHaveBeenCalled();
	});

	it('falls back to whatever held focus at open when no invoker is threaded', () => {
		// The strip and the timeline don't pass an invoker until TASK-2431, and
		// they keep focus on the clicked tile today. Focus entry is about to move
		// focus INTO the viewer, so without this capture those two producers would
		// come out of this commit strictly worse than before it.
		const tile = appRoot.appendChild(document.createElement('button'));
		tile.focus();

		const app = mountViewer();
		expect(document.activeElement).toBe(closeButton());

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(tile);
	});

	it('falls back to <body> when nothing held focus at open either', () => {
		const app = mountViewer();
		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(document.body);
	});

	it('prefers an explicit invoker over the element that held focus', () => {
		const tile = appRoot.appendChild(document.createElement('button'));
		const invoker = appRoot.appendChild(document.createElement('button'));
		tile.focus();

		const app = mountViewer({ invoker });
		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(invoker);
	});

	it('does NOT yank focus back when something else already owns it', () => {
		// A producer that moves focus from its own close handler runs BEFORE this
		// teardown, and a surface opened over the viewer owns focus outright.
		// Either way the restore must decline rather than move focus a second
		// time. (`AttachmentViewerHost` was the first case until TASK-2429 moved
		// the restore here; the guard still covers every other owner.)
		const invoker = appRoot.appendChild(document.createElement('button'));
		const elsewhere = appRoot.appendChild(document.createElement('button'));
		const app = mountViewer({ invoker });

		elsewhere.focus();
		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(document.activeElement).toBe(elsewhere);
	});
});

describe('Lightbox — Tab trap', () => {
	it('wraps forward off the last focusable', () => {
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		const next = root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!;
		next.focus();

		expect(press('Tab')).toBe(true);
		expect(document.activeElement).toBe(closeButton());
	});

	it('wraps backward off the first focusable', () => {
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		closeButton().focus();

		expect(press('Tab', { shiftKey: true })).toBe(true);
		expect(document.activeElement).toBe(
			root().querySelector<HTMLButtonElement>('.lightbox-nav.next')
		);
	});

	it('pulls focus back to the leading edge when it has escaped the viewer', () => {
		// The exact target matters: `nextTrapTarget` returns the FIRST focusable
		// on a forward Tab from outside. Asserting only "somewhere inside" would
		// pass for an implementation that focused the root instead.
		const outside = appRoot.appendChild(document.createElement('button'));
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		outside.focus();

		expect(press('Tab')).toBe(true);
		expect(document.activeElement).toBe(closeButton());

		// ...and the trailing edge on a back Tab from outside.
		outside.focus();
		expect(press('Tab', { shiftKey: true })).toBe(true);
		expect(document.activeElement).toBe(
			root().querySelector('.lightbox-nav.next')
		);
	});

	it('leaves a mid-cycle Tab to the browser', () => {
		// Only the wrap is the trap's business; preventing every Tab would break
		// the natural order inside the viewer.
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		closeButton().focus();
		expect(press('Tab')).toBe(false);
	});
});

describe('Lightbox — Escape ownership', () => {
	it('does NOT close on a raw window keydown: the stack is the sole owner', () => {
		// The local Escape branch was DELETED, not gated. It ignored
		// `defaultPrevented`, so keeping it alongside the stack gave Escape two
		// owners and let one press collapse two layers.
		const onClose = vi.fn();
		mountViewer({ onClose });

		expect(press('Escape')).toBe(false);
		expect(onClose).not.toHaveBeenCalled();

		expect(runTopEscape()).toBe(true);
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('outranks the menu layer', () => {
		// A menu is inert behind the viewer, so the viewer must win the key.
		//
		// The menu handler is registered AFTER the viewer deliberately:
		// `escapeStack` breaks EQUAL-priority ties toward the most recently
		// registered handler, so registering it first would let the viewer win on
		// the tie-break alone and the test would pass even at `menu` priority.
		// Registered last, only a strictly higher priority can win.
		const onClose = vi.fn();
		mountViewer({ onClose });
		const menuClose = vi.fn(() => true);
		pushEscapeHandler(menuClose, ESCAPE_PRIORITY.menu);

		expect(runTopEscape()).toBe(true);
		expect(onClose).toHaveBeenCalledTimes(1);
		expect(menuClose).not.toHaveBeenCalled();
	});

	it('declines Escape once it is no longer the frontmost lease', () => {
		// The gate that the two-viewer case cannot isolate: with two viewers,
		// registration order and lease order agree, so `escapeStack`'s
		// newest-wins tie-break would pick the front one even with the gate
		// deleted. Taking a lease DIRECTLY puts something above the viewer whose
		// escape handler is NOT on the stack at all, so lease order and
		// registration order finally disagree — the viewer must decline, and with
		// nothing else registered the whole stack must decline with it.
		const onClose = vi.fn();
		mountViewer({ onClose });
		const above = document.body.appendChild(document.createElement('div'));
		const lease = acquire(above);

		expect(runTopEscape()).toBe(false);
		expect(onClose).not.toHaveBeenCalled();

		// ...and it takes the key again the moment it is frontmost once more.
		lease.release();
		expect(runTopEscape()).toBe(true);
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('ignores a key another control already handled', () => {
		// The `defaultPrevented` early return. Without it the viewer would page
		// on an arrow a control underneath (or a layer above) has already
		// consumed — the exact two-owners-one-press shape the deleted local
		// Escape branch had.
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		expect(imageSrc()).toContain(IMG_A);

		const event = new KeyboardEvent('keydown', {
			key: 'ArrowRight',
			cancelable: true,
			bubbles: true,
		});
		event.preventDefault();
		window.dispatchEvent(event);
		flushSync();
		expect(imageSrc()).toContain(IMG_A);
	});

	it('closes through a ROUTE-SHAPED driver, and closes exactly one layer', () => {
		// The integration the component depends on, exercised in the shape the
		// two route handlers actually have: bail on a foreign modal, else run the
		// stack and preventDefault. Calling `runTopEscape()` directly (as the
		// tests above do) skips the `hasForeignEscapeOwner()` guard, which is the
		// half that would silently swallow the viewer's Escape if the viewer were
		// not excluded from that selector.
		const paneClose = vi.fn(() => true);
		pushEscapeHandler(paneClose, ESCAPE_PRIORITY.pane);
		const onClose = vi.fn();
		mountViewer({ onClose });

		const routeDriver = (e: KeyboardEvent) => {
			if (e.key !== 'Escape') return;
			if (hasForeignEscapeOwner()) return;
			if (runTopEscape()) e.preventDefault();
		};
		window.addEventListener('keydown', routeDriver);
		try {
			expect(press('Escape')).toBe(true);
		} finally {
			window.removeEventListener('keydown', routeDriver);
		}

		expect(onClose).toHaveBeenCalledTimes(1);
		// ONE layer: the pane underneath must not also close on the same press.
		expect(paneClose).not.toHaveBeenCalled();
	});

	it('unregisters on close, so a later Escape reaches the layer beneath', () => {
		const paneClose = vi.fn(() => true);
		pushEscapeHandler(paneClose, ESCAPE_PRIORITY.pane);
		const app = mountViewer({ onClose: () => {} });

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(runTopEscape()).toBe(true);
		expect(paneClose).toHaveBeenCalledTimes(1);
	});
});

describe('Lightbox — only the frontmost viewer acts', () => {
	function mountTwo() {
		const onCloseBack = vi.fn();
		const onCloseFront = vi.fn();
		mountViewer({
			onClose: onCloseBack,
			images: [
				{ id: IMG_A, alt: 'back-first' },
				{ id: IMG_B, alt: 'back-second' },
			],
		});
		const back = root();
		mountViewer({
			onClose: onCloseFront,
			images: [
				{ id: IMG_A, alt: 'front-first' },
				{ id: IMG_B, alt: 'front-second' },
			],
		});
		const front = root();
		expect(back).not.toBe(front);
		return { back, front, onCloseBack, onCloseFront };
	}

	it('closes exactly the front viewer on one Escape', () => {
		// HONEST SCOPE: this asserts the user-visible contract, but it cannot
		// isolate the component's `isViewerFrontmost` gate. `escapeStack` breaks
		// equal-priority ties toward the most recently registered handler, and
		// registration order and lease order are the same thing here (both are
		// mount order), so the press would land on the front viewer even with the
		// gate removed — verified by mutation. The gate is kept because it makes
		// the ownership rule LOCAL rather than a consequence of another module's
		// tie-break, and because the arrow / Tab gates on the same predicate ARE
		// load-bearing (the two tests below fail without them).
		const { onCloseBack, onCloseFront } = mountTwo();
		expect(runTopEscape()).toBe(true);
		expect(onCloseFront).toHaveBeenCalledTimes(1);
		expect(onCloseBack).not.toHaveBeenCalled();
	});

	it('pages only the front viewer on an arrow key', () => {
		const { back, front } = mountTwo();
		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc(front)).toContain(IMG_B);
		expect(imageSrc(back)).toContain(IMG_A);
	});

	it('does not let the BACK viewer steal focus on Tab', () => {
		// The sharp edge: `nextTrapTarget` deliberately pulls out-of-container
		// focus INWARD, so a background viewer running the trap would drag focus
		// out of the viewer in front of it. Handlers are global; the frontmost
		// check is what stops it.
		const { back, front } = mountTwo();
		expect(front.contains(document.activeElement)).toBe(true);

		press('Tab');
		expect(back.contains(document.activeElement)).toBe(false);
		expect(front.contains(document.activeElement)).toBe(true);
	});
});

describe('Lightbox — a native modal opened OVER the viewer', () => {
	/**
	 * jsdom throws on the `:modal` pseudo-class, so emulate an engine that
	 * supports it — the shape `viewerBackdrop.svelte.test.ts` uses. Both probes
	 * the module makes (`querySelectorAll` and `Element.matches`) are covered.
	 */
	function mockOpenModals(modals: Element[]): void {
		const realQueryAll = document.querySelectorAll.bind(document);
		vi.spyOn(document, 'querySelectorAll').mockImplementation((selector: string) => {
			if (selector !== 'dialog:modal') return realQueryAll(selector);
			return Array.from(realQueryAll('dialog')).filter((d) =>
				modals.includes(d)
			) as unknown as NodeListOf<Element>;
		});
		const realMatches = Element.prototype.matches;
		vi.spyOn(Element.prototype, 'matches').mockImplementation(function (
			this: Element,
			selector: string
		) {
			if (selector !== 'dialog:modal') return realMatches.call(this, selector);
			return realMatches.call(this, 'dialog') && modals.includes(this);
		});
	}

	it('stops trapping Tab while a showModal() dialog is above it', () => {
		// The frontmost LEASE is not the frontmost SURFACE: a `showModal()` dialog
		// lives in the top layer, above any body-portaled viewer, and the manager
		// deliberately leaves it OUT of the inert set so it stays operable. If the
		// viewer kept trapping, `nextTrapTarget` would pull focus out of that
		// dialog and back into the viewer underneath it — the inward-redirect
		// hazard one layer up. Reachable today: the app shell's `?` shortcut opens
		// the Keyboard Shortcuts modal while a viewer is up (TASK-2430 stops the
		// shortcut; this stops the viewer fighting the result either way).
		// The emulation goes in BEFORE the mount: the manager probes `:modal` on
		// its first reconcile, and jsdom's throw makes it cache "unsupported" for
		// the rest of the module's life. Mocking afterwards would be ignored — and
		// the test would then pass for the wrong reason.
		const dialog = document.body.appendChild(document.createElement('dialog'));
		const inDialog = dialog.appendChild(document.createElement('button'));
		mockOpenModals([dialog]);
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		inDialog.focus();

		expect(press('Tab')).toBe(false);
		expect(document.activeElement).toBe(inDialog);
	});

	it('stops paging on arrows while a showModal() dialog is above it', () => {
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([dialog]);
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});

		expect(press('ArrowRight')).toBe(false);
		expect(imageSrc()).toContain(IMG_A);
	});

	it('resumes once the dialog closes', () => {
		// The stand-down must be conditional, not a permanent disable.
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([dialog]);
		mountViewer({
			images: [
				{ id: IMG_A, alt: 'first' },
				{ id: IMG_B, alt: 'second' },
			],
		});
		expect(press('ArrowRight')).toBe(false);

		mockOpenModals([]);
		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);
	});
});

describe('Lightbox — background inertness (delegated)', () => {
	it('asks the manager to inert the app shell, and releases it on close', () => {
		// jsdom has no inertness semantics, so this asserts the manager was
		// DRIVEN — the attribute on the right body children, gone again after
		// close. Whether the background is really unreachable is TASK-2436's.
		const app = mountViewer();
		expect(inertBodyChildren()).toEqual([appRoot]);

		unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
		flushSync();
		expect(inertBodyChildren()).toEqual([]);
	});

	it('keeps the app inert while a SECOND viewer is still open', () => {
		// The refcount is the manager's, but the release ordering is this
		// component's: releasing without the lease stack would un-inert the app
		// behind a viewer that is still up.
		const first = mountViewer();
		const back = root();
		mountViewer();
		// Only the FRONT viewer stays interactive: the app shell AND the viewer
		// beneath it are both inert, which is the stacking the lease order buys.
		expect(inertBodyChildren()).toEqual([appRoot, back]);

		unmount(mounted.splice(mounted.indexOf(first), 1)[0]);
		flushSync();
		expect(inertBodyChildren()).toEqual([appRoot]);
	});

	it('hands focus to the viewer beneath instead of restoring its own invoker', () => {
		// The `stackEmpty` half of the teardown: with a viewer still open,
		// restoring the invoker would yank focus out of the surface the user is
		// actually looking at. The manager owns the handoff; this component's job
		// is to STAND DOWN.
		//
		// HONEST SCOPE, again: the two defences overlap. The manager's handoff
		// has already moved focus INTO the viewer beneath by the time the restore
		// would run, so `restoreFocus`'s own "someone else owns focus" guard
		// declines even with the `stackEmpty` gate removed — verified by
		// mutation. What IS asserted is the contract that matters: on closing the
		// front viewer, focus lands in the one beneath and never on the closed
		// viewer's invoker.
		const invokerBack = appRoot.appendChild(document.createElement('button'));
		const invokerFront = appRoot.appendChild(document.createElement('button'));
		mountViewer({ invoker: invokerBack });
		const back = root();
		const front = mountViewer({ invoker: invokerFront });

		unmount(mounted.splice(mounted.indexOf(front), 1)[0]);
		flushSync();
		expect(document.activeElement).not.toBe(invokerFront);
		expect(back.contains(document.activeElement)).toBe(true);
	});
});
