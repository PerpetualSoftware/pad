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
	 * viewer. Backdrop click closes; clicking the image itself does not.
	 */
	import { untrack } from 'svelte';
	import { attachmentDownloadUrl } from '$lib/markdown/attachments';
	import { paneFocusables, nextTrapTarget } from '$lib/collections/paneFocus';
	import {
		acquire,
		isBlockedByModal,
		isViewerFrontmost,
		VIEWER_ROOT_CLASS,
	} from '$lib/a11y/viewerBackdrop';
	import { pushEscapeHandler, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';

	export interface LightboxImage {
		id: string;
		alt: string;
	}

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

	// Seeded once at mount — the host remounts (null → set) on each open, so
	// no prop-sync effect is needed. untrack makes the initial-value capture
	// explicit (props are constant for this component's lifetime).
	let current = $state(
		untrack(() => Math.min(Math.max(index, 0), Math.max(images.length - 1, 0)))
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

	let hasMultiple = $derived(images.length > 1);
	let img = $derived(images[current]);
	let src = $derived(img ? attachmentDownloadUrl(openWsSlug, img.id) : '');
	// The accessible name: the image's own alt where there is one, else a
	// generic label. Never empty — an unnamed `role="dialog"` is announced as
	// nothing at all.
	let dialogLabel = $derived(img?.alt || 'Attachment viewer');

	// The portaled root. `$state` so the effect below re-runs once `bind:this`
	// lands; read-only inside every effect, so nothing here can self-invalidate
	// a flush (CONVE-1688).
	let rootEl = $state<HTMLElement | null>(null);

	function prev() {
		current = (current - 1 + images.length) % images.length;
	}
	function next() {
		current = (current + 1) % images.length;
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
		const unregisterEscape = pushEscapeHandler(() => {
			if (!isViewerFrontmost(el)) return false;
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
		} else if (e.key === 'ArrowRight' && hasMultiple) {
			e.preventDefault();
			next();
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

	{#if img}
		<img class="lightbox-image" {src} alt={img.alt || 'Attachment'} />
	{/if}

	{#if hasMultiple}
		<div class="lightbox-counter">{current + 1} / {images.length}</div>
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

	.lightbox-image {
		max-width: 92vw;
		max-height: 92vh;
		object-fit: contain;
		border-radius: var(--radius);
		box-shadow: 0 8px 40px rgba(0, 0, 0, 0.5);
		cursor: default;
	}

	.lightbox-close {
		position: absolute;
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
