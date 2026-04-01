<script lang="ts">
	import { SvelteMap } from 'svelte/reactivity';
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { visibility } from '$lib/services/visibility.svelte';
	import type { Item } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import PhaseChart from './PhaseChart.svelte';

	interface Props {
		wsSlug: string;
		itemSlug: string;
		itemId: string;
		phaseFields?: Record<string, any>;
		onTasksChange?: (tasks: Item[]) => void;
	}

	let { wsSlug, itemSlug, itemId, phaseFields, onTasksChange }: Props = $props();

	let tasks = $state<Item[]>([]);
	let loading = $state(true);
	let error = $state('');
	let unsubscribeSSE: (() => void) | null = null;
	let unsubscribeVisibility: (() => void) | null = null;

	const statusOrder: string[] = ['in_progress', 'open', 'blocked', 'done'];
	const flipDurationMs = 200;
	const touchDragDelayMs = 500;

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

	// ── Drag-and-drop state ──────────────────────────────────────────────────
	let isDragging = $state(false);
	let groupData: Record<string, Item[]> = $state({});

	/** Sync groupData from derived groups, but only when not actively dragging */
	$effect(() => {
		const g = groups;
		if (!isDragging) {
			const data: Record<string, Item[]> = {};
			for (const [status, statusTasks] of g) {
				data[status] = [...statusTasks];
			}
			groupData = data;
		}
	});

	function handleConsider(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleFinalize(status: string, e: CustomEvent<DndEvent<Item>>) {
		groupData[status] = e.detail.items;
		isDragging = false;

		const updates = groupData[status]
			.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME])
			.map((item, index) => ({ id: item.id, sort_order: index }));

		// Persist sequentially (SQLite)
		try {
			for (const { id, sort_order } of updates) {
				await api.items.update(wsSlug, id, { sort_order });
			}
		} catch (e) {
			console.error('Failed to persist reorder:', e);
		}
	}

	// ── Data loading ─────────────────────────────────────────────────────────

	async function loadTasks() {
		loading = true;
		error = '';
		try {
			tasks = await api.items.tasks(wsSlug, itemSlug);
			onTasksChange?.(tasks);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load tasks';
			onTasksChange?.([]);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadTasks();
	});

	onMount(() => {
		unsubscribeVisibility = visibility.onTabResume(() => {
			if (!wsSlug || !itemSlug) return;
			loadTasks();
		});
	});

	$effect(() => {
		unsubscribeSSE?.();
		unsubscribeSSE = null;

		if (!wsSlug || !itemSlug) return;

		unsubscribeSSE = sseService.onItemEvent((event) => {
			if (event.collection !== 'tasks') return;
			if (!['item_created', 'item_updated', 'item_archived', 'item_restored'].includes(event.type)) return;
			loadTasks();
		});
	});

	onDestroy(() => {
		unsubscribeSSE?.();
		unsubscribeVisibility?.();
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

	{#if !loading && tasks.length > 0 && phaseFields?.start_date}
		<PhaseChart {tasks} startDate={phaseFields.start_date} endDate={phaseFields.end_date} />
	{/if}

	{#if loading}
		<div class="loading">
			<span class="spinner"></span>
			<span>Loading tasks...</span>
		</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		{#each groups as [status, _statusTasks] (status)}
			<div class="task-group">
				<div class="group-label">{formatLabel(status)} ({(groupData[status] ?? []).length})</div>
				<div
					class="task-list"
					use:dndzone={{
						items: groupData[status] ?? [],
						flipDurationMs,
						type: 'phase-task',
						dropTargetClasses: ['drop-target'],
						delayTouchStart: touchDragDelayMs
					}}
					onconsider={(e) => handleConsider(status, e)}
					onfinalize={(e) => handleFinalize(status, e)}
				>
					{#each groupData[status] ?? [] as task (task.id)}
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

	/* ── Task list (dnd container) ─────────────────────────────────────────── */

	.task-list {
		min-height: 4px;
	}

	:global(.drop-target) {
		outline: 2px dashed var(--accent-blue);
		outline-offset: -2px;
		border-radius: var(--radius-sm);
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
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}

	.task-row:hover {
		background: var(--bg-hover);
	}

	.task-row:active {
		cursor: grabbing;
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
