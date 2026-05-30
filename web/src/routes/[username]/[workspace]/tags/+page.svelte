<script lang="ts">
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import type { TagCount } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	let tags = $state<TagCount[]>([]);
	let loading = $state(true);
	let loadSeq = 0;

	const scrollRestoration = createScrollRestoration({
		ready: () => !loading,
		persistKey: () => (wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null)
	});
	export const snapshot = scrollRestoration.snapshot;

	$effect(() => {
		if (wsSlug) loadTags(wsSlug);
	});

	async function loadTags(ws: string) {
		loading = true;
		const seq = ++loadSeq;
		try {
			const result = await api.tags.list(ws);
			if (seq !== loadSeq) return;
			tags = result;
		} catch {
			if (seq !== loadSeq) return;
			tags = [];
		} finally {
			if (seq === loadSeq) loading = false;
		}
	}

	function tagUrl(tag: string): string {
		return `/${username}/${wsSlug}/tags/${encodeURIComponent(tag)}`;
	}
</script>

<svelte:head>
	<title>Tags - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="tags-page">
	<div class="page-header">
		<div class="page-header-left">
			<h1>🏷 Tags</h1>
			<span class="item-count">{tags.length} tag{tags.length !== 1 ? 's' : ''}</span>
		</div>
	</div>

	{#if loading}
		<div class="tag-cloud">
			{#each Array(6) as _, i (i)}
				<div class="skeleton-chip"></div>
			{/each}
		</div>
	{:else if tags.length === 0}
		<div class="empty-state">
			<div class="empty-icon">🏷</div>
			<h2>No tags yet</h2>
			<p>
				Add tags to an item from its detail page. Tags span collections, so one tag can group
				items of any type — a useful way to view related work together.
			</p>
		</div>
	{:else}
		<div class="tag-cloud">
			{#each tags as t (t.tag)}
				<a class="tag-card" href={tagUrl(t.tag)}>
					<span class="tag-name">{t.tag}</span>
					<span class="tag-count">{t.count}</span>
				</a>
			{/each}
		</div>
	{/if}
</div>

<style>
	.tags-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}

	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
		flex-wrap: wrap;
	}

	.page-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
	}

	.page-header h1 {
		font-size: 1.6em;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0;
	}

	.item-count {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.tag-cloud {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}

	.tag-card {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		text-decoration: none;
		color: var(--text-primary);
		transition:
			border-color 0.15s ease,
			background 0.15s ease;
	}

	.tag-card:hover {
		border-color: var(--text-tertiary, var(--text-secondary));
		background: var(--bg-tertiary);
	}

	.tag-name {
		font-size: 0.9em;
		font-weight: 500;
		word-break: break-word;
	}

	.tag-count {
		font-size: 0.75em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 7px;
		border-radius: 10px;
	}

	.skeleton-chip {
		width: 6rem;
		height: 36px;
		background: var(--bg-secondary);
		border-radius: 999px;
		animation: pulse 1.5s ease-in-out infinite;
	}

	@keyframes pulse {
		0%,
		100% {
			opacity: 0.4;
		}
		50% {
			opacity: 0.7;
		}
	}

	.empty-state {
		text-align: center;
		padding: var(--space-12) var(--space-6);
		color: var(--text-muted);
	}

	.empty-icon {
		font-size: 3em;
		margin-bottom: var(--space-4);
		opacity: 0.4;
	}

	.empty-state h2 {
		font-size: 1.1em;
		font-weight: 600;
		color: var(--text-secondary);
		margin: 0 0 var(--space-2);
	}

	.empty-state p {
		font-size: 0.9em;
		line-height: 1.5;
		max-width: 400px;
		margin: 0 auto;
	}
</style>
