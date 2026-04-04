<script lang="ts">
	import { page } from '$app/state';
	import type { Item, Collection } from '$lib/types';
	import { parseFields, parseSchema, formatItemRef, itemUrlId } from '$lib/types';

	interface Props {
		item: Item;
		collection: Collection;
		compact?: boolean;
		focused?: boolean;
		showCollection?: boolean;
		statusOptions?: string[];
		onStatusClick?: (item: Item, newStatus: string) => void;
		progress?: { total: number; done: number } | null;
		progressLabel?: string;
	}

	let { item, collection, compact = false, focused = false, showCollection = false, statusOptions, onStatusClick, progress = null, progressLabel = 'tasks' }: Props = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let fields = $derived(parseFields(item));
	let schema = $derived(parseSchema(collection));

	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let priorityField = $derived(schema.fields.find((f) => f.key === 'priority'));
	let itemUrl = $derived(`/${wsSlug}/${collection.slug}/${itemUrlId(item)}`);
	let itemRef = $derived(formatItemRef(item));

	let statusCyclable = $derived(
		!!onStatusClick && !!statusOptions && statusOptions.length > 1 && !!fields.status
	);

	let pulsing = $state(false);

	function cycleStatus(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		if (!statusOptions || !onStatusClick || !fields.status) return;
		const currentIndex = statusOptions.indexOf(fields.status);
		const nextIndex = (currentIndex + 1) % statusOptions.length;
		const nextStatus = statusOptions[nextIndex];

		pulsing = true;
		setTimeout(() => { pulsing = false; }, 300);

		onStatusClick(item, nextStatus);
	}

	function statusColor(status: string): string {
		switch (status) {
			case 'open': return 'var(--text-secondary)';
			case 'in_progress': return 'var(--accent-amber)';
			case 'done': return 'var(--accent-green)';
			case 'blocked': return 'var(--accent-orange)';
			default: return 'var(--text-muted)';
		}
	}

	function priorityColor(priority: string): string {
		switch (priority) {
			case 'critical': return 'var(--accent-orange)';
			case 'high': return 'var(--accent-amber)';
			case 'medium': return 'var(--text-secondary)';
			case 'low': return 'var(--text-muted)';
			default: return 'var(--text-muted)';
		}
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<a href={itemUrl} class="item-card" class:compact class:focused>
	<div class="card-top-row">
		{#if showCollection && item.collection_name}
			<span class="collection-badge">
				{#if item.collection_icon}{item.collection_icon} {/if}{item.collection_name}
			</span>
		{/if}
		{#if itemRef}<span class="item-ref">{itemRef}</span>{/if}
	</div>

	<div class="card-title">
		{item.title}
	</div>

	<div class="card-meta">
		{#if statusField && fields.status}
			{#if statusCyclable}
				<button
					class="meta-status meta-status-btn"
					class:pulsing
					style:color={statusColor(fields.status)}
					onclick={cycleStatus}
					title="Click to cycle status"
				>
					{formatLabel(fields.status).toUpperCase()}
				</button>
			{:else}
				<span class="meta-status" style:color={statusColor(fields.status)}>
					{formatLabel(fields.status).toUpperCase()}
				</span>
			{/if}
		{/if}
		{#if priorityField && fields.priority}
			{#if statusField && fields.status}<span class="meta-sep">&middot;</span>{/if}
			<span class="meta-priority" style:color={priorityColor(fields.priority)}>
				{formatLabel(fields.priority)}
			</span>
		{/if}
		{#if item.phase_title}
			<span class="meta-sep">&middot;</span>
			<span class="meta-phase">{item.phase_ref ? `${item.phase_ref}: ${item.phase_title}` : item.phase_title}</span>
		{/if}
		{#if item.agent_role_name}
			<span class="meta-sep">&middot;</span>
			<span class="meta-role">
				{#if item.agent_role_icon}{item.agent_role_icon} {/if}{item.agent_role_name}
			</span>
		{/if}
		{#if item.assigned_user_name}
			<span class="meta-assignee">{item.assigned_user_name}</span>
		{/if}
	</div>

	{#if progress && progress.total > 0}
		<div class="card-progress">
			<div class="card-progress-bar">
				<div class="card-progress-fill" style:width="{Math.round((progress.done / progress.total) * 100)}%"></div>
			</div>
			<span class="card-progress-text">{progress.done}/{progress.total} {progressLabel}</span>
		</div>
	{/if}
</a>

<style>
	.item-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4) var(--space-5);
		text-decoration: none;
		color: inherit;
		transition: background 0.1s;
	}

	.item-card:hover,
	.item-card.focused {
		background: var(--bg-hover);
		text-decoration: none;
	}

	.item-card.focused {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.item-card.compact {
		padding: var(--space-3) var(--space-4);
	}

	.card-top-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.collection-badge {
		background: var(--bg-tertiary);
		padding: 1px 7px;
		border-radius: 10px;
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.75em;
		color: var(--text-muted);
		font-weight: 400;
		white-space: nowrap;
	}

	.card-title {
		font-size: 0.95em;
		color: var(--text-primary);
		line-height: 1.45;
		font-weight: 600;
	}

	.compact .card-title {
		font-size: 0.92em;
	}

	.card-meta {
		display: flex;
		align-items: center;
		gap: 5px;
		flex-wrap: wrap;
	}

	.meta-status {
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		white-space: nowrap;
	}

	.meta-status-btn {
		border: none;
		background: none;
		cursor: pointer;
		padding: 0;
		font-family: inherit;
		line-height: inherit;
		transition: filter 0.1s, transform 0.1s;
	}

	.meta-status-btn:hover {
		filter: brightness(1.3);
		transform: scale(1.05);
	}

	.meta-status-btn:active {
		transform: scale(0.95);
	}

	.meta-status-btn.pulsing {
		animation: status-pulse 0.3s ease-out;
	}

	@keyframes status-pulse {
		0% {
			text-shadow: 0 0 0 currentColor;
		}
		70% {
			text-shadow: 0 0 8px currentColor;
		}
		100% {
			text-shadow: 0 0 0 currentColor;
		}
	}

	.meta-sep {
		font-size: 0.7em;
		color: var(--text-muted);
	}

	.meta-priority {
		font-size: 0.7em;
		font-weight: 600;
		white-space: nowrap;
	}

	.meta-phase {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-purple, var(--text-secondary));
		white-space: nowrap;
	}

	.meta-role {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-teal, var(--accent-blue));
		white-space: nowrap;
	}

	.meta-assignee {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-blue);
		margin-left: auto;
		white-space: nowrap;
	}

	.card-progress {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.card-progress-bar {
		flex: 1;
		height: 4px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		overflow: hidden;
	}
	.card-progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
		transition: width 0.3s ease;
	}
	.card-progress-text {
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}
</style>
