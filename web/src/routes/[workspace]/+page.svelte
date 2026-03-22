<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import type { DashboardResponse, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace);

	let loading = $state(true);
	let dashboard = $state<DashboardResponse | null>(null);
	let collections = $state<Collection[]>([]);
	let pollTimer: ReturnType<typeof setInterval> | undefined;

	$effect(() => {
		if (wsSlug) load(wsSlug);
	});

	onMount(() => {
		pollTimer = setInterval(() => {
			if (wsSlug) load(wsSlug, true);
		}, 30000);
		return () => clearInterval(pollTimer);
	});

	async function load(slug: string, silent = false) {
		if (!silent) loading = true;
		try {
			await workspaceStore.setCurrent(slug);
			const [dash, colls] = await Promise.all([
				api.dashboard.get(slug),
				api.collections.list(slug)
			]);
			dashboard = dash;
			collections = colls;
		} catch {
			// allow partial render
		} finally {
			loading = false;
		}
	}

	let totalItems = $derived(dashboard?.summary.total_items ?? 0);

	function collSlug(name: string): string {
		const coll = collections.find(c => c.name === name);
		return coll?.slug ?? name.toLowerCase().replace(/\s+/g, '-');
	}

	function activityVerb(action: string): string {
		switch (action) {
			case 'created': return '✨ Created';
			case 'updated': return '✏️ Updated';
			case 'archived': return '🗑️ Archived';
			case 'restored': return '♻️ Restored';
			case 'field_changed': return '🔄 Changed';
			default: return action;
		}
	}

	function attentionIcon(type: string): string {
		if (type === 'overdue') return '⏰';
		if (type === 'stalled') return '⚠️';
		if (type === 'phase_complete') return '🎉';
		if (type === 'orphaned') return '🔗';
		return '❓';
	}
</script>

