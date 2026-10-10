<script lang="ts">
	// One in-app tutorial (TASK-3452): the click-to-load player and its
	// chapters, Pad Cloud only. The full transcript lives on the tutorial's
	// getpad.dev page, linked below. A self-hosted server (or Cloud with no
	// catalog) links straight to that page instead.
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import TutorialPlayer from '$lib/components/tutorials/TutorialPlayer.svelte';
	import { tutorialsStore, learnUrl, formatDuration, formatTimestamp } from '$lib/tutorials/tutorials.svelte';

	let loading = $state(true);
	onMount(async () => {
		await tutorialsStore.loadCatalog();
		loading = false;
	});

	const slug = $derived(page.params.slug ?? '');
	const t = $derived(tutorialsStore.catalog ? tutorialsStore.bySlug(slug) : undefined);
	const next = $derived(t?.next ? tutorialsStore.bySlug(t.next) : undefined);
	let player = $state<ReturnType<typeof TutorialPlayer> | null>(null);
</script>

<svelte:head><title>{t ? `${t.title} · Tutorials · Pad` : 'Tutorials · Pad'}</title></svelte:head>

<div class="page">
	<nav class="crumbs" aria-label="Breadcrumb"><a href="/console/tutorials">Tutorials</a></nav>

	{#if !authStore.cloudMode || (!loading && (!t || t.seconds == null))}
		<p class="linkout">
			This tutorial is on getpad.dev:
			<a href={learnUrl(slug)} target="_blank" rel="noopener noreferrer">open it there ↗</a>
		</p>
	{:else if loading || !t}
		<p class="muted">Loading…</p>
	{:else}
		<h1>{t.title}</h1>
		{#if t.summary}<p class="summary">{t.summary}</p>{/if}
		<div class="layout">
			<div class="main">
				<TutorialPlayer bind:this={player} videoId={t.youtube_id} title={t.title} poster={t.poster} />
				<p class="muted small">
					{formatDuration(t.seconds ?? 0)}{#if !t.youtube_id}&nbsp;· The video is coming soon.{/if}
					The full transcript is on <a href={t.url ?? learnUrl(slug)} target="_blank" rel="noopener noreferrer">getpad.dev ↗</a>.
				</p>
			</div>
			{#if t.chapters && t.chapters.length > 0}
				<aside class="chapters" aria-labelledby="chapters">
					<h2 id="chapters">Chapters</h2>
					<ol>
						{#each t.chapters as c, i (i)}
							<li>
								{#if t.youtube_id}
									<button type="button" onclick={() => player?.seek(c.t)}>
										<span class="stamp">{formatTimestamp(c.t)}</span>{c.title}
									</button>
								{:else}
									<span class="row"><span class="stamp">{formatTimestamp(c.t)}</span>{c.title}</span>
								{/if}
							</li>
						{/each}
					</ol>
				</aside>
			{/if}
		</div>
		{#if next}
			<p class="next">
				Next:
				{#if next.seconds != null}
					<a href={`/console/tutorials/${encodeURIComponent(next.slug)}`}>{next.title}</a>
				{:else}
					{next.title} <span class="muted">(coming soon)</span>
				{/if}
			</p>
		{/if}
		<p class="muted small">
			The workspace and people in these videos are fictional. The screens are Pad's real UI,
			rendered from code, and the narration is a synthetic voice.
		</p>
	{/if}
</div>

<style>
	.page {
		max-width: 1100px;
	}
	.crumbs a,
	.linkout a,
	.small a,
	.next a {
		color: var(--accent-blue);
	}
	.crumbs {
		font-size: 0.85rem;
	}
	h1 {
		margin: var(--space-2) 0 0;
		font-size: 1.5rem;
	}
	.summary {
		color: var(--text-secondary);
		margin: var(--space-2) 0 var(--space-4);
	}
	.muted {
		color: var(--text-muted);
	}
	.small {
		font-size: 0.85rem;
	}
	.layout {
		display: grid;
		gap: var(--space-6);
		grid-template-columns: minmax(0, 1fr);
	}
	@media (min-width: 900px) {
		.layout {
			grid-template-columns: minmax(0, 1fr) 16rem;
		}
	}
	.chapters h2 {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin: 0 0 var(--space-2);
	}
	.chapters ol {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.chapters button,
	.row {
		display: flex;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-1) var(--space-2);
		border: 0;
		border-radius: 4px;
		background: none;
		color: var(--text-secondary);
		text-align: left;
		font: inherit;
	}
	.chapters button {
		cursor: pointer;
	}
	.chapters button:hover {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}
	.chapters button:focus-visible {
		outline: 2px solid var(--accent-blue);
	}
	.stamp {
		width: 2.5rem;
		flex-shrink: 0;
		font-family: var(--font-mono, monospace);
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.next {
		margin-top: var(--space-6);
	}
</style>
