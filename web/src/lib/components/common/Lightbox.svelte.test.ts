import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import Lightbox from './Lightbox.svelte';
import type { LightboxImage } from '$lib/attachments/events';
import {
	acquire,
	hasForeignEscapeOwner,
	isBlockedByModal,
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

const IMG_C = 'cccccccc-3333-4333-8333-cccccccccccc';

/**
 * A member of the viewer's set. `mime_type` defaults to an ALLOWLISTED type,
 * because the gate fails closed: a record without one is not viewable, so a
 * null default would silently empty the viewer for every case in this file
 * that is about something else. Pass `null` explicitly to test the unresolved
 * case.
 */
function image(id: string, alt: string, mime: string | null = 'image/png'): LightboxImage {
	return {
		id,
		alt,
		filename: null,
		mime_type: mime,
		size_bytes: null,
		width: null,
		height: null,
	};
}

interface Props {
	images: LightboxImage[];
	index?: number;
	wsSlug: string;
	onClose: () => void;
	invoker?: HTMLElement | null;
}

// Reactive props for the capture-at-open cases ($state may only initialize a
// declaration, hence top level).
const liveProps = $state<Props>({
	images: [image(IMG_A, 'a diagram')],
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
			images: [image(IMG_A, 'a diagram')],
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

/** The `transform` inline style on the viewer's image (empty before it mounts). */
function transformOf(scope: HTMLElement = root()): string {
	return scope.querySelector<HTMLImageElement>('.lightbox-image')?.style.transform ?? '';
}

/**
 * The `scale(...)` factor currently on the image, or NaN if there is none.
 *
 * jsdom lays nothing out, so the geometry the zoom module reads is all zeros —
 * but `maxScale` FALLS BACK to `4` on an unusable geometry (a small image still
 * needs a zoom range), so `zoomTo` still moves off fit here. That is what makes
 * the scale observable in jsdom at all; the pan bounds, which DO need real
 * geometry, are exercised in the browser suite (TASK-2436) and proven in
 * `zoom.test.ts`.
 */
function scaleOf(scope: HTMLElement = root()): number {
	const m = /scale\(([-\d.]+)\)/.exec(transformOf(scope));
	return m ? Number(m[1]) : NaN;
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
		images: [image(IMG_A, 'a diagram')],
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
	// The zoom resize test installs a driving `ResizeObserver` via `vi.stubGlobal`;
	// restore the setup-file's global inert shim so it can't leak into a later test.
	vi.unstubAllGlobals();
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
		mountViewer({ images: [image(IMG_A, '')] });
		expect(root().getAttribute('aria-label')).toBe('Attachment viewer');
	});

	it('gives every control a real accessible name, not a glyph', () => {
		// The button text is "✕" / "‹" / "›", and `title` does not win over
		// element content for the accessible name — so without these the controls
		// are announced as punctuation, and TASK-2436's browser suite (which
		// addresses surfaces BY NAME) would have nothing to target.
		mountViewer({
			images: [
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
			],
		});
		expect(root().getAttribute('aria-label')).toBe('first');
		root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!.click();
		flushSync();
		expect(root().getAttribute('aria-label')).toBe('second');
	});
});

