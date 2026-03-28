<script lang="ts">
	import type { Activity } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';

	let { activities }: { activities: Activity[] } = $props();

	function actionLabel(action: string): string {
		const labels: Record<string, string> = {
			created: 'created', updated: 'updated', archived: 'archived',
			restored: 'restored', read: 'read', searched: 'searched',
		};
		return labels[action] ?? action;
	}

	function actorIcon(actor: string): string {
		return actor === 'agent' ? '🤖' : '👤';
	}

	function actorLabel(a: Activity): string {
		if (a.actor === 'agent') return 'Agent';
		if (a.actor_name) return a.actor_name;
		return 'You';
	}

	function sourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI', web: 'Web', skill: 'Skill',
		};
		return labels[source] ?? source;
	}
</script>

<div class="feed">
	{#each activities as a}
		<div class="entry">
			<span class="actor">{actorIcon(a.actor)}</span>
			<div class="info">
				<span class="action">
					{actorLabel(a)} {actionLabel(a.action)}
					{#if a.item_id}
						a document
					{/if}
				</span>
				<span class="meta">
					via {sourceLabel(a.source)} · <span title={new Date(a.created_at).toLocaleString()}>{relativeTime(a.created_at)}</span>
				</span>
			</div>
		</div>
	{:else}
		<p class="empty">No recent activity.</p>
	{/each}
</div>

<style>
	.feed { display: flex; flex-direction: column; gap: var(--space-1); }
	.entry {
		display: flex;
		align-items: flex-start;
		gap: var(--space-2);
		padding: var(--space-2) 0;
	}
	.actor { font-size: 1em; flex-shrink: 0; }
	.info { display: flex; flex-direction: column; min-width: 0; }
	.action { font-size: 0.9em; }
	.meta { font-size: 0.8em; color: var(--text-muted); }
	.empty { color: var(--text-muted); font-size: 0.9em; }
</style>
