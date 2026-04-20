<!--
	BottomSheet — reusable bottom-sheet / modal primitive.

	Minimal usage:

	```svelte
	<script>
		let open = $state(false);
	</script>
	<button onclick={() => (open = true)}>Open</button>
	<BottomSheet {open} onclose={() => (open = false)} title="Example">
		<p>Sheet content here</p>
	</BottomSheet>
	```

	- Controlled: parent owns `open` state and handles `onclose`.
	- Mobile (< 640px): docked to bottom, full-width, swipe-down to dismiss.
	- Desktop (>= 640px): constrained width, either bottom-anchored
	  (`desktopMode="sheet"`, default) or centered (`desktopMode="centered"`).
-->
<script module lang="ts">
	// ── Shared scroll lock ────────────────────────────────────────────────
	// Multiple BottomSheet instances can be open at once. Track the lock with
	// a module-level counter so the body overflow is only restored when the
	// last sheet closes.
	let scrollLockCount = 0;
	let scrollLockPrev = '';

	export function acquireScrollLock(): () => void {
		if (typeof document === 'undefined') return () => {};
		if (scrollLockCount === 0) {
			scrollLockPrev = document.body.style.overflow;
			document.body.style.overflow = 'hidden';
		}
		scrollLockCount++;
		let released = false;
		return () => {
			if (released) return;
			released = true;
			scrollLockCount--;
			if (scrollLockCount === 0) {
				document.body.style.overflow = scrollLockPrev;
			}
		};
	}

	// ── Stacked-sheet key handling ────────────────────────────────────────
	// Module-level stack of open sheets (most-recent-last). Used so only the
	// topmost sheet handles Escape / Tab — prevents one keypress from closing
	// multiple stacked sheets.
	//
	// Reactive so component instances can $derive their stack index and
	// compute a z-index that matches open-order (logical topmost == visual
	// topmost) even when DOM order differs from open order.
	let openStack = $state<symbol[]>([]);

	export function pushOpenSheet(): symbol {
		const token = Symbol();
		openStack.push(token);
		return token;
	}

	export function popOpenSheet(token: symbol): void {
		const i = openStack.lastIndexOf(token);
		if (i !== -1) openStack.splice(i, 1);
	}

	export function isTopmostSheet(token: symbol): boolean {
		return openStack[openStack.length - 1] === token;
	}

	export function getStackIndex(token: symbol): number {
		return openStack.indexOf(token);
	}
</script>

