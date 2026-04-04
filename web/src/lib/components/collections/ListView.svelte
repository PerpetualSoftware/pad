<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields } from '$lib/types';
	import { SvelteSet } from 'svelte/reactivity';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';
	import EmptyState from '../common/EmptyState.svelte';


	interface Props {
		items: Item[];
		collection: Collection;
		wsSlug?: string;
		groupField?: string;
		focusedItemId?: string | null;
		statusOptions?: string[];
		onStatusChange?: (item: Item, newStatus: string) => void | Promise<void>;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		onArchiveGroup?: (items: Item[]) => void;
		onGroupReorder?: (newOrder: string[]) => void;
		oncreate?: () => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		progressLabel?: string;
	}

	let {
		items,
		collection,
		wsSlug = '',
		groupField = 'status',
		focusedItemId = null,
		statusOptions,
		onStatusChange,
		onReorder,
		onArchiveGroup,
		onGroupReorder,
		oncreate,
		itemProgress,
		progressLabel = 'tasks'
	}: Props = $props();

	let confirmArchiveGroup = $state<string | null>(null);

	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

	let schema = $derived(parseSchema(collection));
	let field = $derived(schema.fields.find((f) => f.key === groupField));
	let groupOptions = $derived(field?.options ?? []);

	/**
	 * Display groups: predefined options first, then any additional
	 * values discovered from items (handles text fields with no options).
	 */
	let displayGroups = $derived.by(() => {
		const known = new Set(groupOptions);
		const extra: string[] = [];
		let hasUngrouped = false;
		for (const item of items) {
			const fields = parseFields(item);
			const value = fields[groupField] ?? '';
			if (!value) {
				hasUngrouped = true;
			} else if (!known.has(value)) {
				known.add(value);
				extra.push(value);
			}
		}
		const groups = [...groupOptions, ...extra.sort()];
		if (hasUngrouped) groups.push('');
		return groups;
	});

	let collapsedGroups = new SvelteSet<string>();

	// Group reordering state
	interface GroupItem { id: string }
	let groupItems = $state<GroupItem[]>([]);
	let isDraggingGroup = $state(false);

	$effect(() => {
		if (!isDraggingGroup) {
			groupItems = displayGroups.map((g) => ({ id: g }));
		}
	});

	function handleGroupConsider(e: CustomEvent<DndEvent<GroupItem>>) {
		groupItems = e.detail.items;
		isDraggingGroup = true;
	}

	function handleGroupFinalize(e: CustomEvent<DndEvent<GroupItem>>) {
		groupItems = e.detail.items;
		isDraggingGroup = false;
		if (onGroupReorder) {
			const newOrder = groupItems
				.filter((g: any) => !g[SHADOW_ITEM_MARKER_PROPERTY_NAME])
				.map((g) => g.id);
			onGroupReorder(newOrder);
		}
	}

	let isDragging = $state(false);
	let groupData: Record<string, Item[]> = $state({});

	/**
	 * Derived group data from props, grouped by the groupField value
	 * and sorted by sort_order within each group.
	 */
	let propGroupData = $derived.by(() => {
		const result: Record<string, Item[]> = {};
		for (const opt of groupOptions) {
			result[opt] = [];
		}
		for (const item of items) {
			const fields = parseFields(item);
			const value = fields[groupField] ?? '';
			if (result[value] !== undefined) {
				result[value].push(item);
			} else {
				result[value] = [item];
			}
		}
		for (const key of Object.keys(result)) {
			result[key].sort((a, b) => a.sort_order - b.sort_order);
		}
		return result;
	});

	/**
	 * Sync the mutable groupData from the derived prop data,
	 * but only when the user is not actively dragging.
	 */
	$effect(() => {
		const data = propGroupData;
		if (!isDragging) {
			groupData = data;
		}
	});

	function handleConsider(groupName: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[groupName] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleFinalize(groupName: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[groupName] = e.detail.items;

		// Capture the desired order BEFORE setting isDragging = false or awaiting,
		// because both can trigger reactive effects that overwrite groupData.
		const reorderUpdates = groupData[groupName]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ slug: item.id, sort_order: index }));

		isDragging = false;

		const { id: itemId, trigger } = e.detail.info;

		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem && onStatusChange) {
				const fields = parseFields(originalItem);
				if (fields[groupField] !== groupName) {
					await onStatusChange(originalItem, groupName);
				}
			}
		}

		if (onReorder && reorderUpdates.length > 0) {
			onReorder(reorderUpdates);
		}
	}

	function itemCount(groupItems: Item[]): number {
		return groupItems.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]).length;
	}

	function toggleGroup(groupName: string) {
		if (collapsedGroups.has(groupName)) {
			collapsedGroups.delete(groupName);
		} else {
			collapsedGroups.add(groupName);
		}
	}

	function formatLabel(value: string): string {
		if (!value) return 'Uncategorized';
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

{#if items.length === 0}
	<EmptyState {collection} {wsSlug} {oncreate} />
{:else}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="list-view"
		use:dndzone={{
			items: groupItems,
			flipDurationMs,
			type: 'list-group',
			dropTargetClasses: ['group-drop-target'],
			morphDisabled: true
		}}
		onconsider={handleGroupConsider}
		onfinalize={handleGroupFinalize}
	>
		{#each groupItems as group (group.id)}
			{@const groupName = group.id}
			{@const grpItems = groupData[groupName] ?? []}
			<div class="item-group">
				<div
					class="group-header"
					role="button"
					tabindex="0"
					onclick={() => toggleGroup(groupName)}
					onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleGroup(groupName); } }}
					aria-expanded={!collapsedGroups.has(groupName)}
				>
					<span class="group-drag-handle" title="Drag to reorder">⠿</span>
					<span class="collapse-icon" class:collapsed={collapsedGroups.has(groupName)}
						>&#9662;</span
					>
					<span class="group-title">{formatLabel(groupName)}</span>
					<span class="group-actions">
						<span class="group-count">{itemCount(grpItems)}</span>
						{#if onArchiveGroup && itemCount(grpItems) > 0}
							{#if confirmArchiveGroup === groupName}
								<span class="archive-confirm">
									<button class="archive-yes" onclick={(e) => { e.stopPropagation(); onArchiveGroup(grpItems); confirmArchiveGroup = null; }}>Archive {itemCount(grpItems)}?</button>
									<button class="archive-no" onclick={(e) => { e.stopPropagation(); confirmArchiveGroup = null; }}>Cancel</button>
								</span>
							{:else}
								<button
									class="archive-group-btn"
									title="Archive all {formatLabel(groupName).toLowerCase()} items"
									onclick={(e) => { e.stopPropagation(); confirmArchiveGroup = groupName; }}
								>&#128451;</button>
							{/if}
						{/if}
					</span>
				</div>

				{#if !collapsedGroups.has(groupName)}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="group-items"
						use:dndzone={{
							items: grpItems,
							flipDurationMs,
							type: 'list-item',
							dropTargetClasses: ['drop-target'],
							delayTouchStart: touchDragDelayMs
						}}
						onconsider={(e) => handleConsider(groupName, e)}
						onfinalize={(e) => handleFinalize(groupName, e)}
						oncontextmenu={(e) => e.preventDefault()}
					>
						{#each grpItems as item (item.id)}
							<div class="list-row" class:kb-focused={focusedItemId === item.id}>
								<ItemCard
									{item}
									{collection}
									compact={false}
									focused={focusedItemId === item.id}
									{statusOptions}
									onStatusClick={onStatusChange}
									progress={itemProgress?.[item.id] ?? null}
									{progressLabel}
								/>
							</div>
						{/each}
						{#if grpItems.length === 0}
							<div class="group-empty">No {formatLabel(groupName).toLowerCase()} items</div>
						{/if}
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}

<style>
	.list-view {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.item-group {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}

	.group-drag-handle {
		color: var(--text-muted);
		font-size: 0.75em;
		opacity: 0;
		transition: opacity 0.15s;
		user-select: none;
		cursor: grab;
	}

	.item-group:hover .group-drag-handle {
		opacity: 0.5;
	}

	.group-drag-handle:active {
		opacity: 1;
		cursor: grabbing;
	}

	.group-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
		font-weight: 600;
		font-size: 0.9em;
	}

	.group-header:hover {
		background: var(--bg-hover);
	}

	.group-actions {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		flex-shrink: 0;
	}

	.group-header:hover .archive-group-btn {
		opacity: 1;
	}

	.archive-group-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.8em;
		cursor: pointer;
		padding: 2px 4px;
		border-radius: var(--radius-sm);
		opacity: 0;
		transition: opacity 0.15s;
		line-height: 1;
	}

	.archive-group-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.archive-confirm {
		display: flex;
		gap: var(--space-1);
		align-items: center;
	}

	.archive-yes {
		background: none;
		border: none;
		color: var(--accent-red, #ef4444);
		font-size: 0.78em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
	}

	.archive-yes:hover {
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.archive-no {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.78em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
	}

	.archive-no:hover {
		color: var(--text-primary);
	}

	.collapse-icon {
		font-size: 0.7em;
		transition: transform 0.15s ease;
		color: var(--text-muted);
	}

	.collapse-icon.collapsed {
		transform: rotate(-90deg);
	}

	.group-title {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.group-count {
		font-size: 0.8em;
		font-weight: 400;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
		flex-shrink: 0;
	}

	.group-items {
		border-top: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		min-height: 32px;
		transition: background 0.15s ease;
	}

	.group-items:global(.drop-target) {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}

	.list-row {
		border-bottom: 1px solid var(--border);
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}

	.list-row:active {
		cursor: grabbing;
	}

	.list-row:last-child {
		border-bottom: none;
	}

	/* Override ItemCard border-radius and border inside list rows */
	.list-row :global(.item-card) {
		border: none;
		border-radius: 0;
		background: transparent;
	}

	.list-row :global(.item-card:hover) {
		background: var(--bg-hover);
	}

	.group-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.82em;
	}
</style>
