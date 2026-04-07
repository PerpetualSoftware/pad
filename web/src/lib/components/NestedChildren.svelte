<script lang="ts">
	import { api } from '$lib/api/client';
	import type { Item } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';

	interface Props {
		wsSlug: string;
		parentSlug: string;
		depth?: number;
		maxDepth?: number;
		terminalStatuses?: string[];
	}

	let { wsSlug, parentSlug, depth = 1, maxDepth = 3, terminalStatuses }: Props = $props();

	const defaultTerminal = ['done', 'completed', 'resolved', 'cancelled', 'rejected', 'wontfix', 'fixed', 'implemented', 'archived', 'disabled', 'deprecated'];
	const terminal = $derived(terminalStatuses ?? defaultTerminal);

	let children = $state<Item[]>([]);
	let loading = $state(true);
	let expandedIds = $state<Set<string>>(new Set());

	$effect(() => {
		void parentSlug;
		loadChildren();
	});

	async function loadChildren() {
		loading = true;
		try {
			children = await api.items.children(wsSlug, parentSlug);
		} catch {
			children = [];
		} finally {
			loading = false;
		}
	}

	function toggleExpand(child: Item) {
		const next = new Set(expandedIds);
		if (next.has(child.id)) {
			next.delete(child.id);
		} else {
			next.add(child.id);
		}
		expandedIds = next;
	}

	let doneCount = $derived(children.filter(c => terminal.includes(parseFields(c).status)).length);
</script>

{#if loading}
	<div class="nested-loading">Loading...</div>
{:else if children.length > 0}
	<div class="nested-children" style:--depth={depth}>
		<div class="nested-progress">
			<span class="nested-count">{doneCount}/{children.length}</span>
			<div class="nested-bar">
				<div class="nested-bar-fill" style:width="{children.length > 0 ? Math.round((doneCount / children.length) * 100) : 0}%"></div>
			</div>
		</div>
		{#each children as child (child.id)}
			{@const fields = parseFields(child)}
			{@const isDone = terminal.includes(fields.status)}
			{@const isExpanded = expandedIds.has(child.id)}
			{@const canExpand = child.has_children && depth < maxDepth}
			<div class="nested-item">
				<div class="nested-row">
					{#if canExpand}
						<button class="expand-toggle" onclick={() => toggleExpand(child)} title={isExpanded ? 'Collapse' : 'Expand'}>
							<span class="expand-icon" class:expanded={isExpanded}>▸</span>
						</button>
					{:else}
						<span class="expand-spacer"></span>
					{/if}
					<a href="/{wsSlug}/{child.collection_slug}/{child.slug}" class="nested-link">
						<span class="nested-ref">{formatItemRef(child) ?? ''}</span>
						<span class="nested-title" class:done={isDone}>{child.title}</span>
					</a>
					{#if fields.priority}
						<span
							class="nested-priority"
							class:high={fields.priority === 'high'}
							class:critical={fields.priority === 'critical'}
						>
							{fields.priority}
						</span>
					{/if}
				</div>
				{#if canExpand && isExpanded}
					<svelte:self wsSlug={wsSlug} parentSlug={child.slug} depth={depth + 1} {maxDepth} {terminalStatuses} />
				{/if}
			</div>
		{/each}
	</div>
{/if}

<style>
	.nested-children {
		margin-left: calc(var(--space-3) + 2px);
		padding-left: var(--space-3);
		border-left: 2px solid var(--border);
	}

	.nested-progress {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
		padding-top: var(--space-1);
	}

	.nested-count {
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.nested-bar {
		flex: 1;
		height: 3px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		overflow: hidden;
		max-width: 80px;
	}

	.nested-bar-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
		transition: width 0.3s ease;
	}

	.nested-item {
		/* container for row + recursive children */
	}

	.nested-row {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		padding: 3px 0;
	}

	.expand-toggle {
		background: none;
		border: none;
		cursor: pointer;
		padding: 0 2px;
		color: var(--text-muted);
		font-size: 0.75em;
		line-height: 1;
		flex-shrink: 0;
		width: 16px;
		text-align: center;
	}

	.expand-toggle:hover {
		color: var(--text-primary);
	}

	.expand-icon {
		display: inline-block;
		transition: transform 0.15s ease;
	}

	.expand-icon.expanded {
		transform: rotate(90deg);
	}

	.expand-spacer {
		width: 16px;
		flex-shrink: 0;
	}

	.nested-link {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		text-decoration: none;
		color: inherit;
		flex: 1;
		min-width: 0;
		border-radius: var(--radius-sm);
		padding: 1px 4px;
	}

	.nested-link:hover {
		background: var(--bg-hover);
	}

	.nested-ref {
		font-family: var(--font-mono);
		font-size: 0.72em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.nested-title {
		font-size: 0.82em;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.nested-title.done {
		text-decoration: line-through;
		color: var(--text-muted);
	}

	.nested-priority {
		font-size: 0.66em;
		padding: 0 4px;
		border-radius: 2px;
		white-space: nowrap;
		font-weight: 500;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		flex-shrink: 0;
	}

	.nested-priority.high {
		color: var(--accent-amber);
		background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
	}

	.nested-priority.critical {
		color: var(--accent-orange);
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}

	.nested-loading {
		font-size: 0.78em;
		color: var(--text-muted);
		padding: var(--space-2) 0 var(--space-2) var(--space-4);
	}
</style>
