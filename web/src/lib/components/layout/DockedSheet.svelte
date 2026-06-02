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

	function onTouchStart(e: TouchEvent) {
		startY = e.touches[0].clientY;
		dragging = true;
	}
	function onTouchMove(e: TouchEvent) {
		if (!dragging) return;
		dragY = Math.max(0, e.touches[0].clientY - startY);
	}
	function onTouchEnd() {
		dragging = false;
		if (dragY > DISMISS_PX) {
			onclose();
		}
		dragY = 0;
	}

	function onKeydown(e: KeyboardEvent) {
		if (open && e.key === 'Escape') onclose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="ds-backdrop" onclick={onclose} transition:fade={{ duration: 160 }}></div>
	<div
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
