<script lang="ts">
	import { page } from '$app/state';
	import type { Item, Collection } from '$lib/types';
	import { parseFields, parseSchema, formatItemRef, itemUrlId } from '$lib/types';

	interface Props {
		item: Item;
		collection: Collection;
		compact?: boolean;
		statusOptions?: string[];
		onStatusClick?: (item: Item, newStatus: string) => void;
		progress?: { total: number; done: number } | null;
		relationLabels?: Record<string, string>;
	}

	let { item, collection, compact = false, statusOptions, onStatusClick, progress = null, relationLabels = {} }: Props = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let fields = $derived(parseFields(item));
	let schema = $derived(parseSchema(collection));

	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let priorityField = $derived(schema.fields.find((f) => f.key === 'priority'));
	let assigneeField = $derived(schema.fields.find((f) => f.key === 'assignee'));

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

	function relativeTime(dateStr: string): string {
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		const diff = now - then;
		const minutes = Math.floor(diff / 60000);
		if (minutes < 1) return 'just now';
		if (minutes < 60) return `${minutes}m ago`;
		const hours = Math.floor(minutes / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		if (days < 30) return `${days}d ago`;
		return new Date(dateStr).toLocaleDateString();
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

<a href={itemUrl} class="item-card" class:compact>
	<div class="card-title">
		{#if itemRef}<span class="item-ref">{itemRef}</span>{/if}
		{item.title}
	</div>

	<div class="card-badges">
		{#if statusField && fields.status}
			{#if statusCyclable}
				<button
					class="badge status-badge status-btn"
					class:pulsing
					style:--badge-color={statusColor(fields.status)}
					onclick={cycleStatus}
					title="Click to cycle status"
				>
					{formatLabel(fields.status)}
				</button>
			{:else}
				<span class="badge status-badge" style:--badge-color={statusColor(fields.status)}>
					{formatLabel(fields.status)}
				</span>
			{/if}
		{/if}
		{#if priorityField && fields.priority}
			<span class="badge priority-badge" style:--badge-color={priorityColor(fields.priority)}>
				{formatLabel(fields.priority)}
			</span>
		{/if}
		{#if fields.phase && relationLabels[fields.phase]}
			<span class="badge phase-badge">
				{relationLabels[fields.phase]}
			</span>
		{/if}
	</div>

	{#if progress && progress.total > 0}
		<div class="card-progress">
			<div class="card-progress-bar">
				<div class="card-progress-fill" style:width="{Math.round((progress.done / progress.total) * 100)}%"></div>
			</div>
			<span class="card-progress-text">{progress.done}/{progress.total} tasks</span>
		</div>
	{/if}

	<div class="card-footer">
		{#if assigneeField && fields.assignee}
			<span class="assignee">{fields.assignee}</span>
		{/if}
		<span class="updated" title={new Date(item.updated_at).toLocaleString()}>{relativeTime(item.updated_at)}</span>
	</div>
</a>

<style>
	.item-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-4) var(--space-5);
		text-decoration: none;
		color: inherit;
		transition: background 0.1s;
	}

	.item-card:hover {
		background: var(--bg-hover);
		text-decoration: none;
	}

	.item-card.compact {
		padding: var(--space-3) var(--space-4);
	}

	.card-title {
		font-size: 0.95em;
		color: var(--text-primary);
		line-height: 1.45;
		font-weight: 500;
	}

	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-muted);
		font-weight: 400;
		margin-right: 4px;
		white-space: nowrap;
	}

	.compact .card-title {
		font-size: 0.92em;
	}

	.card-badges {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		row-gap: var(--space-2);
		flex-wrap: wrap;
	}

	.badge {
		font-size: 0.8em;
		padding: 3px 10px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
		font-weight: 500;
		color: var(--badge-color);
		background: color-mix(in srgb, var(--badge-color) 15%, transparent);
	}

	.status-btn {
		border: none;
		cursor: pointer;
		line-height: inherit;
		font-family: inherit;
		transition: filter 0.1s, transform 0.1s;
	}

	.status-btn:hover {
		filter: brightness(1.3);
		transform: scale(1.05);
	}

	.status-btn:active {
		transform: scale(0.95);
	}

	.status-btn.pulsing {
		animation: status-pulse 0.3s ease-out;
	}

	@keyframes status-pulse {
		0% {
			box-shadow: 0 0 0 0 color-mix(in srgb, var(--badge-color) 50%, transparent);
		}
		70% {
			box-shadow: 0 0 0 6px color-mix(in srgb, var(--badge-color) 0%, transparent);
		}
		100% {
			box-shadow: 0 0 0 0 color-mix(in srgb, var(--badge-color) 0%, transparent);
		}
	}

	.phase-badge {
		font-size: 0.8em;
		color: var(--accent-purple, var(--text-secondary));
		background: color-mix(in srgb, var(--accent-purple, var(--text-secondary)) 12%, transparent);
		padding: 3px 10px;
		border-radius: var(--radius-sm);
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

	.card-footer {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}

	.assignee {
		font-size: 0.8em;
		color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
	}

	.updated {
		font-size: 0.75em;
		color: var(--text-muted);
		margin-left: auto;
	}
</style>
