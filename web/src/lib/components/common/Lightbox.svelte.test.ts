import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import Lightbox from './Lightbox.svelte';
import type { LightboxImage } from '$lib/attachments/events';
// TASK-2477 — the deletion bus is REAL in this suite (the viewer subscribes to
// it); firing `notifyAttachmentDeleted` drives the survivor logic directly.
import { notifyAttachmentDeleted } from '$lib/attachments/events';
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
import {
	reset as resetZoom,
	zoomTo,
	toggleFitOrActual,
	ZOOM_STEP,
	type Geometry,
} from '$lib/attachments/zoom';
// The viewer's real focusable-selection path — the trap cycles over exactly this
// set, so the tests below derive the leading / trailing edge from it rather than
// naming a specific control. The toolbar (TASK-2474) added controls after the
// nav, so the trailing edge is no longer `.lightbox-nav.next`.
import { paneFocusables } from '$lib/collections/paneFocus';
// TASK-2475 — the metadata header renders type/size through these; the tests
// compute their expected strings from the same helpers rather than hardcoding.
import { describeAttachmentType, formatBytes } from '$lib/attachments/display';

// TASK-2460 — the mobile DR-5b cells. `viewport.isMobile` is module state read
// from matchMedia at init (false under jsdom), so a controllable mock lets a few
// tests drive the mobile path. It DEFAULTS to desktop, so every other test in this
// file is unchanged; flip `mobileFlag` BEFORE mounting (the loader captures the
// platform at load time) and the mobile describe restores it.
//
// The mock delegates through a hoisted holder to a module-level `$state`, so a
// flip is genuinely REACTIVE — the viewer's `platform` derived re-runs. A plain
// object getter would be read once at mount and never invalidate, which would make
// the breakpoint-flip test vacuous (it would pass whether or not the load effect
// wrongly tracked `platform`). `.svelte.test.ts` supports runes.
const mobileHolder = vi.hoisted(() => ({ read: () => false }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return mobileHolder.read();
		},
	},
	MOBILE_BREAKPOINT: 768,
	MOBILE_MEDIA_QUERY: '(max-width: 768px)',
}));
let mobileFlag = $state(false);
mobileHolder.read = () => mobileFlag;

// TASK-2475 — the metadata header's B-module fetch. The viewer's images seed
// `size_bytes: null` by default (the `image()` fixture), so the header module
// fires a HEAD to fill it; controlling the result lets the header tests assert
// the fill / transient-retry paths, and keeps the real (failing) fetch out of
// every OTHER test. Defaulted to `ok` in `beforeEach`.
const metaFetch = vi.hoisted(() => vi.fn());
const metaRevalidate = vi.hoisted(() => vi.fn());
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	fetchAttachmentMetadata: (...a: unknown[]) => metaFetch(...a),
	revalidateAttachmentMetadata: (...a: unknown[]) => metaRevalidate(...a),
	invalidateAttachmentMetadata: vi.fn(),
}));

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
	// Toolbar context (TASK-2474).
	mutationsEnabled?: boolean;
	getItemContent?: () => string | null;
	getLiveContent?: () => string | null;
}

// Reactive props for the capture-at-open cases ($state may only initialize a
// declaration, hence top level).
const liveProps = $state<Props>({
	images: [image(IMG_A, 'a diagram')],
	index: 0,
	wsSlug: 'ws-one',
	onClose: () => {},
});

