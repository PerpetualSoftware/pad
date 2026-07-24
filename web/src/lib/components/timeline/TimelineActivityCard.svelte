<script lang="ts">
	import type { Activity } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';
	import { parseFieldChanges } from '$lib/utils/activityChanges';
	import Chip from '$lib/components/common/Chip.svelte';

	let { activity }: { activity: Activity } = $props();

	function parseMetadata(meta: string): Record<string, any> {
		try {
			return JSON.parse(meta);
		} catch {
			return {};
		}
	}

	const metadata = $derived(parseMetadata(activity.metadata));
	const changes = $derived(parseFieldChanges(metadata.changes ?? ''));

	const actionLabels: Record<string, string> = {
		created: 'created',
		updated: 'updated',
		archived: 'archived',
		restored: 'restored',
		moved: 'moved',
		commented: 'commented'
	};

	function getActionLabel(action: string): string {
		return actionLabels[action] ?? action;
	}

	function getActionClass(action: string): string {
		if (action === 'created') return 'action-created';
		if (action === 'archived') return 'action-archived';
		return '';
	}

	function getActorLabel(a: Activity): string {
		return a.actor === 'agent' ? 'Agent' : 'User';
	}

	function getSourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill'
		};
		return labels[source] ?? source;
	}
</script>

<div class="card">
	<div class="row">
		<Chip
			size="sm"
			color={activity.actor === 'agent' ? 'var(--accent-purple)' : 'var(--status-blue)'}
			>{getActorLabel(activity)}</Chip
		>
		{#if activity.actor_name}
			<span class="actor-name">{activity.actor_name}</span>
		{/if}
		<span class="action-label {getActionClass(activity.action)}">{getActionLabel(activity.action)}</span>
		{#if activity.action === 'moved' && metadata.from_collection && metadata.to_collection}
			<span class="move-detail">
				{metadata.from_collection} &rarr; {metadata.to_collection}
			</span>
		{/if}
		<Chip size="sm">{getSourceLabel(activity.source)}</Chip>
		<span class="spacer"></span>
		<span class="timestamp" title={new Date(activity.created_at).toLocaleString()}>{relativeTime(activity.created_at)}</span>
	</div>
	{#if changes.length > 0}
		<div class="changes">
			{#each changes as change, i (i)}
				<span class="change-pill">
					<span class="change-field">{change.field}:</span>
					<span class="change-from">{change.from}</span>
					<span class="change-arrow">&rarr;</span>
					<span class="change-to">{change.to}</span>
				</span>
			{/each}
		</div>
	{/if}
</div>

<style>
	.card {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		min-width: 0;
	}

	.actor-name {
		font-size: 0.85em;
		color: var(--text-primary);
		font-weight: 500;
	}

	.action-label {
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.action-label.action-created {
		color: var(--accent-green);
		font-weight: 500;
	}

	.action-label.action-archived {
		color: var(--accent-red);
		font-weight: 500;
	}

	.move-detail {
		font-size: 0.8em;
		color: var(--text-muted);
	}

	.spacer {
		flex: 1;
	}

	.timestamp {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.changes {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		padding-left: var(--space-1);
	}

	.change-pill {
		display: inline-flex;
		align-items: center;
		gap: 0.3em;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		font-size: 0.75em;
		color: var(--text-muted);
	}

	.change-field {
		font-weight: 600;
		color: var(--text-primary);
	}

	.change-arrow {
		opacity: 0.5;
	}

	.change-to {
		color: var(--text-primary);
	}
</style>