<script lang="ts">
	import { fly, fade } from 'svelte/transition';
	import type { Snippet } from 'svelte';

	interface Props {
		open: boolean;
		onclose: () => void;
		title?: string;
		fullHeight?: boolean;
		maxWidth?: string;
		desktopMode?: 'sheet' | 'centered';
		children: Snippet;
	}

	let {
		open,
		onclose,
		title,
		fullHeight = false,
		maxWidth,
		desktopMode = 'sheet',
		children
	}: Props = $props();

	// `$props.id()` is deterministic across SSR + client, so `aria-labelledby`
	// won't cause hydration mismatches (unlike `Math.random()`). Must be declared
	// at the top level of the component as a variable declaration initializer.
	const uid = $props.id();
	const headingId = `bottom-sheet-heading-${uid}`;

	const DISMISS_THRESHOLD = 80; // px

	// ── Element refs ───────────────────────────────────────────────────────
	let sheetEl = $state<HTMLDivElement | null>(null);
	let headingEl = $state<HTMLHeadingElement | null>(null);

	// ── Swipe-to-dismiss state ─────────────────────────────────────────────
	// `dragY` drives the CSS transform while the user is actively dragging.
	// We only commit a "dismiss" once pointerup fires past the threshold.
	let dragY = $state(0);
	let dragging = $state(false);
	let dragStartY = 0;
	let dragPointerId: number | null = null;


	// ── Open-sheet stack membership ────────────────────────────────────────
	// Track this instance's position in the module-level open-sheet stack so
	// that only the topmost sheet responds to Escape + traps Tab.
	let myToken = $state<symbol | null>(null);
	$effect(() => {
		if (!open) return;
		const token = pushOpenSheet();
		myToken = token;
		return () => {
			myToken = null;
			popOpenSheet(token);
		};
	});

	// ── Reactive z-index tied to open-stack position ──────────────────────
	// Visual stacking must follow open-order, not DOM-order, so the sheet
	// that is logically topmost (and therefore receives Escape / Tab) is
	// always the one rendered on top. Each stack level reserves 2 z-index
	// slots: backdrop (+0) and sheet (+1).
	const BASE_Z = 61;
	const stackIndex = $derived(myToken ? getStackIndex(myToken) : -1);
	const backdropZ = $derived(stackIndex < 0 ? BASE_Z : BASE_Z + stackIndex * 2);
	const sheetZ = $derived(stackIndex < 0 ? BASE_Z + 1 : BASE_Z + stackIndex * 2 + 1);

	// ── Reduced-motion ─────────────────────────────────────────────────────
	// Honor prefers-reduced-motion by collapsing transition durations to 0.
	let reducedMotion = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(prefers-reduced-motion: reduce)');
		reducedMotion = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			reducedMotion = e.matches;
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	const sheetDuration = $derived(reducedMotion ? 0 : 220);
	const backdropDuration = $derived(reducedMotion ? 0 : 160);

	// ── Scroll lock ────────────────────────────────────────────────────────
	// Set body overflow:hidden while open; restore the prior value on close
	// or unmount. Stored on the effect closure so it survives re-runs safely.
	$effect(() => {
		if (!open) return;
		const release = acquireScrollLock();
		return release;
	});

	// ── Focus management ───────────────────────────────────────────────────
	// On open: remember the previously focused element, then focus the first
	// focusable inside the sheet (falling back to the sheet container).
	// On close: restore focus to the previously focused element.
	$effect(() => {
		if (!open) return;
		if (typeof document === 'undefined') return;
		const previouslyFocused = document.activeElement as HTMLElement | null;
		// Defer to the next microtask so the sheet has mounted + transitioned in.
		queueMicrotask(() => {
			if (!sheetEl) return;
			const focusables = getFocusable(sheetEl);
			if (focusables.length > 0) {
				focusables[0].focus();
			} else {
				sheetEl.focus();
			}
		});
		return () => {
			// Skip restoration if another sheet is still in control — otherwise
			// closing a non-topmost stacked sheet could pull focus out of the
			// still-active dialog and break modal isolation.
			const othersStillOpen = openStack.some((t) => t !== myToken);
			const activeDialog =
				typeof document !== 'undefined' &&
				document.activeElement instanceof HTMLElement
					? document.activeElement.closest('[role="dialog"]')
					: null;
			const focusInOtherDialog = activeDialog !== null && activeDialog !== sheetEl;
			if (othersStillOpen || focusInOtherDialog) return;
			// Restore focus if the element is still connected.
			if (previouslyFocused && typeof previouslyFocused.focus === 'function') {
				if (previouslyFocused.isConnected) previouslyFocused.focus();
			}
		};
	});

	function getFocusable(root: HTMLElement): HTMLElement[] {
		const selector =
			'a[href], area[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
		return Array.from(root.querySelectorAll<HTMLElement>(selector)).filter(
			(el) => !el.hasAttribute('inert') && el.offsetParent !== null
		);
	}

	// ── Keyboard handling (Escape + Tab trap) ──────────────────────────────
	function handleKeydown(e: KeyboardEvent) {
		if (!open) return;
		// Only the topmost sheet in the stack responds to Escape / Tab so that
		// stacked sheets don't all close from a single keypress.
		if (myToken === null || !isTopmostSheet(myToken)) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			onclose();
			return;
		}
		if (e.key === 'Tab' && sheetEl) {
			const focusables = getFocusable(sheetEl);
			if (focusables.length === 0) {
				e.preventDefault();
				sheetEl.focus();
				return;
			}
			const first = focusables[0];
			const last = focusables[focusables.length - 1];
			const active = document.activeElement as HTMLElement | null;
			if (e.shiftKey) {
				if (active === first || !sheetEl.contains(active)) {
					e.preventDefault();
					last.focus();
				}
			} else {
				if (active === last || !sheetEl.contains(active)) {
					e.preventDefault();
					first.focus();
				}
			}
		}
	}

	// ── Swipe gesture ──────────────────────────────────────────────────────
	// Only engage on mobile viewports. The drag handle is the pointer target;
	// vertical drag past DISMISS_THRESHOLD triggers onclose().
	function isMobileViewport(): boolean {
		if (typeof window === 'undefined') return false;
		return window.matchMedia('(max-width: 639.98px)').matches;
	}

	function onHandlePointerDown(e: PointerEvent) {
		if (!isMobileViewport()) return;
		dragging = true;
		dragStartY = e.clientY;
		dragPointerId = e.pointerId;
		dragY = 0;
		(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
	}

	function onHandlePointerMove(e: PointerEvent) {
		if (!dragging || e.pointerId !== dragPointerId) return;
		const delta = e.clientY - dragStartY;
		// Clamp upward drag to 0 so the sheet can't travel above its rest position.
		dragY = Math.max(0, delta);
	}

	function onHandlePointerUp(e: PointerEvent) {
		if (!dragging || e.pointerId !== dragPointerId) return;
		const target = e.currentTarget as HTMLElement;
		if (target.hasPointerCapture(e.pointerId)) {
			target.releasePointerCapture(e.pointerId);
		}
		const shouldDismiss = dragY > DISMISS_THRESHOLD;
		dragging = false;
		dragPointerId = null;
		dragY = 0;
		if (shouldDismiss) onclose();
	}

	function onBackdropClick() {
		onclose();
	}

	// Dynamic style for dragging transform + maxWidth override.
	const sheetStyle = $derived.by(() => {
		const parts: string[] = [];
		if (dragging && dragY > 0) {
			parts.push(`transform: translateY(${dragY}px)`);
			parts.push('transition: none');
		}
		if (maxWidth) {
			parts.push(`--bs-max-width: ${maxWidth}`);
		}
		return parts.join('; ');
	});
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<div
		class="bs-backdrop"
		class:bs-centered={desktopMode === 'centered'}
		transition:fade={{ duration: backdropDuration }}
		onclick={onBackdropClick}
		onkeydown={() => {}}
		role="presentation"
		style:z-index={backdropZ}
	></div>

	<div
		bind:this={sheetEl}
		class="bs-sheet"
		class:bs-full-height={fullHeight}
		class:bs-centered={desktopMode === 'centered'}
		role="dialog"
		aria-modal="true"
		aria-labelledby={title ? headingId : undefined}
		aria-label={title ? undefined : 'Dialog'}
		tabindex="-1"
		style={sheetStyle}
		style:z-index={sheetZ}
		transition:fly={{ y: reducedMotion ? 0 : 100, duration: sheetDuration, opacity: 1 }}
	>
		<!--
			Drag handle: pointer target for swipe-down-to-dismiss on mobile.
			Hidden on desktop when in centered mode; on desktop sheet mode
			the handle renders but swipe logic is skipped by the viewport check.
		-->
		<div
			class="bs-handle-row"
			onpointerdown={onHandlePointerDown}
			onpointermove={onHandlePointerMove}
			onpointerup={onHandlePointerUp}
			onpointercancel={onHandlePointerUp}
			role="presentation"
		>
			<div class="bs-handle" aria-hidden="true"></div>
		</div>

		{#if title}
			<header class="bs-header">
				<h2 bind:this={headingEl} id={headingId} class="bs-title">{title}</h2>
				<button
					type="button"
					class="bs-close"
					onclick={onclose}
					aria-label="Close dialog"
				>&#10005;</button>
			</header>
		{/if}

		<div class="bs-content">
			{@render children()}
		</div>
	</div>
{/if}

<style>
	.bs-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		/* z-index is applied inline by the component so visual stacking
		   tracks the open-order stack (see script). Fallback kept below
		   via the sheet rule for any SSR/no-JS edge case. */
	}

	.bs-sheet {
		position: fixed;
		left: 0;
		right: 0;
		bottom: 0;
		/* z-index applied inline — see .bs-backdrop note above. */
		display: flex;
		flex-direction: column;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-bottom: none;
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		box-shadow: 0 -12px 40px rgba(0, 0, 0, 0.4);
		max-height: 90vh;
		width: 100%;
		outline: none;
		/* Prevent content below from bleeding out while corners are rounded. */
		overflow: hidden;
	}

	.bs-sheet.bs-full-height {
		height: 90vh;
	}

	/* Drag handle row — large touch target; visual pill centered. */
	.bs-handle-row {
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-2) 0 var(--space-1);
		touch-action: none;
		cursor: grab;
		flex-shrink: 0;
	}

	.bs-handle-row:active {
		cursor: grabbing;
	}

	.bs-handle {
		width: 36px;
		height: 4px;
		border-radius: 999px;
		background: var(--border);
	}

	.bs-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) var(--space-5) var(--space-3);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.bs-title {
		margin: 0;
		font-size: 1.05em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.bs-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
	}

	.bs-close:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.bs-content {
		padding: var(--space-4) var(--space-5) var(--space-5);
		overflow-y: auto;
		flex: 1 1 auto;
		min-height: 0;
	}

	/* ── Desktop (>= 640px) ─────────────────────────────────────────────── */

	@media (min-width: 640px) {
		/* Default "sheet" mode: bottom-anchored but constrained + centered. */
		.bs-sheet {
			left: 50%;
			right: auto;
			transform: translateX(-50%);
			max-width: var(--bs-max-width, min(520px, 90vw));
			width: 100%;
		}

		/* Override sheet styling when centered-modal mode is requested. */
		.bs-sheet.bs-centered {
			top: 50%;
			bottom: auto;
			transform: translate(-50%, -50%);
			border: 1px solid var(--border);
			border-radius: var(--radius-lg);
			box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
			max-width: var(--bs-max-width, min(520px, 90vw));
		}

		/* In centered mode the drag handle just adds visual noise. */
		.bs-sheet.bs-centered .bs-handle-row {
			display: none;
		}
	}

	/* ── Reduced motion ────────────────────────────────────────────────── */

	@media (prefers-reduced-motion: reduce) {
		.bs-sheet {
			transition: none !important;
		}
	}
</style>
