<script lang="ts">
	import type { Activity } from '$lib/types';
	import { relativeTime } from '$lib/utils/markdown';

	let { activity }: { activity: Activity } = $props();

	function parseMetadata(meta: string): Record<string, any> {
		try {
			return JSON.parse(meta);
		} catch {
			return {};
		}
	}

	interface Change {
		field: string;
		from: string;
		to: string;
	}

	function parseChanges(changesStr: string): Change[] {
		if (!changesStr) return [];
		return changesStr.split(';').map((part) => {
			const trimmed = part.trim();
			const colonIdx = trimmed.indexOf(':');
			if (colonIdx === -1) return null;
			const field = trimmed.slice(0, colonIdx).trim();
			const valuePart = trimmed.slice(colonIdx + 1).trim();
			const arrowParts = valuePart.split('\u2192');
			if (arrowParts.length === 2) {
				return { field, from: arrowParts[0].trim(), to: arrowParts[1].trim() };
			}
			return null;
		}).filter((c): c is Change => c !== null);
	}

	const metadata = $derived(parseMetadata(activity.metadata));
	const changes = $derived(parseChanges(metadata.changes ?? ''));

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

	function getActorBadgeClass(actor: string): string {
		return actor === 'agent' ? 'actor-agent' : 'actor-user';
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
		<span class="badge {getActorBadgeClass(activity.actor)}">{getActorLabel(activity)}</span>
		{#if activity.actor_name}
			<span class="actor-name">{activity.actor_name}</span>
		{/if}
		<span class="action-label {getActionClass(activity.action)}">{getActionLabel(activity.action)}</span>
		{#if activity.action === 'moved' && metadata.from_collection && metadata.to_collection}
			<span class="move-detail">
				{metadata.from_collection} &rarr; {metadata.to_collection}
			</span>
		{/if}
		<span class="badge source-badge">{getSourceLabel(activity.source)}</span>
		<span class="spacer"></span>
		<span class="timestamp" title={new Date(activity.created_at).toLocaleString()}>{relativeTime(activity.created_at)}</span>
	</div>
	{#if changes.length > 0}
		<div class="changes">
			{#each changes as change (change.field)}
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

	.badge {
		display: inline-flex;
		align-items: center;
		padding: 0 var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75em;
		font-weight: 600;
		line-height: 1.75;
		white-space: nowrap;
	}

	.actor-agent {
		background: color-mix(in srgb, var(--accent-purple) 15%, transparent);
		color: var(--accent-purple);
	}

	.actor-user {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
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

	.source-badge {
		background: var(--bg-tertiary);
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
