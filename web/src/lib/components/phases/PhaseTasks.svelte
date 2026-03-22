<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { api } from '$lib/api/client';
	import type { Item } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';

	interface Props {
		wsSlug: string;
		itemSlug: string;
		itemId: string;
	}

	let { wsSlug, itemSlug, itemId }: Props = $props();

	let tasks = $state<Item[]>([]);
	let loading = $state(true);
	let error = $state('');

	const statusOrder: string[] = ['in_progress', 'open', 'blocked', 'done'];

	let doneCount = $derived(tasks.filter((t) => parseFields(t).status === 'done').length);
	let totalCount = $derived(tasks.length);
	let percentage = $derived(totalCount > 0 ? Math.round((doneCount / totalCount) * 100) : 0);

	let groups = $derived.by(() => {
		const map = new SvelteMap<string, Item[]>();
		for (const task of tasks) {
			const status = parseFields(task).status ?? 'open';
			if (!map.has(status)) map.set(status, []);
			map.get(status)!.push(task);
		}
		const sorted: [string, Item[]][] = [];
		for (const s of statusOrder) {
			if (map.has(s)) sorted.push([s, map.get(s)!]);
		}
		// Include any statuses not in the predefined order
		for (const [s, items] of map) {
			if (!statusOrder.includes(s)) sorted.push([s, items]);
		}
		return sorted;
	});

	async function loadTasks() {
		loading = true;
		error = '';
		try {
			tasks = await api.items.tasks(wsSlug, itemSlug);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load tasks';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadTasks();
	});

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}
</script>

<div class="phase-tasks">
	<div class="section-header">
		<h3>Tasks</h3>
		<span class="task-count">{doneCount}/{totalCount} done</span>
	</div>

	<div class="progress-bar">
		<div class="progress-fill" style:width="{percentage}%"></div>
	</div>

	{#if loading}
		<div class="loading">
			<span class="spinner"></span>
			<span>Loading tasks...</span>
		</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		{#each groups as [status, statusTasks] (status)}
			<div class="task-group">
				<div class="group-label">{formatLabel(status)} ({statusTasks.length})</div>
				{#each statusTasks as task (task.id)}
					{@const fields = parseFields(task)}
					{@const isDone = fields.status === 'done'}
					<a href="/{wsSlug}/tasks/{task.slug}" class="task-row">
						<span class="task-ref">{formatItemRef(task) ?? ''}</span>
						<span class="task-title" class:done={isDone}>{task.title}</span>
						{#if fields.priority}
							<span
								class="task-priority"
								class:high={fields.priority === 'high'}
								class:critical={fields.priority === 'critical'}
							>
								{fields.priority}
							</span>
						{/if}
					</a>
				{/each}
			</div>
		{/each}

		{#if tasks.length === 0}
			<div class="empty">No tasks linked to this phase yet</div>
		{/if}
	{/if}
</div>

<style>
	/* ── Container ──────────────────────────────────────────────────────────── */

	.phase-tasks {
		padding: var(--space-4) 0;
		border-top: 1px solid var(--border);
	}

	/* ── Header ─────────────────────────────────────────────────────────────── */

	.section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-3);
	}

	.section-header h3 {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.task-count {
		font-size: 0.8em;
		color: var(--text-muted);
		font-weight: 400;
	}

	/* ── Progress bar ──────────────────────────────────────────────────────── */

	.progress-bar {
		height: 6px;
		background: var(--bg-tertiary);
		border-radius: 3px;
		overflow: hidden;
		margin-bottom: var(--space-3);
	}

	.progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 3px;
		transition: width 0.3s ease;
	}

	/* ── Task groups ───────────────────────────────────────────────────────── */

	.task-group {
		margin-top: var(--space-3);
	}

	.group-label {
		font-size: 0.7em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	/* ── Task row ──────────────────────────────────────────────────────────── */

	.task-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-2);
		text-decoration: none;
		color: inherit;
		border-bottom: 1px solid var(--border);
		transition: background 0.1s;
	}

	.task-row:hover {
		background: var(--bg-hover);
	}

	.task-row:last-child {
		border-bottom: none;
	}

	.task-ref {
		font-family: var(--font-mono);
		font-size: 0.78em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.task-title {
		flex: 1;
		font-size: 0.88em;
		color: var(--text-primary);
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.task-title.done {
		text-decoration: line-through;
		color: var(--text-muted);
	}

	/* ── Priority badge ────────────────────────────────────────────────────── */

	.task-priority {
		font-size: 0.72em;
		padding: 1px 6px;
		border-radius: 3px;
		white-space: nowrap;
		font-weight: 500;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		flex-shrink: 0;
	}

	.task-priority.high {
		color: var(--accent-amber);
		background: color-mix(in srgb, var(--accent-amber) 15%, transparent);
	}

	.task-priority.critical {
		color: var(--accent-orange);
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}

	/* ── Loading ────────────────────────────────────────────────────────────── */

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4) 0;
		color: var(--text-muted);
		font-size: 0.9em;
		justify-content: center;
	}

	.spinner {
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	/* ── Error ──────────────────────────────────────────────────────────────── */

	.error-msg {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	/* ── Empty state ───────────────────────────────────────────────────────── */

	.empty {
		text-align: center;
		color: var(--text-muted);
		font-size: 0.9em;
		padding: var(--space-4) 0;
	}
</style>
