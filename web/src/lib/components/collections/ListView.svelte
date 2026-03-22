<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields } from '$lib/types';
	import { SvelteSet } from 'svelte/reactivity';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ItemCard from './ItemCard.svelte';


	interface Props {
		items: Item[];
		collection: Collection;
		groupField?: string;
		statusOptions?: string[];
		onStatusChange?: (item: Item, newStatus: string) => void;
		onReorder?: (updates: { slug: string; sort_order: number }[]) => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		relationLabels?: Record<string, string>;
	}

	let {
		items,
		collection,
		groupField = 'status',
		statusOptions,
		onStatusChange,
		onReorder,
		itemProgress,
		relationLabels
	}: Props = $props();

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
		for (const item of items) {
			const fields = parseFields(item);
			const value = fields[groupField] ?? '';
			if (value && !known.has(value)) {
				known.add(value);
				extra.push(value);
			}
		}
		return [...groupOptions, ...extra.sort()];
	});

	let collapsedGroups = new SvelteSet<string>();

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
			const value = fields[groupField] ?? 'none';
			if (result[value]) {
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

	function handleFinalize(groupName: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[groupName] = e.detail.items;
		isDragging = false;

		const { id: itemId, trigger } = e.detail.info;

		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = items.find((i) => i.id === itemId);
			if (originalItem && onStatusChange) {
				const fields = parseFields(originalItem);
				if (fields[groupField] !== groupName) {
					onStatusChange(originalItem, groupName);
				}
			}
		}

		if (onReorder) {
			const updates = groupData[groupName]
				.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
				.map((item, index) => ({ slug: item.slug, sort_order: index }));
			if (updates.length > 0) {
				onReorder(updates);
			}
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
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

{#if items.length === 0}
	<div class="empty-state">No items yet</div>
{:else}
	<div class="list-view">
		{#each displayGroups as groupName (groupName)}
			{@const groupItems = groupData[groupName] ?? []}
			<div class="item-group">
				<button
					class="group-header"
					onclick={() => toggleGroup(groupName)}
					aria-expanded={!collapsedGroups.has(groupName)}
				>
					<span class="collapse-icon" class:collapsed={collapsedGroups.has(groupName)}
						>&#9662;</span
					>
					<span class="group-title">{formatLabel(groupName)}</span>
					<span class="group-count">{itemCount(groupItems)}</span>
				</button>

				{#if !collapsedGroups.has(groupName)}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="group-items"
						use:dndzone={{
							items: groupItems,
							flipDurationMs,
							type: 'list-item',
							dropTargetClasses: ['drop-target'],
							delayTouchStart: touchDragDelayMs
						}}
						onconsider={(e) => handleConsider(groupName, e)}
						onfinalize={(e) => handleFinalize(groupName, e)}
						oncontextmenu={(e) => e.preventDefault()}
					>
						{#each groupItems as item (item.id)}
							<div class="list-row">
								<ItemCard
									{item}
									{collection}
									compact={false}
									{statusOptions}
									onStatusClick={onStatusChange}
									progress={itemProgress?.[item.id] ?? null}
									{relationLabels}
								/>
							</div>
						{/each}
						{#if groupItems.length === 0}
							<div class="group-empty">No {formatLabel(groupName).toLowerCase()} items</div>
						{/if}
					</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}

<style>
	.empty-state {
		text-align: center;
		padding: var(--space-8) 0;
		color: var(--text-muted);
		font-size: 0.95em;
	}

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