// Reactive props for the toolbar's permission-withdrawn case (TASK-2474): a
// peek closes the mutation gate mid-confirmation, and `mutationsEnabled` has to
// be flippable live to drive it.
const toolbarProps = $state<Props>({
	images: [image(IMG_A, 'a diagram')],
	index: 0,
	wsSlug: 'ws-one',
	onClose: () => {},
	mutationsEnabled: true,
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

/** The viewer's focusable controls, in tab order — what the trap cycles over. */
function focusables(scope: HTMLElement = root()): HTMLElement[] {
	return paneFocusables(scope);
}

/** The trailing edge of the trap — the last focusable control. */
function lastFocusable(scope: HTMLElement = root()): HTMLElement {
	const f = focusables(scope);
	return f[f.length - 1];
}

/** Whether the mobile sheet layout is selected on this root (TASK-2492 / T5). */
function hasSheet(scope: HTMLElement = root()): boolean {
	return scope.classList.contains('lightbox-sheet');
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
	// Reset call history AND the resolved value each test, so a `toHaveBeenCalled`
	// / call-count assertion can never read a prior test's invocation.
	metaFetch.mockReset();
	metaRevalidate.mockReset();
	metaFetch.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
	metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
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
	it('is an aria-modal dialog named after the image, plus its type', () => {
		mountViewer();
		expect(root().getAttribute('role')).toBe('dialog');
		expect(root().getAttribute('aria-modal')).toBe('true');
		// The accessible name is the display name PLUS the type/size the header
		// shows (3c-ii T2b) — computed from the same helper the component uses. Size
		// is absent here (the seed carries none and the probe is async), so the label
		// is "name, type".
		const type = describeAttachmentType('image/png', null);
		expect(root().getAttribute('aria-label')).toBe(`a diagram, ${type}`);
	});

	it('falls back to a generic name when the image has no alt', () => {
		// An unnamed dialog is announced as nothing at all, so the fallback is
		// part of the contract rather than a nicety — now the display-name fallback
		// ("Attachment") plus the type.
		mountViewer({ images: [image(IMG_A, '')] });
		const type = describeAttachmentType('image/png', null);
		expect(root().getAttribute('aria-label')).toBe(`Attachment, ${type}`);
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
		const type = describeAttachmentType('image/png', null);
		expect(root().getAttribute('aria-label')).toBe(`first, ${type}`);
		root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!.click();
		flushSync();
		expect(root().getAttribute('aria-label')).toBe(`second, ${type}`);
	});
});

describe('Lightbox — admission and the stage arm (3c-ii T3)', () => {
	/**
	 * 3c-ii flips admission: the converged surface opens ANY attachment, so every
	 * entry is navigable and safety moves to the ARM. A non-raster entry — an
	 * unsafe/active type, a file, or a still-unresolved MIME — is KEPT and drawn as
	 * the no-bytes icon fallback, where 3c-i dropped it. These rewrite the 3c-i
	 * "at-open REFUSED" pins (deliberately falsified by the flip) to
	 * admission+fallback assertions; the raster arm still only ever mounts bytes
	 * for a positively-allowlisted RESOLVED MIME.
	 */
	function shown(): string {
		return root().querySelector<HTMLImageElement>('.lightbox-image')?.getAttribute('alt') ?? '';
	}

	it('ADMITS a non-allowlisted type asked to open ON it, drawn as the fallback', () => {
		mountViewer({
			images: [image(IMG_A, 'png', 'image/png'), image(IMG_B, 'svg', 'image/svg+xml')],
			index: 1,
		});
		// The SVG is the requested index — no longer refused; it opens on the
		// no-bytes fallback (no <img>), and both entries are navigable.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('2 / 2');
	});

	it('admits an unsafe-at-open entry as a navigable fallback sibling (3c-i refusal retired)', () => {
		mountViewer({ images: [image(IMG_A, 'png', 'image/png'), image(IMG_B, 'svg', 'image/svg+xml')] });
		// Opens on A (raster); the SVG is a navigable sibling now, not dropped.
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
		expect(shown()).toBe('png');
		press('ArrowRight');
		// Paged onto the SVG: the fallback arm, no <img>.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
	});

	it('pages onto an unsafe entry with ←/→, showing the fallback', () => {
		mountViewer({
			images: [
				image(IMG_A, 'png', 'image/png'),
				image(IMG_B, 'svg', 'image/svg+xml'),
				image(IMG_C, 'jpeg', 'image/jpeg'),
			],
		});
		// All three navigable now.
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 3');
		expect(shown()).toBe('png');
		press('ArrowRight'); // B (svg) → fallback
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		press('ArrowRight'); // C (jpeg) → raster
		expect(shown()).toBe('jpeg');
	});

	it('opens the requested image BY ID even with every member navigable', () => {
		// index 1 names the SECOND element; with nothing filtered out it still
		// resolves by id to that element, not a reindexed position.
		mountViewer({
			images: [
				image(IMG_B, 'svg', 'image/svg+xml'),
				image(IMG_A, 'png', 'image/png'),
				image(IMG_C, 'jpeg', 'image/jpeg'),
			],
			index: 1,
		});
		expect(shown()).toBe('png');
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('2 / 3');
	});

	it('admits an unresolved (null-MIME) entry, showing the fallback until the probe resolves', () => {
		// Synchronously — before the HEAD answers — a null MIME is not raster, so
		// the fallback shows. The delayed reclassification to raster/PDF/ZIP is its
		// own describe below. The probe is pinned pending so the sync state is
		// unambiguous — on BOTH the plain and the forced (T6, the opened entry)
		// paths, so the single opened null entry never resolves out of the fallback.
		metaFetch.mockReturnValue(new Promise(() => {}));
		metaRevalidate.mockReturnValue(new Promise(() => {}));
		mountViewer({ images: [image(IMG_A, 'unprobed', null)] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
	});

	it('pages onto an unresolved sibling, showing the fallback', () => {
		metaFetch.mockReturnValue(new Promise(() => {}));
		mountViewer({
			images: [image(IMG_A, 'png'), image(IMG_B, 'unprobed', null), image(IMG_C, 'jpeg', 'image/jpeg')],
		});
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 3');
		expect(shown()).toBe('png');
		press('ArrowRight'); // B (unresolved) → fallback
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		press('ArrowRight'); // C (jpeg) → raster
		expect(shown()).toBe('jpeg');
	});

	it('shows the fallback for a single unsafe entry rather than an empty viewer', () => {
		mountViewer({ images: [image(IMG_B, 'svg', 'image/svg+xml')] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		// The single unsafe entry now SHOWS (as the fallback), where 3c-i left an
		// empty viewer. Still a real dialog with a way out.
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		expect(closeButton()).not.toBeNull();
		// Arrows are a no-op on a single-member set; still the fallback.
		press('ArrowRight');
		press('ArrowLeft');
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
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

	it('shows the fallback (not drops) when a record resolves unsafe after open', () => {
		// TASK-2476 matrix: safe→unsafe MID-VIEW is the FALLBACK cell — the entry
		// stays navigable and counted, drawn as the no-bytes icon fallback, where
		// pre-3c it was dropped from the set.
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg')];
		mountLive();
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');

		// The late answer arrives: what was believed safe is not.
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'later-svg', 'image/svg+xml')];
		flushSync();

		// Still TWO entries — the flipped one is kept, not dropped.
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
		press('ArrowRight');
		// Landed on the flipped entry: the fallback arm, NO <img>.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		const fb = root().querySelector('.lightbox-fallback');
		expect(fb).not.toBeNull();
		expect(fb?.textContent).toContain('No preview available');
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

	it('shows the fallback for BOTH an unsafe and an unresolved entry added after open', () => {
		// 3c-ii admission flip: added-while-open UNSAFE and UNRESOLVED both admit
		// now, both drawn as the fallback. The 3c-i "unresolved is still refused"
		// cell is retired. The probe is pinned pending so the null-MIME entry stays
		// unresolved (its delayed reclassification is covered separately).
		metaFetch.mockReturnValue(new Promise(() => {}));
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();

		liveProps.images = [
			image(IMG_A, 'png'),
			image(IMG_B, 'svg', 'image/svg+xml'), // unsafe → fallback
			image(IMG_C, 'unprobed', null), // unresolved → fallback (was: refused)
		];
		flushSync();

		// All three navigable now — 1 / 3.
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 3');
		press('ArrowRight'); // B (svg) → fallback
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		press('ArrowRight'); // C (unresolved) → fallback too
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		press('ArrowRight'); // wrap to A → raster
		expect(shown()).toBe('png');
	});

	it('shows the fallback for an unsafe replacement rather than emptying', () => {
		// TASK-2476: the only entry is replaced by an unsafe one — added-while-open,
		// so the fallback cell, not an empty viewer.
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		expect(shown()).toBe('png');

		liveProps.images = [image(IMG_B, 'svg', 'image/svg+xml')];
		flushSync();

		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
	});
});

describe('Lightbox — the file route and reclassification (3c-ii T3)', () => {
	// The converged surface opens files and unresolved rows, drawing the fallback
	// arm with a toolbar the descriptors shape per RESOLVED type, and — when a
	// null-seed open's HEAD answers — RE-DERIVING both the arm and the toolbar.
	function openTool(): Element | null {
		return root().querySelector('.lightbox-toolbar [aria-label="Open in new tab"]');
	}
	function downloadTool(): Element | null {
		return root().querySelector('.lightbox-toolbar [aria-label="Download"]');
	}
	// Let the metadata machine's async HEAD resolve: interleave a microtask drain
	// with an effect flush so a reclassification (and any reload it triggers) settles.
	async function settleAsync() {
		for (let i = 0; i < 8; i++) {
			await Promise.resolve();
			flushSync();
		}
	}

	it('opens a PDF on the fallback arm WITH an Open action', () => {
		mountViewer({ images: [image(IMG_A, 'doc.pdf', 'application/pdf')] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		// canBrowserPreview(pdf) → Open present; Download always present.
		expect(openTool()).not.toBeNull();
		expect(downloadTool()).not.toBeNull();
	});

	it('opens a ZIP on the fallback arm WITHOUT an Open action', () => {
		mountViewer({ images: [image(IMG_A, 'bundle.zip', 'application/zip')] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		// canBrowserPreview(zip) is false → no Open; Download still present.
		expect(openTool()).toBeNull();
		expect(downloadTool()).not.toBeNull();
	});

	it('reclassifies a null-seed entry to the RASTER arm when the probe resolves an image', async () => {
		// T6: the opened entry is force-revalidated, so its resolved type arrives on
		// the revalidation probe (not the plain seed-fill fetch).
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
		mountViewer({ images: [image(IMG_A, 'unprobed', null)] });
		// Before the probe: the no-bytes fallback.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		await settleAsync();
		// After: the resolved image reclassifies to the raster arm — the <img>
		// mounts, the fallback is gone.
		expect(root().querySelector('.lightbox-fallback')).toBeNull();
		expect(root().querySelector('.lightbox-image')).not.toBeNull();
	});

	it('reclassifies a null-seed entry to a PDF fallback WITH Open when the probe resolves PDF', async () => {
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 2048 });
		mountViewer({ images: [image(IMG_A, 'unprobed', null)] });
		// Null MIME → Open not applicable yet.
		expect(openTool()).toBeNull();
		await settleAsync();
		// Resolved PDF: stays the fallback (PDF is not raster) but now offers Open.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		expect(openTool()).not.toBeNull();
	});

	it('reclassifies a null-seed entry to a ZIP fallback WITHOUT Open when the probe resolves ZIP', async () => {
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'application/zip', size: 2048 });
		mountViewer({ images: [image(IMG_A, 'unprobed', null)] });
		await settleAsync();
		// Resolved ZIP: the fallback, and Open never appears.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		expect(openTool()).toBeNull();
	});

	it('gates every action inert on an archived-parent open while the probe is in flight', () => {
		// An archived parent forces a reachability probe (metaRevalidate); until it
		// answers, no live action fires on possibly-unreachable bytes. Probe pinned
		// pending so the gated state is stable.
		metaRevalidate.mockReturnValue(new Promise(() => {}));
		mountViewer({
			images: [image(IMG_A, 'doc.pdf', 'application/pdf')],
			parentArchived: true,
			mutationsEnabled: true,
		});
		const tools = [...root().querySelectorAll('.lightbox-toolbar .lightbox-tool')];
		expect(tools.length).toBeGreaterThan(0);
		for (const t of tools) {
			const inert = t.hasAttribute('disabled') || t.getAttribute('aria-disabled') === 'true';
			expect(inert, `${t.getAttribute('aria-label')} should be inert while unreachable`).toBe(true);
		}
	});

	it('KEEPS archived-parent actions inert when the probe fails (transient), not just while pending', async () => {
		// The gate is on the phase, not `slow`: a probe that fails (or times out to
		// `transient`) leaves reachability UNCONFIRMED, so actions must stay inert —
		// re-enabling them on a non-`ok` answer would offer live actions on bytes
		// that may be unreachable.
		metaRevalidate.mockResolvedValue({ status: 'transient' });
		mountViewer({
			images: [image(IMG_A, 'doc.pdf', 'application/pdf')],
			parentArchived: true,
			mutationsEnabled: true,
		});
		await settleAsync();
		const tools = [...root().querySelectorAll('.lightbox-toolbar .lightbox-tool')];
		expect(tools.length).toBeGreaterThan(0);
		for (const t of tools) {
			const inert = t.hasAttribute('disabled') || t.getAttribute('aria-disabled') === 'true';
			expect(inert, `${t.getAttribute('aria-label')} should stay inert on a transient probe`).toBe(
				true
			);
		}
	});

	it('re-inerts actions during a forced re-probe after a prior ok (ok → archived)', async () => {
		// A live entry resolves reachable, then its parent archives under the open
		// viewer: the machine re-probes (forced), and the fetched fields are retained
		// so `phase` stays `ok` while that probe is in flight. The gate must go inert
		// again on `slow` alone here — the T2a archive transition this anticipates.
		metaFetch.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 2048 });
		metaRevalidate.mockReturnValue(new Promise(() => {})); // the archive re-probe hangs
		const props = $state<Props>({
			images: [image(IMG_A, 'doc.pdf', 'application/pdf')],
			wsSlug: 'ws-1',
			onClose: () => {},
			mutationsEnabled: true,
			parentArchived: false,
		});
		const app = mount(Lightbox, { target: appRoot, props });
		mounted.push(app);
		flushSync();
		await settleAsync(); // initial fetch → ok → actions live
		expect(downloadTool()?.getAttribute('aria-disabled')).not.toBe('true');

		// The parent archives under the open viewer → forced re-probe (hangs).
		props.parentArchived = true;
		flushSync();
		const tools = [...root().querySelectorAll('.lightbox-toolbar .lightbox-tool')];
		expect(tools.length).toBeGreaterThan(0);
		for (const t of tools) {
			const inert = t.hasAttribute('disabled') || t.getAttribute('aria-disabled') === 'true';
			expect(inert, `${t.getAttribute('aria-label')} should re-inert on the archive re-probe`).toBe(
				true
			);
		}
	});

	it('re-enables actions on an archived-parent open once the probe resolves reachable', async () => {
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 2048 });
		mountViewer({
			images: [image(IMG_A, 'doc.pdf', 'application/pdf')],
			parentArchived: true,
			mutationsEnabled: true,
		});
		await settleAsync();
		// Reachable → the always-applicable Download is live again.
		const dl = downloadTool();
		expect(dl).not.toBeNull();
		expect(dl?.getAttribute('aria-disabled')).not.toBe('true');
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
		// More than one focusable, so "first" is separable from "a"/"the root":
		// close, the toolbar's open/download/copy-link (TASK-2474), and prev/next
		// (which now live inside the stage, so they trail the toolbar in DOM order).
		const controls = focusables();
		expect(controls.length).toBeGreaterThan(1);
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
		// The trailing edge is the last toolbar control now, not the nav (TASK-2474).
		lastFocusable().focus();

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
		expect(document.activeElement).toBe(lastFocusable());
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
		expect(document.activeElement).toBe(lastFocusable());
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
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'second', 'image/jpeg')];
		mountLive();
		press('ArrowRight');
		expect(imageSrc()).toContain(IMG_B);
		press('+');
		expect(scaleOf()).toBeCloseTo(1.25);

		// B is REMOVED (a delete elsewhere) → the shown image becomes A → the
		// transform is back at fit. A REMOVAL is used, not a safe→unsafe flip: since
		// TASK-2476 a flip keeps the entry (same id) as the fallback, so it would not
		// change the shown id and the reset would (correctly) not fire.
		liveProps.images = [image(IMG_A, 'png')];
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

		lastFocusable().focus();
		expect(press('Tab')).toBe(true);
		expect(root().contains(document.activeElement)).toBe(true);
		expect(document.activeElement).toBe(closeButton());
	});
});

// TASK-2457 — wheel / ctrl-cmd-wheel zoom, anchored at the cursor.
//
// The listener is registered NON-PASSIVELY on the viewer root, so a dispatched
// WheelEvent's `defaultPrevented` reflects the handler's `preventDefault` — the
// direct assertion the acceptance requires (never inferred from "the page did
// not scroll", which jsdom can't show anyway).

/** Dispatch a wheel on `scope` (the viewer root, where the listener lives). */
function wheel(scope: HTMLElement, init: WheelEventInit): boolean {
	const e = new WheelEvent('wheel', { cancelable: true, bubbles: true, ...init });
	scope.dispatchEvent(e);
	flushSync();
	return e.defaultPrevented;
}

/** Mock the geometry the viewer reads, so the cursor anchor is observable. */
function mockGeometry(scope: HTMLElement, g: Geometry, rectLeft = 0, rectTop = 0): void {
	const image = scope.querySelector<HTMLImageElement>('.lightbox-image')!;
	const stage = scope.querySelector<HTMLElement>('.lightbox-stage')!;
	Object.defineProperty(image, 'offsetWidth', { configurable: true, get: () => g.fittedW });
	Object.defineProperty(image, 'offsetHeight', { configurable: true, get: () => g.fittedH });
	Object.defineProperty(image, 'naturalWidth', { configurable: true, get: () => g.naturalW });
	Object.defineProperty(image, 'naturalHeight', { configurable: true, get: () => g.naturalH });
	Object.defineProperty(stage, 'clientWidth', { configurable: true, get: () => g.stageW });
	Object.defineProperty(stage, 'clientHeight', { configurable: true, get: () => g.stageH });
	stage.getBoundingClientRect = () =>
		({
			left: rectLeft,
			top: rectTop,
			width: g.stageW,
			height: g.stageH,
			right: rectLeft + g.stageW,
			bottom: rectTop + g.stageH,
			x: rectLeft,
			y: rectTop,
			toJSON() {},
		}) as DOMRect;
}

describe('Lightbox — wheel zoom (TASK-2457)', () => {
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

	it('zooms IN and leaves the cursor point anchored — matching the module exactly', () => {
		// A geometry where the image nearly fills the stage, so ONE wheel step
		// overflows and an off-centre cursor produces an observable, module-defined
		// pan (jsdom's zero geometry would hide it — the same limit the anchor key
		// test noted). The image point under the cursor stays there because the
		// component delegates to `zoomTo` with the cursor as the anchor: the
		// resulting transform must equal `zoomTo`'s output byte for byte.
		mountViewer();
		const g: Geometry = {
			stageW: 1000,
			stageH: 1000,
			fittedW: 900,
			fittedH: 900,
			naturalW: 2000,
			naturalH: 2000,
		};
		// A NONZERO stage offset in the viewport, so the anchor must be computed
		// stage-LOCAL (clientX − rect.left). An implementation that fed viewport
		// coordinates straight in would anchor 120/60px off and fail this.
		const rectLeft = 120;
		const rectTop = 60;
		mockGeometry(root(), g, rectLeft, rectTop);
		const clientX = 800;
		const clientY = 500;
		const anchor = { x: clientX - rectLeft, y: clientY - rectTop };
		const expected = zoomTo(resetZoom(), 1 * ZOOM_STEP, anchor, g);

		expect(wheel(root(), { clientX, clientY, deltaY: -1 })).toBe(true);
		expect(transformOf()).toBe(
			`translate(${expected.x}px, ${expected.y}px) scale(${expected.scale})`
		);
		// The off-centre anchor genuinely moved the pan (not a centred no-op), so
		// this is a real anchor assertion, not a vacuous one.
		expect(expected.x).not.toBe(0);
	});

	it('stops propagation while frontmost so restoration never sees the wheel; lets it through otherwise', () => {
		// The `stopPropagation` half — belt to the restoration guard. A frontmost
		// viewer's wheel must not reach a `window` listener (the scroll-restoration
		// one); a non-frontmost viewer declines, so its event DOES bubble through
		// (there, restoration's OWN guard is what protects it — asserted in
		// restore.svelte.test.ts).
		const seen = vi.fn();
		window.addEventListener('wheel', seen);
		try {
			mountViewer();
			const back = root();
			mountViewer();
			const front = root();

			const e1 = new WheelEvent('wheel', {
				cancelable: true,
				bubbles: true,
				clientX: 10,
				clientY: 10,
				deltaY: -1,
			});
			front.dispatchEvent(e1);
			flushSync();
			expect(e1.defaultPrevented).toBe(true);
			expect(seen).not.toHaveBeenCalled();

			const e2 = new WheelEvent('wheel', {
				cancelable: true,
				bubbles: true,
				clientX: 10,
				clientY: 10,
				deltaY: -1,
			});
			back.dispatchEvent(e2);
			flushSync();
			expect(seen).toHaveBeenCalledTimes(1);
		} finally {
			window.removeEventListener('wheel', seen);
		}
	});

	it('ctrl/cmd+wheel ALSO zooms (both plain and modified) and is preventDefaulted', () => {
		// The modifier is NOT a gate for wheel (unlike the +/- keys): ctrl/cmd wheel
		// is the browser's page-zoom, which the viewer overrides while open — hence
		// the non-passive preventDefault.
		mountViewer();
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaY: -1, ctrlKey: true })).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
		mountViewer();
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaY: -1, metaKey: true })).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('a horizontal-only wheel (deltaY 0) is consumed but does NOT zoom', () => {
		// A trackpad side-swipe must not read as a zoom-out; direction is deltaY
		// only. Seed a zoom-in FIRST so a spurious zoom-out would be observable —
		// from fit it would just clamp back to 1 and hide the bug.
		mountViewer();
		wheel(root(), { clientX: 10, clientY: 10, deltaY: -1 });
		expect(scaleOf()).toBeCloseTo(1.25);
		// Still preventDefaulted (the modal owns the wheel), but the zoom is left put.
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaX: 120, deltaY: 0 })).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('a downward wheel zooms OUT (back toward fit)', () => {
		mountViewer();
		wheel(root(), { clientX: 10, clientY: 10, deltaY: -1 });
		wheel(root(), { clientX: 10, clientY: 10, deltaY: -1 });
		expect(scaleOf()).toBeCloseTo(1.25 * 1.25);
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaY: 1 })).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});

	it('no-ops (and does NOT preventDefault) when not the frontmost viewer; the front one acts', () => {
		mountViewer();
		const back = root();
		mountViewer();
		const front = root();
		expect(back).not.toBe(front);

		// Dispatched on the BACKGROUND viewer: it must decline AND leave the event
		// uncancelled (nothing behind it to own the wheel either way).
		expect(wheel(back, { clientX: 10, clientY: 10, deltaY: -1 })).toBe(false);
		expect(scaleOf(back)).toBe(1);

		// Positive control: the FRONT viewer zooms and cancels.
		expect(wheel(front, { clientX: 10, clientY: 10, deltaY: -1 })).toBe(true);
		expect(scaleOf(front)).toBeCloseTo(1.25);
	});

	it('no-ops while a showModal() dialog is above it, and resumes after it closes', () => {
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([dialog]);
		mountViewer();
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaY: -1 })).toBe(false);
		expect(scaleOf()).toBe(1);

		mockOpenModals([]);
		expect(wheel(root(), { clientX: 10, clientY: 10, deltaY: -1 })).toBe(true);
		expect(scaleOf()).toBeCloseTo(1.25);
	});
});

// TASK-2458 — drag-to-pan + double-click toggle (probe).
const REAL_PC = {
	setPointerCapture: Element.prototype.setPointerCapture,
	releasePointerCapture: Element.prototype.releasePointerCapture,
};
let captured: number[] = [];
let released: number[] = [];

function pointerEvent(
	type: string,
	x: number,
	y: number,
	opts: { buttons?: number; pointerType?: string; button?: number; pointerId?: number } = {}
): Event {
	const e = new MouseEvent(type, { bubbles: true, cancelable: true, clientX: x, clientY: y });
	Object.defineProperty(e, 'pointerId', { value: opts.pointerId ?? 1 });
	Object.defineProperty(e, 'buttons', { value: opts.buttons ?? 1 });
	Object.defineProperty(e, 'button', { value: opts.button ?? 0 });
	Object.defineProperty(e, 'pointerType', { value: opts.pointerType ?? 'mouse' });
	return e;
}

