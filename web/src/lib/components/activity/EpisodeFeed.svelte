<script lang="ts">
	import { api } from '$lib/api/client';
	import { relativeTime } from '$lib/utils/markdown';
	import { foldEpisodes, type Episode } from '$lib/utils/activityEpisodes';
	import EmptyState from '$lib/components/common/EmptyState.svelte';
	import type { Activity } from '$lib/types';

	interface Props {
		activities: Activity[];
		wsSlug: string;
		username: string;
	}

	let { activities, wsSlug, username }: Props = $props();

	let episodes = $derived(foldEpisodes(activities));
	let liveEpisodes = $derived(episodes.filter((e) => e.live));
	let earlierEpisodes = $derived(episodes.filter((e) => !e.live));

	function episodeVerb(actions: string[]): string {
		if (actions.length > 1) return 'working on';
		switch (actions[0]) {
			case 'created':
				return 'created';
			case 'commented':
				return 'commenting on';
			case 'updated':
			case 'field_changed':
				return 'working on';
			default:
				return actions[0];
		}
	}

	// Checkpoint lines (newest comment's first line) keyed by episode key.
	// Best-effort enrichment for the first few LIVE episodes only; `fetched`
	// remembers which (episode, latest event id) pairs were already requested,
	// so a card re-fetches only when its latest.id moves. No polling.
	let checkpoints = $state<Record<string, string>>({});
	const fetched = new Set<string>();

	$effect(() => {
		for (const ep of liveEpisodes.filter((e) => e.itemSlug).slice(0, 4)) {
			const fetchKey = `${ep.key}:${ep.latest.id}`;
			if (fetched.has(fetchKey)) continue;
			fetched.add(fetchKey);
			loadCheckpoint(ep);
		}
	});

	async function loadCheckpoint(ep: Episode) {
		if (!ep.itemSlug) return;
		try {
			const comments = await api.comments.list(wsSlug, ep.itemSlug);
			if (!comments || comments.length === 0) return;
			let newest = comments[0];
			for (const c of comments) {
				if (c.created_at > newest.created_at) newest = c;
			}
			const line = newest.body.split('\n')[0].slice(0, 160).trim();
			if (line) checkpoints[ep.key] = line;
		} catch {
			// best-effort: the card simply renders without a checkpoint line
		}
	}
</script>

{#snippet card(ep: Episode, live: boolean)}
	<div class="episode-card" class:agent={ep.actorKind === 'agent'} class:earlier={!live}>
		<div class="ep-main">
			<span class="ep-actor" title={ep.actorLabel}>{ep.actorLabel}</span>
			<span class="ep-verb">{episodeVerb(ep.actions)}</span>
			{#if ep.itemRef}
				{#if ep.itemSlug && ep.collectionSlug}
					<a class="ep-ref" href="/{username}/{wsSlug}/{ep.collectionSlug}/{ep.itemSlug}"
						>{ep.itemRef}</a
					>
				{:else}
					<span class="ep-ref">{ep.itemRef}</span>
				{/if}
			{/if}
			{#if ep.itemTitle}
				{#if !ep.itemRef && ep.itemSlug && ep.collectionSlug}
					<a class="ep-title" href="/{username}/{wsSlug}/{ep.collectionSlug}/{ep.itemSlug}"
						>{ep.itemTitle}</a
					>
				{:else}
					<span class="ep-title">{ep.itemTitle}</span>
				{/if}
			{/if}
			<span class="ep-meta"
				>{relativeTime(ep.first.created_at)} · {ep.count}
				{ep.count === 1 ? 'event' : 'events'}</span
			>
		</div>
		{#if live && checkpoints[ep.key]}
			<div class="ep-checkpoint">latest: {checkpoints[ep.key]}</div>
		{/if}
	</div>
{/snippet}

{#if episodes.length === 0}
	<EmptyState icon="~" title="No activity yet" />
{:else}
	<div class="episode-feed">
		{#if liveEpisodes.length > 0}
			<div class="feed-section">
				<div class="section-label">
					<span class="live-dot" aria-hidden="true"></span>
					Happening now
				</div>
				<div class="cards">
					{#each liveEpisodes as ep (ep.key)}
						{@render card(ep, true)}
					{/each}
				</div>
			</div>
		{/if}
		{#if earlierEpisodes.length > 0}
			<div class="feed-section">
				<div class="section-label">Earlier</div>
				<div class="cards">
					{#each earlierEpisodes as ep (ep.key)}
						{@render card(ep, false)}
					{/each}
				</div>
			</div>
		{/if}
	</div>
{/if}

<style>
	.episode-feed {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.feed-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.section-label {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 11px;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
	}

	.live-dot {
		width: 7px;
		height: 7px;
		border-radius: 50%;
		background: var(--accent-green);
		flex-shrink: 0;
	}

	.cards {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	/* ── Episode Card ─────────────────────────────────────────────────── */
	.episode-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		background: var(--card-bg);
		border: 1px solid var(--card-border);
		border-left: 3px solid transparent;
		border-radius: var(--radius-lg);
		padding: var(--space-3) var(--space-4);
	}
	.episode-card.earlier {
		background: var(--bg-secondary);
		border-color: var(--border);
		opacity: 0.85;
	}
	/* After .earlier so agent cards keep the accent edge in both sections. */
	.episode-card.agent {
		border-left-color: var(--accent-primary);
	}

	.ep-main {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		min-width: 0;
	}
	.ep-actor {
		font-size: 12.5px;
		font-weight: 700;
		color: var(--text-primary);
		white-space: nowrap;
		/* An actor label is now arbitrary client-supplied text for agents
		   (whatever went in X-Pad-Agent) as well as for people. `nowrap`
		   without a bound lets one long name push the rest of the card's
		   line out; the full value stays in the title attribute. */
		max-width: 20ch;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.ep-verb {
		font-size: 12.5px;
		color: var(--text-muted);
		white-space: nowrap;
	}
	.ep-ref {
		font-family: var(--font-mono);
		font-size: 11px;
		color: var(--accent-primary);
		background: color-mix(in srgb, var(--accent-primary) 12%, transparent);
		padding: 1px 6px;
		border-radius: 5px;
		text-decoration: none;
		white-space: nowrap;
		flex-shrink: 0;
	}
	a.ep-ref:hover {
		text-decoration: underline;
	}
	a.ep-title {
		color: inherit;
	}
	a.ep-title:hover {
		text-decoration: underline;
	}
	.ep-title {
		flex: 1;
		min-width: 0;
		font-size: 0.875em;
		font-weight: 600;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.ep-meta {
		margin-left: auto;
		flex-shrink: 0;
		font-size: 0.8em;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}

	.ep-checkpoint {
		font-size: 12px;
		font-style: italic;
		color: var(--text-secondary);
		border-left: 2px solid var(--border);
		padding-left: 10px;
		display: -webkit-box;
		-webkit-line-clamp: 2;
		line-clamp: 2;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}
</style>
