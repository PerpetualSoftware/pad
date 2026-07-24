<!--
	ItemActionsMenu — per-item reorder kebab (IDEA-1898).

	A small top-right menu on each item with move-to-top / up / down /
	move-to-bottom. The menu-driven counterpart to drag-to-reorder: works
	on touch (board drag is disabled on mobile) and in long lists where
	dragging across many rows is painful.

	The host (ListView / BoardView / TableView / ChildItems) owns all
	ordering context — it passes a bound `onReorder(dir)` and the set of
	`disabledDirs` for this item's position. This component is purely the
	trigger + emitting the chosen direction; the shared Menu primitive
	(PLAN-2290 Phase 2) owns positioning, outside-click, keyboard nav,
	and menu a11y.

	Menu runs in PORTAL mode here, non-negotiably: ItemCard's host rows
	establish paint containment via `content-visibility: auto` (ListView
	rows, BoardView cards, TableView rows — all for off-screen
	virtualization), which clips absolutely-positioned descendants to the
	row box. The portal's body-level fixed panel escapes the containment
	entirely.

	No mobile sheet on purpose: the small fixed dropdown works on every
	viewport (a 188px panel fits even on a 320px phone), and a sheet for
	a 4-item menu read as "taking over the card".

	Why a dedicated component (not QuickActionsMenu): QuickActionsMenu is
	coupled to prompt-template quick actions (resolve/copy, inline create
	form, emoji picker). This is a tiny fixed 4-action menu, so a focused
	component is simpler than bending that one.
-->
<script lang="ts">
	import type { Item } from '$lib/types';
	import type { ReorderDirection } from '$lib/collections/reorder';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';

	// Horizontal (adjacent-column) moves ride a SEPARATE optional callback so
	// the shared vertical `onReorder` type stays 'top'|'bottom'|'up'|'down'
	// for the List/Table/Child hosts (TASK-1908 / DR-6). MenuDirection is
	// internal to this menu — it never appears on a prop those hosts consume.
	type MenuDirection = ReorderDirection | 'left' | 'right';

	interface Props {
		item: Item;
		/** Fire the vertical reorder. The host has the item + group context bound. */
		onReorder: (dir: ReorderDirection) => void;
		/**
		 * Fire an adjacent-column move (board only). Only wired by BoardView;
		 * omitted by every other host, so left/right never fire elsewhere.
		 */
		onMove?: (dir: 'left' | 'right') => void;
		/** Render the Move left / Move right entries (BoardView passes true). */
		horizontal?: boolean;
		/** Directions to hide (edge of group / board) — see disabledDirections(). */
		disabledDirs?: Set<MenuDirection>;
		/** Accessible label suffix, e.g. the item title. */
		label?: string;
	}

	let { item, onReorder, onMove, horizontal = false, disabledDirs, label }: Props = $props();

	interface Action {
		dir: MenuDirection;
		icon: string;
		text: string;
	}
	const VERTICAL_ACTIONS: Action[] = [
		{ dir: 'top', icon: '⤒', text: 'Move to top' },
		{ dir: 'up', icon: '↑', text: 'Move up' },
		{ dir: 'down', icon: '↓', text: 'Move down' },
		{ dir: 'bottom', icon: '⤓', text: 'Move to bottom' }
	];
	const HORIZONTAL_ACTIONS: Action[] = [
		{ dir: 'left', icon: '←', text: 'Move left' },
		{ dir: 'right', icon: '→', text: 'Move right' }
	];
	let actions = $derived(
		[...VERTICAL_ACTIONS, ...(horizontal ? HORIZONTAL_ACTIONS : [])].filter(
			(a) => !disabledDirs?.has(a.dir)
		)
	);

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement>();

	function pick(dir: MenuDirection) {
		if (disabledDirs?.has(dir)) return;
		// Close before firing so the menu can't be machine-gunned against
		// the optimistic reorder state — a second move needs a reopen, by
		// which point the new order has settled. (Drag is naturally
		// debounced; menu clicks are not.)
		open = false;
		if (dir === 'left' || dir === 'right') {
			onMove?.(dir);
		} else {
			onReorder(dir);
		}
	}

	function onTriggerClick(e: MouseEvent) {
		// The trigger lives inside the card's <a>; stop the click from
		// navigating (mirrors toggleStar in ItemCard). This is card
		// behavior, NOT an outside-click workaround — it stays under the
		// Menu primitive.
		e.preventDefault();
		e.stopPropagation();
		open = !open;
	}
</script>

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

		<Menu
			{open}
			onclose={() => (open = false)}
			trigger={triggerEl}
			mode="portal"
			width={188}
			ariaLabel={label ? `Reorder ${label}` : 'Reorder item'}
		>
			{#each actions as a (a.dir)}
				<MenuItem icon={a.icon} onclick={() => pick(a.dir)}>{a.text}</MenuItem>
			{/each}
		</Menu>
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
</style>