function panX(scope: HTMLElement = root()): number {
	const m = /translate\(([-\d.]+)px,/.exec(transformOf(scope));
	return m ? Number(m[1]) : NaN;
}

const OVERFLOW_G: Geometry = {
	stageW: 1000,
	stageH: 1000,
	fittedW: 900,
	fittedH: 900,
	naturalW: 2000,
	naturalH: 2000,
};

/** dblclick at the stage centre → actual size, so drags have pan room. */
function zoomToActual(scope: HTMLElement = root()): void {
	scope.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
	flushSync();
}
/** The pan bound at `actualScale` for OVERFLOW_G: (900·(2000/900) − 1000)/2. */
const ACTUAL_BOUND = (OVERFLOW_G.fittedW * (OVERFLOW_G.naturalW / OVERFLOW_G.fittedW) - OVERFLOW_G.stageW) / 2;

function mockOpenModals(modals: Element[]): void {
	const realQueryAll = document.querySelectorAll.bind(document);
	vi.spyOn(document, 'querySelectorAll').mockImplementation((selector: string) => {
		if (selector !== 'dialog:modal') return realQueryAll(selector);
		return Array.from(realQueryAll('dialog')).filter((d) =>
			modals.includes(d)
		) as unknown as NodeListOf<Element>;
	});
	const realMatches = Element.prototype.matches;
	vi.spyOn(Element.prototype, 'matches').mockImplementation(function (this: Element, selector: string) {
		if (selector !== 'dialog:modal') return realMatches.call(this, selector);
		return realMatches.call(this, 'dialog') && modals.includes(this);
	});
}

describe('Lightbox — drag-to-pan and double-click (TASK-2458)', () => {
	beforeEach(() => {
		captured = [];
		released = [];
		(Element.prototype as unknown as Record<string, unknown>).setPointerCapture = function (id: number) {
			captured.push(id);
		};
		(Element.prototype as unknown as Record<string, unknown>).releasePointerCapture = function (id: number) {
			released.push(id);
		};
	});
	afterEach(() => {
		if (REAL_PC.setPointerCapture === undefined)
			delete (Element.prototype as unknown as Record<string, unknown>).setPointerCapture;
		else Element.prototype.setPointerCapture = REAL_PC.setPointerCapture;
		if (REAL_PC.releasePointerCapture === undefined)
			delete (Element.prototype as unknown as Record<string, unknown>).releasePointerCapture;
		else Element.prototype.releasePointerCapture = REAL_PC.releasePointerCapture;
	});

	// ── pan, positively AND clamped (the anti-false-green acceptance) ──
	it('pans in-bounds by the drag delta, then clamps at the edge and moves NO FURTHER', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual(); // scale ~2.22, pan bound ±ACTUAL_BOUND (500), pan still 0
		expect(scaleOf()).toBeCloseTo(2000 / 900);

		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		// A small in-bounds drag MOVES the transform by exactly the delta.
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1); // a real drag engaged + captured
		expect(panX()).toBeCloseTo(100);

		// Over-drag to the same edge: it clamps at the bound...
		root().dispatchEvent(pointerEvent('pointermove', 500 + ACTUAL_BOUND + 300, 500));
		flushSync();
		expect(panX()).toBeCloseTo(ACTUAL_BOUND);
		// ...and dragging further still moves it no further (the clamp, not the delta).
		root().dispatchEvent(pointerEvent('pointermove', 500 + ACTUAL_BOUND + 900, 500));
		flushSync();
		expect(panX()).toBeCloseTo(ACTUAL_BOUND);

		root().dispatchEvent(pointerEvent('pointerup', 500 + ACTUAL_BOUND + 900, 500, { buttons: 0 }));
		expect(released).toContain(1);
	});

	// ── double-click toggle, anchored, RETURNS ──
	/** The realistic sequence a browser fires for a double-click on `el`. */
	function doubleClick(el: Element, clientX: number, clientY: number): void {
		el.dispatchEvent(new MouseEvent('click', { bubbles: true, clientX, clientY }));
		el.dispatchEvent(new MouseEvent('click', { bubbles: true, clientX, clientY }));
		el.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX, clientY }));
		flushSync();
	}

	it('two successive anchored double-clicks ON THE IMAGE toggle fit → actual → fit, never closing', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		mockGeometry(root(), OVERFLOW_G);
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		expect(scaleOf()).toBe(1);

		// The full click/click/dblclick sequence on the IMAGE (target !== backdrop),
		// off-centre so the toggle-to-actual is genuinely anchored. The constituent
		// clicks must NOT dismiss.
		doubleClick(image, 800, 500);
		expect(onClose).not.toHaveBeenCalled();
		const expected = toggleFitOrActual(resetZoom(), { x: 800, y: 500 }, OVERFLOW_G);
		expect(transformOf()).toBe(
			`translate(${expected.x}px, ${expected.y}px) scale(${expected.scale})`
		);
		expect(expected.scale).toBeGreaterThan(1);
		expect(expected.x).not.toBe(0);

		// The SECOND double-click returns to fit, centred — the toggle is two-way.
		doubleClick(image, 800, 500);
		expect(onClose).not.toHaveBeenCalled();
		expect(transformOf()).toBe('translate(0px, 0px) scale(1)');
	});

	it('a double-click while a pan is still being suppressed does NOT toggle (drag XOR toggle)', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		// Engage and release a pan — its click is now being swallowed (suppressClick).
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		root().dispatchEvent(pointerEvent('pointerup', 600, 500, { buttons: 0 }));
		flushSync();
		// A double-click landing while that suppression is still live must stand
		// down — a gesture is a drag OR a toggle, never both (same swallow the
		// backdrop click consults).
		image.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 800, clientY: 500 }));
		flushSync();
		expect(scaleOf()).toBe(1);
	});

	it('a double-click does NOT toggle while a native modal is above it; it resumes after', () => {
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([dialog]);
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;

		image.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf()).toBe(1); // gated out by isBlockedByModal

		mockOpenModals([]);
		image.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf()).toBeCloseTo(2000 / 900); // resumes
	});

	// ── drag-vs-click disambiguation ──
	it('a below-threshold press released over the backdrop still CLOSES', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		root().dispatchEvent(pointerEvent('pointerdown', 100, 100));
		root().dispatchEvent(pointerEvent('pointermove', 102, 101)); // < 4px → still a click
		root().dispatchEvent(pointerEvent('pointerup', 102, 101, { buttons: 0 }));
		flushSync();
		// The click the press synthesizes, on the backdrop itself.
		root().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onClose).toHaveBeenCalledTimes(1);
		expect(captured).toEqual([]); // never engaged a drag
	});

	it('a past-threshold DRAG released over the backdrop does NOT close', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		root().dispatchEvent(pointerEvent('pointerdown', 100, 100));
		root().dispatchEvent(pointerEvent('pointermove', 200, 200)); // > 4px → a pan
		root().dispatchEvent(pointerEvent('pointerup', 200, 200, { buttons: 0 }));
		flushSync();
		// The click a pan synthesizes is suppressed (cleared only on the next tick,
		// which this synchronous dispatch beats).
		root().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onClose).not.toHaveBeenCalled();
		expect(captured).toContain(1);
	});

	it('CONTROL: a plain backdrop click (no gesture) still closes', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		root().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	// ── arbitration: the START gate ──
	it('does not START a pan when the viewer is not frontmost; the front one does', () => {
		mountViewer();
		const back = root();
		mockGeometry(back, OVERFLOW_G);
		mountViewer();
		const front = root();
		mockGeometry(front, OVERFLOW_G);
		zoomToActual(front);

		// Drag on the BACKGROUND viewer: no capture, no pan.
		back.dispatchEvent(pointerEvent('pointerdown', 500, 500));
		back.dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toEqual([]);

		// Positive control: the FRONT viewer pans.
		front.dispatchEvent(pointerEvent('pointerdown', 500, 500));
		front.dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);
		expect(panX(front)).toBeCloseTo(100);
	});

	// ── arbitration: whole-gesture, a STACKED-VIEWER transition mid-drag ──
	it('ABORTS a captured drag when a SECOND viewer becomes frontmost mid-gesture', () => {
		mountViewer();
		const first = root();
		mockGeometry(first, OVERFLOW_G);
		zoomToActual(first);

		first.dispatchEvent(pointerEvent('pointerdown', 500, 500));
		first.dispatchEvent(pointerEvent('pointermove', 600, 500)); // positive control: pans to 100
		flushSync();
		expect(captured).toContain(1);
		expect(panX(first)).toBeCloseTo(100);
		const frozen = panX(first);

		// A second viewer opens — `first` is no longer frontmost.
		mountViewer();

		// A further move on `first` (still captured there) must ABORT: release the
		// capture, leave the transform where it was.
		first.dispatchEvent(pointerEvent('pointermove', 900, 500));
		flushSync();
		expect(released).toContain(1);
		expect(panX(first)).toBeCloseTo(frozen);
	});

	// ── arbitration: whole-gesture, a NATIVE-MODAL transition mid-drag ──
	it('ABORTS a captured drag when a native modal opens above it mid-gesture', () => {
		const dialog = document.body.appendChild(document.createElement('dialog'));
		// Establish `:modal` support with NO modal first (jsdom throws on the
		// pseudo-class, and the manager caches support on its first probe), so the
		// drag can start; then add the dialog mid-gesture.
		mockOpenModals([]);
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();

		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500)); // positive control: pans
		flushSync();
		expect(captured).toContain(1);
		expect(panX()).toBeCloseTo(100);
		const frozen = panX();

		mockOpenModals([dialog]); // a showModal() dialog is now above the viewer

		root().dispatchEvent(pointerEvent('pointermove', 900, 500));
		flushSync();
		expect(released).toContain(1);
		expect(panX()).toBeCloseTo(frozen);
	});

	// ── integration details ──
	it('marks the image non-draggable so a native image drag cannot pre-empt the pan', () => {
		mountViewer();
		expect(root().querySelector('.lightbox-image')?.getAttribute('draggable')).toBe('false');
	});

	// DELIBERATELY INVERTED from the pre-3d contract (TASK-2517, falsify-don't-
	// contort). The former test here pinned "a touch pointerdown is IGNORED — no
	// registry, no anything". V1 replaces it with the V1 touch contract, split into
	// two honest halves: (a) a touch press+MOVE arms nothing (no capture, no pan) and
	// the mouse path stays byte-identical; (b) a real touch TAP (press+up, NO move)
	// still closes via the backdrop. Keeping them separate avoids conflating "no pan"
	// with "tap closes" by injecting a click after an unrealistic 100px touch move.
	it('V1: a TOUCH press+move arms nothing (no capture, no pan); the mouse path is byte-identical', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();

		// Touch press + move: no capture taken, the transform does not move (V1 arms
		// no touch drag — touch pan is V2's).
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500, { pointerType: 'touch' }));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500, { pointerType: 'touch' }));
		flushSync();
		expect(captured).toEqual([]);
		expect(panX()).toBeCloseTo(0); // unmoved — no pan

		// The mouse path is byte-identical: it still captures and pans.
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);
		expect(panX()).toBeCloseTo(100);
	});

	it('V1: a real TOUCH TAP (press+up, no move) still closes via the backdrop', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		// A genuine tap — down + up at the SAME point, no drag. Touch arms nothing, so
		// `suppressClick` stays false and the tap's synthesized backdrop click closes,
		// exactly as a plain backdrop click does (the V1 touch surface: taps).
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500, { pointerType: 'touch' }));
		root().dispatchEvent(pointerEvent('pointerup', 500, 500, { pointerType: 'touch', buttons: 0 }));
		root().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('drops the transform transition while dragging (image tracks the pointer), restoring it after', () => {
		// jsdom applies no transitions, so assert the mechanism: the `panning` class
		// (which sets transition:none) is present only for the duration of the drag.
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		expect(image.classList.contains('panning')).toBe(false);

		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(image.classList.contains('panning')).toBe(true);

		root().dispatchEvent(pointerEvent('pointerup', 600, 500, { buttons: 0 }));
		flushSync();
		expect(image.classList.contains('panning')).toBe(false);
	});

	it('a TOUCH or second pointer cannot move or END an active mouse drag', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		// Mouse drag engaged (pointerId 1).
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(panX()).toBeCloseTo(100);

		// A touch move with a DIFFERENT pointerId (not captured) must not pan...
		root().dispatchEvent(
			pointerEvent('pointermove', 900, 500, { pointerId: 2, pointerType: 'touch' })
		);
		flushSync();
		expect(panX()).toBeCloseTo(100);

		// ...and a touch pointerup must not END the mouse drag.
		root().dispatchEvent(
			pointerEvent('pointerup', 900, 500, { pointerId: 2, pointerType: 'touch', buttons: 0 })
		);
		flushSync();
		expect(released).not.toContain(2);

		// The mouse (pointerId 1) is still driving: its move pans further.
		root().dispatchEvent(pointerEvent('pointermove', 700, 500));
		flushSync();
		expect(panX()).toBeCloseTo(200);
	});

	it('a second primary pointerdown mid-drag does NOT hijack the gesture', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500)); // mouse id 1
		root().dispatchEvent(pointerEvent('pointermove', 600, 500)); // captured, x = 100
		flushSync();
		expect(panX()).toBeCloseTo(100);

		// A second primary press (a pen, id 2) lands mid-drag — it must be ignored,
		// not re-arm the gesture onto itself.
		root().dispatchEvent(
			pointerEvent('pointerdown', 200, 200, { pointerId: 2, pointerType: 'pen' })
		);
		// The ORIGINAL pointer (id 1) still drives the pan.
		root().dispatchEvent(pointerEvent('pointermove', 700, 500));
		flushSync();
		expect(panX()).toBeCloseTo(200);
		// ...and the interloper's own move does nothing.
		root().dispatchEvent(
			pointerEvent('pointermove', 900, 900, { pointerId: 2, pointerType: 'pen' })
		);
		flushSync();
		expect(panX()).toBeCloseTo(200);
	});

	it('releases the capture on pointercancel', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);
		root().dispatchEvent(pointerEvent('pointercancel', 600, 500));
		expect(released).toContain(1);
	});

	// ── registry hygiene: the pointer set drains, never leaks (round-2 P1) ──
	//
	// The registry is inert plumbing in V1, so a leak has no other observable
	// consequence — assert the drain invariant directly via the test-only accessor.
	type RegistrySized = { __registrySize: () => number };

	it('drains the registry on pointerup AND on pointercancel — a touch leaves no entry', () => {
		const app = mountViewer() as unknown as RegistrySized;
		// A touch enters the registry...
		root().dispatchEvent(pointerEvent('pointerdown', 300, 300, { pointerType: 'touch', pointerId: 7 }));
		expect(app.__registrySize()).toBe(1);
		// ...and its pointerup drains it (deleted BEFORE the owner guard — the touch
		// never owned a gesture, so an owner-guarded early return would have leaked it).
		root().dispatchEvent(
			pointerEvent('pointerup', 300, 300, { pointerType: 'touch', pointerId: 7, buttons: 0 })
		);
		expect(app.__registrySize()).toBe(0);

		// A browser-claimed touch (pointercancel under touch-action:auto) also drains.
		root().dispatchEvent(pointerEvent('pointerdown', 300, 300, { pointerType: 'touch', pointerId: 8 }));
		expect(app.__registrySize()).toBe(1);
		root().dispatchEvent(pointerEvent('pointercancel', 300, 300, { pointerType: 'touch', pointerId: 8 }));
		expect(app.__registrySize()).toBe(0);
	});

	it('a pointercancel STORM during a mouse drag leaves no leak and never ends the mouse gesture', () => {
		const app = mountViewer() as unknown as RegistrySized;
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		// The mouse (id 1) owns the drag.
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500, { pointerId: 1 }));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500, { pointerId: 1 }));
		flushSync();
		expect(panX()).toBeCloseTo(100);

		// A storm of browser-claimed touches arrive and each cancels. Every delete runs
		// FIRST, before the owner guard, so none leaks AND none ends the mouse drag.
		for (const id of [2, 3, 4, 5]) {
			root().dispatchEvent(pointerEvent('pointerdown', 200, 200, { pointerType: 'touch', pointerId: id }));
		}
		expect(app.__registrySize()).toBe(5); // the mouse owner + four live touches
		for (const id of [2, 3, 4, 5]) {
			root().dispatchEvent(pointerEvent('pointercancel', 200, 200, { pointerType: 'touch', pointerId: id }));
		}
		// The foreign touches drained; only the mouse owner remains.
		expect(app.__registrySize()).toBe(1);
		// The mouse drag is intact — its next move still pans...
		root().dispatchEvent(pointerEvent('pointermove', 700, 500, { pointerId: 1 }));
		flushSync();
		expect(panX()).toBeCloseTo(200);
		// ...and the mouse up drains the last entry.
		root().dispatchEvent(pointerEvent('pointerup', 700, 500, { pointerId: 1, buttons: 0 }));
		expect(app.__registrySize()).toBe(0);
	});

	it('a TOUCH tap on chrome arms nothing (no capture, no pan) and drains', () => {
		const app = mountViewer() as unknown as RegistrySized;
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		const closeBtn = root().querySelector<HTMLButtonElement>('.lightbox-close')!;
		// A touch press on the close chrome bubbles to the backdrop handler: it enters
		// the registry, arms no drag, takes no capture, and the up drains it — the
		// chrome's own click is left free to fire.
		closeBtn.dispatchEvent(pointerEvent('pointerdown', 20, 20, { pointerType: 'touch', pointerId: 3 }));
		expect(app.__registrySize()).toBe(1);
		closeBtn.dispatchEvent(pointerEvent('pointerup', 20, 20, { pointerType: 'touch', pointerId: 3, buttons: 0 }));
		flushSync();
		expect(captured).toEqual([]);
		expect(panX()).toBeCloseTo(0); // the chrome press engaged no pan on the stage
		expect(app.__registrySize()).toBe(0);
	});

	it("a stale armed owner's missed off-root pointerup is reconciled out by the next press", () => {
		const app = mountViewer() as unknown as RegistrySized;
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		// A pen arms a drag (id 2) but its pointerup fires OFF-ROOT before capture — no
		// up is ever delivered, so it can't self-delete. The entry is now in the registry.
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500, { pointerType: 'pen', pointerId: 2 }));
		expect(app.__registrySize()).toBe(1);
		// A fresh mouse press (id 1) supersedes the stale armed gesture and reconciles
		// the phantom pen entry out — leaving only the live pointer, not two.
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500, { pointerId: 1 }));
		expect(app.__registrySize()).toBe(1);
	});

	// ── gesture-state hygiene (round 1) ──
	it('a wheel DURING a captured drag does not snap the pan back (rebases the baseline)', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 700, 500)); // captured, x = 200
		flushSync();
		expect(panX()).toBeCloseTo(200);

		// A wheel arrives mid-drag, anchored at the stage CENTRE (500) — distinct
		// from the drag position — so it moves the pan to a value the pre-wheel
		// origin+delta does NOT reproduce. The baseline must rebase to the wheel's
		// pointer position (500) and post-wheel pan, so a following pointermove to
		// that same point (zero net delta) leaves the transform where the wheel put
		// it. Without the rebase, that move snaps the pan back to the pre-wheel
		// origin (0) — a visible jump.
		wheel(root(), { clientX: 500, clientY: 500, deltaY: -1 });
		const afterWheel = transformOf();
		expect(panX()).not.toBeCloseTo(200); // the wheel genuinely moved the pan
		root().dispatchEvent(pointerEvent('pointermove', 500, 500));
		flushSync();
		expect(transformOf()).toBe(afterWheel);
	});

	it('a keyboard zoom (+) DURING a captured drag does not snap the pan back', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 700, 500)); // captured, x = 200
		flushSync();
		expect(panX()).toBeCloseTo(200);

		// `+` zooms about the stage centre mid-drag, moving the pan; the drag
		// baseline must rebase to the last pointer position (700), so a move back to
		// that point is a zero net delta and holds. Without the rebase it snaps to
		// the pre-key origin + total delta.
		expect(press('+')).toBe(true);
		const afterKey = transformOf();
		expect(panX()).not.toBeCloseTo(200);
		root().dispatchEvent(pointerEvent('pointermove', 700, 500));
		flushSync();
		expect(transformOf()).toBe(afterKey);
	});

	it('an external zoom while ARMED (before the drag threshold) rebases, so engaging does not jump', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		// Arm the gesture (pointerdown) but stay below the 4px threshold.
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		// A wheel zooms while merely ARMED (not yet dragging), anchored off-centre so
		// it moves the pan.
		wheel(root(), { clientX: 800, clientY: 500, deltaY: -1 });
		const afterWheelPan = panX();
		expect(afterWheelPan).not.toBeCloseTo(0);

		// Cross the threshold with a small move from the WHEEL pointer position: the
		// engage must continue from the post-wheel pan (armed rebase), NOT snap to
		// origin(0) + the full delta from the original press point.
		root().dispatchEvent(pointerEvent('pointermove', 810, 500));
		flushSync();
		expect(captured).toContain(1);
		expect(panX()).toBeCloseTo(afterWheelPan + 10);
	});

	it('a stale armed gesture (missed pointerup) does not resume as a phantom drag on a later control press', () => {
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		// Arm a gesture whose pointerup never arrives (the pointer left the root
		// before the drag threshold, so there was no capture to deliver it).
		root().dispatchEvent(pointerEvent('pointerdown', 100, 100));
		// A later press lands on a control — excluded from starting a pan, and must
		// also clear the stale arm so the following move can't engage from a dead
		// baseline.
		const close = closeButton();
		close.dispatchEvent(pointerEvent('pointerdown', 20, 20));
		close.dispatchEvent(pointerEvent('pointermove', 300, 300)); // far past threshold
		flushSync();
		expect(captured).toEqual([]);
	});

	it('a double-click on a nav control does NOT also toggle zoom', () => {
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		mockGeometry(root(), OVERFLOW_G);
		const next = root().querySelector<HTMLButtonElement>('.lightbox-nav.next')!;
		expect(scaleOf()).toBe(1);
		next.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 20, clientY: 20 }));
		flushSync();
		expect(scaleOf()).toBe(1); // the toggle stood down for the control
	});

	it('a mid-capture ABORT leaves the SAME viewer able to start a clean next gesture', () => {
		const dialog = document.body.appendChild(document.createElement('dialog'));
		mockOpenModals([]); // establish :modal support with nothing open
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);

		mockOpenModals([dialog]); // blocked → the next move aborts
		root().dispatchEvent(pointerEvent('pointermove', 900, 500));
		flushSync();
		expect(released).toContain(1);
		mockOpenModals([]); // unblocked; same viewer frontmost again

		// A fresh gesture engages cleanly — no stuck `dragging` / capture from abort.
		captured.length = 0;
		released.length = 0;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500));
		flushSync();
		expect(captured).toContain(1);
		root().dispatchEvent(pointerEvent('pointerup', 560, 500, { buttons: 0 }));
		expect(released).toContain(1);
	});

	it('a buttons-released move after a drag releases capture, not leaking into the next gesture', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);

		// A move with the primary button no longer held — a pointerup we never saw.
		root().dispatchEvent(pointerEvent('pointermove', 650, 500, { buttons: 0 }));
		flushSync();
		expect(released).toContain(1);

		captured.length = 0;
		released.length = 0;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500));
		flushSync();
		expect(captured).toContain(1);
	});

	it('lostpointercapture ends the gesture; the next one starts clean', () => {
		mountViewer();
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(captured).toContain(1);

		root().dispatchEvent(pointerEvent('lostpointercapture', 600, 500));
		flushSync();

		captured.length = 0;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500));
		flushSync();
		expect(captured).toContain(1);
	});

	it('a press ON a control (close / nav) never starts a pan', () => {
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		mockGeometry(root(), OVERFLOW_G);
		zoomToActual();
		const close = closeButton();
		// Press on the control, then move well past the threshold: a drag OFF a
		// button must not engage a pan (its own click stays intact).
		close.dispatchEvent(pointerEvent('pointerdown', 20, 20));
		close.dispatchEvent(pointerEvent('pointermove', 200, 200));
		flushSync();
		expect(captured).toEqual([]);
	});

	// ── the pointer entry points carry the same gates ──
	it('double-click does NOT toggle when the viewer is not frontmost; the front one does', () => {
		mountViewer();
		const back = root();
		mockGeometry(back, OVERFLOW_G);
		mountViewer();
		const front = root();
		mockGeometry(front, OVERFLOW_G);

		back.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf(back)).toBe(1); // background viewer did not toggle

		front.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf(front)).toBeCloseTo(2000 / 900); // frontmost did
	});
});

