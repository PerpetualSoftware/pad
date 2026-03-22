<script lang="ts">
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import type { SearchResult } from '$lib/types';
	import { getFieldValue } from '$lib/types';

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
			goto(`/${ws}/${collSlug}/${r.item.slug}`);
		}
		uiStore.closeSearch();
	}

	function stripHtml(s: string): string {
		return s.replace(/<[^>]*>/g, '');
	}
</script>

{#if uiStore.searchOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={() => uiStore.closeSearch()}>
		<div class="palette" onclick={(e) => e.stopPropagation()} onkeydown={handleKeydown}>
			<input
				bind:this={inputEl}
				bind:value={query}
				oninput={doSearch}
				placeholder="Search items..."
				class="search-input"
			/>

			{#if results.length > 0}
				<div class="results">
					{#each results as r, i}
						<button
							class="result"
							class:selected={i === selectedIdx}
							onclick={() => selectResult(r)}
						>
							<div class="result-main">
								<span class="result-icon">{r.item.collection_icon || '📦'}</span>
								<span class="result-title">{r.item.title}</span>
								{#if getFieldValue(r.item, 'status')}
									<span class="result-status">{getFieldValue(r.item, 'status')}</span>
								{/if}
							</div>
							{#if r.snippet}
								<div class="result-snippet">{stripHtml(r.snippet)}</div>
							{/if}
						</button>
					{/each}
				</div>
			{:else if query.trim()}
				<div class="no-results">No results found</div>
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
	.search-input {
		width: 100%;
		padding: var(--space-4) var(--space-5);
		background: transparent;
		border: none;
		border-bottom: 1px solid var(--border);
		font-size: 1.1em;
		border-radius: 0;
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
	.result-icon { font-size: 1em; }
	.result-title { font-weight: 500; flex: 1; }
	.result-status {
		font-size: 0.75em;
		padding: 2px 8px;
		border-radius: 999px;
		background: var(--bg-tertiary);
		color: var(--text-secondary);
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
</style>
