<script lang="ts">
	import type { Item, Collection } from '$lib/types';
	import { parseSchema, parseFields, formatItemRef, itemUrlId } from '$lib/types';
	import { page } from '$app/state';
	import EmptyState from '../common/EmptyState.svelte';

	interface Props {
		items: Item[];
		collection: Collection;
		wsSlug?: string;
		onStatusChange?: (item: Item, newStatus: string) => void | Promise<void>;
		oncreate?: () => void;
		itemProgress?: Record<string, { total: number; done: number }>;
		progressLabel?: string;
		relationLabels?: Record<string, string>;
	}

	let {
		items,
		collection,
		wsSlug = '',
		onStatusChange,
		oncreate,
		itemProgress,
		progressLabel,
		relationLabels
	}: Props = $props();

	let resolvedWsSlug = $derived(wsSlug || page.params.workspace || '');
	let schema = $derived(parseSchema(collection));
	let visibleFields = $derived(schema.fields.filter((f) => !f.computed));

	let sortKey = $state('');
	let sortDir = $state<'asc' | 'desc'>('asc');

	let sortedItems = $derived.by(() => {
		if (!sortKey) return items;

		const sorted = [...items].sort((a, b) => {
			let aVal: any;
			let bVal: any;

			if (sortKey === 'title') {
				aVal = a.title;
				bVal = b.title;
			} else {
				const aFields = parseFields(a);
				const bFields = parseFields(b);
				aVal = aFields[sortKey] ?? '';
				bVal = bFields[sortKey] ?? '';
			}

			if (typeof aVal === 'number' && typeof bVal === 'number') {
				return sortDir === 'asc' ? aVal - bVal : bVal - aVal;
			}

			const aStr = String(aVal).toLowerCase();
			const bStr = String(bVal).toLowerCase();
			const cmp = aStr.localeCompare(bStr);
			return sortDir === 'asc' ? cmp : -cmp;
		});

		return sorted;
	});

	function toggleSort(key: string) {
		if (sortKey === key) {
			sortDir = sortDir === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			sortDir = 'asc';
		}
	}

	function cycleStatus(item: Item, options: string[]) {
		if (!onStatusChange) return;
		const fields = parseFields(item);
		const current = fields.status ?? '';
		const idx = options.indexOf(current);
		const next = options[(idx + 1) % options.length];
		onStatusChange(item, next);
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	function relativeTime(dateStr: string): string {
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		const diff = now - then;
		const minutes = Math.floor(diff / 60000);
		if (minutes < 1) return 'just now';
		if (minutes < 60) return `${minutes}m ago`;
		const hours = Math.floor(minutes / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		if (days < 30) return `${days}d ago`;
		return new Date(dateStr).toLocaleDateString();
	}
</script>

{#if items.length === 0}
	<EmptyState {collection} wsSlug={resolvedWsSlug} {oncreate} />
{:else}
<div class="table-scroll">
	<table class="table-view">
		<thead>
			<tr>
				<th class="col-ref">Ref</th>
				<th class="col-title">
					<button class="sort-btn" onclick={() => toggleSort('title')}>
						Title {sortKey === 'title' ? (sortDir === 'asc' ? '\u2191' : '\u2193') : ''}
					</button>
				</th>
				{#each visibleFields as field (field.key)}
					<th>
						<button class="sort-btn" onclick={() => toggleSort(field.key)}>
							{field.label || field.key} {sortKey === field.key ? (sortDir === 'asc' ? '\u2191' : '\u2193') : ''}
						</button>
					</th>
				{/each}
				<th class="col-updated">Updated</th>
			</tr>
		</thead>
		<tbody>
			{#each sortedItems as item (item.id)}
				{@const fields = parseFields(item)}
				<tr>
					<td class="col-ref"><span class="ref">{formatItemRef(item) ?? ''}</span></td>
					<td class="col-title">
						<a href="/{resolvedWsSlug}/{collection.slug}/{itemUrlId(item)}" class="title-link">{item.title}</a>
						{#if itemProgress?.[item.id]}
							{@const p = itemProgress[item.id]}
							<div class="cell-progress">
								<div class="cell-progress-bar"><div class="cell-progress-fill" style:width="{Math.round((p.done / p.total) * 100)}%"></div></div>
								<span class="cell-progress-text">{p.done}/{p.total}</span>
							</div>
						{/if}
					</td>
					{#each visibleFields as field (field.key)}
						<td>
							{#if field.key === 'status' && field.options && onStatusChange}
								<button class="cell-status" onclick={() => cycleStatus(item, field.options!)} title="Click to cycle">
									{formatLabel(fields[field.key] ?? '')}
								</button>
							{:else if field.key === 'phase' && fields[field.key] && relationLabels?.[fields[field.key]]}
								<span class="cell-relation">{relationLabels[fields[field.key]]}</span>
							{:else}
								<span class="cell-value">{fields[field.key] ?? ''}</span>
							{/if}
						</td>
					{/each}
					<td class="col-updated"><span class="cell-date">{relativeTime(item.updated_at)}</span></td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>
{/if}

<style>
	.table-scroll {
		overflow-x: auto;
		-webkit-overflow-scrolling: touch;
	}

	.table-view {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.88em;
	}

	.table-view th {
		text-align: left;
		padding: var(--space-2) var(--space-3);
		border-bottom: 2px solid var(--border);
		font-weight: 600;
		font-size: 0.85em;
		color: var(--text-secondary);
		white-space: nowrap;
		position: sticky;
		top: 0;
		background: var(--bg-primary);
		z-index: 1;
	}

	.table-view td {
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border-subtle, var(--border));
		vertical-align: middle;
	}

	.table-view tbody tr:hover {
		background: var(--bg-hover);
	}

	.sort-btn {
		background: none;
		border: none;
		color: inherit;
		font: inherit;
		font-weight: 600;
		cursor: pointer;
		padding: 0;
		white-space: nowrap;
	}

	.sort-btn:hover {
		color: var(--accent-blue);
	}

	.col-ref { width: 70px; }
	.col-title { min-width: 200px; }
	.col-updated { width: 90px; }

	.ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.title-link {
		color: var(--text-primary);
		text-decoration: none;
		font-weight: 500;
	}

	.title-link:hover {
		color: var(--accent-blue);
	}

	.cell-value {
		color: var(--text-secondary);
		font-size: 0.9em;
	}

	.cell-status {
		background: none;
		border: none;
		font: inherit;
		font-size: 0.85em;
		color: var(--text-secondary);
		cursor: pointer;
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
	}

	.cell-status:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.cell-relation {
		font-size: 0.85em;
		color: var(--accent-purple, var(--text-secondary));
	}

	.cell-date {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.cell-progress {
		display: flex;
		align-items: center;
		gap: 4px;
		margin-top: 2px;
	}

	.cell-progress-bar {
		flex: 1;
		height: 3px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		max-width: 60px;
	}

	.cell-progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
	}

	.cell-progress-text {
		font-size: 0.7em;
		color: var(--text-muted);
	}
</style>