// TASK-2459 — the DR-5b loader WIRED into the viewer. The request logic is
// proven in viewerImageLoader.svelte.test.ts; these pin the wiring: the <img>
// shows the loader's URL, its decode feeds the fallback detector / upgrade, and
// the spinner / error / retry states render.

/** A LightboxImage with explicit pixel dimensions. */
function sized(id: string, alt: string, width: number, height: number, mime = 'image/png'): LightboxImage {
	return { id, alt, filename: null, mime_type: mime, size_bytes: null, width, height };
}
/** Simulate the <img> decoding at `naturalWidth x naturalHeight`, then flush. */
function fireLoad(naturalWidth: number, naturalHeight: number, scope: HTMLElement = root()): void {
	const el = scope.querySelector<HTMLImageElement>('.lightbox-image');
	if (!el) throw new Error('no image to load');
	Object.defineProperty(el, 'naturalWidth', { configurable: true, get: () => naturalWidth });
	Object.defineProperty(el, 'naturalHeight', { configurable: true, get: () => naturalHeight });
	el.dispatchEvent(new Event('load'));
	flushSync();
}

describe('Lightbox — DR-5b image loading (TASK-2459)', () => {
	it('a large image loads the THUMBNAIL first, then upgrades to the original on decode', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(imageSrc()).toContain('variant=thumb-md');
		expect(imageSrc()).toContain(IMG_A);

		// The thumb decodes at a bounded size → the background upgrade swaps in.
		fireLoad(1024, 768);
		expect(imageSrc()).toContain(IMG_A);
		expect(imageSrc()).not.toContain('variant='); // the original, no variant
	});

	it('the FALLBACK case does not upgrade (a thumb-md request served the original)', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(imageSrc()).toContain('variant=thumb-md');
		// Decoded ABOVE the thumbnail bound → it WAS the original: no second request.
		fireLoad(5000, 5000);
		expect(imageSrc()).toContain('variant=thumb-md');
	});

	it('an unknown-dimensions image requests the ORIGINAL directly (no thumb)', () => {
		mountViewer({ images: [image(IMG_A, 'a')] }); // the helper leaves dims null
		expect(imageSrc()).toContain(IMG_A);
		expect(imageSrc()).not.toContain('variant=');
	});

	it('shows a loading spinner until the image decodes, then hides it', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(root().querySelector('.lightbox-loading')).not.toBeNull();
		fireLoad(1024, 768); // thumb decoded → upgrade requested (still loading)
		expect(root().querySelector('.lightbox-loading')).not.toBeNull();
		fireLoad(5000, 5000); // original decoded → ready
		expect(root().querySelector('.lightbox-loading')).toBeNull();
	});

	it('a load error shows a retryable error; retry RE-REQUESTS and hands off focus', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		const el = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		el.dispatchEvent(new Event('error'));
		flushSync();
		expect(root().querySelector('.lightbox-error')).not.toBeNull();

		const retry = root().querySelector<HTMLButtonElement>('.lightbox-retry')!;
		// The retry control carries pointer-events:auto explicitly (the stage is off).
		expect(getComputedStyle(retry).pointerEvents).not.toBe('none');
		retry.focus();
		retry.click();
		flushSync();
		// Re-requested → loading again → the error (and its button) are gone, and
		// focus was handed off before the button disappeared (TASK-2456).
		expect(root().querySelector('.lightbox-error')).toBeNull();
		expect(document.activeElement).toBe(closeButton());
	});

	it('ABORTS on navigate: the src points at the new image, dropping the old URL', () => {
		mountViewer({
			images: [sized(IMG_A, 'a', 5000, 5000), sized(IMG_B, 'b', 800, 600, 'image/jpeg')],
		});
		expect(imageSrc()).toContain(IMG_A);
		expect(imageSrc()).toContain('variant=thumb-md');

		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);
		expect(imageSrc()).not.toContain(IMG_A); // the old URL is gone at once
		expect(imageSrc()).not.toContain('variant='); // B is small ≤1024 → original
	});

	it('retry RE-MOUNTS the <img> so a same-URL failure is actually re-requested', () => {
		mountViewer({ images: [image(IMG_A, 'a')] }); // unknown dims → original URL directly
		const before = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		const beforeSrc = before.getAttribute('src');
		before.dispatchEvent(new Event('error'));
		flushSync();
		root().querySelector<HTMLButtonElement>('.lightbox-retry')!.click();
		flushSync();
		const after = root().querySelector<HTMLImageElement>('.lightbox-image');
		// A NEW element (the load token changed the {#key}) at the SAME URL — the
		// browser re-requests even though a naive `src=''`→same-URL would no-op.
		expect(after).not.toBe(before);
		expect(after?.getAttribute('src')).toBe(beforeSrc);
	});

	it('a focused retry that unmounts on set-shrink does not strand focus on <body>', () => {
		liveProps.images = [image(IMG_A, 'a')];
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		root().querySelector<HTMLImageElement>('.lightbox-image')!.dispatchEvent(new Event('error'));
		flushSync();
		const retry = root().querySelector<HTMLButtonElement>('.lightbox-retry')!;
		retry.focus();
		expect(document.activeElement).toBe(retry);

		// The set shrinks to empty — the errored image and its retry button unmount.
		liveProps.images = [];
		flushSync();
		expect(root().querySelector('.lightbox-retry')).toBeNull();
		expect(document.activeElement).toBe(closeButton());
		expect(document.activeElement).not.toBe(document.body);
	});

	it('a LATE decode for a navigated-away image does not corrupt the current one', () => {
		mountViewer({
			images: [sized(IMG_A, 'a', 5000, 5000), sized(IMG_B, 'b', 5000, 5000, 'image/jpeg')],
		});
		const staleImg = root().querySelector<HTMLImageElement>('.lightbox-image')!; // A's element
		expect(staleImg.getAttribute('src')).toContain('variant=thumb-md');

		// Navigate to B — the id-keyed <img> remounts, detaching A's element.
		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);
		expect(imageSrc()).toContain('variant=thumb-md');

		// A's thumbnail finishes decoding LATE at a FALLBACK size (>1024) on the now
		// detached element. It must not mark B ready or suppress B's upgrade.
		Object.defineProperty(staleImg, 'naturalWidth', { configurable: true, get: () => 5000 });
		Object.defineProperty(staleImg, 'naturalHeight', { configurable: true, get: () => 5000 });
		staleImg.dispatchEvent(new Event('load'));
		flushSync();
		expect(imageSrc()).toContain(IMG_B);
		expect(imageSrc()).toContain('variant=thumb-md'); // B untouched

		// B's OWN decode still drives B's upgrade.
		fireLoad(1024, 768);
		expect(imageSrc()).toContain(IMG_B);
		expect(imageSrc()).not.toContain('variant=');
	});

	it('zoom/pan still work on the image the loader RE-MOUNTED after navigation', () => {
		// The {#key} remounts the <img> on nav, rebinding `imgEl`. The zoom reads
		// geometry from `imgEl`; a stale bind would read the detached element and
		// pan nothing.
		mountViewer({
			images: [sized(IMG_A, 'a', 5000, 5000), sized(IMG_B, 'b', 5000, 5000, 'image/jpeg')],
		});
		expect(press('ArrowRight')).toBe(true);
		expect(imageSrc()).toContain(IMG_B);

		mockGeometry(root(), OVERFLOW_G); // mocks the NEW (B's) image element
		root().dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf()).toBeCloseTo(2000 / 900); // zoom read the rebound element

		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(panX()).toBeCloseTo(100);
	});

	it('DR-16: an unsafe set renders the fallback with NO image and NO request', () => {
		mountViewer({ images: [image(IMG_A, 'svg', 'image/svg+xml')] });
		// The unsafe entry admits to the no-bytes fallback (3c-ii): no <img>, no
		// loading spinner — the arm mounts and requests nothing. Where 3c-i refused
		// it outright, 3c-ii shows the fallback, and the no-bytes invariant holds.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-loading')).toBeNull();
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
	});

	it('a same-id re-emit that FILLS dimensions re-runs the DR-5b policy', () => {
		// The load key is id + dimensions, so a re-derived set with the SAME values
		// does not reload, but an async metadata fill (unknown → sized) MUST — a
		// stale dimension is a stale policy.
		liveProps.images = [image(IMG_A, 'a')]; // unknown dims → original directly
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		expect(imageSrc()).not.toContain('variant='); // unknown → the original

		// The same id re-emits with LARGE dimensions filled in.
		liveProps.images = [sized(IMG_A, 'a', 5000, 5000)];
		flushSync();
		expect(imageSrc()).toContain('variant=thumb-md'); // policy re-ran → thumb first
	});

	it('the retry button is excluded from pan + zoom-toggle gestures over the stage', () => {
		mountViewer({ images: [sized(IMG_A, 'a', 5000, 5000)] });
		root().querySelector<HTMLImageElement>('.lightbox-image')!.dispatchEvent(new Event('error'));
		flushSync();
		mockGeometry(root(), OVERFLOW_G);
		const retry = root().querySelector<HTMLButtonElement>('.lightbox-retry')!;
		const before = scaleOf();
		// A double-click ON retry must not toggle zoom (excluded like close / nav).
		retry.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf()).toBeCloseTo(before);
		// A drag STARTING on retry must not pan the broken stage.
		retry.dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 600, 500));
		flushSync();
		expect(panX()).toBeCloseTo(0);
	});

	it('releases a detached element on unmount, and a late error on it does not clobber the live image', () => {
		// TASK-2476 hardens the detached-element handling: on unmount the `<img>`'s
		// src is CLEARED (`releaseImg`), aborting its request so it can never issue or
		// complete a same-URL late event. The GENERATION fence remains the
		// loader-level defense (unit-tested in viewerImageLoader — the A→B→A same-URL
		// stale decode/error cases); this covers the component wiring: the outgoing
		// element is released, and a late error on it cannot flip the live image.
		mountViewer({
			images: [sized(IMG_A, 'a', 800, 600), sized(IMG_B, 'b', 800, 600, 'image/jpeg')],
		});
		const firstA = root().querySelector<HTMLImageElement>('.lightbox-image')!; // E1
		expect(press('ArrowRight')).toBe(true); // → B (E1 detaches → src cleared)
		expect(press('ArrowLeft')).toBe(true); // → A again (E3 mounts, fresh)
		const liveA = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		expect(liveA).not.toBe(firstA);
		// The detached element was RELEASED — its src is gone, its request aborted.
		expect(firstA.getAttribute('src')).toBeNull();
		// The live element holds A's URL.
		expect(liveA.getAttribute('src')).toContain(IMG_A);

		fireLoad(800, 600); // the LIVE A decodes fine → ready, no error
		expect(root().querySelector('.lightbox-error')).toBeNull();

		// A late error on the released detached element must not flip the live image.
		firstA.dispatchEvent(new Event('error'));
		flushSync();
		expect(root().querySelector('.lightbox-error')).toBeNull();
	});
});

