<script lang="ts">
	// A one-time tutorial suggestion (TASK-3452): the console's empty state and
	// a workspace's setup launchpad each show one, for the tutorial that matches
	// what the user is about to do. Dismissing it is stored on the user, so it
	// stays gone on every device; the user menu's Tutorials entry is the way
	// back.
	//
	// Pad Cloud: the poster (same-origin, proxied by the server) and a link to
	// the in-app player. Self-hosted, or Cloud with no catalog: plain text and a
	// link to getpad.dev/learn, so nothing is requested from a third party.
	//
	// Renders nothing until the dismissals have loaded: showing the card while
	// that is unknown would show a dismissed card for a moment on every load.
	import { onMount } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { tutorialsStore, learnUrl, formatDuration } from '$lib/tutorials/tutorials.svelte';
	import type { UIDismissalKey } from '$lib/types';

	let {
		dismissKey,
		slug,
		fallbackTitle,
		lead = 'New to Pad?'
	}: {
		dismissKey: UIDismissalKey;
		slug: string;
		/** Shown when there is no catalog (self-hosted, or Cloud's fetch failed). */
		fallbackTitle: string;
		lead?: string;
	} = $props();

	onMount(() => {
		void tutorialsStore.loadDismissed();
		void tutorialsStore.loadCatalog();
	});

	const entry = $derived(tutorialsStore.catalog ? tutorialsStore.bySlug(slug) : undefined);
	const inApp = $derived(authStore.cloudMode && !!entry && entry.seconds != null);
	const visible = $derived(tutorialsStore.dismissed !== null && !tutorialsStore.dismissed.has(dismissKey));
</script>

{#if visible}
	<aside class="suggestion" aria-label="Suggested tutorial">
		{#if inApp && entry?.poster}
			<a class="thumb" href={`/console/tutorials/${encodeURIComponent(slug)}`} tabindex="-1" aria-hidden="true">
				<img src={entry.poster} alt="" width="160" height="90" loading="lazy" decoding="async" />
			</a>
		{/if}
		<div class="body">
			<p class="lead">{lead} Watch a short tutorial:</p>
			{#if inApp && entry}
				<a class="title" href={`/console/tutorials/${encodeURIComponent(slug)}`}>
					{entry.title}
				</a>
				<span class="meta">{formatDuration(entry.seconds ?? 0)}</span>
			{:else}
				<a class="title" href={learnUrl(slug)} target="_blank" rel="noopener noreferrer">
					{entry?.title ?? fallbackTitle} <span class="ext">on getpad.dev ↗</span>
				</a>
			{/if}
		</div>
		<button type="button" class="dismiss" aria-label="Dismiss this suggestion" onclick={() => tutorialsStore.dismiss(dismissKey)}>
			<span aria-hidden="true">×</span>
		</button>
	</aside>
{/if}

<style>
	.suggestion {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		margin-top: var(--space-4);
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		text-align: left;
	}
	.thumb {
		flex-shrink: 0;
		width: 128px;
		aspect-ratio: 16 / 9;
		overflow: hidden;
		border-radius: calc(var(--radius) / 2);
	}
	.thumb img {
		width: 100%;
		height: 100%;
		object-fit: cover;
	}
	.body {
		flex: 1;
		min-width: 0;
	}
	.lead {
		margin: 0;
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.title {
		color: var(--accent-blue);
		font-weight: 600;
		text-decoration: none;
	}
	.title:hover {
		text-decoration: underline;
	}
	.ext,
	.meta {
		font-size: 0.8rem;
		font-weight: 400;
		color: var(--text-muted);
	}
	.meta {
		margin-left: var(--space-2);
	}
	.dismiss {
		align-self: flex-start;
		border: 0;
		background: none;
		color: var(--text-muted);
		font-size: 1.1rem;
		line-height: 1;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: 4px;
	}
	.dismiss:hover {
		color: var(--text-primary);
	}
	.dismiss:focus-visible {
		outline: 2px solid var(--accent-blue);
	}
	@media (max-width: 480px) {
		.thumb {
			display: none;
		}
	}
</style>
