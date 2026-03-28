<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import type { SearchResult } from '$lib/types';
	import { getFieldValue, itemUrlId, formatItemRef } from '$lib/types';

	let query = $state('');
	let results = $state<SearchResult[]>([]);
	let selectedIdx = $state(0);
	let searchTimeout: ReturnType<typeof setTimeout>;
	let inputEl = $state<HTMLInputElement>();

	$effect(() => {
		if (uiStore.searchOpen) {
			requestAnimationFrame(() => inputEl?.focus());
		} else {
			query = '';
			results = [];
			selectedIdx = 0;
		}
	});

	function doSearch() {
		clearTimeout(searchTimeout);
		if (!query.trim()) {
			results = [];
			return;
		}
		searchTimeout = setTimeout(async () => {
			const ws = workspaceStore.current?.slug;
			try {
				const resp = await api.search(query, ws);
				results = resp.results;
				selectedIdx = 0;
			} catch {
				results = [];
			}
		}, 200);
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			uiStore.closeSearch();
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			selectedIdx = Math.min(selectedIdx + 1, results.length - 1);
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			selectedIdx = Math.max(selectedIdx - 1, 0);
		} else if (e.key === 'Enter' && results.length > 0) {
			e.preventDefault();
			selectResult(results[selectedIdx]);
		}
	}

	function selectResult(r: SearchResult) {
		const ws = workspaceStore.current?.slug;
		const collSlug = r.item.collection_slug;
		if (ws && collSlug) {
			goto(`/${ws}/${collSlug}/${itemUrlId(r.item)}`);
		}
		uiStore.closeSearch();
	}

	function stripHtml(s: string): string {
		return s.replace(/<[^>]*>/g, '');
	}

	function statusColor(status: string): string {
		const s = status?.toLowerCase().replace(/-/g, '_');
		if (['done', 'completed', 'fixed', 'implemented', 'resolved'].includes(s)) return 'var(--accent-green)';
		if (['in_progress', 'exploring', 'fixing'].includes(s)) return 'var(--accent-amber)';
		if (['open', 'new', 'draft', 'todo', 'planned'].includes(s)) return 'var(--accent-blue)';
		if (s === 'active') return 'var(--accent-cyan)';
		return 'var(--text-muted)';
	}
</script>

{#if uiStore.searchOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => uiStore.closeSearch()}>
		<div class="palette" onclick={(e) => e.stopPropagation()} onkeydown={handleKeydown}>
			<div class="search-row">
				<svg class="search-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>
				<input
					bind:this={inputEl}
					bind:value={query}
					oninput={doSearch}
					placeholder="Search items, collections, docs..."
					class="search-input"
				/>
				<kbd class="search-hint">esc</kbd>
			</div>

			{#if results.length > 0}
				<div class="results">
					{#each results as r, i}
						{@const ref = formatItemRef(r.item)}
						{@const status = getFieldValue(r.item, 'status')}
						<button
							class="result"
							class:selected={i === selectedIdx}
							onclick={() => selectResult(r)}
						>
							<div class="result-main">
								<span class="result-icon">{r.item.collection_icon || '📦'}</span>
								{#if ref}
									<span class="result-ref">{ref}</span>
								{/if}
								<span class="result-title">{r.item.title}</span>
								{#if status}
									<span class="result-status" style="background: color-mix(in srgb, {statusColor(status)} 15%, transparent); color: {statusColor(status)};">
										{status.replace(/_/g, ' ')}
									</span>
								{/if}
							</div>
							{#if r.snippet}
								<div class="result-snippet">{stripHtml(r.snippet)}</div>
							{/if}
						</button>
					{/each}
				</div>
			{:else if query.trim()}
				<div class="no-results">No results for "{query}"</div>
			{:else}
				<div class="search-tips">
					<span class="tip-label">Try searching for</span>
					<span class="tip-example">task names, ideas, docs, or any text</span>
				</div>
			{/if}
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		padding-top: 15vh;
	}
	.palette {
		width: 100%;
		max-width: 560px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		overflow: hidden;
		max-height: 60vh;
		display: flex;
		flex-direction: column;
	}
	.search-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: 0 var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.search-icon {
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.search-input {
		flex: 1;
		padding: var(--space-4) 0;
		background: transparent;
		border: none;
		font-size: 1.1em;
		border-radius: 0;
	}
	.search-input:focus {
		border: none;
	}
	.search-hint {
		font-size: 0.7em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		padding: 1px 6px;
		border-radius: 3px;
		font-family: var(--font-mono);
		flex-shrink: 0;
	}
	.results {
		overflow-y: auto;
		padding: var(--space-2);
	}
	.result {
		display: block;
		width: 100%;
		text-align: left;
		padding: var(--space-3) var(--space-3);
		border-radius: var(--radius);
	}
	.result:hover, .result.selected {
		background: var(--bg-hover);
	}
	.result-main {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.result-icon { font-size: 1em; flex-shrink: 0; }
	.result-ref {
		font-family: var(--font-mono);
		font-size: 0.75em;
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.result-title { font-weight: 500; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.result-status {
		font-size: 0.7em;
		padding: 2px 8px;
		border-radius: 999px;
		flex-shrink: 0;
		text-transform: capitalize;
	}
	.result-snippet {
		font-size: 0.85em;
		color: var(--text-muted);
		margin-top: 2px;
		margin-left: 24px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.no-results {
		padding: var(--space-4);
		text-align: center;
		color: var(--text-muted);
	}
	.search-tips {
		padding: var(--space-4);
		text-align: center;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.tip-label {
		font-size: 0.8em;
		color: var(--text-muted);
	}
	.tip-example {
		font-size: 0.85em;
		color: var(--text-secondary);
	}
</style>