// ── TASK-2460: the mobile DR-5b cells wired into the viewer ──────────────────
// The request policy is proven in viewerImageLoader.svelte.test.ts; these pin the
// WIRING under a mobile viewport — the tap affordance renders and issues the
// request, zoom-past-fit upgrades the thumb, and zoom is disabled with no bitmap.
describe('Lightbox — mobile tap-to-load and zoom-past-fit (TASK-2460)', () => {
	beforeEach(() => {
		mobileFlag = true;
		flushSync();
	});
	afterEach(() => {
		mobileFlag = false;
		flushSync();
	});

	function tapButton(scope: HTMLElement = root()): HTMLButtonElement {
		return scope.querySelector<HTMLButtonElement>('.lightbox-tap-load')!;
	}

	it('mobile + large: NO automatic request — a named tap affordance instead', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		// The assertion that fails if the deferral is dropped: no <img>, no request.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		const tap = tapButton();
		expect(tap).not.toBeNull();
		// Named, and NOT pointer-dead under the stage's `pointer-events: none`.
		expect(tap.textContent?.trim()).toBe('Tap to load full image');
		expect(getComputedStyle(tap).pointerEvents).not.toBe('none');
	});

	it('mobile + unknown dims: also deferred (tap affordance, no request)', () => {
		mountViewer({ images: [image(IMG_A, 'unknown')] }); // helper leaves dims null
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(tapButton()).not.toBeNull();
	});

	it('TAP-ONLY leg: the tap itself issues the request (the original, no variant)', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		tapButton().click();
		flushSync();
		// The tap loaded the ORIGINAL directly (mobile large has no thumb).
		expect(imageSrc()).toContain(IMG_A);
		expect(imageSrc()).not.toContain('variant=');
		// The affordance is gone (replaced by the image it loads).
		expect(root().querySelector('.lightbox-tap-load')).toBeNull();
	});

	it('a focused tap that unmounts on load hands focus off (does not strand <body>)', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		const tap = tapButton();
		tap.focus();
		expect(document.activeElement).toBe(tap);
		tap.click();
		flushSync();
		expect(root().querySelector('.lightbox-tap-load')).toBeNull();
		expect(document.activeElement).not.toBe(document.body);
	});

	it('the placeholder takes the image aspect ratio when known, a neutral box otherwise', () => {
		// Genuinely LARGE (> 8 MP) so it is the deferred cell, not the thumb cell.
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 2500)] });
		expect(tapButton().getAttribute('style')).toContain('aspect-ratio: 5000 / 2500');
		unmount(mounted.pop()!);

		mountViewer({ images: [image(IMG_B, 'unknown')] });
		const style = tapButton().getAttribute('style') ?? '';
		expect(style).not.toContain('aspect-ratio');
		expect(style).toContain('height'); // a neutral, explicitly-sized box
	});

	it('mobile thumb cell: zoom-past-fit upgrades to the original exactly once', () => {
		mountViewer({ images: [sized(IMG_A, 'wide', 2000, 100)] });
		expect(imageSrc()).toContain('variant=thumb-md'); // thumb painted, no auto-upgrade
		fireLoad(1024, 51);
		expect(imageSrc()).toContain('variant=thumb-md'); // mobile: stays the thumb

		// Zoom past fit — the same element is reused (no flash), src swaps to original.
		const thumbEl = root().querySelector<HTMLImageElement>('.lightbox-image');
		expect(press('+')).toBe(true);
		expect(scaleOf()).toBeGreaterThan(1); // actually past fit
		expect(imageSrc()).toContain(IMG_A);
		expect(imageSrc()).not.toContain('variant='); // the original
		expect(root().querySelector('.lightbox-image')).toBe(thumbEl); // reused, no remount

		// Further zoom steps do NOT re-request — one fetch, not one per step.
		expect(press('+')).toBe(true);
		expect(imageSrc()).not.toContain('variant=');
		expect(root().querySelector('.lightbox-image')).toBe(thumbEl);
	});

	it('a zoom-past-fit made BEFORE the thumb paints upgrades the moment it paints', () => {
		mountViewer({ images: [sized(IMG_A, 'wide', 2000, 100)] });
		expect(imageSrc()).toContain('variant=thumb-md'); // loading, not yet decoded
		// Zoom past fit while still loading — no original yet (the trigger waits for
		// a painted bitmap).
		expect(press('+')).toBe(true);
		expect(scaleOf()).toBeGreaterThan(1);
		expect(imageSrc()).toContain('variant=thumb-md'); // no upgrade before paint

		// The thumb paints while ALREADY zoomed past fit → the original is fetched
		// now, with no further gesture, so the user is never stranded on the thumb.
		fireLoad(1024, 51);
		expect(imageSrc()).not.toContain('variant=');
	});

	it('zoom is DISABLED while nothing is decoded (the deferred cell): keys do nothing', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(root().querySelector('.lightbox-image')).toBeNull();
		// The zoom keys are consumed (owned by the modal) but inert — no image loads,
		// the affordance stays, and nothing throws.
		expect(press('0')).toBe(true);
		expect(press('+')).toBe(true);
		expect(press('-')).toBe(true);
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(tapButton()).not.toBeNull();
	});

	it('a drag over the deferred placeholder is inert: it captures nothing', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		// The pan handlers live on the backdrop and `setPointerCapture` there once a
		// drag engages past threshold. WITHOUT the `bitmapPresent` gate the move below
		// would arm a drag and capture; the gate keeps it fully inert. Spying on the
		// capture makes this regression-sensitive (a passed pointerup would otherwise
		// clear `dragging` and mask a missing gate).
		// jsdom has no `setPointerCapture` (the production code try/catches it), so
		// install a mock fn to observe the attempt rather than spying a missing method.
		const captureSpy = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = captureSpy;
		root().dispatchEvent(pointerEvent('pointerdown', 100, 100));
		root().dispatchEvent(pointerEvent('pointermove', 320, 100)); // well past threshold
		flushSync();
		expect(captureSpy).not.toHaveBeenCalled();
		root().dispatchEvent(pointerEvent('pointerup', 320, 100));
		flushSync();
		// Nothing loaded; the affordance is intact and the tap still issues the request.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(tapButton()).not.toBeNull();
		tapButton().click();
		flushSync();
		expect(imageSrc()).toContain(IMG_A);
	});

	it('V1: a TOUCH tap on the deferred affordance keeps first-tap priority (it loads)', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		const tap = tapButton();
		expect(tap).not.toBeNull();
		// A touch press on the affordance bubbles to the backdrop handler: in V1 it
		// enters the registry but arms NOTHING and takes NO capture (the deferred cell
		// has no bitmap anyway), so it does not swallow or pre-empt the tap. The tap's
		// own click then issues the request — first-tap priority preserved.
		tap.dispatchEvent(pointerEvent('pointerdown', 100, 100, { pointerType: 'touch', pointerId: 9 }));
		tap.dispatchEvent(pointerEvent('pointerup', 100, 100, { pointerType: 'touch', pointerId: 9, buttons: 0 }));
		flushSync();
		expect(root().querySelector('.lightbox-image')).toBeNull(); // the tap-move alone loaded nothing
		tap.click(); // the synthesized click the tap produces
		flushSync();
		expect(imageSrc()).toContain(IMG_A);
		expect(root().querySelector('.lightbox-tap-load')).toBeNull(); // replaced by the image it loaded
	});

	it('mobile→desktop breakpoint flip does NOT retro-fetch: the affordance stays', () => {
		mountViewer({ images: [sized(IMG_A, 'big', 5000, 5000)] });
		expect(tapButton()).not.toBeNull();
		// Flip to desktop AFTER the load captured mobile — the reactive flip now
		// really invalidates `platform`, and the load effect (which untracks it) must
		// still not re-run, so nothing is fetched and the affordance stays.
		mobileFlag = false;
		flushSync();
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector('.lightbox-tap-load')).not.toBeNull();
	});
});

// ── TASK-2492 — the AM-3 Lightbox-owned phone sheet layout ───────────────────
//
// WHAT JSDOM CANNOT PROVE, and is therefore T7's browser suite:
//  • The bottom-anchored GEOMETRY — the dock sitting at the bottom edge and the
//    stage filling the space above it — needs a layout engine.
//  • The pan/zoom BOUNDS adapting to the smaller mobile stage (`readGeometry`
//    over the resized box).
//  • The `@media (forced-colors: active)` sheet-chrome fill/border being visible.
//  • The DR-18 `@media (max-width: 768px)` label reveal on the phone.
//  • Real TOUCH: native pinch still available (no `touch-action` change), no
//    pointer capture on the sheet chrome, two-pointer input not swallowed — the
//    3d handoff criterion.
// The class is the SELECTION mechanism (a DOM fact) and the whole assertable
// surface here: jsdom's getComputedStyle does NOT apply `<style>`-element rules
// (the existing tap-to-load `pointer-events` assertions only ever assert
// `.not.toBe(...)`, which passes on the empty string jsdom returns), so the CSS
// layout itself — flex-direction, docking, `position: static` — is unassertable
// and is named above for T7. What IS assertable, and is the byte-identical
// guarantee, is that every sheet rule is scoped under `.lightbox-sheet`: with the
// class ABSENT on desktop no sheet rule can match, so the desktop layout is
// untouched by construction.
describe('Lightbox — mobile sheet layout (3c-ii T5 / AM-3)', () => {
	afterEach(() => {
		// Every test here drives `mobileFlag`; make sure it can't leak to a later suite.
		mobileFlag = false;
		flushSync();
	});

	it('selects the sheet class ONLY on mobile (desktop path carries no sheet rule)', () => {
		mobileFlag = false;
		flushSync();
		mountViewer();
		// Desktop: no sheet class → none of the `.lightbox-sheet`-scoped rules match,
		// so the desktop layout is byte-identical by construction.
		expect(hasSheet()).toBe(false);
		unmount(mounted.pop()!);

		mobileFlag = true;
		flushSync();
		mountViewer();
		expect(hasSheet()).toBe(true);
	});

	it('re-lays-out the SAME instance on a mid-open flip — zoom + element identity survive', () => {
		mobileFlag = false;
		flushSync();
		mountViewer();
		const rootBefore = root();
		const imgBefore = rootBefore.querySelector('.lightbox-image');
		expect(imgBefore).not.toBeNull();
		expect(hasSheet(rootBefore)).toBe(false);
		// Zoom past fit so there is real state to survive the re-layout.
		expect(press('+')).toBe(true);
		const scaledTo = scaleOf(rootBefore);
		expect(scaledTo).toBeGreaterThan(1);
		const srcBefore = imageSrc(rootBefore);

		// Flip to mobile MID-OPEN.
		mobileFlag = true;
		flushSync();

		// No remount: still exactly one viewer, the SAME root element, now sheet-classed.
		expect(roots()).toHaveLength(1);
		expect(root()).toBe(rootBefore);
		expect(hasSheet()).toBe(true);
		// The SAME <img> element AND the SAME src — the load effect untracks
		// `platform`, so a flip neither reloads (src unchanged) nor re-keys the stage
		// (element identity) — and the zoom transform is intact.
		expect(root().querySelector('.lightbox-image')).toBe(imgBefore);
		expect(imageSrc()).toBe(srcBefore);
		expect(scaleOf()).toBe(scaledTo);

		// ...and back to desktop: same instance again, sheet gone, zoom still there.
		mobileFlag = false;
		flushSync();
		expect(roots()).toHaveLength(1);
		expect(root()).toBe(rootBefore);
		expect(hasSheet()).toBe(false);
		expect(root().querySelector('.lightbox-image')).toBe(imgBefore);
		expect(imageSrc()).toBe(srcBefore);
		expect(scaleOf()).toBe(scaledTo);
	});

	it('keeps the docked chrome excluded from pan, wheel-zoom AND double-click', () => {
		// The docked toolbar/meta are the SAME elements with the SAME classes, so they
		// stay in all THREE gesture-exclusion `.closest()` lists (pointerdown /
		// dblclick / WHEEL — the classic miss). Exercised with a real raster bitmap so
		// each gesture otherwise COULD act; a live control at the end proves the
		// handlers are armed and it is the exclusions that held.
		mobileFlag = true;
		flushSync();
		// A SMALL sized image so the mobile THUMB cell renders a real <img> (a large or
		// unknown-dimension image defers to tap-to-load, leaving nothing to zoom).
		mountViewer({ images: [sized(IMG_A, 'wide', 2000, 100)] });
		const toolbar = root().querySelector<HTMLElement>('.lightbox-toolbar')!;
		const meta = root().querySelector<HTMLElement>('.lightbox-meta')!;
		expect(toolbar).not.toBeNull();
		expect(meta).not.toBeNull();

		// (1) pointerdown on the docked toolbar does not arm/capture a pan.
		const captureSpy = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = captureSpy;
		toolbar.dispatchEvent(pointerEvent('pointerdown', 100, 700));
		root().dispatchEvent(pointerEvent('pointermove', 340, 700)); // well past threshold
		flushSync();
		expect(captureSpy).not.toHaveBeenCalled();

		// (2) a wheel over the docked meta is CONSUMED (the modal owns the wheel) but
		//     does NOT zoom the image behind it.
		expect(scaleOf()).toBe(1);
		expect(wheel(meta, { deltaY: -120 })).toBe(true);
		expect(scaleOf()).toBe(1);

		// (3) a double-click on the docked toolbar does NOT toggle fit↔actual.
		toolbar.dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 100, clientY: 700 }));
		flushSync();
		expect(scaleOf()).toBe(1);
		// CONTROL for (3): the SAME double-click over the backdrop DOES toggle to
		// actual size — the dblclick handler is live, so the toolbar exclusion is what
		// held (removing the handler would leave scale at 1 and pass without this).
		root().dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 100, clientY: 100 }));
		flushSync();
		expect(scaleOf()).toBeGreaterThan(1);
		expect(press('0')).toBe(true); // back to fit for the wheel leg
		expect(scaleOf()).toBe(1);

		// CONTROL for (2): the SAME wheel over the backdrop DOES zoom — the wheel
		// handler is live, so the meta exclusion is what held (not a dead wheel path).
		expect(wheel(root(), { deltaY: -120 })).toBe(true);
		expect(scaleOf()).toBeGreaterThan(1);
	});

	it('does not dismiss on a click of the docked chrome (target is not the backdrop)', () => {
		mobileFlag = true;
		flushSync();
		const onClose = vi.fn();
		mountViewer({ onClose });
		root()
			.querySelector<HTMLElement>('.lightbox-meta')!
			.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		root()
			.querySelector<HTMLElement>('.lightbox-toolbar')!
			.dispatchEvent(new MouseEvent('click', { bubbles: true }));
		flushSync();
		expect(onClose).not.toHaveBeenCalled();
	});
});

