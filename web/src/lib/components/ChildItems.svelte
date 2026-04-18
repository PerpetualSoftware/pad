<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import type { Item } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import ChildChart from './ChildChart.svelte';
	import NestedChildren from './NestedChildren.svelte';

	interface Props {
		wsSlug: string;
		username?: string;
		itemSlug: string;
		itemId: string;
		parentFields?: Record<string, any>;
		terminalStatuses?: string[];
		onChildrenChange?: (children: Item[]) => void;
	}

	let { wsSlug, username = '', itemSlug, itemId, parentFields, terminalStatuses, onChildrenChange }: Props = $props();

	const defaultTerminal = ['done', 'completed', 'resolved', 'cancelled', 'rejected', 'wontfix', 'fixed', 'implemented', 'archived', 'disabled', 'deprecated'];
	const terminal = $derived(terminalStatuses ?? defaultTerminal);

	let children = $state<Item[]>([]);
	let loading = $state(true);
	let error = $state('');
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeSync: (() => void) | null = null;

	let expandedIds = $state<Set<string>>(new Set());

	function toggleExpand(child: Item) {
		const next = new Set(expandedIds);
		if (next.has(child.id)) {
			next.delete(child.id);
		} else {
			next.add(child.id);
		}
		expandedIds = next;
	}

	const statusOrder: string[] = ['in_progress', 'open', 'blocked', 'done'];
	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

	let doneCount = $derived(children.filter((t) => terminal.includes(parseFields(t).status)).length);
	let totalCount = $derived(children.length);
	let percentage = $derived(totalCount > 0 ? Math.round((doneCount / totalCount) * 100) : 0);

	/** Set of child item IDs — exposed for deduplication by the parent page */
	export function getChildIds(): Set<string> {
		return new Set(children.map(c => c.id));
	}

	let groups = $derived.by(() => {
		const map = new SvelteMap<string, Item[]>();
		for (const child of children) {
			const status = parseFields(child).status ?? 'open';
			if (!map.has(status)) map.set(status, []);
			map.get(status)!.push(child);
		}
		const sorted: [string, Item[]][] = [];
		for (const s of statusOrder) {
			if (map.has(s)) sorted.push([s, map.get(s)!]);
		}
		for (const [s, items] of map) {
			if (!statusOrder.includes(s)) sorted.push([s, items]);
		}
		return sorted;
	});

	// ── Drag-and-drop state ──────────────────────────────────────────────────
	let isDragging = $state(false);
	let groupData: Record<string, Item[]> = $state({});

	$effect(() => {
		const g = groups;
		if (!isDragging) {
			const data: Record<string, Item[]> = {};
			for (const [status, statusChildren] of g) {
				data[status] = [...statusChildren];
			}
			groupData = data;
		}
	});

	function handleConsider(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleFinalize(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		isDragging = false;

		const updates = groupData[status]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ id: item.id, sort_order: index }));

		try {
			for (const { id, sort_order } of updates) {
				await api.items.update(wsSlug, id, { sort_order });
			}
		} catch (e) {
			console.error('Failed to persist reorder:', e);
		}
	}

	// ── Data loading ─────────────────────────────────────────────────────────

	async function loadChildren() {
		loading = true;
		error = '';
		try {
			children = await api.items.children(wsSlug, itemSlug);
			onChildrenChange?.(children);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load children';
			onChildrenChange?.([]);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadChildren();
	});

	onMount(() => {
		unsubscribeSync = syncService.onSync((result) => {
			if (!wsSlug || !itemSlug) return;
			// Only reload children on actual changes, not when caught up
			if (result.type !== 'caught_up') {
				loadChildren();
			}
		});
	});

	$effect(() => {
		unsubscribeSSE?.();
		unsubscribeSSE = null;

		if (!wsSlug || !itemSlug) return;

		unsubscribeSSE = sseService.onItemEvent((event) => {
			if (!['item_created', 'item_updated', 'item_archived', 'item_restored'].includes(event.type)) return;
			// Skip self-triggered content saves — they don't affect children
			if (event.type === 'item_updated' && (editorStore.dirty || Date.now() - editorStore.lastSaveTime < 5000)) return;
			loadChildren();
		});
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeSync?.();
	});

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

