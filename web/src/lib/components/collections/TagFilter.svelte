<script lang="ts">
	/**
	 * Multi-select tag filter for the collection page. Renders one toggle
	 * chip per tag present in the *current collection*, each showing the
	 * tag's item count within that collection. Selection is OR semantics —
	 * the caller shows items carrying ANY selected tag.
	 *
	 * Counts are computed client-side from the local index (the collection
	 * page already holds every row's tags), so this is workspace-agnostic
	 * and needs no network round-trip. Chip styling mirrors
	 * `fields/TagInput.svelte` for visual consistency.
	 */
	interface Props {
		/** Collection-scoped tags with counts, ordered by count desc. */
		tags: { tag: string; count: number }[];
		/** Currently-selected tag values. */
		selected: string[];
		onchange: (selected: string[]) => void;
	}

	let { tags, selected, onchange }: Props = $props();

	let selectedSet = $derived(new Set(selected));

	function toggle(tag: string) {
		if (selectedSet.has(tag)) {
			onchange(selected.filter((t) => t !== tag));
		} else {
			onchange([...selected, tag]);
		}
	}

	function clearAll() {
		onchange([]);
	}
</script>

{#if tags.length > 0}
	<div class="tag-filter" role="group" aria-label="Filter by tag">
		{#each tags as { tag, count } (tag)}
			{@const active = selectedSet.has(tag)}
			<button
				type="button"
				class="tag-chip"
				class:active
				aria-pressed={active}
				onclick={() => toggle(tag)}
			>
				<span class="tag-name">{tag}</span>
				<span class="tag-count">{count}</span>
			</button>
		{/each}
		{#if selected.length > 0}
			<button type="button" class="tag-clear" onclick={clearAll}>Clear tags</button>
		{/if}
	</div>
{/if}

<style>
	.tag-filter {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1, 0.25rem);
		align-items: center;
	}
	.tag-chip {
		display: inline-flex;
		align-items: center;
		gap: 0.4em;
		padding: 0.1em 0.5em;
		font-size: var(--text-xs, 0.75rem);
		line-height: 1.5;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		color: var(--text-primary);
		white-space: nowrap;
		cursor: pointer;
	}
	.tag-chip:hover:not(.active) {
		background: var(--bg-hover, var(--bg-tertiary));
	}
	.tag-chip.active {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
		font-weight: 600;
	}
	.tag-count {
		font-variant-numeric: tabular-nums;
		opacity: 0.7;
		font-size: 0.92em;
	}
	.tag-chip.active .tag-count {
		opacity: 0.85;
	}
	.tag-clear {
		background: none;
		border: none;
		padding: 0.1em 0.4em;
		font-size: var(--text-xs, 0.75rem);
		color: var(--text-secondary);
		cursor: pointer;
		text-decoration: underline;
	}
	.tag-clear:hover {
		color: var(--text-primary);
	}
</style>