describe('Lightbox — the modal contract holds UNDER the mobile sheet (3c-ii T5)', () => {
	// The contract is layout-independent: every invariant the desktop suites assert
	// must hold identically with the sheet class on. Re-run the load-bearing ones
	// under the mobile viewport mock.
	beforeEach(() => {
		mobileFlag = true;
		flushSync();
	});
	afterEach(() => {
		mobileFlag = false;
		flushSync();
	});

	it('is still an aria-modal dialog portaled to <body>, carrying both marker classes', () => {
		mountViewer();
		expect(hasSheet()).toBe(true);
		expect(root().getAttribute('role')).toBe('dialog');
		expect(root().getAttribute('aria-modal')).toBe('true');
		expect(root().parentElement).toBe(document.body);
		expect(root().classList.contains(VIEWER_ROOT_CLASS)).toBe(true);
	});

	it('still moves focus to the close button on open', () => {
		mountViewer({ images: [image(IMG_A, 'first'), image(IMG_B, 'second')] });
		expect(document.activeElement).toBe(closeButton());
	});

	it('still traps Tab — wraps forward off the last focusable', () => {
		mountViewer({ images: [image(IMG_A, 'first'), image(IMG_B, 'second')] });
		lastFocusable().focus();
		expect(press('Tab')).toBe(true);
		expect(document.activeElement).toBe(closeButton());
	});

	it('still owns Escape through the shared stack (no local window branch)', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		// A raw window keydown does NOT close — the stack is the sole owner.
		expect(press('Escape')).toBe(false);
		expect(onClose).not.toHaveBeenCalled();
		expect(runTopEscape()).toBe(true);
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('still closes on a backdrop click', () => {
		const onClose = vi.fn();
		mountViewer({ onClose });
		root().dispatchEvent(new MouseEvent('click', { bubbles: true }));
		flushSync();
		expect(onClose).toHaveBeenCalledTimes(1);
	});
});

// ── TASK-2461 final-pass fixes ──────────────────────────────────────────────
describe('Lightbox — re-clamp on same-id decode + inert error state (TASK-2461)', () => {
	it('re-clamps a stranded scale when a same-id reload decodes a SMALLER bitmap', () => {
		// A same-id dimension fill reloads the image with a smaller bitmap (a large
		// original swapped for a thumb), lowering actualScale and MAX_SCALE. The reset
		// effect fires only on image CHANGE and the ResizeObserver only on stage
		// resize — neither catches this; the DECODE (onload) must re-clamp.
		mountViewer();
		const image = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		const stage = root().querySelector<HTMLElement>('.lightbox-stage')!;
		// Big bitmap fitted small → actualScale 10, ceiling 40 — lots of headroom.
		let fitted = 100;
		let natural = 1000;
		Object.defineProperty(image, 'offsetWidth', { configurable: true, get: () => fitted });
		Object.defineProperty(image, 'offsetHeight', { configurable: true, get: () => fitted });
		Object.defineProperty(image, 'naturalWidth', { configurable: true, get: () => natural });
		Object.defineProperty(image, 'naturalHeight', { configurable: true, get: () => natural });
		Object.defineProperty(stage, 'clientWidth', { configurable: true, get: () => fitted });
		Object.defineProperty(stage, 'clientHeight', { configurable: true, get: () => fitted });

		for (let i = 0; i < 8; i++) press('+');
		expect(scaleOf(), 'zoomed well past the eventual ceiling').toBeGreaterThan(5);

		// The reload decodes a 1:1 bitmap: actualScale → 1, ceiling → 4. The stranded
		// scale (~5.96) must be pulled back to 4 by the onload re-clamp.
		natural = fitted;
		image.dispatchEvent(new Event('load'));
		flushSync();
		expect(scaleOf(), 'the decode re-clamped the stranded scale to the new max').toBeCloseTo(4);
	});

	it('disables zoom keys and drag in the ERROR state, and re-enables them after a successful retry', () => {
		mountViewer({ images: [sized(IMG_A, 'a', 5000, 5000)] });
		mockGeometry(root(), OVERFLOW_G);
		fireLoad(1024, 768); // thumb decoded → ready, background original in flight
		expect(press('+')).toBe(true);
		const zoomed = scaleOf();
		expect(zoomed, 'zoom works with a decoded bitmap').toBeGreaterThan(1);

		// The original FAILS → error state. `errored()` flips only the phase;
		// `displaySrc` stays set, so without the phase guard `bitmapPresent` would
		// still be true and gestures would act over the error UI.
		root().querySelector<HTMLImageElement>('.lightbox-image')!.dispatchEvent(new Event('error'));
		flushSync();
		expect(root().querySelector('.lightbox-error')).not.toBeNull();

		// ALL zoom gestures inert — the transform does not move: keys, wheel AND
		// double-click (each of which the broken <img>'s geometry would otherwise let
		// through).
		press('+');
		press('0');
		wheel(root(), { clientX: 500, clientY: 500, deltaY: -1 });
		root().dispatchEvent(new MouseEvent('dblclick', { bubbles: true, clientX: 500, clientY: 500 }));
		flushSync();
		expect(scaleOf(), 'keys / wheel / dblclick are inert in the error state').toBeCloseTo(zoomed);
		// DRAG inert — a pointerdown does not arm/capture.
		const capture = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = capture;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 700, 500));
		flushSync();
		expect(capture, 'drag does not arm in the error state').not.toHaveBeenCalled();

		// CONTROL — a successful retry re-enables gestures.
		root().querySelector<HTMLButtonElement>('.lightbox-retry')!.click();
		flushSync();
		mockGeometry(root(), OVERFLOW_G); // the retry remounted the <img>
		fireLoad(2000, 2000); // the original re-decodes → ready
		expect(press('+')).toBe(true);
		expect(scaleOf(), 'zoom works again after a successful retry').toBeGreaterThan(zoomed);
	});

	it('aborts a LIVE drag when the bitmap vanishes mid-drag (background original fails)', () => {
		mountViewer({ images: [sized(IMG_A, 'a', 5000, 5000)] });
		mockGeometry(root(), OVERFLOW_G);
		fireLoad(1024, 768); // thumb ready; background original in flight
		zoomToActual(); // pan room
		const capture = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = capture;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500)); // engage the drag
		flushSync();
		expect(capture, 'the drag armed on a decoded bitmap').toHaveBeenCalled();
		const panned = panX();

		// The original FAILS mid-drag → error. A further move must ABORT, not pan the
		// broken UI.
		root().querySelector<HTMLImageElement>('.lightbox-image')!.dispatchEvent(new Event('error'));
		flushSync();
		root().dispatchEvent(pointerEvent('pointermove', 720, 500));
		flushSync();
		expect(panX(), 'the drag aborted; no further pan over the error UI').toBeCloseTo(panned);
	});
});

describe('Lightbox — action toolbar (TASK-2474)', () => {
	// The shared descriptor list, drawn over the stage. The viewer only ever holds
	// IMAGES, so Open/Download/Copy-link always apply; Delete is gated on the
	// host's `mutationsEnabled`. `api.attachments.delete` is NOT mocked in this
	// file, so these cases stop at the confirmation GATE (open / cancel / abandon)
	// — the full delete network path is the strip/host origin tests' (which mock
	// it). Here the module's own state machine is what is under test.
	function toolbar(scope: HTMLElement = root()): HTMLElement | null {
		return scope.querySelector<HTMLElement>('.lightbox-toolbar');
	}
	function tools(scope: HTMLElement = root()): HTMLElement[] {
		return Array.from(scope.querySelectorAll<HTMLElement>('.lightbox-tool'));
	}
	function toolLabels(scope: HTMLElement = root()): string[] {
		return tools(scope).map((t) => t.getAttribute('aria-label') ?? '');
	}
	function tool(label: string, scope: HTMLElement = root()): HTMLElement {
		const found = tools(scope).find((t) => t.getAttribute('aria-label') === label);
		if (!found) throw new Error(`no toolbar control labelled ${label}`);
		return found;
	}

	beforeEach(() => {
		Object.assign(toolbarProps, {
			images: [image(IMG_A, 'a diagram')],
			index: 0,
			wsSlug: 'ws-one',
			onClose: () => {},
			mutationsEnabled: true,
			getItemContent: undefined,
			getLiveContent: undefined,
		});
	});

	it('renders the read-only actions with no Delete when mutations are disabled', () => {
		mountViewer({ mutationsEnabled: false });
		expect(toolbar()).not.toBeNull();
		const labels = toolLabels();
		expect(labels).toContain('Open in new tab');
		expect(labels).toContain('Download');
		expect(labels).toContain('Copy workspace link');
		expect(labels).not.toContain('Delete');
	});

	it('defaults to read-only (no Delete) when mutationsEnabled is omitted', () => {
		mountViewer();
		expect(toolbar()).not.toBeNull();
		expect(toolLabels()).not.toContain('Delete');
	});

	it('offers Delete when the host granted mutationsEnabled', () => {
		mountViewer({ mutationsEnabled: true });
		expect(toolLabels()).toContain('Delete');
	});

	it('renders Open and Download as real anchors carrying href/download/target', () => {
		mountViewer({ mutationsEnabled: false, images: [image(IMG_A, 'a diagram')] });
		const open = tool('Open in new tab');
		const download = tool('Download');
		expect(open.tagName).toBe('A');
		expect(open.getAttribute('href')).toContain(`/workspaces/ws-one/attachments/${IMG_A}`);
		expect(open.getAttribute('target')).toBe('_blank');
		expect(open.getAttribute('rel')).toBe('noopener noreferrer');
		expect(download.tagName).toBe('A');
		expect(download.getAttribute('href')).toContain(`/workspaces/ws-one/attachments/${IMG_A}`);
		// A REAL download attribute, not decoration — the value is the display name.
		expect(download.hasAttribute('download')).toBe(true);
	});

	it('renders Copy-link and Delete as buttons, not anchors', () => {
		mountViewer({ mutationsEnabled: true });
		expect(tool('Copy workspace link').tagName).toBe('BUTTON');
		expect(tool('Delete').tagName).toBe('BUTTON');
	});

	it('drills down to the shared confirmation and back on Cancel (DR-18)', () => {
		mountViewer({ mutationsEnabled: true });
		tool('Delete').click();
		flushSync();
		// The action buttons are replaced by the shared confirm rows.
		const confirm = root().querySelector('.lightbox-delete-confirm');
		expect(confirm).not.toBeNull();
		expect(tools()).toHaveLength(0);
		// MenuItem rows carry a leading glyph in their text, so match on substrings.
		const rows = Array.from(confirm!.querySelectorAll<HTMLElement>('button')).map(
			(b) => b.textContent?.trim() ?? ''
		);
		expect(rows.some((r) => r.includes('Cancel'))).toBe(true);
		expect(rows.some((r) => r.includes('Delete file'))).toBe(true);
		// The hedged (not-referenced) arm, since no content getter was threaded.
		expect(confirm!.querySelector('.attachment-delete-prompt')?.textContent).toContain(
			'may still be referenced'
		);

		// Cancel returns to the toolbar without deleting anything.
		Array.from(confirm!.querySelectorAll('button'))
			.find((b) => b.textContent?.includes('Cancel'))!
			.click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).toBeNull();
		expect(toolLabels()).toContain('Delete');
	});

	it('warns about a live reference when the body still uses the attachment', () => {
		// The delete-warning check reads the live getter at confirm time (DR-5).
		mountViewer({
			mutationsEnabled: true,
			getLiveContent: () => `text ![x](pad-attachment:${IMG_A}) more`,
		});
		tool('Delete').click();
		flushSync();
		expect(root().querySelector('.attachment-delete-prompt')?.textContent).toContain(
			"still used in this item's content"
		);
	});

	it('abandons an open confirmation when mutation permission is withdrawn', () => {
		// A pane going peeked closes the mutation gate mid-confirmation; the shared
		// module abandons the drill-down rather than leaving a live Delete button for
		// an action that can no longer happen (DR-8). Driven through a reactive prop.
		mounted.push(mount(Lightbox, { target: appRoot, props: toolbarProps }));
		flushSync();
		tool('Delete').click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();

		toolbarProps.mutationsEnabled = false;
		flushSync();
		// The confirmation is gone AND Delete is no longer offered.
		expect(root().querySelector('.lightbox-delete-confirm')).toBeNull();
		expect(toolLabels()).not.toContain('Delete');
	});

	it('shows no toolbar when the set is empty', () => {
		// A genuinely empty set leaves no `img`, so no toolbar. (An unresolved-MIME
		// entry no longer empties the set — 3c-ii admits it to the fallback, which
		// DOES carry a toolbar; see the file-route describe.)
		mountViewer({ images: [] });
		expect(toolbar()).toBeNull();
	});

	// ── Codex-review fixes (TASK-2474) ────────────────────────────────────────

	it('renders the drill-down as a role="menu" of role="menuitem" rows', () => {
		// The confirm rows are role="menuitem" (from MenuItem); they must be parented
		// by role="menu", NOT the actions' role="toolbar".
		mountViewer({ mutationsEnabled: true });
		tool('Delete').click();
		flushSync();
		const menu = root().querySelector('.lightbox-delete-confirm');
		expect(menu?.getAttribute('role')).toBe('menu');
		expect(menu?.querySelectorAll('[role="menuitem"]').length).toBeGreaterThanOrEqual(2);
		// ...and the toolbar's role="toolbar" is gone while confirming (not nesting a
		// menu inside a toolbar).
		expect(root().querySelector('[role="toolbar"]')).toBeNull();
	});

	it('moves focus to the first drill-down row and rovers the tab stop', () => {
		mountViewer({ mutationsEnabled: true });
		tool('Delete').click();
		flushSync();
		const rows = Array.from(
			root().querySelectorAll<HTMLElement>('.lightbox-delete-confirm [role="menuitem"]')
		);
		// Cancel is first (the shared confirm's order), and focus lands on it.
		expect(document.activeElement).toBe(rows[0]);
		// Roving tabindex: exactly the active row is in the tab order, the rest are
		// -1 — so Tab EXITS the menu to the chrome (ARIA menu), not between rows.
		expect(rows[0].tabIndex).toBe(0);
		expect(rows.slice(1).every((r) => r.tabIndex === -1)).toBe(true);
	});

	it('Up/Down move focus between drill-down rows and roll the tab stop', () => {
		mountViewer({ mutationsEnabled: true });
		tool('Delete').click();
		flushSync();
		const rows = Array.from(
			root().querySelectorAll<HTMLElement>('.lightbox-delete-confirm [role="menuitem"]')
		);
		expect(document.activeElement).toBe(rows[0]);
		expect(press('ArrowDown')).toBe(true);
		expect(document.activeElement).toBe(rows[1]);
		expect(rows[1].tabIndex).toBe(0);
		expect(rows[0].tabIndex).toBe(-1);
	});

	it('Escape backs out of the drill-down before it closes the viewer', () => {
		const onClose = vi.fn();
		mounted.push(
			mount(Lightbox, { target: appRoot, props: { ...toolbarProps, onClose } })
		);
		flushSync();
		tool('Delete').click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();

		// First Escape cancels the confirmation; the viewer stays open.
		expect(runTopEscape()).toBe(true);
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).toBeNull();
		expect(onClose).not.toHaveBeenCalled();

		// Second Escape closes the viewer.
		expect(runTopEscape()).toBe(true);
		flushSync();
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('abandons the drill-down when the shown image changes under it', () => {
		// A delete elsewhere shrinks the set so a DIFFERENT member is shown; a
		// confirmation for the gone image must not linger over the new one.
		Object.assign(toolbarProps, {
			images: [image(IMG_A, 'first'), image(IMG_B, 'second')],
		});
		mounted.push(mount(Lightbox, { target: appRoot, props: toolbarProps }));
		flushSync();
		tool('Delete').click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();

		// The shown image (A) drops out; B moves into view.
		toolbarProps.images = [image(IMG_B, 'second')];
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).toBeNull();
	});

	it('consumes a wheel over the toolbar without zooming the image', () => {
		mountViewer({ mutationsEnabled: false });
		fireLoad(2000, 2000);
		mockGeometry(root(), {
			stageW: 1000,
			stageH: 1000,
			fittedW: 900,
			fittedH: 900,
			naturalW: 2000,
			naturalH: 2000,
		});
		const before = scaleOf();

		// A wheel over a toolbar control is consumed (the modal owns the wheel) but
		// must NOT zoom.
		expect(wheel(tool('Download'), { deltaY: -100, clientX: 500, clientY: 500 })).toBe(true);
		expect(scaleOf()).toBe(before);

		// Control: the SAME wheel over the image DOES zoom — so the exclusion is what
		// stopped the toolbar wheel, not a dead wheel path.
		expect(
			wheel(root().querySelector<HTMLElement>('.lightbox-image')!, {
				deltaY: -100,
				clientX: 500,
				clientY: 500,
			})
		).toBe(true);
		expect(scaleOf()).toBeGreaterThan(before);
	});
});

