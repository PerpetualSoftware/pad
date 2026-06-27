<!--
	ItemActionsMenu — per-item reorder kebab (IDEA-1898).

	A small top-right menu on each item with move-to-top / up / down /
	move-to-bottom. The menu-driven counterpart to drag-to-reorder: works
	on touch (board drag is disabled on mobile) and in long lists where
	dragging across many rows is painful.

	The host (ListView / BoardView / TableView / ChildItems) owns all
	ordering context — it passes a bound `onReorder(dir)` and the set of
	`disabledDirs` for this item's position. This component is purely the
	menu surface: open/close, positioning, a11y, and emitting the chosen
	direction.

	Why a dedicated component (not QuickActionsMenu): QuickActionsMenu is
	coupled to prompt-template quick actions (resolve/copy, inline create
	form, emoji picker). This is a tiny fixed 4-action menu, so a focused
	component is simpler than bending that one.
-->
<script lang="ts">
	import { tick } from 'svelte';
	import type { Item } from '$lib/types';
	import type { ReorderDirection } from '$lib/collections/reorder';

	interface Props {
		item: Item;
		/** Fire the reorder. The host has the item + group context bound. */
		onReorder: (dir: ReorderDirection) => void;
		/** Directions to hide (edge of group) — see disabledDirections(). */
		disabledDirs?: Set<ReorderDirection>;
		/** Accessible label suffix, e.g. the item title. */
		label?: string;
	}

	let { item, onReorder, disabledDirs, label }: Props = $props();

	interface Action {
		dir: ReorderDirection;
		icon: string;
		text: string;
	}
	const ALL_ACTIONS: Action[] = [
		{ dir: 'top', icon: '⤒', text: 'Move to top' },
		{ dir: 'up', icon: '↑', text: 'Move up' },
		{ dir: 'down', icon: '↓', text: 'Move down' },
		{ dir: 'bottom', icon: '⤓', text: 'Move to bottom' }
	];
	let actions = $derived(ALL_ACTIONS.filter((a) => !disabledDirs?.has(a.dir)));

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement>();
	let panelEl = $state<HTMLDivElement>();
	let x = $state(0);
	let y = $state(0);
	let activeIndex = $state(0);

	const PANEL_W = 188;
	const ROW_H = 38;

	/**
	 * Portal the desktop panel to <body>. ItemCard's host rows establish
	 * paint containment via `content-visibility: auto` (ListView rows,
	 * BoardView cards, TableView rows — all for off-screen virtualization),
	 * which clips descendant overflow to the row box. An absolutely-
	 * positioned dropdown would be sheared off at ~36–60px; a body-level
	 * portal with fixed coords escapes the containment entirely. Mirrors
	 * the EmojiPickerButton pattern.
	 */
	function portal(node: HTMLElement) {
		document.body.appendChild(node);
		return {
			destroy() {
				node.remove();
			}
		};
	}

	function openMenu() {
		if (!triggerEl || actions.length === 0) return;
		const r = triggerEl.getBoundingClientRect();
		// Right-align the panel under the trigger; flip above if it would
		// leave the viewport, clamp to the left edge. The small fixed
		// dropdown works on every viewport (it fits a 188px panel even on a
		// 320px phone), so there's no separate mobile sheet — a sheet for a
		// 4-item menu read as "taking over the card".
		let left = r.right - PANEL_W;
		if (left < 8) left = 8;
		const estH = actions.length * ROW_H + 8;
		let top = r.bottom + 4;
		if (top + estH > window.innerHeight - 8) {
			top = Math.max(8, r.top - estH - 4);
		}
		x = left;
		y = top;
		open = true;
		activeIndex = 0;
		tick().then(() => focusItem(0));
	}

	function closeMenu(returnFocus = true) {
		open = false;
		if (returnFocus) triggerEl?.focus();
	}

	function pick(dir: ReorderDirection) {
		if (disabledDirs?.has(dir)) return;
		// Close before firing so the menu can't be machine-gunned against
		// the optimistic reorder state — a second move needs a reopen, by
		// which point the new order has settled. (Drag is naturally
		// debounced; menu clicks are not.)
		closeMenu(false);
		onReorder(dir);
	}

	function onTriggerClick(e: MouseEvent) {
		// The trigger lives inside the card's <a>; stop the click from
		// navigating (mirrors toggleStar in ItemCard).
		e.preventDefault();
		e.stopPropagation();
		if (open) closeMenu();
		else openMenu();
	}

	function focusItem(i: number) {
		const btns = panelEl?.querySelectorAll<HTMLButtonElement>('.iam-item');
		btns?.[i]?.focus();
	}

	function onPanelKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.preventDefault();
			closeMenu();
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			activeIndex = (activeIndex + 1) % actions.length;
			focusItem(activeIndex);
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			activeIndex = (activeIndex - 1 + actions.length) % actions.length;
			focusItem(activeIndex);
		} else if (e.key === 'Home') {
			e.preventDefault();
			activeIndex = 0;
			focusItem(0);
		} else if (e.key === 'End') {
			e.preventDefault();
			activeIndex = actions.length - 1;
			focusItem(activeIndex);
		}
	}

	// Instance-scoped outside-click: don't match a shared class (that would
	// also match OTHER item menus' triggers and keep this one open when
	// another opens). Check this instance's own trigger + portaled panel.
	function onWindowClick(e: MouseEvent) {
		if (!open) return;
		const t = e.target as Node | null;
		if (!t) return;
		if (triggerEl?.contains(t) || panelEl?.contains(t)) return;
		closeMenu(false);
	}

	// A scroll while open detaches the fixed panel from its row — close it.
	// Capture catches inner scroll containers (board column, table scroll)
	// in addition to the window. Only listens while open.
	$effect(() => {
		if (!open) return;
		const onScroll = () => closeMenu(false);
		window.addEventListener('scroll', onScroll, true);
		return () => window.removeEventListener('scroll', onScroll, true);
	});
