<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { marked } from 'marked';

	let token = $derived(page.params.token ?? '');

	let loading = $state(true);
	let error = $state('');
	let requireAuth = $state(false);

	let shareType = $state<'item' | 'collection' | ''>('');
	let itemData = $state<{
		title: string;
		fields: Record<string, any>;
		content: string;
		collection_name?: string;
		collection_icon?: string;
		item_ref?: string;
	} | null>(null);
	let collectionData = $state<{
		name: string;
		icon?: string;
		description?: string;
		items: { title: string; item_ref?: string; status?: string }[];
	} | null>(null);

	let renderedContent = $derived.by(() => {
		if (!itemData?.content) return '';
		try {
			return marked(itemData.content) as string;
		} catch {
			return itemData.content;
		}
	});

	onMount(async () => {
		if (!token) {
			error = 'Invalid share link.';
			loading = false;
			return;
		}

		try {
			const data = await api.share.get(token);

			if (data.require_auth) {
				requireAuth = true;
				loading = false;
				return;
			}

			if (data.type === 'item') {
				shareType = 'item';
				let fields: Record<string, any> = {};
				if (data.item?.fields) {
					try {
						fields = typeof data.item.fields === 'string' ? JSON.parse(data.item.fields) : data.item.fields;
					} catch {
						fields = {};
					}
				}
				itemData = {
					title: data.item?.title ?? 'Untitled',
					fields,
					content: data.item?.content ?? '',
					collection_name: data.item?.collection_name,
					collection_icon: data.item?.collection_icon,
					item_ref: data.item?.item_ref
				};
			} else if (data.type === 'collection') {
				shareType = 'collection';
				collectionData = {
					name: data.collection?.name ?? 'Collection',
					icon: data.collection?.icon,
					description: data.collection?.description,
					items: data.collection?.items ?? []
				};
			} else {
				error = 'Unknown share type.';
			}
		} catch (e: any) {
			if (e.code === 'unauthorized' || e.code === 'auth_required') {
				requireAuth = true;
			} else {
				error = e.message ?? 'Failed to load shared content.';
			}
		} finally {
			loading = false;
		}
	});

	function formatFieldValue(value: unknown): string {
		if (value === null || value === undefined) return '';
		if (Array.isArray(value)) return value.join(', ');
		return String(value);
	}

	function formatFieldLabel(key: string): string {
		return key.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<svelte:head>
	{#if itemData}
		<title>{itemData.title} - Shared via Pad</title>
	{:else if collectionData}
		<title>{collectionData.name} - Shared via Pad</title>
	{:else}
		<title>Shared - Pad</title>
	{/if}
</svelte:head>

<div class="share-page">
	<div class="share-container">
		{#if loading}
			<div class="share-loading">
				<div class="loading-spinner"></div>
				<p>Loading shared content...</p>
			</div>
		{:else if requireAuth}
			<div class="share-auth">
				<div class="auth-icon">
					<svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
						<rect x="3" y="11" width="18" height="11" rx="2" ry="2"/>
						<path d="M7 11V7a5 5 0 0 1 10 0v4"/>
					</svg>
				</div>
				<h1>Sign in to view</h1>
				<p>This shared content requires authentication.</p>
				<a href="/login" class="auth-link">Sign in</a>
			</div>
		{:else if error}
			<div class="share-error">
				<h1>Unable to load</h1>
				<p>{error}</p>
			</div>
		{:else if shareType === 'item' && itemData}
			<article class="share-item">
				{#if itemData.collection_name}
					<div class="item-collection-badge">
						{#if itemData.collection_icon}
							<span class="collection-icon">{itemData.collection_icon}</span>
						{/if}
						<span>{itemData.collection_name}</span>
						{#if itemData.item_ref}
							<span class="item-ref">{itemData.item_ref}</span>
						{/if}
					</div>
				{/if}

				<h1 class="item-title">{itemData.title}</h1>

				{#if Object.keys(itemData.fields).length > 0}
					<div class="item-fields">
						{#each Object.entries(itemData.fields) as [key, value] (key)}
							{#if value !== null && value !== undefined && value !== ''}
								<div class="field-chip">
									<span class="field-chip-label">{formatFieldLabel(key)}</span>
									<span class="field-chip-value">{formatFieldValue(value)}</span>
								</div>
							{/if}
						{/each}
					</div>
				{/if}

				{#if itemData.content}
					<div class="item-content">
						{@html renderedContent}
					</div>
				{/if}
			</article>
		{:else if shareType === 'collection' && collectionData}
			<div class="share-collection">
				<div class="collection-header">
					{#if collectionData.icon}
						<span class="collection-icon-large">{collectionData.icon}</span>
					{/if}
					<h1>{collectionData.name}</h1>
				</div>

				{#if collectionData.description}
					<p class="collection-description">{collectionData.description}</p>
				{/if}

				{#if collectionData.items.length > 0}
					<div class="collection-items">
						{#each collectionData.items as item, i (i)}
							<div class="collection-item-row">
								<span class="collection-item-title">{item.title}</span>
								{#if item.item_ref}
									<span class="collection-item-ref">{item.item_ref}</span>
								{/if}
								{#if item.status}
									<span class="collection-item-status">{item.status}</span>
								{/if}
							</div>
						{/each}
					</div>
				{:else}
					<p class="collection-empty">No items in this collection.</p>
				{/if}
			</div>
		{/if}
	</div>

	<footer class="share-footer">
		<span>Powered by <a href="https://getpad.dev" target="_blank" rel="noopener noreferrer">Pad</a></span>
	</footer>
</div>

<style>
	.share-page {
		min-height: 100vh;
		display: flex;
		flex-direction: column;
		background: var(--bg-primary);
		color: var(--text-primary);
	}

	.share-container {
		flex: 1;
		width: 100%;
		max-width: 780px;
		margin: 0 auto;
		padding: var(--space-8) var(--space-5);
	}

	/* Loading */
	.share-loading {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-10) 0;
		color: var(--text-muted);
	}

	.loading-spinner {
		width: 32px;
		height: 32px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.8s linear infinite;
	}

	@keyframes spin {
		to { transform: rotate(360deg); }
	}

	/* Auth required */
	.share-auth {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.auth-icon {
		color: var(--text-muted);
	}

	.share-auth h1 {
		font-size: 1.5em;
		font-weight: 600;
	}

	.share-auth p {
		color: var(--text-secondary);
		font-size: 0.95em;
	}

	.auth-link {
		display: inline-block;
		margin-top: var(--space-2);
		padding: var(--space-2) var(--space-6);
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius);
		font-weight: 500;
		text-decoration: none;
		transition: filter 0.15s ease;
	}

	.auth-link:hover {
		filter: brightness(1.1);
		text-decoration: none;
	}

	/* Error */
	.share-error {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.share-error h1 {
		font-size: 1.3em;
		font-weight: 600;
	}

	.share-error p {
		color: var(--text-secondary);
	}

	/* Item view */
	.share-item {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}

	.item-collection-badge {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.collection-icon {
		font-size: 1.1em;
	}

	.item-ref {
		color: var(--text-muted);
		font-weight: 400;
	}

	.item-title {
		font-size: 2em;
		font-weight: 700;
		line-height: 1.25;
		letter-spacing: -0.02em;
	}

	.item-fields {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.field-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: 999px;
		font-size: 0.82em;
	}

	.field-chip-label {
		color: var(--text-muted);
		font-weight: 500;
	}

	.field-chip-value {
		color: var(--text-primary);
	}

	.item-content {
		font-family: var(--font-content);
		font-size: 1em;
		line-height: 1.7;
		color: var(--text-primary);
	}

	/* Markdown content styles */
	.item-content :global(h1) {
		font-size: 1.6em;
		font-weight: 700;
		margin: 1.5em 0 0.5em;
		line-height: 1.3;
	}

	.item-content :global(h2) {
		font-size: 1.3em;
		font-weight: 600;
		margin: 1.4em 0 0.4em;
		line-height: 1.3;
	}

	.item-content :global(h3) {
		font-size: 1.1em;
		font-weight: 600;
		margin: 1.2em 0 0.3em;
	}

	.item-content :global(p) {
		margin: 0.8em 0;
	}

	.item-content :global(ul),
	.item-content :global(ol) {
		margin: 0.8em 0;
		padding-left: 1.5em;
	}

	.item-content :global(li) {
		margin: 0.3em 0;
	}

	.item-content :global(pre) {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		overflow-x: auto;
		font-family: var(--font-mono);
		font-size: 0.88em;
		margin: 1em 0;
	}

	.item-content :global(code) {
		font-family: var(--font-mono);
		font-size: 0.9em;
		background: var(--bg-tertiary);
		padding: 0.15em 0.4em;
		border-radius: var(--radius-sm);
	}

	.item-content :global(pre code) {
		background: none;
		padding: 0;
	}

	.item-content :global(blockquote) {
		border-left: 3px solid var(--accent-blue);
		padding-left: var(--space-4);
		margin: 1em 0;
		color: var(--text-secondary);
	}

	.item-content :global(table) {
		width: 100%;
		border-collapse: collapse;
		margin: 1em 0;
	}

	.item-content :global(th),
	.item-content :global(td) {
		border: 1px solid var(--border);
		padding: var(--space-2) var(--space-3);
		text-align: left;
	}

	.item-content :global(th) {
		background: var(--bg-secondary);
		font-weight: 600;
	}

	.item-content :global(hr) {
		border: none;
		border-top: 1px solid var(--border);
		margin: 1.5em 0;
	}

	.item-content :global(img) {
		max-width: 100%;
		border-radius: var(--radius);
	}

	.item-content :global(a) {
		color: var(--accent-blue);
	}

	/* Collection view */
	.share-collection {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}

	.collection-header {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.collection-icon-large {
		font-size: 1.6em;
	}

	.collection-header h1 {
		font-size: 1.8em;
		font-weight: 700;
		letter-spacing: -0.02em;
	}

	.collection-description {
		color: var(--text-secondary);
		font-size: 0.95em;
	}

	.collection-items {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.collection-item-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border-radius: var(--radius);
		border: 1px solid var(--border-subtle);
	}

	.collection-item-title {
		flex: 1;
		font-weight: 500;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.collection-item-ref {
		font-size: 0.8em;
		color: var(--text-muted);
		font-family: var(--font-mono);
		flex-shrink: 0;
	}

	.collection-item-status {
		font-size: 0.78em;
		font-weight: 500;
		color: var(--text-secondary);
		background: var(--bg-tertiary);
		padding: 2px 10px;
		border-radius: 999px;
		flex-shrink: 0;
		text-transform: capitalize;
	}

	.collection-empty {
		color: var(--text-muted);
		font-size: 0.9em;
		padding: var(--space-4) 0;
	}

	/* Footer */
	.share-footer {
		text-align: center;
		padding: var(--space-6) var(--space-5);
		border-top: 1px solid var(--border-subtle);
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.share-footer a {
		color: var(--text-secondary);
		font-weight: 500;
	}

	.share-footer a:hover {
		color: var(--accent-blue);
	}

	@media (max-width: 640px) {
		.share-container {
			padding: var(--space-5) var(--space-4);
		}

		.item-title {
			font-size: 1.5em;
		}
	}
</style>