describe('Lightbox — metadata header (TASK-2475)', () => {
	// Filename / type / size for the shown image, seeded from the LightboxImage and
	// completed by the B module. The metadata mock (top of file) controls the HEAD.
	function metaImage(over: Partial<LightboxImage> = {}): LightboxImage {
		return {
			id: IMG_A,
			alt: 'a diagram',
			filename: 'photo.png',
			mime_type: 'image/png',
			size_bytes: 2048,
			width: null,
			height: null,
			...over,
		};
	}
	function metaName(): string | null {
		return root().querySelector('.lightbox-meta-name')?.textContent ?? null;
	}
	function metaDetail(): string | null {
		return root().querySelector('.lightbox-meta-detail')?.textContent ?? null;
	}
	function metaError(): HTMLElement | null {
		return root().querySelector('.lightbox-meta-error');
	}
	async function settleAsync() {
		for (let i = 0; i < 5; i++) await Promise.resolve();
		flushSync();
	}

	it('renders a complete seed, and the forced open-probe (T6) fires ONCE with no-store and does not override the seed', async () => {
		// T6 always-revalidate-on-open: even a complete seed (both MIME and size) is
		// revalidated once per open with a `no-store` HEAD — the strip's old zero-probe
		// fast path is gone, deliberately. The displayed fields still come from the
		// SEED (seed-wins merge), so the header is unchanged; what changes is that a
		// probe now fires, and it is the FORCED one.
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 2048 });
		mountViewer({ images: [metaImage()] });
		await settleAsync();
		expect(metaName()).toBe('photo.png');
		expect(metaDetail()).toBe(
			`${describeAttachmentType('image/png', 'photo.png')} · ${formatBytes(2048)}`
		);
		// Exactly one probe, and it is the forced revalidation — never the plain
		// seed-fill fetch (which the old fast path would have skipped entirely).
		expect(metaRevalidate).toHaveBeenCalledTimes(1);
		expect(metaFetch).not.toHaveBeenCalled();
		// The `cache: 'no-store'` option is inspected on the OPTIONS the mock actually
		// received (4th arg), not a local constant — the browser-cache-bypass contract.
		expect(metaRevalidate.mock.calls[0][3]).toEqual({ cache: 'no-store' });
	});

	it('falls back filename → alt when the filename is null', async () => {
		mountViewer({ images: [metaImage({ filename: null })] });
		await settleAsync();
		expect(metaName()).toBe('a diagram');
	});

	it('treats a whitespace-only filename as absent and uses alt', async () => {
		mountViewer({ images: [metaImage({ filename: '   ' })] });
		await settleAsync();
		expect(metaName()).toBe('a diagram');
	});

	it('falls back to the generic label when filename AND alt are blank', async () => {
		mountViewer({ images: [metaImage({ filename: null, alt: '' })] });
		await settleAsync();
		expect(metaName()).toBe('Attachment');
	});

	it('treats a whitespace-only alt as blank too, reaching the generic label', async () => {
		// The chain blank-normalizes alt, not just filename — an all-spaces alt must
		// not win over 'Attachment' as an empty-but-present string would.
		mountViewer({ images: [metaImage({ filename: null, alt: '   ' })] });
		await settleAsync();
		expect(metaName()).toBe('Attachment');
	});

	it('omits the size half when size stays unknown — detail is type only, no "0 B"', async () => {
		// A viewer image always has a known MIME (the last-mile gate), so the
		// type-omitted branch is unreachable here; what IS reachable is a null size
		// that the fetch fails to fill. The detail must then be TYPE ONLY, with no
		// stray " · " and no "0 B" (formatBytes is never fed null). T6: the opened
		// entry's probe is the forced revalidation.
		metaRevalidate.mockResolvedValue({ status: 'transient' });
		mountViewer({ images: [metaImage({ size_bytes: null })] });
		await settleAsync();
		expect(metaName()).toBe('photo.png');
		expect(metaDetail()).toBe(describeAttachmentType('image/png', 'photo.png'));
	});

	it('fetches to fill a null size and updates the detail line', async () => {
		// T6: the opened entry is force-revalidated, so the fill arrives on that probe.
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 4096 });
		mountViewer({ images: [metaImage({ size_bytes: null })] });
		await settleAsync();
		expect(metaRevalidate).toHaveBeenCalled();
		expect(metaDetail()).toContain(formatBytes(4096));
	});

	it('a fetched field never overwrites a non-null seed', async () => {
		// Seed MIME is the image PNG; the fetch returns a VISIBLY DIFFERENT type
		// family (application/pdf) plus a size. The size fills (was null) but the type
		// must stay the seed's — fields merge seed-wins. The fetched family is chosen
		// distinct from the seed's on purpose: `image/gif` also labels as "Image", so
		// an overwrite bug would have slipped through; a PDF label would not. T6: the
		// opened entry's probe is the forced revalidation.
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'application/pdf', size: 4096 });
		mountViewer({ images: [metaImage({ filename: null, size_bytes: null })] });
		await settleAsync();
		const seededType = describeAttachmentType('image/png', null);
		expect(seededType).not.toBe(describeAttachmentType('application/pdf', null));
		expect(metaDetail()).toBe(`${seededType} · ${formatBytes(4096)}`);
	});

	it('shows a retryable error beside the name on a transient failure, then recovers', async () => {
		// T6: the opened entry's probe is the forced revalidation — so BOTH the initial
		// open probe and the Retry go through `metaRevalidate` (transient, then ok).
		metaRevalidate.mockResolvedValue({ status: 'transient' });
		mountViewer({ images: [metaImage({ size_bytes: null })] });
		await settleAsync();
		// Beside what it already knows — never a blank sheet (DR-10).
		expect(metaName()).toBe('photo.png');
		expect(metaError()).not.toBeNull();

		// Retry REVALIDATES (invalidate-then-fetch), never replays the cached failure.
		metaRevalidate.mockResolvedValue({ status: 'ok', mime: 'image/png', size: 8192 });
		const retryBtn = root().querySelector<HTMLButtonElement>('.lightbox-meta-retry')!;
		retryBtn.focus();
		retryBtn.click();
		await settleAsync();
		expect(metaRevalidate).toHaveBeenCalled();
		expect(metaError()).toBeNull();
		expect(metaDetail()).toContain(formatBytes(8192));
		// Retry unmounts on success (the transient state clears); focus must not be
		// stranded on <body> outside the modal — it is re-homed into the viewer.
		expect(document.activeElement).not.toBe(document.body);
		expect(root().contains(document.activeElement)).toBe(true);
	});

	it('ellipsizes a 200-char filename while the accessible name carries it in full', async () => {
		const long = 'a'.repeat(196) + '.png';
		expect(long.length).toBe(200);
		mountViewer({ images: [metaImage({ filename: long })] });
		await settleAsync();
		const nameEl = root().querySelector<HTMLElement>('.lightbox-meta-name')!;
		// The FULL value is in title (DR-13) and in the element text — that is the
		// info-preservation half, and it is what jsdom can prove. The VISUAL ellipsis
		// (overflow/text-overflow/nowrap on `.lightbox-meta-name`) is not applied by
		// jsdom's `getComputedStyle` (it ignores scoped `<style>` rules), so it is a
		// browser-suite concern, like every other measured-layout claim in this file.
		expect(nameEl.getAttribute('title')).toBe(long);
		expect(nameEl.textContent).toBe(long);
	});
});

describe('Lightbox — the fallback arm (TASK-2476)', () => {
	// The stage's second arm: a navigable entry the viewer cannot draw as an image
	// (an unsafe/active type that flipped or was added while open). These drive the
	// live props to reach it, since the open gate keeps unsafe entries out of the
	// initial set.
	function mountLive() {
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		return app;
	}
	function fallback(): HTMLElement | null {
		return root().querySelector('.lightbox-fallback');
	}

	it('mounts no <img>, disposes the loader AND releases the detached element on the arm flip', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		// The raster arm: an <img> holding A's src is mounted. Capture it — the flip
		// detaches it, and the no-bytes invariant requires its request released too.
		const priorImg = root().querySelector<HTMLImageElement>('.lightbox-image')!;
		expect(priorImg).not.toBeNull();
		expect(priorImg.getAttribute('src')).toContain(IMG_A);

		// A flips unsafe — SAME id and dims, only the renderer arm differs. The load
		// key carries the arm, so the load effect re-fires and DISPOSES the loader;
		// without that the safe bytes would linger behind the fallback.
		liveProps.images = [image(IMG_A, 'png', 'image/svg+xml')];
		flushSync();

		// No <img> in the tree, and the DETACHED element's src is CLEARED — its
		// native request is aborted, not left in flight until GC (releaseImg).
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(root().querySelector(`img[src*="${IMG_A}"]`)).toBeNull();
		expect(priorImg.getAttribute('src')).toBeNull();
		// The fallback is what shows instead.
		expect(fallback()).not.toBeNull();
	});

	it('renders the icon, name, type · size and "No preview available"', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		liveProps.images = [
			{
				id: IMG_A,
				alt: 'plans',
				filename: 'plans.svg',
				mime_type: 'image/svg+xml',
				size_bytes: 4096,
				width: null,
				height: null,
			},
		];
		flushSync();
		const fb = fallback()!;
		expect(fb).not.toBeNull();
		// The large family icon (AttachmentIcon renders inline SVG).
		expect(fb.querySelector('.lightbox-fallback-icon svg')).not.toBeNull();
		// The display name, full value also in title (DR-13).
		const nameEl = fb.querySelector('.lightbox-fallback-name');
		expect(nameEl?.textContent).toBe('plans.svg');
		expect(nameEl?.getAttribute('title')).toBe('plans.svg');
		// Type · size and the honest note.
		expect(fb.querySelector('.lightbox-fallback-detail')?.textContent).toContain(formatBytes(4096));
		expect(fb.textContent).toContain('No preview available');
	});

	it('is navigable and counted, paged onto with ←/→', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		liveProps.images = [image(IMG_A, 'png'), image(IMG_B, 'diagram', 'image/svg+xml')];
		flushSync();
		// Two navigable members — the counter counts the fallback entry.
		expect(root().querySelector('.lightbox-counter')?.textContent).toBe('1 / 2');
		expect(root().querySelector('.lightbox-image')?.getAttribute('alt')).toBe('png');
		press('ArrowRight');
		// Paged onto the fallback entry.
		expect(root().querySelector('.lightbox-image')).toBeNull();
		expect(fallback()).not.toBeNull();
	});

	it('disables zoom on the fallback arm — nothing to transform', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		liveProps.images = [image(IMG_A, 'png', 'image/svg+xml')];
		flushSync();
		expect(fallback()).not.toBeNull();
		// The zoom key is consumed (not leaked to the inert page) but inert — there
		// is no bitmap, and none appears.
		expect(press('+')).toBe(true);
		expect(press('0')).toBe(true);
		expect(root().querySelector('.lightbox-image')).toBeNull();
	});

	it('re-homes focus when the arm swap removes the focused raster control', () => {
		liveProps.images = [image(IMG_A, 'png')];
		mountLive();
		// Drive to the error state so the Retry button exists, and focus it.
		root().querySelector('.lightbox-image')!.dispatchEvent(new Event('error'));
		flushSync();
		const retry = root().querySelector<HTMLButtonElement>('.lightbox-retry')!;
		retry.focus();
		expect(document.activeElement).toBe(retry);

		// A flips unsafe → the raster arm (with Retry) unmounts, the fallback mounts.
		liveProps.images = [image(IMG_A, 'png', 'image/svg+xml')];
		flushSync();
		expect(root().querySelector('.lightbox-retry')).toBeNull();
		expect(fallback()).not.toBeNull();
		// Focus is not stranded on <body> outside the modal — it is re-homed.
		expect(document.activeElement).not.toBe(document.body);
		expect(root().contains(document.activeElement)).toBe(true);
	});

	it('reactively cancels a live drag when the bitmap vanishes via a prop flip', () => {
		// The pointer handlers catch a flip that arrives WITH a move/up event, but the
		// arm can flip from a PROP change while the pointer is held STILL — no event to
		// carry the abort. The reactive cancel must tear the gesture down and release
		// the capture, so a stale gesture cannot pan the reloaded image after an
		// A→unsafe→A flip.
		liveProps.images = [sized(IMG_A, 'a', 5000, 5000)];
		mountLive();
		mockGeometry(root(), OVERFLOW_G);
		fireLoad(1024, 768); // decoded → bitmapPresent true
		zoomToActual(); // pan room
		const capture = vi.fn();
		const release = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = capture;
		(root() as unknown as { releasePointerCapture: unknown }).releasePointerCapture = release;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500)); // engage the drag
		flushSync();
		expect(capture, 'the drag armed on the decoded bitmap').toHaveBeenCalled();

		// A flips unsafe via a prop change — NO further pointer event.
		liveProps.images = [sized(IMG_A, 'a', 5000, 5000, 'image/svg+xml')];
		flushSync();
		// The gesture was cancelled: the capture is released and the fallback shows.
		expect(release, 'the reactive cancel released the capture').toHaveBeenCalled();
		expect(fallback()).not.toBeNull();
	});
});

