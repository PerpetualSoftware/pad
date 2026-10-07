<script lang="ts">
	// In-app tutorial library (TASK-3452). Pad Cloud shows the getpad.dev catalog
	// here, fetched by the server; a self-hosted server has no catalog, so this
	// page links to getpad.dev/learn (as does Cloud if the catalog is
	// unavailable). Reached from the user menu's Tutorials entry.
	import { onMount } from 'svelte';
	import PageHeader from '$lib/components/common/PageHeader.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { tutorialsStore, learnUrl, formatDuration } from '$lib/tutorials/tutorials.svelte';

	let loading = $state(true);
	onMount(async () => {
		await tutorialsStore.loadCatalog();
		loading = false;
	});

	const catalog = $derived(tutorialsStore.catalog);
	const byPath = (id: string) => catalog?.tutorials.filter((t) => t.path === id) ?? [];
</script>

<svelte:head><title>Tutorials · Pad</title></svelte:head>

<div class="page">
	<PageHeader title="Tutorials" description="Short, narrated tutorials. Each one does a single real task in Pad, start to finish." />

	{#if !authStore.cloudMode || (!loading && !catalog)}
		<p class="linkout">
			The tutorials live on getpad.dev:
			<a href={learnUrl()} target="_blank" rel="noopener noreferrer">getpad.dev/learn ↗</a>
		</p>
	{:else if loading}
		<p class="muted">Loading…</p>
	{:else if catalog}
		{#each catalog.paths as path (path.id)}
			{@const tutorials = byPath(path.id)}
			{#if tutorials.length > 0}
				<section class="path" aria-labelledby={`path-${path.id}`}>
					<h2 id={`path-${path.id}`}>{path.title}</h2>
					<p class="muted">{path.blurb}</p>
					<ol class="grid">
						{#each tutorials as t (t.slug)}
							<li>
								{#if t.seconds != null}
									<a class="card" href={`/console/tutorials/${encodeURIComponent(t.slug)}`}>
										<div class="thumb">
											{#if t.poster}<img src={t.poster} alt="" width="1280" height="720" loading="lazy" decoding="async" />{/if}
											<span class="len">{formatDuration(t.seconds)}</span>
										</div>
										<span class="title">{t.title}</span>
										{#if t.summary}<span class="summary">{t.summary}</span>{/if}
									</a>
								{:else}
									<div class="card planned">
										<div class="thumb"><span class="soon">Coming soon</span></div>
										<span class="title">{t.title}</span>
										{#if t.summary}<span class="summary">{t.summary}</span>{/if}
									</div>
								{/if}
							</li>
						{/each}
					</ol>
				</section>
			{/if}
		{/each}
		<p class="muted small">
			Also on <a href={learnUrl()} target="_blank" rel="noopener noreferrer">getpad.dev/learn ↗</a>, with full transcripts.
		</p>
	{/if}
</div>

<style>
	.page {
		max-width: 1100px;
	}
	.muted {
		color: var(--text-muted);
	}
	.small {
		font-size: 0.85rem;
		margin-top: var(--space-6);
	}
	.linkout a,
	.small a {
		color: var(--accent-blue);
	}
	.path {
		margin-top: var(--space-6);
	}
	.path h2 {
		font-size: 1.1rem;
		margin: 0;
	}
	.path p {
		margin: var(--space-1) 0 var(--space-3);
	}
	.grid {
		list-style: none;
		padding: 0;
		margin: 0;
		display: grid;
		gap: var(--space-4);
		grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
	}
	.card {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		height: 100%;
		padding-bottom: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: inherit;
		text-decoration: none;
		overflow: hidden;
	}
	a.card:hover .title {
		color: var(--accent-blue);
	}
	a.card:focus-visible {
		outline: 2px solid var(--accent-blue);
	}
	.thumb {
		position: relative;
		aspect-ratio: 16 / 9;
		background: var(--bg-tertiary, var(--bg-secondary));
		display: flex;
		align-items: center;
		justify-content: center;
	}
	.thumb img {
		position: absolute;
		inset: 0;
		width: 100%;
		height: 100%;
		object-fit: cover;
	}
	.len {
		position: absolute;
		bottom: var(--space-1);
		right: var(--space-2);
		padding: 0 0.35rem;
		border-radius: 3px;
		background: rgb(0 0 0 / 0.7);
		color: #fff;
		font-size: 0.75rem;
	}
	.soon {
		color: var(--text-muted);
		font-size: 0.85rem;
	}
	.title {
		padding: 0 var(--space-3);
		font-weight: 600;
		margin-top: var(--space-2);
	}
	.summary {
		padding: 0 var(--space-3);
		font-size: 0.85rem;
		color: var(--text-secondary);
	}
	.planned .title {
		color: var(--text-secondary);
	}
</style>
