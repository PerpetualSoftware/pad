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
	 */
	function blockedByFrontLayer(): boolean {
		return isBlockedByModal(panelEl);
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

	function onKeydown(e: KeyboardEvent) {
		if (!open || e.key !== 'Escape') return;
		// ONLY the frontmost-layer check. This handler still closes on an Escape a
		// control already handled, exactly as it always has — TASK-2430 added a
		// `defaultPrevented` bail here and it was REVERTED: it is a defensible
		// change on its own merits, but it fires with NO viewer present, and this
		// task's contract is that an empty lease leaves behaviour untouched. It
		// belongs in its own item, not smuggled into an attachments phase.
		if (blockedByFrontLayer()) return;
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
		aria-modal="true"
		aria-label={label}
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
		<div class="ds-content">
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
	.ds-content {
		overflow-y: auto;
		padding: 0 0 var(--space-3);
		flex: 1 1 auto;
		min-height: 0;
	}
</style>