describe('Lightbox — deletion subscription (DR-5c / TASK-2477)', () => {
	// The viewer subscribes to the REAL deletion bus and reconciles by IDENTITY:
	// the shown image is tracked by id, the index derived, so a delete advances or
	// closes (never lands on a position that now names a different member). The
	// bus is real in this suite, so `notifyAttachmentDeleted` drives it directly.
	function mountLive() {
		const app = mount(Lightbox, { target: appRoot, props: liveProps });
		mounted.push(app);
		flushSync();
		return app;
	}
	function shownAlt(): string {
		const raster = root().querySelector<HTMLImageElement>('.lightbox-image');
		if (raster) return raster.getAttribute('alt') ?? '';
		return root().querySelector('.lightbox-fallback-name')?.textContent ?? '';
	}
	function counterText(): string | null {
		return root().querySelector('.lightbox-counter')?.textContent ?? null;
	}
	function del(id: string) {
		notifyAttachmentDeleted(id);
		flushSync();
	}

	it('deleting the ONLY image closes the viewer', () => {
		const onClose = vi.fn();
		mountViewer({ images: [image(IMG_A, 'a')], onClose });
		del(IMG_A);
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('deleting the SHOWN image advances to the next survivor', () => {
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg'), image(IMG_C, 'c', 'image/gif')],
		});
		expect(shownAlt()).toBe('a');
		del(IMG_A);
		// Advanced to the one that FOLLOWED A.
		expect(shownAlt()).toBe('b');
		expect(counterText()).toBe('1 / 2');
	});

	it('deleting the LAST shown image wraps to the first survivor', () => {
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg'), image(IMG_C, 'c', 'image/gif')],
		});
		press('ArrowRight');
		press('ArrowRight');
		expect(shownAlt()).toBe('c'); // on the last
		del(IMG_C);
		// Wrap-around: the deleted last advances to the first survivor.
		expect(shownAlt()).toBe('a');
		expect(counterText()).toBe('1 / 2');
	});

	it('deleting an EARLIER image keeps the SAME image shown (identity, not index)', () => {
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg'), image(IMG_C, 'c', 'image/gif')],
		});
		press('ArrowRight');
		press('ArrowRight');
		expect(shownAlt()).toBe('c'); // position 3 of 3
		del(IMG_A);
		// An index-based viewer would now show B (C shifted to index 1); identity
		// keeps C on screen.
		expect(shownAlt()).toBe('c');
		expect(counterText()).toBe('2 / 2');
	});

	it('deleting down to ONE image drops the counter and nav', () => {
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		expect(counterText()).toBe('1 / 2');
		del(IMG_A);
		expect(shownAlt()).toBe('b');
		expect(root().querySelector('.lightbox-counter')).toBeNull();
		expect(root().querySelector('.lightbox-nav')).toBeNull();
	});

	it('deleting every image (zero left) closes on the last one', () => {
		const onClose = vi.fn();
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')], onClose });
		del(IMG_A); // → advance to B, still open
		expect(onClose).not.toHaveBeenCalled();
		expect(shownAlt()).toBe('b');
		del(IMG_B); // zero left → close
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('reopening (a fresh mount) clears tombstones — a re-added image shows again', () => {
		const first = mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		del(IMG_A);
		expect(shownAlt()).toBe('b'); // A tombstoned for THIS instance
		unmount(mounted.splice(mounted.indexOf(first), 1)[0]);
		flushSync();

		// A fresh instance (every producer keys the mount) with A back in the list:
		// no cross-open tombstone leakage, so A opens.
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		expect(shownAlt()).toBe('a');
	});

	it('an OWN toolbar delete flows through the bus identically (advance)', () => {
		// mutationsEnabled + a full confirm→delete, but api.attachments.delete is not
		// mocked here — so drive the announce directly (what the descriptor does on a
		// 204) and assert the viewer reconciles it via the SAME survivor path.
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')],
			mutationsEnabled: true,
		});
		// The bus announce (own or external) is the one path.
		del(IMG_A);
		expect(shownAlt()).toBe('b');
	});

	it('deletes a non-image FALLBACK entry too', () => {
		// The survivor logic composes with D's navigable: a fallback-arm entry (an
		// unsafe MIME that flipped mid-view) is deletable, and deleting it advances.
		liveProps.images = [image(IMG_A, 'a'), image(IMG_B, 'b')];
		mountLive();
		liveProps.images = [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/svg+xml')]; // B → fallback
		flushSync();
		press('ArrowRight'); // → B, the fallback arm
		expect(root().querySelector('.lightbox-fallback')).not.toBeNull();
		del(IMG_B);
		// Advanced off the fallback back to A (the raster arm).
		expect(shownAlt()).toBe('a');
		expect(root().querySelector('.lightbox-fallback')).toBeNull();
	});

	it('a delete of a DIFFERENT image while the confirm drill-down is up leaves it up', () => {
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')],
			mutationsEnabled: true,
		});
		// Open the delete confirmation on the shown image A.
		root().querySelector<HTMLButtonElement>(
			'.lightbox-toolbar .lightbox-tool[aria-label="Delete"]'
		)!.click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();

		// An EXTERNAL delete of B (not shown) — A stays shown, its confirm stays up.
		del(IMG_B);
		expect(shownAlt()).toBe('a');
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();
	});

	it('a delete of the SHOWN image while its own confirm is up advances with a CLEAN toolbar', () => {
		// The confirm gate holds `runToolbarAction` awaiting inside `run`, so
		// `toolbarBusy` is true the whole time the drill-down is up. An external delete
		// of that same shown image (another in-page surface, or — via the metadata
		// `missing` path — another tab) advances to a survivor; the subject-change reset
		// must zero `toolbarBusy`, or the survivor's Delete shows a stale "Deleting…" for
		// a request that was never about it. The fenced `finally` deliberately does NOT
		// clear it (the continuation is stale), so this asserts the reset is load-bearing.
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')],
			mutationsEnabled: true,
		});
		root().querySelector<HTMLButtonElement>(
			'.lightbox-toolbar .lightbox-tool[aria-label="Delete"]'
		)!.click();
		flushSync();
		expect(root().querySelector('.lightbox-delete-confirm')).not.toBeNull();

		del(IMG_A); // the shown image is deleted from under the open confirm
		expect(shownAlt()).toBe('b'); // advanced to the survivor
		expect(root().querySelector('.lightbox-delete-confirm')).toBeNull(); // confirm abandoned
		const deleteLabel = root()
			.querySelector('.lightbox-toolbar .lightbox-tool[aria-label="Delete"] .lightbox-tool-label')
			?.textContent?.trim();
		expect(deleteLabel).toBe('Delete'); // NOT the stale "Deleting…"
	});

	it('a delete of an unrelated attachment (not in the set) is a harmless no-op', () => {
		const onClose = vi.fn();
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')], onClose });
		del('ffffffff-0000-4000-8000-ffffffffffff'); // never in the set
		expect(onClose).not.toHaveBeenCalled();
		expect(shownAlt()).toBe('a');
		expect(counterText()).toBe('1 / 2');
	});

	it('a delete of an already-dangling shown id is a harmless no-op (no throw)', () => {
		// The shown image is removed straight from the PROP (not the bus), so shownId
		// dangles and the index derives to survivors[0]. A later bus delete of that
		// gone id must not read after[-1] or advance.
		liveProps.images = [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')];
		mountLive();
		press('ArrowRight'); // shownId = B
		expect(shownAlt()).toBe('b');
		liveProps.images = [image(IMG_A, 'a')]; // B removed from the prop → shownId dangles
		flushSync();
		expect(shownAlt()).toBe('a'); // derived falls to the survivor
		del(IMG_B); // a bus delete of the gone B — must not throw or disturb A
		expect(shownAlt()).toBe('a');
	});

	it('a delete on an already-empty viewer does not close it', () => {
		const onClose = vi.fn();
		liveProps.images = [image(IMG_A, 'a')];
		liveProps.onClose = onClose;
		mountLive();
		liveProps.images = []; // all removed via the prop → empty, still mounted
		flushSync();
		expect(root().querySelector('.lightbox-image')).toBeNull();
		// A delete (of the gone one, or an unrelated id) must NOT close the
		// already-empty viewer — the close is for a delete that EMPTIES a set, not one
		// that finds it already empty.
		del(IMG_A);
		del('ffffffff-0000-4000-8000-ffffffffffff');
		expect(onClose).not.toHaveBeenCalled();
	});

	it('deleting the shown image mid-DRAG advances and resets the zoom (stale drag stays harmless)', () => {
		mountViewer({
			images: [sized(IMG_A, 'a', 5000, 5000), sized(IMG_B, 'b', 800, 600, 'image/jpeg')],
		});
		mockGeometry(root(), OVERFLOW_G);
		fireLoad(1024, 768); // A decoded → bitmapPresent, pan room
		zoomToActual();
		expect(scaleOf()).toBeGreaterThan(1); // zoomed in on A
		const capture = vi.fn();
		(root() as unknown as { setPointerCapture: unknown }).setPointerCapture = capture;
		root().dispatchEvent(pointerEvent('pointerdown', 500, 500));
		root().dispatchEvent(pointerEvent('pointermove', 560, 500)); // engage the drag
		flushSync();
		expect(capture, 'the drag armed on A').toHaveBeenCalled();

		// A is deleted while the drag is live → advance to B, and the id-keyed reset
		// returns the transform to fit. The stale drag baseline is deliberately LEFT
		// (as arrow-nav mid-drag does) but harmless: at fit `clampPan` pins the pan to
		// 0, so no baseline can jump the image.
		del(IMG_A);
		expect(shownAlt()).toBe('b');
		expect(scaleOf(), 'the advance reset the transform to fit').toBe(1);
	});

	// ── The metadata `missing` phase as a deletion (3c-i final fix) ────────────
	// The deletion bus is process-local, so an out-of-page delete (another tab, a
	// job, the API) reaches the viewer only as the metadata machine's authoritative
	// `missing` (404) phase — which routes through the SAME tombstone path.
	// Interleave a microtask drain with an effect flush per hop: a cascade (each 404
	// advances to the next, whose OWN async HEAD then 404s) needs the effect to run
	// BETWEEN async resolutions, not once at the end.
	async function settleAsync() {
		for (let i = 0; i < 8; i++) {
			await Promise.resolve();
			flushSync();
		}
	}

	it('an authoritative metadata 404 (missing) for the shown image advances to a survivor', async () => {
		// A (the OPENED entry) and B (reached by the advance) are BOTH probed via the
		// forced revalidation — 3c-iii U3 forces one no-store probe per (open, entry)
		// pair, advances included. Both mocks return the same per-uuid answer anyway, so
		// A 404s wherever its probe is dispatched from.
		const perUuid = (_ws: unknown, uuid: string) =>
			Promise.resolve(
				uuid === IMG_A
					? { status: 'missing' as const }
					: { status: 'ok' as const, mime: 'image/png', size: 2048 }
			);
		metaFetch.mockImplementation(perUuid);
		metaRevalidate.mockImplementation(perUuid);
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')] });
		await settleAsync();
		// A's HEAD 404'd → A is tombstoned → advance to B; the set is now single.
		expect(shownAlt()).toBe('b');
		expect(counterText()).toBeNull();
	});

	it('an authoritative metadata 404 for a SINGLE-item surface shows the inert overlay, not a close (3c-ii T2b)', async () => {
		// The retired panel's behavior, preserved: a single file the user opened that
		// turns out to be gone shows "no longer available" rather than flash-closing.
		// (A multi-image set still closes on all-missing — the next test.)
		const onClose = vi.fn();
		// T6: the single opened entry is force-revalidated, so its 404 lands on the
		// revalidation probe.
		metaRevalidate.mockResolvedValue({ status: 'missing' });
		mountViewer({ images: [image(IMG_A, 'a')], onClose });
		await settleAsync();
		expect(onClose).not.toHaveBeenCalled();
		const missing = root().querySelector('.lightbox-missing');
		expect(missing).not.toBeNull();
		expect(missing?.textContent).toContain('no longer available');
		// No bytes: the missing arm mounts no <img>.
		expect(root().querySelector('.lightbox-image')).toBeNull();
	});

	it('an ENTIRE set going missing cascades advance→advance→close (terminates)', async () => {
		const onClose = vi.fn();
		// Every image 404s: A advances to B, B's own probe 404s and advances to C,
		// C's 404 empties the set → close. The effect re-runs once per subject change
		// (each HEAD is async), so the cascade settles rather than looping. A (opened)
		// AND B/C (advanced-to) all probe via the forced revalidation (3c-iii U3 forces
		// per (open, entry) pair) — both mocks 404 regardless.
		metaFetch.mockResolvedValue({ status: 'missing' });
		metaRevalidate.mockResolvedValue({ status: 'missing' });
		mountViewer({
			images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg'), image(IMG_C, 'c', 'image/gif')],
			onClose,
		});
		await settleAsync();
		expect(onClose).toHaveBeenCalledTimes(1);
	});

	it('a TRANSIENT metadata failure does NOT delete the shown image (DR-17)', async () => {
		const onClose = vi.fn();
		// T6: the opened entry A is force-revalidated, so its transient answer arrives
		// on the revalidation probe.
		metaRevalidate.mockResolvedValue({ status: 'transient' });
		mountViewer({ images: [image(IMG_A, 'a'), image(IMG_B, 'b', 'image/jpeg')], onClose });
		await settleAsync();
		// A non-404 failure is retryable, never a deletion — the viewer stays on A and
		// shows the header's inline retry instead.
		expect(shownAlt()).toBe('a');
		expect(onClose).not.toHaveBeenCalled();
		expect(root().querySelector('.lightbox-meta-error')).not.toBeNull();
	});
});
