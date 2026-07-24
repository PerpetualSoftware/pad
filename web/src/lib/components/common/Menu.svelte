<script lang="ts">
	import { tick } from 'svelte';
	import type { Snippet } from 'svelte';
	import { pushEscapeHandler, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { clickOutside } from '$lib/utils/clickOutside';
	import { portal } from '$lib/utils/portalAction';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import BottomSheet from './BottomSheet.svelte';


	interface Props {
		/** Controlled by the parent (trigger owns the state). */
		open: boolean;
		onclose: () => void;
		/** The trigger element — focus returns here on close and it is
		 *  exempt from outside-click. */
		trigger?: HTMLElement;
		/** anchored = absolute below a position:relative wrapper;
		 *  portal = fixed coords portaled to <body> (escapes overflow and
		 *  content-visibility containment; flips above when cramped). */
		mode?: 'anchored' | 'portal';
		align?: 'right' | 'left';
		/** Portal mode: panel width used for viewport clamping. */
		width?: number;
		/** Swap to BottomSheet at the mobile breakpoint. */
		sheetOnMobile?: boolean;
		sheetTitle?: string;
		ariaLabel?: string;
		/** Extra containers that count as "inside" for outside-click
		 *  (e.g. a nested portaled emoji picker). */
		exempt?: () => (Element | null | undefined)[];
		children: Snippet;
	}

	let {
		open,
		onclose,
		trigger,
		mode = 'anchored',
		align = 'right',
		width = 220,
		sheetOnMobile = false,
		sheetTitle,
		ariaLabel,
		exempt,
		children
	}: Props = $props();

	let panelEl: HTMLDivElement | undefined = $state(undefined);
	let coords = $state({ top: 0, left: 0 });

	const useSheet = $derived(sheetOnMobile && viewport.isMobile);

	// Portal-mode placement: fixed coords from the trigger rect, flipping
	// above when there's no room below and clamping to the viewport.
	function place() {
		if (!trigger) return;
		const r = trigger.getBoundingClientRect();
		const panelH = panelEl?.offsetHeight ?? 240;
		const below = r.bottom + 6;
		const top = below + panelH > window.innerHeight && r.top - panelH - 6 > 0
			? r.top - panelH - 6
			: below;
		let left = align === 'right' ? r.right - width : r.left;
		left = Math.max(8, Math.min(left, window.innerWidth - width - 8));
		coords = { top, left };
	}

	// Focus the first row on open (menus are keyboard surfaces); return
	// focus to the trigger on close. Reads `open`/`panelEl`, writes neither.
	$effect(() => {
		if (open && !useSheet) {
			tick().then(() => {
				if (mode === 'portal') place();
				firstItem()?.focus();
			});
		}
	});

	// ESC chain registration while open (desktop panel only — BottomSheet
	// owns ESC on mobile).
	$effect(() => {
		if (!open || useSheet) return;
		const unregister = pushEscapeHandler(() => {
			close();
			return true;
		}, ESCAPE_PRIORITY.menu);
		return unregister;
	});

	// Portal mode: any scroll closes (coords would go stale).
	$effect(() => {
		if (!open || useSheet || mode !== 'portal') return;
		const onScroll = () => close();
		window.addEventListener('scroll', onScroll, { capture: true, passive: true });
		return () => window.removeEventListener('scroll', onScroll, { capture: true });
	});

	function close() {
		onclose();
		trigger?.focus();
	}

	function items(): HTMLElement[] {
		if (!panelEl) return [];
		return Array.from(
			panelEl.querySelectorAll<HTMLElement>('[role^="menuitem"]:not(:disabled)')
		);
	}

	function firstItem(): HTMLElement | undefined {
		return items()[0];
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.stopPropagation();
			close();
			return;
		}
		const list = items();
		if (list.length === 0) return;
		const idx = list.indexOf(document.activeElement as HTMLElement);
		let next = -1;
		if (e.key === 'ArrowDown') next = idx < list.length - 1 ? idx + 1 : 0;
		else if (e.key === 'ArrowUp') next = idx > 0 ? idx - 1 : list.length - 1;
		else if (e.key === 'Home') next = 0;
		else if (e.key === 'End') next = list.length - 1;
		if (next >= 0) {
			e.preventDefault();
			list[next].focus();
		}
	}
</script>

{#if useSheet}
	{#if open}
		<BottomSheet {open} onclose={onclose} title={sheetTitle}>
			{@render children()}
		</BottomSheet>
	{/if}
{:else if open}
	{#if mode === 'portal'}
		<div
			class="menu-panel portal"
			style:top="{coords.top}px"
			style:left="{coords.left}px"
			style:width="{width}px"
			role="menu"
			aria-label={ariaLabel}
			tabindex="-1"
			bind:this={panelEl}
			use:portal
			use:clickOutside={{ onOutside: onclose, extra: () => [trigger, ...(exempt?.() ?? [])] }}
			onkeydown={onKeydown}
		>
			{@render children()}
		</div>
	{:else}
		<div
			class="menu-panel anchored"
			class:align-left={align === 'left'}
			role="menu"
			aria-label={ariaLabel}
			tabindex="-1"
			bind:this={panelEl}
			use:clickOutside={{ onOutside: onclose, extra: () => [trigger, ...(exempt?.() ?? [])] }}
			onkeydown={onKeydown}
		>
			{@render children()}
		</div>
	{/if}
{/if}

<style>
	.menu-panel {
		background: var(--bg-raised, var(--bg-tertiary));
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: var(--shadow-md);
		padding: 5px;
		min-width: 180px;
		max-height: 340px;
		overflow-y: auto;
		z-index: 200;
	}

	.anchored {
		position: absolute;
		top: calc(100% + 6px);
		right: 0;
	}

	.anchored.align-left {
		right: auto;
		left: 0;
	}

	/* Portaled panels can't use scoped descendant selectors on children —
	   rows are MenuItem components with their own scoped styles, so the
	   panel shell itself is all that needs styling here. */
	.portal {
		position: fixed;
	}
</style>
