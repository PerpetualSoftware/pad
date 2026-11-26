<script lang="ts">
	// The Home Agents panel (DOC-4851 section 4): one strip per agent across
	// the caller's workspaces. No agents, no panel.
	//
	// Live updates come through homeLive. No $effect applies a hint: the
	// registered callback either loads or, while a drag or write holds the
	// panel, records that a refresh is owed and the release runs it. Both
	// trackers are plain lets.
	import { onDestroy, onMount } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { agentsOverview } from '$lib/stores/agentsOverview.svelte';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import { homeLive } from '$lib/services/homeLive';
	import AgentRow from './AgentRow.svelte';

	let { compact = false }: { compact?: boolean } = $props();

	const agents = $derived(agentsOverview.data?.agents ?? []);
	const busy = $derived(agents.filter(a => a.summary.now_count > 0).length);
	const parked = $derived(agents.filter(a => a.lanes.some(lane => lane.park)).length);
	let expanded = $state<Record<string, true>>({});

	let held = 0;
	let owed = false;
	function refresh() {
		if (held > 0) { owed = true; return; }
		owed = false;
		void agentsOverview.load();
	}
	/** Hold refreshes until the returned release; release(true) also owes one. */
	function hold() {
		held++;
		let released = false;
		return (refreshAfter = false) => {
			if (released) return;
			released = true;
			held = Math.max(0, held - 1);
			if (refreshAfter) owed = true;
			if (held === 0 && owed) refresh();
		};
	}
	function toggle(slug: string) {
		if (expanded[slug]) {
			const next = { ...expanded };
			delete next[slug];
			expanded = next;
		} else {
			// Phone: one agent open at a time.
			expanded = compact ? { [slug]: true } : { ...expanded, [slug]: true };
		}
	}
	onDestroy(authStore.onIdentityChange(() => { expanded = {}; held = 0; owed = false; }));
	onMount(() => {
		void agentsOverview.load();
		return homeLive.register(refresh);
	});
</script>

{#if agentsOverview.loading && !agentsOverview.data}
	<section class="panel agents" aria-label="Agents" aria-busy="true">
		<div class="skeleton" data-testid="agents-skeleton"></div>
	</section>
{:else if agentsOverview.error && !agentsOverview.data}
	<section class="panel agents" aria-label="Agents">
		<p class="error">{agentsOverview.error} <button type="button" class="link" onclick={() => agentsOverview.load()}>Retry</button></p>
	</section>
{:else if agents.length}
	<section class="panel agents" aria-label="Agents">
		<header>
			<h2>Agents <span class="badge">{agents.length}</span></h2>
			<span class="sum"><strong>{busy}</strong> busy{#if !compact} · <strong>{agents.length - busy}</strong> idle{/if} · <strong>{parked}</strong> parked</span>
		</header>
		{#if agentsOverview.error}<p class="error">{agentsOverview.error} <button type="button" class="link" onclick={() => agentsOverview.load()}>Retry</button></p>{/if}
		<ul>
			{#each agents as agent (agent.slug)}
				<AgentRow {agent} {compact} zone={homeOverview.zone} expanded={!!expanded[agent.slug]} ontoggle={() => toggle(agent.slug)} {hold} />
			{/each}
		</ul>
	</section>
{/if}

<style>
	.panel { border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg-secondary); box-shadow: 0 1px 3px rgba(0,0,0,.12); overflow: hidden; }
	header { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-3) var(--space-4); min-height: 52px; border-bottom: 1px solid var(--border); }
	h2 { margin: 0; font-size: .95rem; }
	.badge { margin-left: var(--space-1); border-radius: 99px; padding: 2px 7px; background: var(--bg-tertiary); color: var(--text-muted); font-size: .75rem; }
	.sum { color: var(--text-secondary); font-size: .75rem; }
	ul { margin: 0; padding: 0; list-style: none; }
	.skeleton { height: 52px; margin: var(--space-3) var(--space-4); border-radius: var(--radius-sm); background: linear-gradient(90deg, var(--bg-tertiary), var(--bg-hover), var(--bg-tertiary)); background-size: 200% 100%; animation: shimmer 1.4s ease-in-out infinite; }
	@keyframes shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }
	@media (prefers-reduced-motion: reduce) { .skeleton { animation: none; } }
	.error { margin: 0; padding: var(--space-3) var(--space-4); color: var(--text-secondary); font-size: .85rem; }
	.link { border: none; background: none; padding: 0; color: var(--accent-blue); font: inherit; font-weight: 600; cursor: pointer; }
	@media (max-width: 768px) { header { min-height: 46px; } }
</style>