{#if loading || children.length > 0}
<div class="child-items">
	<div class="section-header">
		<h3>Children</h3>
		<span class="child-count">{doneCount}/{totalCount} done</span>
	</div>

	<div class="progress-bar">
		<div class="progress-fill" style:width="{percentage}%"></div>
	</div>

	{#if !loading && children.length >= 2}
		<ChildChart {children} startDate={parentFields?.start_date} endDate={parentFields?.end_date} {terminalStatuses} />
	{/if}

	{#if loading}
		<div class="loading">
			<span class="spinner"></span>
			<span>Loading children...</span>
		</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		{#each groups as [status, _statusChildren] (status)}
			<div class="child-group">
				<div class="group-label">{formatLabel(status)} ({(groupData[status] ?? []).length})</div>
				<div
					class="child-list"
					use:dndzone={{
						items: groupData[status] ?? [],
						flipDurationMs,
						type: 'child-item',
						dropTargetClasses: ['drop-target'],
						delayTouchStart: touchDragDelayMs
					}}
					onconsider={(e) => handleConsider(status, e)}
					onfinalize={(e) => handleFinalize(status, e)}
				>
					{#each groupData[status] ?? [] as child (child.id)}
						{@const fields = parseFields(child)}
						{@const isDone = terminal.includes(fields.status)}
						{@const isExpanded = expandedIds.has(child.id)}
						{@const canExpand = child.has_children}
						<div class="child-item-wrapper">
							<div class="child-row-container">
								{#if canExpand}
									<button class="expand-toggle" onclick={(e) => { e.preventDefault(); toggleExpand(child); }} title={isExpanded ? 'Collapse' : 'Expand'}>
										<span class="expand-icon" class:expanded={isExpanded}>▸</span>
									</button>
								{/if}
								<a href="/{username}/{wsSlug}/{child.collection_slug}/{child.slug}" class="child-row" class:has-toggle={canExpand}>
									<span class="child-ref">{formatItemRef(child) ?? ''}</span>
									<span class="child-title" class:done={isDone}>{child.title}</span>
									{#if fields.priority}
										<span
											class="child-priority"
											class:high={fields.priority === 'high'}
											class:critical={fields.priority === 'critical'}
										>
											{fields.priority}
										</span>
									{/if}
								</a>
							</div>
							{#if canExpand && isExpanded}
								<NestedChildren {wsSlug} {username} parentSlug={child.slug} depth={1} maxDepth={3} {terminalStatuses} />
							{/if}
						</div>
					{/each}
				</div>
			</div>
		{/each}

	{/if}
</div>

<!-- Print-only flat checklist (PLAN-620 / TASK-624). Hidden on screen;
     visible in print via @media print rule below. The interactive
     `.child-items` view is hidden in print so this takes its place. -->
{#if !loading && !error && children.length > 0}
	<div class="print-children" aria-hidden="true">
		<div class="print-children-header">
			Children ({doneCount}/{totalCount} done)
		</div>
		<ul class="print-child-list">
			{#each children as child (child.id)}
				{@const childFields = parseFields(child)}
				{@const isDone = terminal.includes(childFields.status)}
				<li class="print-child-row" class:done={isDone}>
					<span class="print-check">{isDone ? '[x]' : '[ ]'}</span>
					{#if formatItemRef(child)}
						<span class="print-child-ref">{formatItemRef(child)}</span>
					{/if}
					<span class="print-child-title">{child.title}</span>
					{#if childFields.status}
						<span class="print-child-status">({formatLabel(childFields.status)})</span>
					{/if}
				</li>
			{/each}
		</ul>
	</div>
{/if}
{/if}

<style>
	.child-items {
		padding: var(--space-4) 0;
		border-top: 1px solid var(--border);
	}

	.section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-3);
	}

	.section-header h3 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.child-count {
		font-size: 0.8em;
		color: var(--text-muted);
		font-weight: 400;
	}

	.progress-bar {
		height: 6px;
		background: var(--bg-tertiary);
		border-radius: 3px;
		overflow: hidden;
		margin-bottom: var(--space-3);
	}

	.progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 3px;
		transition: width 0.3s ease;
	}

	.child-group {
		margin-top: var(--space-3);
	}

	.group-label {
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	.child-item-wrapper {
		/* container for row + nested children */
	}

	.child-row-container {
		display: flex;
		align-items: center;
	}

	.expand-toggle {
		background: none;
		border: none;
		cursor: pointer;
		padding: 0 2px;
		color: var(--text-muted);
		font-size: 0.8em;
		line-height: 1;
		flex-shrink: 0;
		width: 20px;
		text-align: center;
	}

	.expand-toggle:hover {
		color: var(--text-primary);
	}

	.expand-icon {
		display: inline-block;
		transition: transform 0.15s ease;
	}

	.expand-icon.expanded {
		transform: rotate(90deg);
	}

	.child-list {
		min-height: 4px;
	}

	:global(.drop-target) {
		outline: 2px dashed var(--accent-blue);
		outline-offset: -2px;
		border-radius: var(--radius-sm);
	}

	.child-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-2);
		text-decoration: none;
		color: inherit;
		border-bottom: 1px solid var(--border);
		transition: background 0.1s;
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}

	.child-row:hover {
		background: var(--bg-hover);
	}

	.child-row:active {
		cursor: grabbing;
	}

	.child-row:last-child {
		border-bottom: none;
	}

	.child-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.child-title {
		flex: 1;
		font-size: 0.88em;
		color: var(--text-primary);
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.child-title.done {
		text-decoration: line-through;
		color: var(--text-muted);
	}

	.child-priority {
		font-size: 0.72em;
		padding: 1px 6px;
		border-radius: 3px;
		white-space: nowrap;
		font-weight: 500;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		flex-shrink: 0;
	}

	.child-priority.high {
		color: var(--accent-amber);
		background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
	}

	.child-priority.critical {
		color: var(--accent-orange);
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) 0;
		color: var(--text-muted);
		font-size: 0.9em;
		justify-content: center;
	}

	.spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.error-msg {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	/* -----------------------------------------------------------------
	   Print-only flat checklist (PLAN-620 / TASK-624).
	   Hidden on screen. In print:
	     - hide the interactive `.child-items` view (chart, drag-drop
	       groups, expand toggles, progress bar) since those rely on
	       state and controls that have no meaning on paper;
	     - show a plain `<ul>` of children in a simple checkbox layout:
	         [x] TASK-621 · Title (done)
	         [ ] TASK-622 · Title (in progress)
	     - the block is `break-inside: avoid` where it fits so the list
	       doesn't split across pages.
	   ----------------------------------------------------------------- */
	.print-children {
		display: none;
	}

	@media print {
		.child-items {
			display: none !important;
		}

		.print-children {
			display: block;
			margin: 14pt 0 0 0;
			padding-top: 8pt;
			border-top: 1px solid #ccc;
			page-break-inside: avoid;
			break-inside: avoid;
		}
		.print-children-header {
			font-size: 10pt;
			font-weight: 600;
			text-transform: uppercase;
			letter-spacing: 0.05em;
			color: #333;
			margin: 0 0 6pt 0;
		}
		.print-child-list {
			list-style: none;
			padding: 0;
			margin: 0;
		}
		.print-child-row {
			font-size: 10pt;
			line-height: 1.45;
			padding: 1pt 0;
			color: #000;
			break-inside: avoid;
		}
		.print-check {
			display: inline-block;
			width: 15pt;
			font-family: var(--font-mono);
			color: #000;
			font-weight: 500;
		}
		.print-child-ref {
			font-weight: 500;
			margin-right: 4pt;
			color: #333;
			font-variant-numeric: tabular-nums;
		}
		.print-child-title {
			color: #000;
		}
		.print-child-row.done .print-child-title {
			color: #555;
		}
		.print-child-status {
			color: #777;
			margin-left: 4pt;
			font-size: 9pt;
			font-style: italic;
		}
	}

</style>
