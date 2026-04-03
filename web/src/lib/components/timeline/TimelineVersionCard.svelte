<script lang="ts">
	import type { Version, Item } from '$lib/types';
	import { api } from '$lib/api/client';
	import DiffView from '$lib/components/versions/DiffView.svelte';
	import { relativeTime } from '$lib/utils/markdown';

	interface Props {
		version: Version;
		wsSlug: string;
		itemSlug: string;
		currentContent: string;
		onRestore?: (item: Item) => void;
	}

	let { version, wsSlug, itemSlug, currentContent, onRestore }: Props = $props();

	let expanded = $state(false);
	let confirming = $state(false);
	let restoring = $state(false);

	function toggle() {
		expanded = !expanded;
		if (!expanded) {
			confirming = false;
		}
	}

	function startRestore() {
		confirming = true;
	}

	function cancelRestore() {
		confirming = false;
	}

	async function confirmRestore() {
		restoring = true;
		try {
			const updatedItem = await api.versions.restore(wsSlug, itemSlug, version.id);
			confirming = false;
			onRestore?.(updatedItem);
		} finally {
			restoring = false;
		}
	}

	function actorLabel(actor: string): string {
		return actor === 'agent' ? 'Agent' : 'User';
	}

	function sourceLabel(source: string): string {
		const labels: Record<string, string> = {
			cli: 'CLI',
			web: 'Web',
			skill: 'Skill'
		};
		return labels[source] ?? source;
	}
</script>

<div class="version-card" class:expanded>
	<button class="card-header" type="button" onclick={toggle}>
		<span class="icon">&#x1F4C4;</span>
		<div class="header-content">
			<span class="label">Content updated</span>
			{#if version.change_summary}
				<span class="change-summary">{version.change_summary}</span>
			{/if}
		</div>
		<div class="badges">
			<span
				class="badge"
				class:badge-agent={version.created_by === 'agent'}
				class:badge-user={version.created_by !== 'agent'}
			>
				{actorLabel(version.created_by)}
			</span>
			<span class="badge badge-source">
				{sourceLabel(version.source)}
			</span>
		</div>
		<span class="timestamp" title={new Date(version.created_at).toLocaleString()}>
			{relativeTime(version.created_at)}
		</span>
		<span class="chevron" class:open={expanded}>&#x25B8;</span>
	</button>

	{#if expanded}
		<div class="card-body">
			<div class="diff-container">
				<DiffView oldContent={version.content} newContent={currentContent} />
			</div>

			<div class="restore-area">
				{#if confirming}
					<div class="confirm-prompt">
						<span class="confirm-text">Restore to this version?</span>
						<div class="confirm-actions">
							<button
								class="btn-cancel"
								type="button"
								onclick={cancelRestore}
								disabled={restoring}
							>
								Cancel
							</button>
							<button
								class="btn-restore-confirm"
								type="button"
								onclick={confirmRestore}
								disabled={restoring}
							>
								{restoring ? 'Restoring...' : 'Confirm Restore'}
							</button>
						</div>
					</div>
				{:else}
					<button
						class="btn-restore"
						type="button"
						onclick={startRestore}
					>
						Restore this version
					</button>
				{/if}
			</div>
		</div>
	{/if}
</div>

<style>
	.version-card {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		overflow: hidden;
	}

	.version-card.expanded {
		border-color: var(--accent-blue);
	}

	.card-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
		font: inherit;
	}

	.card-header:hover {
		background: var(--bg-tertiary);
	}

	.icon {
		flex-shrink: 0;
		font-size: 1em;
		line-height: 1;
	}

	.header-content {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex: 1;
		min-width: 0;
	}

	.label {
		font-size: 0.85em;
		font-weight: 500;
		white-space: nowrap;
	}

	.change-summary {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.badges {
		display: flex;
		gap: var(--space-1);
		flex-shrink: 0;
	}

	.badge {
		display: inline-flex;
		align-items: center;
		padding: 1px var(--space-2);
		border-radius: 9999px;
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.03em;
		line-height: 1.6;
	}

	.badge-agent {
		background: color-mix(in srgb, var(--accent-purple, #a855f7) 15%, transparent);
		color: var(--accent-purple, #a855f7);
	}

	.badge-user {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}

	.badge-source {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.timestamp {
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.chevron {
		font-size: 0.75em;
		color: var(--text-muted);
		transition: transform 0.15s ease;
		flex-shrink: 0;
	}

	.chevron.open {
		transform: rotate(90deg);
	}

	.card-body {
		border-top: 1px solid var(--border);
		padding: var(--space-3);
		background: var(--bg-tertiary);
	}

	.diff-container {
		margin-bottom: var(--space-3);
	}

	.restore-area {
		display: flex;
		justify-content: flex-end;
	}

	.btn-restore {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-restore:hover {
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border-color: var(--accent-blue);
		color: var(--accent-blue);
	}

	.confirm-prompt {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-yellow, #eab308) 8%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-yellow, #eab308) 30%, transparent);
		border-radius: var(--radius);
		flex-wrap: wrap;
		width: 100%;
	}

	.confirm-text {
		font-size: 0.8em;
		color: var(--text-secondary);
		font-weight: 500;
	}

	.confirm-actions {
		display: flex;
		gap: var(--space-2);
		margin-left: auto;
	}

	.btn-cancel {
		padding: var(--space-1) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.btn-cancel:hover:not(:disabled) {
		background: var(--bg-primary);
		color: var(--text-primary);
	}

	.btn-restore-confirm {
		padding: var(--space-1) var(--space-3);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.8em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-restore-confirm:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-restore-confirm:disabled,
	.btn-cancel:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
