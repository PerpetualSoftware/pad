<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { onMount, onDestroy } from 'svelte';
	import { browser } from '$app/environment';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { visibility } from '$lib/services/visibility.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import { itemUrlId } from '$lib/types';
	import OnboardingChecklist from '$lib/components/OnboardingChecklist.svelte';
	import type { DashboardResponse, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');

	let loading = $state(true);
	let dashboard = $state<DashboardResponse | null>(null);
	let collections = $state<Collection[]>([]);
	let pollTimer: ReturnType<typeof setInterval> | undefined;
	let onboardingDismissed = $state(false);

	// Sync dismissed state from localStorage when workspace changes
	$effect(() => {
		if (browser && wsSlug) {
			onboardingDismissed = localStorage.getItem(`pad-onboarding-dismissed-${wsSlug}`) === 'true';
		}
	});

	function dismissOnboarding() {
		onboardingDismissed = true;
		if (browser) localStorage.setItem(`pad-onboarding-dismissed-${wsSlug}`, 'true');
	}

	function showOnboarding() {
		onboardingDismissed = false;
		if (browser) localStorage.removeItem(`pad-onboarding-dismissed-${wsSlug}`);
	}

	$effect(() => {
		if (wsSlug) load(wsSlug);
	});

	let unsubscribeVisibility: (() => void) | null = null;

	onMount(() => {
		pollTimer = setInterval(() => {
			if (wsSlug) load(wsSlug, true);
		}, 30000);
		unsubscribeVisibility = visibility.onTabResume(() => {
			if (wsSlug) load(wsSlug, true);
		});
		return () => clearInterval(pollTimer);
	});

	onDestroy(() => {
		unsubscribeVisibility?.();
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

	function statusColor(status: string): string {
		const s = status.toLowerCase().replace(/-/g, '_');
		if (['done', 'completed', 'fixed', 'implemented', 'resolved'].includes(s)) return 'var(--accent-green)';
		if (['in_progress', 'exploring', 'fixing', 'confirmed', 'in_review'].includes(s)) return 'var(--accent-amber)';
		if (['open', 'new', 'draft', 'todo', 'planned'].includes(s)) return 'var(--accent-blue)';
		if (['cancelled', 'rejected', 'wontfix'].includes(s)) return 'var(--accent-gray)';
		if (s === 'active') return 'var(--accent-cyan)';
		if (['archived', 'disabled', 'deprecated'].includes(s)) return 'var(--text-muted)';
		return 'var(--text-secondary)';
	}

	function priorityColor(priority: string): string {
		switch (priority?.toLowerCase()) {
			case 'critical': return 'var(--accent-orange)';
			case 'high': return 'var(--accent-amber)';
			case 'medium': return 'var(--text-secondary)';
			case 'low': return 'var(--text-muted)';
			default: return 'var(--text-muted)';
		}
	}

	function collProgress(coll: Collection): { total: number; done: number; pct: number } {
		const breakdown = dashboard?.summary.by_collection[coll.name] ?? dashboard?.summary.by_collection[coll.slug] ?? {};
		let total = 0, done = 0;
		for (const [status, count] of Object.entries(breakdown)) {
			total += count;
			const s = status.toLowerCase().replace(/-/g, '_');
			if (['done', 'completed', 'fixed', 'implemented', 'resolved'].includes(s)) done += count;
		}
		return { total, done, pct: total > 0 ? Math.round((done / total) * 100) : 0 };
	}

	function activityVerb(action: string): string {
		switch (action) {
			case 'created': return 'Created';
			case 'updated': return 'Updated';
			case 'archived': return 'Archived';
			case 'restored': return 'Restored';
			case 'field_changed': return 'Changed';
			default: return action;
		}
	}

	let creating = $state(false);

	async function quickCreate(collectionSlug: string) {
		if (creating) return;
		creating = true;
		try {
			const coll = collections.find(c => c.slug === collectionSlug);
			const defaultFields: Record<string, any> = {};
			let contentTemplate = '';
			if (coll) {
				try {
					const schema = JSON.parse(coll.schema);
					const statusField = schema.fields?.find((f: any) => f.key === 'status');
					if (statusField?.options?.length) defaultFields.status = statusField.options[0];
				} catch { /* ignore */ }
				try {
					const settings = JSON.parse(coll.settings);
					if (settings.content_template) contentTemplate = settings.content_template;
				} catch { /* ignore */ }
			}
			const item = await api.items.create(wsSlug, collectionSlug, {
				title: 'Untitled',
				content: contentTemplate,
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			goto(`/${wsSlug}/${collectionSlug}/${itemUrlId(item)}?new=1`);
		} catch {
			// Fall back to form creation
			goto(`/${wsSlug}/new?collection=${collectionSlug}`);
		} finally {
			creating = false;
		}
	}

	function attentionIcon(type: string): string {
		if (type === 'overdue') return '\u23f0';
		if (type === 'stalled') return '\u26a0';
		if (type === 'phase_complete' || type === 'phase_completion') return '\ud83c\udf89';
		if (type === 'orphaned_task' || type === 'orphaned') return '\ud83d\udd17';
		return '?';
	}

	function activityIcon(action: string): string {
		if (action === 'created') return '+';
		if (action === 'updated' || action === 'field_changed') return '\u2022';
		if (action === 'archived') return '\u2212';
		if (action === 'restored') return '\u21ba';
		return '\u2022';
	}

	function parseActivityChanges(metadata?: string): string {
		if (!metadata) return '';
		try {
			const meta = JSON.parse(metadata);
			return meta.changes || '';
		} catch {
			return '';
		}
	}
</script>

<div class="dashboard">
	{#if loading}
		<div class="skeleton-dashboard">
			<div class="skeleton-header">
				<div class="skeleton-line" style="width: 200px; height: 28px;"></div>
				<div class="skeleton-line" style="width: 60px; height: 16px;"></div>
			</div>
			<div class="skeleton-grid">
				{#each Array(4) as _, i (i)}
					<div class="skeleton-card">
						<div class="skeleton-line" style="width: 60%; height: 14px;"></div>
						<div class="skeleton-line" style="width: 80%; height: 12px;"></div>
						<div class="skeleton-line" style="width: 100%; height: 4px;"></div>
					</div>
				{/each}
			</div>
		</div>
	{:else if dashboard}
		<!-- 1. Header -->
		<header class="dash-header">
			<div class="dash-header-left">
				<h1>{workspaceStore.current?.name ?? wsSlug}</h1>
				<span class="item-count">{totalItems} item{totalItems !== 1 ? 's' : ''}</span>
			</div>
			<div class="dash-header-actions">
				<button class="btn btn-secondary" onclick={() => quickCreate('ideas')} disabled={creating}>💡 New Idea</button>
				<button class="btn btn-primary" onclick={() => quickCreate('tasks')} disabled={creating}>+ New Task</button>
			</div>
		</header>

		<!-- 2. Onboarding -->
		{#if totalItems === 0 && !onboardingDismissed}
			<div class="onboarding-wrapper">
				<OnboardingChecklist {wsSlug} byCollection={dashboard.summary.by_collection} ondismiss={dismissOnboarding} />
			</div>
		{:else if totalItems === 0 && onboardingDismissed}
			<div class="onboarding-reshow">
				<button class="reshow-btn" onclick={showOnboarding}>Show setup guide</button>
			</div>
		{/if}

		<!-- 3. Active Work -->
		{#if dashboard.active_items.length > 0}
			<section class="section">
				<div class="section-header">
					<span class="section-label">Active Work</span>
					<span class="section-badge">{dashboard.active_items.length}</span>
				</div>
				<div class="active-grid">
					{#each dashboard.active_items as item (item.slug)}
						<a href="/{wsSlug}/{item.collection_slug}/{item.slug}" class="active-card">
							<div class="active-card-top">
								{#if item.item_ref}
									<span class="active-ref">{item.item_ref}</span>
								{/if}
								<span class="active-icon">{item.collection_icon}</span>
							</div>
							<div class="active-title">{item.title}</div>
							<div class="active-card-bottom">
								<span class="status-pill" style="background: color-mix(in srgb, {statusColor(item.status)} 15%, transparent); color: {statusColor(item.status)};">
									{item.status.replace(/-/g, ' ')}
								</span>
								{#if item.priority}
									<span class="active-priority" style="color: {priorityColor(item.priority)};">{item.priority}</span>
								{/if}
								<span class="active-time" title={new Date(item.updated_at).toLocaleString()}>{relativeTime(item.updated_at)}</span>
							</div>
						</a>
					{/each}
				</div>
			</section>
		{/if}

		<!-- 4. Active Phases -->
		{#if dashboard.active_phases.length > 0}
			<section class="section">
				<div class="section-header">
					<span class="section-label">Active Phases</span>
				</div>
				<div class="phase-list">
					{#each dashboard.active_phases as phase (phase.slug)}
						<a href="/{wsSlug}/phases/{phase.slug}" class="phase-row">
							<span class="phase-title">{phase.title}</span>
							<div class="progress-bar">
								<div class="progress-fill" style="width: {phase.progress}%"></div>
							</div>
							<span class="phase-meta">{phase.done_count}/{phase.task_count} &middot; {phase.progress}%</span>
						</a>
					{/each}
				</div>
			</section>
		{/if}

		<!-- 5. Collections Grid -->
		<section class="section">
			<div class="section-header">
				<span class="section-label">Collections</span>
			</div>
			<div class="coll-grid">
				{#each collections as coll (coll.slug)}
					{@const breakdown = dashboard.summary.by_collection[coll.name] ?? dashboard.summary.by_collection[coll.slug] ?? {}}
					{@const prog = collProgress(coll)}
					<a href="/{wsSlug}/{coll.slug}" class="coll-card">
						<div class="coll-card-header">
							<span class="coll-card-name">
								{#if coll.icon}<span class="coll-icon">{coll.icon}</span>{/if}
								{coll.name}
							</span>
							<span class="coll-card-count">{prog.total}</span>
						</div>
						<div class="coll-statuses">
							{#if Object.keys(breakdown).length > 0}
								{#each Object.entries(breakdown) as [status, count] (status)}
									<span class="coll-status-item">
										<span class="coll-status-dot" style="background: {statusColor(status)};"></span>
										<span class="coll-status-count">{count}</span>
										<span class="coll-status-name">{status.replace(/-/g, ' ')}</span>
									</span>
								{/each}
							{:else}
								<span class="coll-status-empty">No items yet</span>
							{/if}
						</div>
						<div class="coll-progress-bar">
							<div class="coll-progress-fill" style="width: {prog.pct}%"></div>
						</div>
					</a>
				{/each}
				<a href="/{wsSlug}/settings" class="coll-card coll-card-new">
					<div class="coll-card-header">
						<span class="coll-card-name">
							<span class="coll-icon">+</span>
							New Collection
						</span>
					</div>
					<div class="coll-statuses">
						<span class="coll-status-empty">Create a custom collection</span>
					</div>
				</a>
			</div>
		</section>

		<!-- 6. Attention + Up Next (side by side) -->
		{#if dashboard.attention.length > 0 || dashboard.suggested_next.length > 0}
			<section class="section dual-section">
				{#if dashboard.attention.length > 0}
					<div class="dual-col">
						<div class="section-header">
							<span class="section-label">Needs Attention</span>
							<span class="section-badge">{dashboard.attention.length}</span>
						</div>
						<div class="attention-list">
							{#each dashboard.attention as alert (`${alert.type}:${alert.item_slug}`)}
								<div class="attention-card">
									<span class="attention-icon">{attentionIcon(alert.type)}</span>
									<div class="attention-content">
										<a href="/{wsSlug}/{alert.collection}/{alert.item_slug}" class="attention-title">{alert.item_title}</a>
										<span class="attention-reason">{alert.reason}</span>
									</div>
								</div>
							{/each}
						</div>
					</div>
				{/if}
				{#if dashboard.suggested_next.length > 0}
					<div class="dual-col">
						<div class="section-header">
							<span class="section-label">Up Next</span>
						</div>
						<div class="suggested-list">
							{#each dashboard.suggested_next.slice(0, 3) as sug, i (sug.item_slug)}
								<div class="suggested-card">
									<span class="sug-num">{i + 1}</span>
									<div class="sug-content">
										<a href="/{wsSlug}/{sug.collection}/{sug.item_slug}" class="sug-title">{sug.item_title}</a>
										<span class="sug-reason">{sug.reason}</span>
									</div>
								</div>
							{/each}
						</div>
					</div>
				{/if}
			</section>
		{/if}

		<!-- 7. Recent Activity -->
		{#if dashboard.recent_activity.length > 0}
			<section class="section">
				<div class="section-header">
					<span class="section-label">Recent Activity</span>
					<a href="/{wsSlug}/activity" class="section-link">View all</a>
				</div>
				<div class="activity-list">
					{#each dashboard.recent_activity.slice(0, 10) as activity, i (i)}
						{@const changes = parseActivityChanges(activity.metadata)}
						<div class="activity-row">
							{#if activity.actor === 'agent'}
								<span class="actor-badge agent">agent</span>
							{:else if activity.actor_name}
								<span class="actor-name">{activity.actor_name}</span>
							{:else if activity.source === 'cli'}
								<span class="actor-badge cli">cli</span>
							{/if}
							<span class="activity-dot" style="color: {activity.action === 'created' ? 'var(--accent-green)' : activity.action === 'archived' ? 'var(--text-muted)' : 'var(--accent-blue)'};">{activityIcon(activity.action)}</span>
							<span class="activity-verb">{activityVerb(activity.action)}</span>
							{#if activity.item_title}
								<a href="/{wsSlug}/{activity.collection_slug}/{activity.item_slug}" class="activity-item">{activity.item_title}</a>
							{/if}
							{#if changes}
								<span class="activity-changes">{changes}</span>
							{/if}
							<span class="activity-time" title={new Date(activity.created_at).toLocaleString()}>{relativeTime(activity.created_at)}</span>
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
	/* ── Layout ─────────────────────────────────────────────────────────── */
	.dashboard {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}
	.loading {
		text-align: center;
		padding-top: 20vh;
		color: var(--text-muted);
	}

	/* ── Header ─────────────────────────────────────────────────────────── */
	.dash-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-8);
		flex-wrap: wrap;
	}
	.dash-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
	}
	.dash-header h1 {
		font-size: 1.6em;
		font-weight: 700;
	}
	.item-count {
		font-size: 0.9em;
		color: var(--text-muted);
	}
	.dash-header-actions {
		display: flex;
		gap: var(--space-2);
	}
	.btn {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.85em;
		font-weight: 600;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		text-decoration: none;
		transition: background 0.15s, border-color 0.15s, opacity 0.15s;
		white-space: nowrap;
	}
	.btn:hover {
		text-decoration: none;
	}
	.btn-primary {
		background: var(--accent-blue);
		color: #fff;
	}
	.btn-primary:hover {
		opacity: 0.9;
	}
	.btn-secondary {
		background: transparent;
		color: var(--text-secondary);
		border: 1px solid var(--border);
	}
	.btn-secondary:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}

	/* ── Onboarding ─────────────────────────────────────────────────────── */
	.onboarding-wrapper {
		margin-bottom: var(--space-6);
	}
	.onboarding-reshow {
		margin-bottom: var(--space-4);
	}
	.reshow-btn {
		background: none;
		border: 1px dashed var(--border);
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		transition: color 0.15s, border-color 0.15s;
	}
	.reshow-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	/* ── Section headers ────────────────────────────────────────────────── */
	.section {
		margin-bottom: var(--space-8);
	}
	.section-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-4);
	}
	.section-label {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
	}
	.section-badge {
		font-size: 0.7em;
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 10px;
		color: var(--text-secondary);
	}
	.section-link {
		font-size: 0.8em;
		color: var(--text-muted);
		text-decoration: none;
		margin-left: auto;
	}
	.section-link:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	/* ── Active Work cards ──────────────────────────────────────────────── */
	.active-grid {
		display: grid;
		grid-template-columns: repeat(2, 1fr);
		gap: var(--space-3);
	}
	.active-card {
		display: block;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-left: 3px solid var(--accent-amber);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s, background 0.15s;
	}
	.active-card:hover {
		border-left-color: var(--accent-blue);
		border-color: var(--accent-blue);
		background: var(--bg-hover);
		text-decoration: none;
	}
	.active-card-top {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: var(--space-1);
	}
	.active-ref {
		font-family: var(--font-mono);
		font-size: 0.75em;
		color: var(--text-muted);
	}
	.active-icon {
		font-size: 1em;
	}
	.active-title {
		font-weight: 600;
		font-size: 0.95em;
		color: var(--text-primary);
		margin-bottom: var(--space-2);
		line-height: 1.3;
	}
	.active-card-bottom {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.status-pill {
		display: inline-flex;
		font-size: 0.75em;
		padding: 2px 10px;
		border-radius: 10px;
		white-space: nowrap;
		text-transform: capitalize;
	}
	.active-priority {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: capitalize;
	}
	.active-time {
		font-size: 0.75em;
		color: var(--text-muted);
		margin-left: auto;
	}

	/* ── Active Phases ──────────────────────────────────────────────────── */
	.phase-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.phase-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s, background 0.15s;
	}
	.phase-row:hover {
		border-color: var(--accent-blue);
		text-decoration: none;
	}
	.phase-title {
		font-weight: 600;
		font-size: 0.9em;
		color: var(--text-primary);
		min-width: 120px;
		white-space: nowrap;
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
	.phase-meta {
		font-size: 0.8em;
		color: var(--text-secondary);
		white-space: nowrap;
		min-width: 80px;
		text-align: right;
	}

	/* ── Collections Grid ───────────────────────────────────────────────── */
	.coll-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
		gap: var(--space-3);
	}
	.coll-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: 14px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s, background 0.15s;
	}
	.coll-card:hover {
		border-color: var(--accent-blue);
		text-decoration: none;
	}
	.coll-card-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
	}
	.coll-card-name {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-weight: 600;
		font-size: 0.92em;
		color: var(--text-primary);
	}
	.coll-icon {
		font-size: 1.05em;
	}
	.coll-card-count {
		font-size: 1.3em;
		font-weight: 700;
		color: var(--text-muted);
		line-height: 1;
	}
	.coll-statuses {
		display: flex;
		flex-wrap: wrap;
		gap: 4px 8px;
	}
	.coll-status-item {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		font-size: 0.75em;
		color: var(--text-muted);
	}
	.coll-status-dot {
		display: inline-block;
		width: 7px;
		height: 7px;
		border-radius: 50%;
		flex-shrink: 0;
	}
	.coll-status-count {
		font-weight: 600;
		color: var(--text-secondary);
	}
	.coll-status-name {
		text-transform: capitalize;
	}
	.coll-status-empty {
		font-size: 0.78em;
		color: var(--text-muted);
		font-style: italic;
	}
	.coll-progress-bar {
		height: 4px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		overflow: hidden;
		margin-top: auto;
	}
	.coll-progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
		transition: width 0.3s ease;
	}
	.coll-card-new {
		border-style: dashed;
		opacity: 0.6;
		transition: opacity 0.15s, border-color 0.15s;
		justify-content: center;
	}
	.coll-card-new:hover {
		opacity: 1;
	}

	/* ── Dual section (Attention + Up Next) ─────────────────────────────── */
	.dual-section {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-6);
	}
	.dual-col {
		min-width: 0;
	}

	/* ── Attention ──────────────────────────────────────────────────────── */
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
		flex-shrink: 0;
		font-size: 1em;
	}
	.attention-content {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}
	.attention-title {
		font-weight: 600;
		font-size: 0.9em;
		color: var(--text-primary);
		text-decoration: none;
	}
	.attention-title:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.attention-reason {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	/* ── Suggested / Up Next ────────────────────────────────────────────── */
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
		min-width: 0;
	}
	.sug-title {
		font-weight: 600;
		font-size: 0.9em;
		color: var(--text-primary);
		text-decoration: none;
	}
	.sug-title:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.sug-reason {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	/* ── Recent Activity ────────────────────────────────────────────────── */
	.activity-list {
		display: flex;
		flex-direction: column;
	}
	.activity-row {
		display: flex;
		align-items: baseline;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		font-size: 0.85em;
		border-radius: var(--radius-sm);
		transition: background 0.15s;
	}
	.activity-row:hover {
		background: var(--bg-secondary);
	}
	.activity-dot {
		font-size: 0.9em;
		font-weight: 700;
		flex-shrink: 0;
		width: 14px;
		text-align: center;
	}
	.activity-verb {
		white-space: nowrap;
		color: var(--text-secondary);
	}
	.activity-item {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.activity-item:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.activity-time {
		color: var(--text-muted);
		font-size: 0.9em;
		margin-left: auto;
		white-space: nowrap;
		flex-shrink: 0;
	}
	.actor-badge {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		padding: 1px 6px;
		border-radius: 3px;
		flex-shrink: 0;
	}
	.actor-badge.agent {
		background: color-mix(in srgb, var(--accent-purple) 15%, transparent);
		color: var(--accent-purple);
	}
	.actor-badge.cli {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	.actor-name {
		font-size: 0.8em;
		font-weight: 600;
		color: var(--text-secondary);
		white-space: nowrap;
		flex-shrink: 0;
	}
	.activity-changes {
		font-size: 0.75em;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		max-width: 200px;
	}

	/* ── Skeleton loader ────────────────────────────────────────────────── */
	.skeleton-dashboard {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-8) var(--space-6);
	}
	.skeleton-header {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		margin-bottom: var(--space-8);
	}
	.skeleton-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
		gap: var(--space-3);
	}
	.skeleton-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		padding: 14px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.skeleton-line {
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	@keyframes skeleton-pulse {
		0%, 100% { opacity: 0.5; }
		50% { opacity: 1; }
	}

	/* ── Responsive ─────────────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.active-grid {
			grid-template-columns: 1fr;
		}
		.dual-section {
			grid-template-columns: 1fr;
		}
		.dash-header {
			flex-direction: column;
			align-items: flex-start;
			gap: var(--space-3);
		}
		.coll-grid {
			grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
		}
	}
</style>
