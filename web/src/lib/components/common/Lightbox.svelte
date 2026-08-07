<script lang="ts">
	/**
	 * Full-screen image viewer for attachment thumbnails (IDEA-1660).
	 * Opened by a host that captures a click on an `img[data-attachment-id]`
	 * and passes the attachment id(s) — the lightbox loads the ORIGINAL
	 * (un-variant) blob so the expanded view is full resolution regardless
	 * of the thumbnail variant shown inline.
	 *
	 * MODAL CONTRACT (PLAN-2392 phase 3a / TASK-2429, DR-4b). This is a real
	 * modal now: `role="dialog"` + `aria-modal`, portaled to `<body>`, focus
	 * entry + restore, a Tab trap, background inertness through the shared
	 * viewer-backdrop manager, and Escape via the shared `escapeStack`. It is
	 * the contract the editor's hand-rolled `showModal()` dialog was getting
	 * from the platform for free, written out by hand — because 3a's later
	 * tasks delete that dialog and route the inline body images here.
	 *
	 * Keyboard: Esc closes (through `escapeStack`, NOT a local listener),
	 * ←/→ navigate when multiple images were passed, Tab cycles within the
	 * viewer, `+`/`-` zoom about the stage centre and `0` resets (PLAN-2392
	 * phase 3b / TASK-2455). Backdrop click closes; clicking the image itself
	 * does not.
	 */
	import { untrack } from 'svelte';
	import { attachmentDownloadUrl } from '$lib/markdown/attachments';
	import { paneFocusables, nextTrapTarget } from '$lib/collections/paneFocus';
	import {
		acquire,
		isBlockedByModal,
		isViewerFrontmost,
		noteEscapeConsumedByViewer,
		VIEWER_ROOT_CLASS,
	} from '$lib/a11y/viewerBackdrop';
	import { pushEscapeHandler, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { canOpenInViewer } from '$lib/attachments/display';
	import {
		reset as resetZoom,
		clampState,
		zoomTo,
		stageCenter,
		ZOOM_STEP,
		type Geometry,
		type ZoomState,
	} from '$lib/attachments/zoom';
	/**
	 * ONE definition of what an image in this viewer is (PLAN-2392 / TASK-2431).
	 *
	 * It used to be declared here as `{id, alt}` and again — as a superset — on
	 * the open-viewer channel, with a comment noting the two were structurally
	 * compatible. Two declarations of the same thing drift the moment one gains
	 * a field, which is exactly what TASK-2431 does (`mime_type` is what makes
	 * the DR-16 open gate total, and `size_bytes` / `width` / `height` land now
	 * so phase 3b's pixel-based loading policy need not reopen the event, the
	 * host and every producer). The channel's is now the only one.
	 *
	 * Everything past `id` / `alt` is NULLABLE and this component treats it as
	 * such: an inline image's metadata comes from a HEAD probe that may not have
	 * completed, and an upload event carries only four fields.
	 *
	 * Producers that mount this component directly import the type from the
	 * CHANNEL too, not from here — a `.svelte` module cannot re-export a type,
	 * and a local `interface … extends` would be a second declaration, which is
	 * the drift this consolidation removes.
	 */
	import type { LightboxImage } from '$lib/attachments/events';

	interface Props {
		images: LightboxImage[];
		/** Index to open at (clamped). */
		index?: number;
		wsSlug: string;
		onClose: () => void;
		/**
		 * The control that opened the viewer — focus goes back to it on close.
		 * OPTIONAL: the strip, the timeline and the NodeView thread real values in
		 * TASK-2431 / TASK-2433. Until then it falls back to whatever held focus
		 * at open (see below), which is the same element in every current path.
		 */
		invoker?: HTMLElement | null;
	}

	let { images, index = 0, wsSlug, onClose, invoker = null }: Props = $props();

	/**
	 * THE LAST-MILE GATE (PLAN-2392 DR-16 / TASK-2431).
	 *
	 * Every producer filters its own set before opening — that is where the
	 * gate belongs, because only the producer knows which of its rows are
	 * images at all. This is the same rule re-stated at the point of USE, and it
	 * is not redundant with them:
	 *
	 *  - `←/→` page through a set the producer chose ONCE. A producer's filter
	 *    is a statement about the moment it built the list; this one has to hold
	 *    for every frame the viewer shows.
	 *  - the set is `readonly` on the channel but a plain array as a prop, and a
	 *    producer that mutates it — or a record inside it — after emitting is
	 *    not a hypothesis this component can rule out.
	 *  - a producer added later inherits the rule instead of having to know it.
	 *
	 * IT FAILS CLOSED. Only a POSITIVELY allowlisted `mime_type` is viewable; a
	 * null / undefined / unresolved one is not. "Not yet known" is not evidence
	 * that a file is a PNG, and this is the last thing standing between a set
	 * and a rendered image — the place where the benefit of the doubt is worth
	 * least. An earlier revision admitted null on the grounds that an inline
	 * image's probe is often unasked at click time; that reasoning belongs to
	 * the PRODUCER, which can wait for the probe, and it had the effect of
	 * letting an emitter hand over `[safe, unresolved]` and letting the user
	 * arrow onto the unresolved one. The producers that exist today lose
	 * nothing: the strip always has the MIME from its list row, and the timeline
	 * already excludes unresolved entries from the set it builds.
	 *
	 * THE CONTRACT FOR NEW PRODUCERS, therefore: resolve the MIME before you
	 * emit. Passing a possibly-null value is not "let the viewer decide", it is
	 * an image that silently will not open.
	 *
	 * DERIVED, NOT CAPTURED. Every other open-time value here is `untrack`ed
	 * because the props are constant for the instance's life — but that is a
	 * claim about the CURRENT producers, and this filter is exactly the thing
	 * that must not rest on one. As a `$derived` it re-runs when the array is
	 * replaced or when a record's MIME resolves to something unsafe after the
	 * viewer opened, so a stale capture cannot outlive its own truth.
	 */
	let viewable = $derived(images.filter((im) => canOpenInViewer(im.mime_type)));

	// Seeded once at mount — the host remounts (null → set) on each open, so
	// no prop-sync effect is needed. untrack makes the initial-value capture
	// explicit.
	//
	// Resolved through the ID rather than carried across as a number: filtering
	// reindexes everything after a refusal, so the requested POSITION can name a
	// different image (or none) in the filtered set. Where the requested image
	// is the one refused, there is nothing to land on and the first viewable
	// image is what opens.
	let current = $state(
		untrack(() => {
			const wanted = images[Math.min(Math.max(index, 0), Math.max(images.length - 1, 0))];
			const at = wanted ? viewable.findIndex((im) => im.id === wanted.id) : -1;
			return at < 0 ? 0 : at;
		})
	);

	// CAPTURED AT OPEN, never read live (TASK-2429). The pane switches workspace
	// without remounting whatever is above it, so a live read could rebuild the
	// URLs of already-captured attachment ids against a DIFFERENT workspace —
	// serving a 404, or worse, another workspace's attachment at the same id.
	// `invoker` is captured for the same reason: the value that opened this
	// viewer is the one to return focus to.
	const openWsSlug = untrack(() => wsSlug);
	// The invoker falls back to WHATEVER HELD FOCUS AT OPEN (the pattern
	// `BottomSheet` uses), not to nothing. Focus entry is about to move focus
	// into the viewer, so without this the producers that don't thread an
	// invoker yet — the strip and the timeline, until TASK-2431 — would be
	// strictly worse off than before this component managed focus at all: they
	// keep focus on the clicked tile today, and a null invoker would drop it to
	// `<body>` on close. Captured at init, BEFORE the entry focus runs.
	const openInvoker = untrack(() => {
		if (invoker) return invoker;
		if (typeof document === 'undefined') return null;
		const active = document.activeElement;
		return active && active !== document.body ? (active as HTMLElement) : null;
	});

	// EVERYTHING PAST THIS POINT READS `viewable`, NEVER `images` — the nav
	// wrap-around, the counter and the rendered `<img>` alike. A single read of
	// the unfiltered prop below would reopen the hole this filter closes.
	let hasMultiple = $derived(viewable.length > 1);
	// The position actually shown, clamped. `current` is what the user's ←/→
	// moved, but the set can SHRINK underneath it — a record whose MIME resolves
	// to something unsafe after open drops out of `viewable`, and the array can
	// be replaced outright. Clamping in a derived (rather than writing `current`
	// from an effect, which would be an effect writing state it reads) keeps the
	// viewer on a real member instead of blanking or showing `undefined`.
	let shownIndex = $derived(
		Math.min(Math.max(current, 0), Math.max(viewable.length - 1, 0))
	);
	let img = $derived(viewable[shownIndex]);
	let src = $derived(img ? attachmentDownloadUrl(openWsSlug, img.id) : '');
	// The accessible name: the image's own alt where there is one, else a
	// generic label. Never empty — an unnamed `role="dialog"` is announced as
	// nothing at all.
	let dialogLabel = $derived(img?.alt || 'Attachment viewer');

	// The portaled root. `$state` so the effect below re-runs once `bind:this`
	// lands; read-only inside every effect, so nothing here can self-invalidate
	// a flush (CONVE-1688).
	let rootEl = $state<HTMLElement | null>(null);

	// ── Zoom / pan (PLAN-2392 phase 3b / TASK-2455) ──────────────────────────
	//
	// The transform is `translate(x,y) scale(scale)` on the <img>, about the
	// stage centre. Every number comes from `$lib/attachments/zoom` (TASK-2454),
	// which owns the arithmetic and its bounds; this component only MEASURES the
	// rendered geometry, wires the keys, and re-clamps on resize.
	let zoom = $state<ZoomState>(resetZoom());
	// The stage is the 92vw×92vh box the bare <img> used to be; the <img> sits
	// inside it, `object-fit: contain`. Both are read live for geometry — never
	// through `getBoundingClientRect()` on the transformed image, which returns
	// the POST-scale box and would make the pan bounds grow with the zoom.
	let stageEl = $state<HTMLElement | null>(null);
	let imgEl = $state<HTMLImageElement | null>(null);

	/**
	 * The measured geometry, or null before there is anything to measure.
	 *
	 * `offsetWidth` / `offsetHeight` are the UNSCALED layout box — transforms do
	 * not touch them, which is the whole reason they, not `getBoundingClientRect`,
	 * are the source here. A not-yet-decoded bitmap reads back all zeros; the zoom
	 * module is defensive about that and still returns an in-bounds transform, so
	 * no guard is needed here.
	 */
	function readGeometry(): Geometry | null {
		const stage = stageEl;
		const image = imgEl;
		if (!stage || !image) return null;
		return {
			stageW: stage.clientWidth,
			stageH: stage.clientHeight,
			fittedW: image.offsetWidth,
			fittedH: image.offsetHeight,
			naturalW: image.naturalWidth,
			naturalH: image.naturalHeight,
		};
	}

	// `+` / `-` zoom about the stage centre. Reading and writing `zoom` from an
	// EVENT handler is fine — the CONVE-1688 rule is about `$effect`s that read
	// the state they write, not about handlers.
	function stepZoom(factor: number) {
		const g = readGeometry();
		if (!g) return;
		zoom = zoomTo(zoom, zoom.scale * factor, stageCenter(g), g);
	}

	// Stepped from `shownIndex`, not from `current`: after the set shrinks they
	// differ, and moving from the raw value would jump relative to a position
	// the user was never on. Both are no-ops on an empty set — reachable only
	// through the nav controls / arrow keys, which `hasMultiple` already hides
	// and gates, but written so the modulo can never be `% 0`.
	function prev() {
		if (viewable.length === 0) return;
		current = (shownIndex - 1 + viewable.length) % viewable.length;
	}
	function next() {
		if (viewable.length === 0) return;
		current = (shownIndex + 1) % viewable.length;
	}

	/**
	 * Return focus where it came from, on close.
	 *
	 * Declines when focus has ALREADY moved somewhere outside this viewer —
	 * a surface opened over the viewer owns focus outright, and so does a
	 * producer that moves focus from its own close handler (which runs BEFORE
	 * this teardown). `AttachmentViewerHost` used to be exactly that producer;
	 * TASK-2429 moved the restore here precisely because its version ran while
	 * the invoker was still inert, so this guard is now about the surfaces the
	 * viewer does not control rather than about that host. Focus resting on
	 * `<body>` / nowhere is the adrift state a teardown leaves behind, and
	 * counts as ours to move.
	 *
	 * The invoker is verified rather than trusted: it can have been detached
	 * (an editor NodeView is re-rendered on any document change), hidden, or
	 * inerted since it was captured, and `focus()` on such an element silently
	 * does nothing on some engines and drops focus to `<body>` on others. So we
	 * focus it and CHECK, falling back to parking focus on `<body>` — which is
	 * where the browser would have put it anyway, but deterministically, and
	 * without leaving focus inside a subtree that is about to be removed.
	 */
	function restoreFocus(root: HTMLElement): void {
		const active = document.activeElement;
		if (active !== null && active !== document.body && !root.contains(active)) return;

		if (openInvoker?.isConnected) {
			openInvoker.focus?.({ preventScroll: true });
			if (document.activeElement === openInvoker) return;
		}
		(document.activeElement as HTMLElement | null)?.blur?.();
	}

	// ONE effect owns the whole modal contract, because the steps are ordered
	// with respect to each other and to teardown: portal → lease → focus entry →
	// Escape registration, unwound in reverse.
	$effect(() => {
		const el = rootEl;
		if (!el) return;

		// PORTAL TO <body> DIRECTLY — deliberately NOT `portalAction.ts`, which
		// targets the nearest ancestor `<dialog>` when there is one: exactly the
		// wrong target for a surface that must sit ABOVE everything. A `position:
		// fixed` overlay is only viewport-fixed while no ancestor establishes a
		// containing block, and `transform` / `filter` / `contain: layout` on any
		// ancestor silently does (see the container-query foot-gun). `<body>` is
		// the only parent with no such ancestor, and it is what the backdrop
		// manager's inert bookkeeping requires: it writes `inert` on BODY CHILDREN
		// and exempts this one.
		document.body.appendChild(el);

		// Background inertness is the manager's, not ours — no hand-rolled
		// `inert`, no `aria-hidden` sweep. It refcounts, so two viewers stacked
		// (the strip's and a NodeView's) can't clobber each other's writes.
		const lease = acquire(el);

		// Focus ENTRY goes to the first tabbable DESCENDANT (the close button),
		// not the root: a screen-reader user landing on the container has to
		// discover the controls, and the trap below cycles from wherever focus is.
		// The root is `tabindex="-1"` purely as the fallback for a viewer with no
		// tabbable control yet (single image, still loading) — focus must not stay
		// on whatever is behind the backdrop.
		const entry = paneFocusables(el)[0] ?? el;
		entry.focus({ preventScroll: true });

		// Escape has ONE owner: the shared stack. The local `<svelte:window>`
		// Escape branch this component used to carry was DELETED rather than
		// gated — it ignored `defaultPrevented`, so with the stack also running,
		// a single press closed the viewer AND the layer beneath it.
		//
		// Registered ABOVE `menu` (40) so the viewer is the innermost layer.
		// Declines (returns false) unless this viewer is the FRONTMOST lease, so
		// with two viewers open one press closes exactly the top one and the
		// stack falls through to it rather than to an unrelated layer.
		const unregisterEscape = pushEscapeHandler((event) => {
			if (!isViewerFrontmost(el)) return false;
			// Mark the DISPATCH before closing, not after (TASK-2448 / BUG-2441).
			// `onClose()` runs the teardown below synchronously — the lease is gone
			// by the time it returns — so a later `window` listener in this same
			// event would be told "no viewer" and close a second layer. The mark is
			// the only thing that survives that, and it has to be in place before
			// the state it stands in for is destroyed.
			if (event) noteEscapeConsumedByViewer(event);
			onClose();
			return true;
		}, ESCAPE_PRIORITY.viewer);

		return () => {
			unregisterEscape();
			// Release BEFORE restoring focus, and let the RESULT decide: when a
			// viewer remains beneath this one the manager has already handed focus
			// into it, and restoring our own invoker would yank focus out of a
			// viewer the user is still looking at. Only the last one out restores.
			const { stackEmpty } = lease.release();
			if (stackEmpty) restoreFocus(el);
			// Svelte removes the node itself, but it is reparented out of its
			// anchor — remove it explicitly so the DOM can never be left with a
			// stranded backdrop, and so the manager's next reconcile sees the
			// body child list it expects.
			el.remove();
		};
	});

	// Reset the transform whenever the SHOWN image changes — arrow nav, or the
	// set shrinking under `current` so a different member is shown (TASK-2455).
	// Close needs no handling: every producer keys the mount, so closing
	// unmounts this instance and the next open starts from `resetZoom()`.
	//
	// `lastResetForId` is a PLAIN let, not `$state`: an effect that read and wrote
	// the same `$state` would self-depend and abort its own flush (CONVE-1688),
	// stranding unrelated reactivity nearby. This effect reads `img?.id` (tracked)
	// and writes `zoom` (which it never reads) plus this sentinel (a plain let, so
	// never tracked) — nothing it writes is anything it reads. Seeded to the
	// current id so the mount does not fire a redundant reset.
	let lastResetForId: string | undefined = untrack(() => img?.id);
	$effect(() => {
		const id = img?.id;
		if (id === lastResetForId) return;
		lastResetForId = id;
		zoom = resetZoom();
	});

	// Re-clamp on stage resize. `maxScale` is geometry-dependent and geometry is
	// viewport-dependent, so ENLARGING the window lowers the ceiling and can
	// strand a previously-valid scale above it. `clampState` re-clamps SCALE
	// first, then pan — the order a geometry change needs; clamping pan first
	// would bound it against a scale that is about to change. The callback runs
	// outside any tracked scope, so its `zoom` read/write is not a self-write.
	// Guarded for environments without `ResizeObserver` (SSR); the jsdom test
	// project ships a global stand-in (`src/test/setup-jsdom.ts`).
	$effect(() => {
		const stage = stageEl;
		if (!stage || typeof ResizeObserver === 'undefined') return;
		const ro = new ResizeObserver(() => {
			const g = readGeometry();
			if (g) zoom = clampState(zoom, g);
		});
		ro.observe(stage);
		return () => ro.disconnect();
	});

	function onKeydown(e: KeyboardEvent) {
		// A control that already handled this key owns it.
		if (e.defaultPrevented) return;
		const el = rootEl;
		// Every mounted viewer listens on `window`, so ONLY the frontmost may act.
		// This matters more than the usual layer-isolation argument: `nextTrapTarget`
		// deliberately pulls out-of-container focus back INWARD, so a background
		// viewer running the trap would drag focus into itself, out of the viewer
		// in front of it.
		if (!el || !isViewerFrontmost(el)) return;
		// ...and a `showModal()` dialog opened OVER the viewer owns the top layer,
		// above any body-portaled surface, so the frontmost LEASE is not
		// necessarily the frontmost SURFACE. Without this the viewer's trap would
		// fight a native modal for Tab and pull focus back out of it — the exact
		// inward-redirect hazard the frontmost gate exists to prevent, one layer
		// up. The manager already keeps such a dialog out of the inert set
		// (`keepInteractiveAsDialog`), so it is reachable and this is real.
		if (isBlockedByModal(el)) return;

		if (e.key === 'Tab') {
			// Reuses the pane's tested trap math (paneFocus.ts) — one trap
			// implementation for the whole app, not a second one here.
			const target = nextTrapTarget(
				paneFocusables(el),
				document.activeElement,
				e.shiftKey,
				el
			);
			if (target) {
				e.preventDefault();
				target.focus({ preventScroll: true });
			}
			return;
		}

		if (e.key === 'ArrowLeft' && hasMultiple) {
			e.preventDefault();
			prev();
			return;
		}
		if (e.key === 'ArrowRight' && hasMultiple) {
			e.preventDefault();
			next();
			return;
		}

		// Zoom: `+` / `-` about the stage centre, `0` resets. INSIDE the gates
		// above by construction — placed after `defaultPrevented`,
		// `isViewerFrontmost` and `isBlockedByModal` have each had their say, so a
		// zoom key can never steal a press another owner or a layer above is due.
		//
		// The modifier rule is a CONTRACT, not a nicety: this listens on `window`,
		// so acting on Ctrl/Cmd/Alt-`-`/`0` would swallow the browser's own
		// page-zoom (and OS) shortcuts from every surface while a viewer is open.
		// Leave those keys entirely — do not act, and do NOT `preventDefault`, or
		// the native shortcut is cancelled even though we declined to handle it.
		// `shiftKey` is fine (on most layouts `+` IS Shift+`=`), so it is absent
		// from the guard.
		if (e.ctrlKey || e.metaKey || e.altKey) return;
		if (e.key === '+' || e.key === '=') {
			// `+` — including the numpad, whose `.key` is also `'+'` — and bare `=`.
			e.preventDefault();
			stepZoom(ZOOM_STEP);
		} else if (e.key === '-') {
			// `-`, including the numpad, whose `.key` is also `'-'`.
			e.preventDefault();
			stepZoom(1 / ZOOM_STEP);
		} else if (e.key === '0') {
			e.preventDefault();
			zoom = resetZoom();
		}
		// NO Escape branch. See the registration above.
	}

	// Close only on a click of the backdrop itself — clicks on the image or
	// controls have a different target, so they don't dismiss. This avoids
	// putting a click handler (and its a11y burden) on the <img>.
	function onBackdropClick(e: MouseEvent) {
		if (e.target === e.currentTarget) onClose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

<!--
	The backdrop click has no keyboard twin ON THIS ELEMENT by design: the
	keyboard route out is Escape (through the shared stack) and the close
	button, which is the first thing focus lands on. Adding a key handler here
	would put a second, undocumented dismissal on the container.
-->
<!-- svelte-ignore a11y_click_events_have_key_events -->
<div
	bind:this={rootEl}
	class="lightbox-backdrop {VIEWER_ROOT_CLASS}"
	role="dialog"
	aria-modal="true"
	aria-label={dialogLabel}
	tabindex="-1"
	onclick={onBackdropClick}
>
	<!--
		Explicit `aria-label`s: the glyph IS the label otherwise ("✕", "‹", "›"),
		and `title` does not win over element content for the accessible name. The
		browser suite (TASK-2436) also addresses these controls BY NAME, never by
		class or a bare `[role="dialog"]`.
	-->
	<button
		class="lightbox-close"
		type="button"
		title="Close (Esc)"
		aria-label="Close"
		onclick={onClose}
	>
		&#10005;
	</button>

	{#if hasMultiple}
		<button
			class="lightbox-nav prev"
			type="button"
			title="Previous (←)"
			aria-label="Previous image"
			onclick={prev}
		>
			&#8249;
		</button>
		<button
			class="lightbox-nav next"
			type="button"
			title="Next (→)"
			aria-label="Next image"
			onclick={next}
		>
			&#8250;
		</button>
	{/if}

	<!--
		The STAGE is the 92vw×92vh box the bare <img> used to be; the image sits
		inside it, `object-fit: contain`, and carries the zoom transform. The stage
		is `pointer-events: none` so a click on the empty letterbox around the image
		falls through to the backdrop and closes (exactly as it did when the image
		was the only thing here); the image re-enables pointer events so a click ON
		it does not close — and so 3c's drag / 3d's pinch have a target. The controls
		sit ABOVE the stage (their own `z-index`), never inside it.
	-->
	<div class="lightbox-stage" bind:this={stageEl}>
		{#if img}
			<img
				bind:this={imgEl}
				class="lightbox-image"
				{src}
				alt={img.alt || 'Attachment'}
				style="transform: translate({zoom.x}px, {zoom.y}px) scale({zoom.scale});"
			/>
		{/if}
	</div>

	{#if hasMultiple}
		<!-- `shownIndex`, so the counter names the image actually on screen even
		     after the set shrank under `current`. -->
		<div class="lightbox-counter">{shownIndex + 1} / {viewable.length}</div>
	{/if}
</div>

<style>
	.lightbox-backdrop {
		position: fixed;
		inset: 0;
		/*
		 * ABOVE EVERY OTHER OVERLAY IN THE APP (TASK-2429). The viewer is a modal:
		 * it inerts every other body child, so anything that PAINTS over it is a
		 * surface the user can see but cannot touch — the worst of both. The app
		 * shell wrapper is `display: contents` (`app.html`), which creates no
		 * stacking context, so every fixed overlay in the tree competes with this
		 * one in the ROOT stacking context — being a body child is not by itself
		 * protection.
		 *
		 * What this value must out-rank, highest first (swept for TASK-2429 — CSS
		 * declarations AND `style.cssText` written from TypeScript, which a CSS-only
		 * grep misses):
		 *   99999  EmojiPickerButton's desktop dropdown (`EmojiPickerButton.svelte`,
		 *          body-portaled via `portalAction`) — the highest in the tree by
		 *          two orders of magnitude, and the reason 1000 was not enough
		 *    1000  the editor's block-drag ghost, appended to `<body>` and styled
		 *          inline (`editor/block-drag-handle.ts::createGhost`)
		 *     200  Menu (body-portaled) and the editor's slash/link popovers
		 *     199  the editor's block context menu
		 *     100  toasts, the notification panel, the editor's mobile toolbar
		 *   45-60  DockedSheet / BottomSheet / Sidebar / CommandPalette / TopBar
		 * (`ItemDetail`'s 1000 is a `@media print` footer, not a runtime overlay.)
		 *
		 * A NEW OVERLAY ABOVE THIS VALUE IS A BUG unless it is meant to cover a
		 * modal viewer. One thing legitimately renders above it and needs no
		 * z-index to do so: a native `showModal()` dialog, which the platform puts
		 * in the TOP LAYER — the manager deliberately leaves those interactive
		 * (`viewerBackdrop.ts::keepInteractiveAsDialog`) and the key handler stands
		 * down for them (`isBlockedByModal`).
		 *
		 * jsdom cannot see any of this; the paint-order proof is TASK-2436's.
		 */
		z-index: 100000;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.82);
		backdrop-filter: blur(2px);
		cursor: zoom-out;
	}

	.lightbox-stage {
		/* The box the bare <img> used to occupy — TASK-2454's coordinate system. */
		width: 92vw;
		height: 92vh;
		flex: none;
		display: flex;
		align-items: center;
		justify-content: center;
		/*
		 * The empty letterbox around the image is transparent to pointer events, so
		 * a click there reaches the backdrop and closes — the pre-zoom behaviour.
		 * The image below re-enables them. Deliberately NOT `touch-action: none`
		 * here: that would kill native pinch before 3d's handler exists, leaving
		 * phone users no zoom at all in the interval (out of TASK-2455's scope).
		 */
		pointer-events: none;
	}

	.lightbox-image {
		max-width: 100%;
		max-height: 100%;
		object-fit: contain;
		border-radius: var(--radius);
		box-shadow: 0 8px 40px rgba(0, 0, 0, 0.5);
		cursor: default;
		pointer-events: auto;
		transform-origin: center;
		transition: transform 0.15s ease-out;
	}

	/* Reduced motion suppresses the TRANSITION only — the zoom still works
	   (Modal.svelte's precedent). */
	@media (prefers-reduced-motion: reduce) {
		.lightbox-image {
			transition: none;
		}
	}

	.lightbox-close {
		position: absolute;
		/* Above the stage's stacking context. Small on purpose: the viewer's own
		   z-index sweep (TASK-2436) forbids any app overlay at or above 100000. */
		z-index: 1;
		top: var(--space-3);
		right: var(--space-3);
		width: 40px;
		height: 40px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.1rem;
		cursor: pointer;
		line-height: 1;
	}

	.lightbox-close:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav {
		position: absolute;
		z-index: 1;
		top: 50%;
		transform: translateY(-50%);
		width: 48px;
		height: 48px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.8rem;
		cursor: pointer;
		line-height: 1;
	}

	.lightbox-nav:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav.prev {
		left: var(--space-3);
	}

	.lightbox-nav.next {
		right: var(--space-3);
	}

	.lightbox-counter {
		position: absolute;
		z-index: 1;
		bottom: var(--space-3);
		left: 50%;
		transform: translateX(-50%);
		padding: var(--space-1) var(--space-3);
		background: rgba(0, 0, 0, 0.5);
		border-radius: 9999px;
		color: #fff;
		font-size: 0.8rem;
	}
</style>
