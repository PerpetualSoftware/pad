<!--
	DockedSheet — a bottom sheet that docks ABOVE the mobile bottom nav
	(PLAN-1694). Unlike common/BottomSheet (a full-screen z-50 overlay that
	covers everything), this anchors its bottom edge at the top of the nav bar,
	so the BottomNav stays visible + tappable and the originating slot stays
	lit. ~2/3 viewport height, grab handle, slide-up, swipe-down / tap-out /
	Escape to dismiss.

	Used by WorkspaceSheet and YouSheet — the two purpose-designed mobile nav
	surfaces. Pure shell; callers provide the content via the `children` snippet.
-->
<script lang="ts">
	import type { Snippet } from 'svelte';
	import { fly, fade } from 'svelte/transition';
	import { cubicOut } from 'svelte/easing';
	import { isBlockedByModal } from '$lib/a11y/viewerBackdrop';
	import { createFocusReturn } from '$lib/a11y/focusReturn';
	import { coveredPage } from '$lib/stores/coveredPage.svelte';
	import { tick } from 'svelte';
	import { nextTrapTargetAcross } from '$lib/collections/paneFocus';

	let {
		open,
		onclose,
		label = 'Menu',
		children
	}: {
		open: boolean;
		onclose: () => void;
		label?: string;
		children: Snippet;
	} = $props();

	// Swipe-down-to-dismiss: track the drag offset and apply it as a transform
	// on the panel; release past the threshold closes, otherwise it snaps back.
	let dragY = $state(0);
	let dragging = $state(false);
	let startY = 0;
	const DISMISS_PX = 90;

	let panelEl = $state<HTMLElement | null>(null);
	let contentEl = $state<HTMLElement | null>(null);

	/**
	 * TASK-2430 — this sheet is a GLOBAL Escape/gesture owner (three instances
	 * stay mounted by `BottomNav`), so it has to ask whether something is in
	 * FRONT of it before acting. `isBlockedByModal` is the shared arbitration
	 * helper: it answers from viewer LEASE state plus the native `dialog:modal`
	 * top layer, and returns **false on an empty stack** — with no viewer and no
	 * native modal open, every path below behaves exactly as it did before.
	 *
	 * The argument is the SURFACE ASKING TO ACT (our own panel), not
	 * `event.target`: a viewer opened FROM this sheet leaves focus/target inside
	 * the sheet, and target-based arbitration would then wrongly let the sheet
	 * dismiss itself out from under the viewer it launched.
	 *
	 * TASK-2448 adds the optional `event`. `isBlockedByModal` alone answers "is a
	 * viewer in front RIGHT NOW", and for a KEYDOWN that is the wrong moment to
	 * ask: the viewer's own Escape handler runs earlier in the same dispatch and
	 * tears the viewer down synchronously, so by the time this listener runs the
	 * lease is already gone and the honest answer to the live question is "no".
	 * Passing the event asks the EVENT-SCOPED question instead — "has a viewer
	 * already spent this press" — which the viewer answered before it died. The
	 * gesture paths below keep the live question: a touch sequence is not one
	 * dispatch, and nothing marks it.
	 */
	function blockedByFrontLayer(event?: Event): boolean {
		return isBlockedByModal(panelEl, event);
	}

	function cancelDrag() {
		dragging = false;
		dragY = 0;
	}

	function onTouchStart(e: TouchEvent) {
		// Gesture START gate.
		if (blockedByFrontLayer()) return;
		startY = e.touches[0].clientY;
		dragging = true;
	}
	function onTouchMove(e: TouchEvent) {
		if (!dragging) return;
		// STRADDLE gate: a swipe can begin before a viewer opens and keep
		// delivering moves afterwards (touch events continue to their original
		// target). Abandon the drag rather than keep translating the panel.
		if (blockedByFrontLayer()) {
			cancelDrag();
			return;
		}
		dragY = Math.max(0, e.touches[0].clientY - startY);
	}
	function onTouchEnd() {
		// STRADDLE gate on the terminal event — the release is what would actually
		// call `onclose()`, so this is the one that must not fire under a viewer.
		//
		// The gate is the ONLY addition: no `if (!dragging) return` above it, so
		// the un-blocked path below is byte for byte the original terminal
		// cleanup. An early bail there was tried and removed — with a stray or
		// duplicate touchend it skipped `dragging = false` / `dragY = 0`, which is
		// a state-path difference in a task that promises none without a viewer,
		// for no benefit (`dragY` is only ever non-zero while `dragging`).
		if (blockedByFrontLayer()) {
			cancelDrag();
			return;
		}
		dragging = false;
		if (dragY > DISMISS_PX) {
			onclose();
		}
		dragY = 0;
	}

	/*
	 * BUG-3386: a downward drag in the CONTENT closes the sheet too, when the
	 * content is already scrolled to its top. That is the grip's gesture
	 * continued into the list, the way a native sheet behaves; before, the
	 * same drag pulled the page underneath to refresh. A drag that starts
	 * with the list scrolled, or that moves up first, is a scroll and is
	 * left alone. CONTENT_SLOP keeps a tap or a jitter from engaging it.
	 */
	const CONTENT_SLOP = 8;
	let contentArmed = false;
	function onContentTouchStart(e: TouchEvent) {
		contentArmed = false;
		if (blockedByFrontLayer()) return;
		if (!contentEl || contentEl.scrollTop > 0) return;
		startY = e.touches[0].clientY;
		contentArmed = true;
	}
	function onContentTouchMove(e: TouchEvent) {
		if (dragging) {
			onTouchMove(e);
			return;
		}
		if (!contentArmed || !contentEl) return;
		const dy = e.touches[0].clientY - startY;
		if (dy < 0 || contentEl.scrollTop > 0) {
			contentArmed = false; // a scroll, not a pull
			return;
		}
		if (dy > CONTENT_SLOP) {
			if (blockedByFrontLayer()) {
				contentArmed = false;
				return;
			}
			startY = e.touches[0].clientY;
			dragging = true;
		}
	}
	function onContentTouchEnd() {
		contentArmed = false;
		if (dragging) onTouchEnd();
	}

	/*
	 * TASK-2235: focus. The sheet is NOT modal and no longer says it is: it
	 * leaves the bottom nav live on purpose (the slot stays lit and another
	 * slot is one tap away), so `aria-modal` would hide that live nav from a
	 * screen reader. What it does keep from a modal: focus moves in on open and
	 * back to the trigger on close, and Tab cycles through the sheet and the
	 * nav, never into the page its backdrop covers.
	 */
	const focusReturn = createFocusReturn();
	$effect(() => {
		const el = panelEl;
		if (open && el) {
			focusReturn.save();
			if (!el.contains(document.activeElement)) el.focus({ preventScroll: true });
		} else if (!open) {
			// After a tick (TASK-3520): the trigger may sit in a region this overlay
	// made inert, and focus() on an inert element silently does nothing.
			void tick().then(() => focusReturn.restore());
		}
	});
	$effect(() => () => void tick().then(() => focusReturn.restore()));

	// The page behind leaves the screen-reader tree while the sheet is open,
	// all but the bottom nav, which stays live by design (TASK-3520).
	$effect(() => {
		if (!open) return;
		return coveredPage.enter({ keepBottomNav: true });
	});

	function onTab(e: KeyboardEvent) {
		if (!panelEl || blockedByFrontLayer(e)) return;
		const nav = document.querySelector<HTMLElement>('nav.bottom-nav');
		const target = nextTrapTargetAcross(nav ? [panelEl, nav] : [panelEl], document.activeElement, e.shiftKey);
		if (target) {
			e.preventDefault();
			target.focus({ preventScroll: true });
		}
	}

	function onKeydown(e: KeyboardEvent) {
		if (open && e.key === 'Tab') {
			onTab(e);
			return;
		}
		if (!open || e.key !== 'Escape') return;
		// A HELD Escape fires many auto-repeat keydowns, and each is a FRESH
		// event object — so the viewer's per-event consumption mark (BUG-2441)
		// cannot cover them: by the second repeat its lease is already gone and
		// the event is unmarked, so a hold would close the viewer and then this
		// sheet. Only the initial physical press acts. The two route guards
		// already do exactly this ([collection]/+page.svelte, [slug]/+page.svelte);
		// this closes the same hole for the owners that did not (TASK-2448).
		if (e.repeat) return;
		// ONLY the frontmost-layer check. This handler still closes on an Escape a
		// control already handled, exactly as it always has — TASK-2430 added a
		// `defaultPrevented` bail here and it was REVERTED: it is a defensible
		// change on its own merits, but it fires with NO viewer present, and this
		// task's contract is that an empty lease leaves behaviour untouched. It
		// belongs in its own item, not smuggled into an attachments phase.
		//
		// DELIBERATE, NAMED BEHAVIOUR CHANGE (TASK-2448 / BUG-2441) — the FOURTH
		// named parity exception of PLAN-2392 phase 3a. This sheet now declines an
		// Escape a VIEWER has already consumed in the same dispatch. It is not the
		// reverted `defaultPrevented` bail: this marker is set ONLY by a frontmost
		// viewer's escape handler, so with no viewer open it can never be set and
		// this line is exactly `blockedByFrontLayer()` as before — the empty-stack
		// contract 2430 broke is kept intact.
		if (blockedByFrontLayer(e)) return;
		onclose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="ds-backdrop" onclick={onclose} transition:fade={{ duration: 160 }}></div>
	<div
		bind:this={panelEl}
		class="ds-panel"
		role="dialog"
		aria-label={label}
		tabindex="-1"
		style:transform={dragY ? `translateY(${dragY}px)` : undefined}
		style:transition={dragging ? 'none' : undefined}
		transition:fly={{ y: 360, duration: 240, easing: cubicOut }}
	>
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div
			class="ds-grip"
			ontouchstart={onTouchStart}
			ontouchmove={onTouchMove}
			ontouchend={onTouchEnd}
		>
			<span class="ds-handle" aria-hidden="true"></span>
		</div>
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div
			class="ds-content"
			bind:this={contentEl}
			ontouchstart={onContentTouchStart}
			ontouchmove={onContentTouchMove}
			ontouchend={onContentTouchEnd}
			ontouchcancel={onContentTouchEnd}
		>
			{@render children()}
		</div>
	</div>
{/if}

<style>
	.ds-backdrop {
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		/* Stop above the nav so it's never covered (and stays tappable). */
		bottom: calc(var(--bottom-nav-height) + env(safe-area-inset-bottom, 0px));
		background: rgba(0, 0, 0, 0.45);
		z-index: 45;
	}
	.ds-panel {
		position: fixed;
		left: 0;
		right: 0;
		bottom: calc(var(--bottom-nav-height) + env(safe-area-inset-bottom, 0px));
		z-index: 46;
		max-height: 66vh;
		display: flex;
		flex-direction: column;
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		box-shadow: 0 -16px 48px rgba(0, 0, 0, 0.45);
		overscroll-behavior: contain;
	}
	.ds-grip {
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-2) 0;
		flex-shrink: 0;
		cursor: grab;
		touch-action: none;
	}
	.ds-handle {
		width: 36px;
		height: 4px;
		border-radius: 999px;
		background: var(--border);
	}
	/* The list is the scroller, so it is the element that has to stop scroll
	   chaining (BUG-3386): `contain` on .ds-panel, which never scrolls, did
	   nothing, and a pull at the list's top chained to the page and
	   pull-to-refreshed it. */
	.ds-content {
		overflow-y: auto;
		overscroll-behavior: contain;
		padding: 0 0 var(--space-3);
		flex: 1 1 auto;
		min-height: 0;
	}
	/* And no page pull-to-refresh at all while a sheet is open (BUG-3386):
	   a drag on the backdrop, on the grip, or on a list too short to scroll
	   never reaches a scroller of the sheet's, and would reach the page. */
	:global(html:has(.ds-panel)),
	:global(body:has(.ds-panel)) {
		overscroll-behavior-y: none;
	}
</style>