</script>

<svelte:window onclick={onWindowClick} />

{#if actions.length > 0}
	<span class="item-actions-menu">
		<button
			bind:this={triggerEl}
			type="button"
			class="iam-trigger"
			class:open
			aria-haspopup="menu"
			aria-expanded={open}
			aria-label={label ? `Reorder ${label}` : 'Reorder item'}
			title="Reorder"
			onclick={onTriggerClick}
		>
			⋮
		</button>

		{#if open}
			<div
				bind:this={panelEl}
				class="iam-panel"
				role="menu"
				tabindex="-1"
				use:portal
				style="position:fixed; left:{x}px; top:{y}px; z-index:99999;"
				onkeydown={onPanelKeydown}
			>
				{#each actions as a, i (a.dir)}
					<button
						type="button"
						class="iam-item"
						role="menuitem"
						tabindex={i === activeIndex ? 0 : -1}
						onclick={() => pick(a.dir)}
					>
						<span class="iam-icon" aria-hidden="true">{a.icon}</span>
						{a.text}
					</button>
				{/each}
			</div>
		{/if}
	</span>
{/if}

<style>
	.item-actions-menu {
		display: inline-flex;
		flex-shrink: 0;
	}

	.iam-trigger {
		border: none;
		background: none;
		cursor: pointer;
		padding: 0 2px;
		font-size: 1em;
		line-height: 1;
		color: var(--text-muted);
		/* Steady (not hover-only) opacity so it stays reachable on touch,
		   where the menu is the only reorder mechanism. */
		opacity: 0.5;
		border-radius: var(--radius-sm);
		transition: opacity 0.15s, color 0.15s;
	}

	.iam-trigger:hover,
	.iam-trigger.open {
		opacity: 1;
		color: var(--text-primary);
	}

	.iam-trigger:focus-visible {
		opacity: 1;
		outline: 2px solid var(--accent-blue);
		outline-offset: 1px;
	}

	/* Portaled panel lives at <body>, so it can't rely on any ancestor
	   variables being in scope — it uses the same global custom props the
	   rest of the app's surfaces use. */
	:global(.iam-panel) {
		min-width: 188px;
		padding: var(--space-1);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		box-shadow: var(--shadow-md, 0 4px 12px rgba(0, 0, 0, 0.15));
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	:global(.iam-item) {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: 8px 10px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.875em;
		text-align: left;
		cursor: pointer;
	}

	:global(.iam-item:hover),
	:global(.iam-item:focus-visible) {
		background: var(--bg-hover);
		outline: none;
	}

	:global(.iam-icon) {
		width: 1.1em;
		text-align: center;
		flex-shrink: 0;
		color: var(--text-muted);
	}

	/* Comfortable tap targets on touch without a separate sheet. */
	@media (pointer: coarse) {
		:global(.iam-item) {
			padding: 12px 12px;
		}
	}
</style>