<div class="dashboard">
	{#if loading}
		<div class="loading">Loading dashboard...</div>
	{:else if dashboard}
		<header class="dash-header">
			<h1>{workspaceStore.current?.name ?? wsSlug}</h1>
			<span class="item-count">{totalItems} items</span>
		</header>

		{#if totalItems === 0}
			<div class="welcome-box">
				<div class="welcome-icon">🚀</div>
				<h2>Welcome to {workspaceStore.current?.name ?? 'your workspace'}</h2>
				<p>Your project is ready. Start by creating items or use <kbd>/pad</kbd> in Claude Code to manage your project conversationally.</p>
				<div class="welcome-actions">
					<a href="/{wsSlug}/new" class="welcome-btn primary">Create your first item</a>
					<a href="/{wsSlug}/tasks" class="welcome-btn secondary">Browse collections</a>
				</div>
			</div>
		{/if}

		<!-- Collection Summary -->
		<section class="section">
			<h2>Collections</h2>
			<div class="card-grid">
				{#each collections as coll (coll.slug)}
					{@const breakdown = dashboard.summary.by_collection[coll.name] ?? dashboard.summary.by_collection[coll.slug] ?? {}}
					<a href="/{wsSlug}/{coll.slug}" class="summary-card">
						<div class="card-title">
							{#if coll.icon}<span class="card-icon">{coll.icon}</span>{/if}
							{coll.name}
						</div>
						<div class="card-stats">
							{#if Object.keys(breakdown).length > 0}
								{#each Object.entries(breakdown) as [status, count] (status)}
									<span class="stat-chip">{count} {status}</span>
								{/each}
							{:else}
								<span class="stat-chip empty">empty</span>
							{/if}
						</div>
					</a>
				{/each}
				<a href="/{wsSlug}/settings" class="summary-card new-collection-card">
					<div class="card-title">
						<span class="card-icon">＋</span>
						New Collection
					</div>
					<div class="card-stats">
						<span class="stat-chip empty">create custom collection</span>
					</div>
				</a>
			</div>
		</section>

		<!-- Active Phases -->
		{#if dashboard.active_phases.length > 0}
			<section class="section">
				<h2>Active Phases</h2>
				<div class="phase-list">
					{#each dashboard.active_phases as phase (phase.slug)}
						<div class="phase-row">
							<div class="phase-info">
								<a href="/{wsSlug}/phases/{phase.slug}" class="phase-title">{phase.title}</a>
								<span class="phase-counts">{phase.done_count}/{phase.task_count} tasks</span>
							</div>
							<div class="progress-bar">
								<div class="progress-fill" style="width: {phase.progress}%"></div>
							</div>
							<span class="phase-pct">{phase.progress}%</span>
						</div>
					{/each}
				</div>
			</section>
		{/if}

		<!-- Attention Items -->
		{#if dashboard.attention.length > 0}
			<section class="section">
				<h2>Needs Attention</h2>
				<div class="attention-list">
					{#each dashboard.attention as alert (alert.item_slug)}
						<div class="attention-card">
							<span class="attention-icon">{attentionIcon(alert.type)}</span>
							<div class="attention-content">
								<a href="/{wsSlug}/{alert.collection}/{alert.item_slug}">{alert.item_title}</a>
								<span class="attention-reason">{alert.reason}</span>
							</div>
						</div>
					{/each}
				</div>
			</section>
		{/if}

		<!-- Suggested Next -->
		{#if dashboard.suggested_next.length > 0}
			<section class="section">
				<h2>Suggested Next</h2>
				<div class="suggested-list">
					{#each dashboard.suggested_next.slice(0, 3) as sug, i (sug.item_slug)}
						<div class="suggested-card">
							<span class="sug-num">{i + 1}</span>
							<div class="sug-content">
								<a href="/{wsSlug}/{sug.collection}/{sug.item_slug}">{sug.item_title}</a>
								<span class="sug-reason">{sug.reason}</span>
							</div>
						</div>
					{/each}
				</div>
			</section>
		{/if}

		<!-- Recent Activity -->
		{#if dashboard.recent_activity.length > 0}
			<section class="section">
				<h2>Recent Activity</h2>
				<div class="activity-list">
					{#each dashboard.recent_activity.slice(0, 10) as activity, i (i)}
						<div class="activity-row">
							<span class="activity-verb">{activityVerb(activity.action)}</span>
							{#if activity.item_title}
								<a href="/{wsSlug}/{activity.collection_slug}/{activity.item_slug}" class="activity-item">{activity.item_title}</a>
							{/if}
							<span class="activity-meta">
								by {activity.actor === 'user' ? 'you' : activity.actor}
								via {activity.source}
								· {relativeTime(activity.created_at)}
							</span>
						</div>
					{/each}
				</div>
			</section>
		{/if}
	{:else}
		<div class="loading">No dashboard data available.</div>
	{/if}
</div>

<style>
	.dashboard {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}
	.welcome-box {
		text-align: center;
		padding: var(--space-10) var(--space-6);
	}
	.welcome-icon { font-size: 3em; margin-bottom: var(--space-4); }
	.welcome-box h2 { font-size: 1.3em; font-weight: 600; margin: 0 0 var(--space-2) 0; }
	.welcome-box p { font-size: 0.9em; color: var(--text-muted); margin: 0 0 var(--space-6) 0; max-width: 480px; margin-left: auto; margin-right: auto; }
	.welcome-box kbd {
		background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: 3px;
		padding: 1px 5px; font-size: 0.9em; font-family: inherit;
	}
	.welcome-actions { display: flex; gap: var(--space-3); justify-content: center; flex-wrap: wrap; }
	.welcome-btn {
		padding: var(--space-2) var(--space-5); border-radius: var(--radius); font-weight: 600;
		font-size: 0.9em; text-decoration: none; transition: opacity 0.1s;
	}
	.welcome-btn.primary { background: var(--accent-blue); color: #fff; }
	.welcome-btn.secondary { background: var(--bg-tertiary); color: var(--text-primary); border: 1px solid var(--border); }
	.welcome-btn:hover { opacity: 0.85; }

	.loading {
		text-align: center;
		padding-top: 20vh;
		color: var(--text-muted);
	}

	/* Header */
	.dash-header {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		margin-bottom: var(--space-8);
	}
	.dash-header h1 { font-size: 1.6em; }
	.item-count {
		font-size: 0.9em;
		color: var(--text-muted);
	}

	/* Sections */
	.section { margin-bottom: var(--space-8); }
	h2 {
		font-size: 1.1em;
		color: var(--text-secondary);
		margin-bottom: var(--space-4);
	}

	/* Collection cards */
	.card-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
		gap: var(--space-3);
	}
	.summary-card {
		display: block;
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s;
	}
	.summary-card:hover {
		border-color: var(--accent-blue);
		text-decoration: none;
	}
	.card-title {
		font-weight: 600;
		font-size: 0.95em;
		margin-bottom: var(--space-2);
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.card-icon { font-size: 1.1em; }
	.card-stats {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
	}
	.stat-chip {
		font-size: 0.78em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 8px;
		border-radius: 10px;
	}
	.stat-chip.empty {
		font-style: italic;
		opacity: 0.6;
	}
	.new-collection-card {
		border-style: dashed;
		opacity: 0.7;
		transition: opacity 0.15s, border-color 0.15s;
	}
	.new-collection-card:hover {
		opacity: 1;
	}

	/* Phases */
	.phase-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.phase-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.phase-info {
		display: flex;
		flex-direction: column;
		min-width: 140px;
	}
	.phase-title {
		font-weight: 600;
		font-size: 0.9em;
		color: var(--text-primary);
		text-decoration: none;
	}
	.phase-title:hover { color: var(--accent-blue); text-decoration: underline; }
	.phase-counts {
		font-size: 0.78em;
		color: var(--text-muted);
	}
	.progress-bar {
		flex: 1;
		height: 6px;
		background: var(--bg-tertiary);
		border-radius: 3px;
		overflow: hidden;
	}
	.progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 3px;
		transition: width 0.3s ease;
	}
	.phase-pct {
		font-size: 0.85em;
		color: var(--text-secondary);
		min-width: 36px;
		text-align: right;
	}

	/* Attention */
	.attention-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.attention-card {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-left: 3px solid var(--accent-amber);
		border-radius: var(--radius);
	}
	.attention-icon {
		font-weight: 700;
		color: var(--accent-amber);
		flex-shrink: 0;
	}
	.attention-content {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.attention-content a {
		font-weight: 600;
		font-size: 0.9em;
	}
	.attention-reason {
		font-size: 0.8em;
		color: var(--text-muted);
	}

	/* Suggested */
	.suggested-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.suggested-card {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.sug-num {
		width: 22px;
		height: 22px;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.75em;
		font-weight: 700;
		background: var(--bg-tertiary);
		color: var(--text-muted);
		border-radius: 50%;
		flex-shrink: 0;
	}
	.sug-content {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.sug-content a {
		font-weight: 600;
		font-size: 0.9em;
	}
	.sug-reason {
		font-size: 0.8em;
		color: var(--text-muted);
	}

	/* Activity */
	.activity-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.activity-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		font-size: 0.85em;
		border-radius: var(--radius-sm);
		flex-wrap: wrap;
	}
	.activity-row:hover {
		background: var(--bg-secondary);
	}
	.activity-verb {
		white-space: nowrap;
	}
	.activity-item {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
	}
	.activity-item:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.activity-meta {
		color: var(--text-muted);
		font-size: 0.9em;
	}
</style>