describe('Lightbox — the last-mile open gate (TASK-2431)', () => {
	/**
	 * The producers filter their own sets, and these do not replace that. They
	 * cover the case a producer's filter structurally cannot: the set is
	 * captured at open, and ←/→ page through it for as long as the viewer is
	 * up. Anything that arrives in the array — a stale capture, a future
	 * producer that forgets — must still be unreachable frame by frame.
	 */
	function shown(): string {
		return root().querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt') ?? '';
	}

	it('never shows a known non-allowlisted type, even when asked to open ON it', () => {
		mountViewer({
			images: [image(IMG_A, 'png', 'image/png'), image(IMG_B, 'svg', 'image/svg+xml')],
			index: 1,
		});

		// The requested image is refused, so the viewer opens on what is left
		// rather than on the SVG — and with one member, there is no ←/→ at all.
		expect(shown()).toBe('png');
		expect(imageSrc()).toContain(IMG_A);
		expect(root().querySelector('.lightbox-counter')).toBeNull();
		expect(root().querySelector('.lightbox-nav')).toBeNull();
	});

	it('cannot be paged onto one with ←/→', () => {
		mountViewer({
			images: [
				image(IMG_A, 'png', 'image/png'),
				image(IMG_B, 'svg', 'image/svg+xml'),
				image(IMG_C, 'jpeg', 'image/jpeg'),
			],
		});

		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		const seen = [shown()];
		for (let i = 0; i < 3; i++) {
			press('ArrowRight');
			seen.push(shown());
		}
		press('ArrowLeft');
		seen.push(shown());

		expect(seen).toEqual(['png', 'jpeg', 'png', 'jpeg', 'png']);
		expect(seen).not.toContain('svg');
	});

	it('keeps the requested image when EARLIER members are filtered out', () => {
		// The requested image is at position 1 in the given set and position 0
		// in the filtered one. Carrying the NUMBER across — even clamped to the
		// filtered length, which is the plausible wrong version — lands on the
		// image after it. Only resolving by id opens what was asked for.
		mountViewer({
			images: [
				image(IMG_B, 'svg', 'image/svg+xml'),
				image(IMG_A, 'png', 'image/png'),
				image(IMG_C, 'jpeg', 'image/jpeg'),
			],
			index: 1,
		});

		expect(shown()).toBe('png');
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
	});

	it('REFUSES an image whose type is not resolved', () => {
		// This test previously asserted the opposite, on the reasoning that an
		// inline image's probe is often unasked at click time. That reasoning
		// belongs to the PRODUCER — which can wait for the probe — and asserting
		// it HERE pinned open the exact hole the task exists to close: an emitter
		// could hand over `[safe, unresolved]` and the user could arrow onto the
		// unresolved one. This is the last thing between a set and a rendered
		// image; "not yet known" is not evidence that a file is a PNG.
		mountViewer({ images: [image(IMG_A, 'unprobed', null)] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
	});

	it('cannot be paged onto an unresolved sibling', () => {
		mountViewer({
			images: [image(IMG_A, 'png'), image(IMG_B, 'unprobed', null), image(IMG_C, 'jpeg', 'image/jpeg')],
		});

		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
		const seen = [shown()];
		for (let i = 0; i < 3; i++) {
			press('ArrowRight');
			seen.push(shown());
		}
		expect(seen).toEqual(['png', 'jpeg', 'png', 'jpeg']);
		expect(seen).not.toContain('unprobed');
	});

	it('shows nothing at all rather than one refused image', () => {
		mountViewer({ images: [image(IMG_B, 'svg', 'image/svg+xml')] });

		expect(root().querySelector('.lightbox-image')).toBeNull();
		// Still a real dialog with a way out — the failure mode is an empty
		// viewer, never a rendered one.
		expect(closeButton()).not.toBeNull();
		// And the arrows cannot divide by an empty set.
		press('ArrowRight');
		press('ArrowLeft');
		expect(root().querySelector('.lightbox-image')).toBeNull();
	});
});

describe('Lightbox — the set changing under an OPEN viewer (TASK-2431)', () => {
	/**
	 * The producers hand over a set once and the viewer pages through it for as
	 * long as it is up, so "was safe when the list was built" is not the claim
	 * that has to hold — "is safe on the frame being shown" is. These drive the
	 * live props (`liveProps`, the reactive object the file already uses for the
	 * capture-at-open cases) rather than remounting, which is the only way to
	 * reach a set that changes under a viewer that is already open.
	 */
	function shown(): string {
		return root().querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt') ?? '';
	}

	function mountLive() {
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		return app;
	}

	it('drops a record whose MIME resolves to something unsafe AFTER open', () => {
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg')];
		mountLive();
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		// The late answer arrives: what was believed safe is not. A set captured
		// once would keep paging onto it.
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg', 'image/svg+xml')];
		flushSync();

		expect(shown()).toBe('png');
		expect(root().querySelector('.lightbox-counter')).toBeNull();
		press('ArrowRight');
		expect(shown()).toBe('png');
	});

	it('keeps showing a real image when the one under it is removed', () => {
		liveProps.images = [image(IMG_A, 'png'), image(IMG_C, 'jpeg', 'image/jpeg')];
		mountLive();
		press('ArrowRight');
		expect(shown()).toBe('jpeg');

		// The set shrinks beneath the position the user navigated to — a delete
		// from another surface, a reload. The index must clamp, not blank out or
		// render `undefined`.
		liveProps.images = [image(IMG_A, 'png')];
		flushSync();

		expect(shown()).toBe('png');
		expect(imageSrc()).toContain(IMG_A);
	});

	it('cannot be navigated onto an unsafe entry ADDED after open', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();

		liveProps.images = [
			image(IMG_A, 'png'),
			image(IMG_B, 'svg', 'image/svg+xml'),
			image(IMG_C, 'unprobed', null),
		];
		flushSync();

		// Both additions are refused, so the viewer is still single-image.
		expect(root().querySelector('.lightbox-counter')).toBeNull();
		press('ArrowRight');
		press('ArrowLeft');
		expect(shown()).toBe('png');
	});

	it('empties rather than showing an unsafe replacement', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		expect(shown()).toBe('png');

		liveProps.images = [image(IMG_B, 'svg', 'image/svg+xml')];
		flushSync();

		expect(root().querySelector('.lightbox-image')).toBeNull();
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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

	it('BUG-2441: a LATER window listener stands down even though the lease is already gone', () => {
		// The bug reproduced honestly in jsdom, which needs the one thing the
		// other cases in this file leave out: an `onClose` that actually TEARS THE
		// VIEWER DOWN, synchronously, inside the driver's handler — which is what
		// Svelte does in the browser, and what releases the lease mid-dispatch.
		// With a `vi.fn()` onClose the lease survives the press and the sheet's
		// live guard answers correctly all by itself; that is exactly why the
		// unit suite missed this and TASK-2436's browser suite caught it.
		//
		// The sheet stand-in is deliberately shaped like `DockedSheet` /
		// `BottomSheet`: a `window` keydown listener registered AFTER the driver,
		// guarded by `isBlockedByModal(ownEl, event)`.
		const app = mountViewer({
			onClose: () => {
				unmount(mounted.splice(mounted.indexOf(app), 1)[0]);
				flushSync();
			},
		});

		const sheetEl = appRoot.appendChild(document.createElement('div'));
		const sheetClose = vi.fn();

		const routeDriver = (e: KeyboardEvent) => {
			if (e.key !== 'Escape') return;
			if (hasForeignEscapeOwner()) return;
			if (runTopEscape(e)) e.preventDefault();
		};
		const sheetOwner = (e: KeyboardEvent) => {
			if (e.key !== 'Escape') return;
			if (isBlockedByModal(sheetEl, e)) return;
			sheetClose();
		};
		window.addEventListener('keydown', routeDriver);
		window.addEventListener('keydown', sheetOwner);
		try {
			press('Escape');
			// The viewer is gone — so the sheet's LIVE guard now answers "nothing
			// in front of you". Asserted, because it is the premise of the bug: if
			// this were false the test would prove nothing.
			expect(roots()).toHaveLength(0);
			expect(isBlockedByModal(sheetEl)).toBe(false);
			// ...and yet the sheet did not act, because the press was already spent.
			expect(sheetClose).not.toHaveBeenCalled();

			// EMPTY-STACK REGRESSION, on the same wiring: the NEXT press is the
			// sheet's. The fix must not leave it permanently deaf.
			press('Escape');
			expect(sheetClose).toHaveBeenCalledTimes(1);
		} finally {
			window.removeEventListener('keydown', routeDriver);
			window.removeEventListener('keydown', sheetOwner);
		}
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
				image(IMG_A, 'back-first'),
				image(IMG_B, 'back-second'),
			],
		});
		const back = root();
		mountViewer({
			onClose: onCloseFront,
			images: [
				image(IMG_A, 'front-first'),
				image(IMG_B, 'front-second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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
				image(IMG_A, 'first'),
				image(IMG_B, 'second'),
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

// TASK-2455 — the zoom transform wired into the viewer (PLAN-2392 phase 3b).
//
// jsdom has no layout, so these assert the WIRING and the ARBITRATION, not the
// pan geometry: which key does what, that the modifier / gate rules hold, and
// that the transform resets when the shown image changes. The arithmetic those
// keys drive is proven browser-free in `zoom.test.ts`; the pixel geometry is
// TASK-2436's browser suite.

describe('Lightbox — zoom keys', () => {
	it('zooms IN on + (identity → one step), centred', () => {
		mountViewer();
		expect(scaleOf()).toBe(1);
		expect(transformOf()).toBe('translate(0px, 0px) scale(1)');

		expect(press('+')).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
		// A centre-anchored zoom leaves the pan at zero.
		expect(transformOf()).toContain('translate(0px, 0px)');
	});

	it('accepts a bare = as zoom-IN (the unshifted key on most layouts)', () => {
		mountViewer();
		expect(press('=')).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('accepts the numpad plus — matched on .key, so .code is irrelevant', () => {
		// The numpad's `+` reports `key === '+'` (only `code` says `NumpadAdd`), so
		// a code-based match would silently miss it. This pins the key-based one.
		mountViewer();
		const event = new KeyboardEvent('keydown', {
			key: '+',
			code: 'NumpadAdd',
			cancelable: true,
			bubbles: true,
		});
		window.dispatchEvent(event);
		flushSync();
		expect(event.defaultPrevented).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('zooms OUT on - (back toward fit)', () => {
		mountViewer();
		press('+');
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25 * 1.25);
		expect(press('-')).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('does not zoom below fit: - at fit is a no-op', () => {
		// The floor is FIT (=1); `clampScale` holds it, so `-` at fit changes
		// nothing rather than shrinking the image past its fitted box.
		mountViewer();
		expect(press('-')).toBe(true);
		expect(scaleOf()).toBe(1);
	});

	it('resets to fit, centred, on 0', () => {
		mountViewer();
		press('+');
		press('+');
		expect(scaleOf()).toBeGreaterThan(1);
		expect(press('0')).toBe(true);
		expect(scaleOf()).toBe(1);
		expect(transformOf()).toBe('translate(0px, 0px) scale(1)');
	});

	it('IGNORES + when Alt is held — and does NOT preventDefault', () => {
		// Alt-modified is the OS's, not ours. Acting would be wrong; ALSO
		// preventing default would cancel the OS shortcut even though we declined.
		mountViewer();
		expect(press('+', { altKey: true })).toBe(false);
		expect(scaleOf()).toBe(1);
	});

	it('IGNORES - when Ctrl is held (browser page-zoom-out) — and does NOT preventDefault', () => {
		// The whole reason the modifier rule is a contract: this listens on
		// `window`, so swallowing Ctrl+- would break page-zoom from every surface
		// while a viewer is open.
		mountViewer();
		expect(press('-', { ctrlKey: true })).toBe(false);
		expect(scaleOf()).toBe(1);
	});

	it('IGNORES 0 when Cmd is held (browser reset-zoom) — and does NOT preventDefault', () => {
		mountViewer();
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);
		// Cmd+0 is the browser's reset — leave the viewer's own zoom untouched and
		// let the event through.
		expect(press('0', { metaKey: true })).toBe(false);
		expect(scaleOf()).toBeCloseTo(1.25);
	});
});

describe('Lightbox — zoom keys are ARBITRATED, not just handled', () => {
	// A `+`/`-`/`0` branch placed BEFORE the existing gates would pass every zoom
	// test above while stealing a press another owner or a layer is due. Each gate
	// gets its own leg with a positive control.

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

	it('declines a key another control already handled (defaultPrevented) — control: an un-consumed one acts', () => {
		mountViewer();
		const consumed = new KeyboardEvent('keydown', { key: '+', cancelable: true, bubbles: true });
		consumed.preventDefault();
		window.dispatchEvent(consumed);
		flushSync();
		expect(scaleOf()).toBe(1);

		// Positive control: the SAME key, not pre-consumed, does zoom.
		expect(press('+')).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('zooms only the FRONTMOST viewer, never the one behind it', () => {
		mountViewer();
		const back = root();
		mountViewer();
		const front = root();
		expect(back).not.toBe(front);

		expect(press('+')).toBe(true);
		expect(scaleOf(front)).toBeCloseTo(1.25);
		expect(scaleOf(back)).toBe(1);
	});

	it('stands down while a showModal() dialog is above it, and resumes after it closes', () => {
		// The emulation goes in BEFORE the mount: the manager probes `:modal` on
		// its first reconcile and jsdom's throw makes it cache "unsupported" for
		// the module's life, so mocking afterwards would be ignored.
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([dialog]);
		mountViewer();

		expect(press('+')).toBe(false);
		expect(scaleOf()).toBe(1);

		mockOpenModals([]);
		expect(press('+')).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});
});

describe('Lightbox — the transform resets when the shown image changes (TASK-2455)', () => {
	function mountLive() {
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		return app;
	}

	it('resets on arrow navigation', () => {
		mountViewer({ images: [image(IMG_A, 'first'), image(IMG_B, 'second')] });
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);

		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);
		expect(scaleOf()).toBe(1);
	});

	it('resets when the set shrinks under `current` so a DIFFERENT image is shown', () => {
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg')];
		mountLive();
		press('ArrowRight');
		expect(imageSrc()).toContain(IMG_B);
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);

		// B's MIME resolves unsafe → it drops out → the shown image becomes A →
		// the transform is back at fit.
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg', 'image/svg+xml')];
		flushSync();
		expect(imageSrc()).toContain(IMG_A);
		expect(scaleOf()).toBe(1);
	});

	it('does NOT reset (and keeps the zoom) when an image is added AFTER the shown one', () => {
		// The reset keys on the shown image's identity, not on the array changing:
		// growing the set past `current` leaves the shown image — and its zoom —
		// alone.
		liveProps.images = [image(IMG_A, 'only')];
		mountLive();
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);

		liveProps.images = [image(IMG_A, 'only'), image(IMG_C, 'second', 'image/jpeg')];
		flushSync();
		expect(imageSrc()).toContain(IMG_A);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('a reset does not wedge the scheduler: unrelated reactivity still flushes AFTER a navigate', () => {
		// Acceptance #3. The symptom of an $effect writing a $state it also reads is
		// that OTHER reactivity near it silently strands. So the test must FIRE the
		// reset effect (the effect that writes `zoom`) and THEN prove an unrelated
		// derived still recomputes. Navigation is what fires it; growing the set
		// without navigating leaves `img.id` unchanged, so the reset effect never
		// runs and could not have wedged anything — a weaker test that a
		// self-dependent reset would still pass.
		liveProps.images = [image(IMG_A, 'first'), image(IMG_B, 'second')];
		mountLive();
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);

		// Navigation fires the reset effect (img A → B), taking the transform back
		// to fit — the write a self-dependent effect would abort mid-flush.
		expect(press('ArrowRight')).toBe(true);
		expect(scaleOf()).toBe(1);

		// UNRELATED to the reset: grow the set past the shown image. Its counter
		// derived must still recompute — a wedged scheduler would strand it at the
		// pre-update value.
		liveProps.images = [
			image(IMG_A, 'first'),
			image(IMG_B, 'second'),
			image(IMG_C, 'third', 'image/jpeg'),
		];
		flushSync();
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('2 / 3');
	});
});

describe('Lightbox — resize re-clamps SCALE first, then pan (TASK-2455)', () => {
	it('pulls a now-out-of-range scale down when the stage grows and lowers the ceiling', () => {
		// jsdom fires no ResizeObserver, so DRIVE one by hand and mock the geometry
		// the component reads. The scenario is the headline one: enlarging the
		// window lowers `actualScale` and with it `maxScale`, stranding a
		// previously-valid scale above the new ceiling. Only `clampState` (scale
		// THEN pan) fixes it — `clampPan` alone would leave the scale untouched.
		let roCallback: ResizeObserverCallback | null = null;
		let observed: Element | null = null;
		vi.stubGlobal(
			'ResizeObserver',
			class {
				constructor(cb: ResizeObserverCallback) {
					roCallback = cb;
				}
				observe(target: Element) {
					observed = target;
				}
				unobserve() {}
				disconnect() {}
			}
		);

		mountViewer();
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		const stage = root().querySelector<HTMLElement>('.lightbox-stage')!;
		// It observes the STAGE (whose size IS the viewport-dependent geometry),
		// not the image or the backdrop.
		expect(observed).toBe(stage);

		// A big bitmap fitted into a small box: actualScale = 1000/100 = 10, so the
		// ceiling starts at 40 — lots of headroom to zoom into.
		let fitted = 100;
		Object.defineProperty(image, 'offsetWidth', { configurable: true, get: () => fitted });
		Object.defineProperty(image, 'offsetHeight', { configurable: true, get: () => fitted });
		Object.defineProperty(image, 'naturalWidth', { configurable: true, get: () => 1000 });
		Object.defineProperty(image, 'naturalHeight', { configurable: true, get: () => 1000 });
		Object.defineProperty(stage, 'clientWidth', { configurable: true, get: () => fitted });
		Object.defineProperty(stage, 'clientHeight', { configurable: true, get: () => fitted });

		// Zoom well past 4 (1.25^8 ≈ 5.96), which the ceiling of 40 permits.
		for (let i = 0; i < 8; i++) press('+');
		expect(scaleOf()).toBeGreaterThan(5);

		// The stage grows until the image fits 1:1: actualScale → 1, ceiling → 4.
		// The current scale (~5.96) is now above it.
		fitted = 1000;
		expect(roCallback).not.toBeNull();
		roCallback!([] as unknown as ResizeObserverEntry[], null as unknown as ResizeObserver);
		flushSync();

		// Re-clamped to the new ceiling — proof the SCALE was clamped, not only pan.
		expect(scaleOf()).toBeCloseTo(4);
	});
});

describe('Lightbox — + / - anchor at the stage centre (TASK-2455)', () => {
	it('keeps the pan at ZERO when zooming a centred image that overflows the stage', () => {
		// The only leg that pins the ANCHOR. Everywhere else jsdom's all-zero
		// geometry forces `stageCenter` to (0,0) and the pan to 0, so an off-centre
		// anchor is invisible. Here the (mocked) geometry lets the zoomed image
		// OVERFLOW the stage, freeing the pan — and a centre-anchored zoom of an
		// already-centred image must still leave it at (0,0). A top-left (or any
		// non-centre) anchor would push a non-zero translate and fail this.
		mountViewer();
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		const stage = root().querySelector<HTMLElement>('.lightbox-stage')!;
		Object.defineProperty(image, 'offsetWidth', { configurable: true, get: () => 400 });
		Object.defineProperty(image, 'offsetHeight', { configurable: true, get: () => 400 });
		Object.defineProperty(image, 'naturalWidth', { configurable: true, get: () => 2000 });
		Object.defineProperty(image, 'naturalHeight', { configurable: true, get: () => 2000 });
		Object.defineProperty(stage, 'clientWidth', { configurable: true, get: () => 1000 });
		Object.defineProperty(stage, 'clientHeight', { configurable: true, get: () => 1000 });

		// 400 × 1.25^5 ≈ 1220 > 1000, so the image overflows and pan is unclamped.
		for (let i = 0; i < 5; i++) press('+');
		expect(scaleOf()).toBeGreaterThan(2.5);
		expect(transformOf()).toContain('translate(0px, 0px)');
	});
});

// TASK-2456 — focus handoff when a focused viewer control disappears.
//
// aria-modal promises focus never leaves the surface, but a conditionally
// rendered nav button that had focus drops focus to <body> behind the inerted
// app when the set shrinks to one. These are SAME-INSTANCE (a remount would hide
// the whole thing) and assert focus lands on a control INSIDE the viewer.

describe('Lightbox — focus handoff on a shrinking set (TASK-2456)', () => {
	function mountLive() {
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		return app;
	}

	it('CONTROL: a removed focused control drops focus to <body> in this environment', () => {
		// The defect reproduced honestly, so the positive test below is not
		// vacuous: removing a focused element strands focus on <body> here just as
		// it does in a real engine. (Done on a detached fixture so it does not fight
		// Svelte for ownership of a live viewer node.)
		const fixture = appRoot.appendChild(document.createElement('div'));
		const btn = fixture.appendChild(document.createElement('button'));
		btn.focus();
		expect(document.activeElement).toBe(btn);
		btn.remove();
		expect(document.activeElement).toBe(document.body);
	});

	it('hands focus to the close button when the focused Next control is removed by a shrink', () => {
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'jpeg', 'image/jpeg')];
		mountLive();
		const before = root();
		const nextBtn = before.querySelector<HTMLButtonElement>('.lightbox-nav.next')!;
		nextBtn.focus();
		// Precondition, so the assertion is about a real handoff and not a viewer
		// that happened to already have focus on the close button.
		expect(document.activeElement).toBe(nextBtn);

		// The set shrinks to one: the nav buttons unmount. Without the handoff,
		// focus would now be on <body>, behind everything the viewer inerted.
		liveProps.images = [image(IMG_A, 'png')];
		flushSync();

		// SAME instance — this is the same-instance property the acceptance names;
		// a keyed remount would trivially start focused and false-green the rest.
		expect(root()).toBe(before);
		expect(before.querySelector('.lightbox-nav')).toBeNull();
		// INSIDE the viewer — not merely "not on body".
		expect(before.contains(document.activeElement)).toBe(true);
		expect(document.activeElement).toBe(closeButton(before));
		expect(document.activeElement).not.toBe(document.body);
	});

	it('does not steal focus when it is already on a surviving control', () => {
		// A shrink while focus is on the CLOSE button (which survives) must leave
		// it there — the handoff repairs a lost focus, it does not re-home a fine one.
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'jpeg', 'image/jpeg')];
		mountLive();
		closeButton().focus();
		expect(document.activeElement).toBe(closeButton());

		liveProps.images = [image(IMG_A, 'png')];
		flushSync();
		expect(document.activeElement).toBe(closeButton());
	});

	it('a BACKGROUND viewer does not grab focus when ITS OWN set shrinks', () => {
		// The isViewerFrontmost guard on the repair effect. With a second viewer
		// stacked over the first, the front one owns focus; a shrink in the BACK
		// viewer must not pull focus out of the front into the inert background.
		// Without the guard the reactive repair would see "focus is not inside ME"
		// and yank it to the back viewer's fallback.
		liveProps.images = [image(IMG_A, 'back-a'), image(IMG_B, 'back-b', 'image/jpeg')];
		const backApp = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(backApp);
		flushSync();
		const back = root();
		mountViewer({ images: [image(IMG_A, 'front-a'), image(IMG_B, 'front-b')] });
		const front = root();
		expect(front).not.toBe(back);
		expect(front.contains(document.activeElement)).toBe(true);

		liveProps.images = [image(IMG_A, 'back-a')];
		flushSync();

		expect(front.contains(document.activeElement)).toBe(true);
		expect(back.contains(document.activeElement)).toBe(false);
	});
});

describe('Lightbox — Tab still cycles at maximum zoom (TASK-2456)', () => {
	it('wraps forward off the last control to the close button even at the zoom ceiling', () => {
		// At max zoom the transformed image visually overlaps the controls, but the
		// trap is JS-based (paneFocus) and the image is not focusable, so Tab order
		// is unaffected. Driving the zoom to the ceiling first proves it.
		mountViewer({ images: [image(IMG_A, 'first'), image(IMG_B, 'second')] });
		for (let i = 0; i < 10; i++) press('+');
		expect(scaleOf()).toBeCloseTo(4);

		const nextBtn = root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!;
		nextBtn.focus();
		expect(press('Tab')).toBe(true);
		expect(root().contains(document.activeElement)).toBe(true);
		expect(document.activeElement).toBe(closeButton());
	});
});
