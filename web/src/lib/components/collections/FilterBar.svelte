<script lang="ts">
	import type { Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';

	interface Props {
		collection: Collection;
		activeFilters: Record<string, string>;
		searchQuery: string;
		onFilterChange: (filters: Record<string, string>) => void;
		onSearchChange: (query: string) => void;
		relationLabels?: Record<string, string>;
	}

	let { collection, activeFilters, searchQuery, onFilterChange, onSearchChange, relationLabels = {} }: Props = $props();

	let schema = $derived(parseSchema(collection));
	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let statusOptions = $derived(statusField?.options ?? []);

	let activeStatus = $derived(activeFilters.status ?? 'all');
	let showAll = $derived(activeStatus === 'all');

	function setStatusFilter(value: string) {
		const next = { ...activeFilters };
		if (value === 'all') {
			delete next.status;
		} else {
			next.status = value;
		}
		onFilterChange(next);
	}

	let hasParentFilter = $derived(Object.keys(relationLabels).length > 0);
	let activeParent = $derived(activeFilters.parent ?? '');

	function setParentFilter(e: Event) {
		const value = (e.target as HTMLSelectElement).value;
		const next = { ...activeFilters };
		if (value === '') {
			delete next.parent;
		} else {
			next.parent = value;
		}
		onFilterChange(next);
	}

	function handleSearchInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onSearchChange(target.value);
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<div class="filter-bar">
	{#if statusOptions.length > 0}
		<div class="status-filters">
			<button
				class="filter-btn"
				class:active={showAll}
				onclick={() => setStatusFilter('all')}
			>All</button>
			{#each statusOptions as option (option)}
				<button
					class="filter-btn"
					class:active={activeStatus === option}
					onclick={() => setStatusFilter(option)}
				>{formatLabel(option)}</button>
			{/each}
		</div>
	{/if}

	{#if hasParentFilter}
		<select class="parent-filter" value={activeParent} onchange={setParentFilter}>
			<option value="">All plans</option>
			{#each Object.entries(relationLabels) as [id, label] (id)}
				<option value={id}>{label}</option>
			{/each}
		</select>
	{/if}

	<div class="search-wrapper">
		<input
			type="text"
			class="search-input"
			placeholder="Search {collection.name.toLowerCase()}..."
			value={searchQuery}
			oninput={handleSearchInput}
		/>
	</div>
</div>

<style>
	.filter-bar {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
		flex: 1;
	}

	.status-filters {
		display: flex;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}

	.filter-btn {
		background: var(--bg-secondary);
		border: none;
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
	}

	.filter-btn:not(:last-child) {
		border-right: 1px solid var(--border);
	}

	.filter-btn.active {
		background: var(--bg-tertiary);
		color: var(--text-primary);
		font-weight: 600;
	}

	.filter-btn:hover:not(.active) {
		background: var(--bg-hover);
	}

	.parent-filter {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
		max-width: 180px;
	}
	.parent-filter:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.search-wrapper {
		flex: 1;
		min-width: 140px;
		max-width: 260px;
	}

	.search-input {
		width: 100%;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
	}

	.search-input::placeholder {
		color: var(--text-muted);
	}

	.search-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}
</style>
