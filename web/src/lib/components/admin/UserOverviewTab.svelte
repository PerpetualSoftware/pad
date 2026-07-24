<!--
  Overview tab — first thing an admin sees when opening a user modal.
  Vitals header + 3 engagement metric tiles + recent items list.

  Sparkline deliberately omitted per PLAN-1542 decisions; api_requests_7d
  metric also omitted pending IDEA-1556. PLAN-1542 / TASK-1553.

  Consumes:
    GET /admin/users/{id}/metrics  (T1547)
    GET /admin/users/{id}/activity?limit=5  (T1546, filtered to item writes)
-->
<script lang="ts">
	import { adminFetch, type AdminUser } from '$lib/stores/admin.svelte';

	interface Props {
		user: AdminUser;
		/** Tab gating so we don't pre-fetch metrics for hidden tabs. */
		active: boolean;
	}

	let { user, active }: Props = $props();

	interface Metrics {
		days_since_write: number | null;
		writes_7d: number;
		collections_touched_30d: number;
	}

	interface ActivityEvent {
		id: string;
		document_id?: string;
		action: string;
		workspace_id?: string;
		actor: string;
		source: string;
		user_id?: string;
		created_at: string;
		actor_name?: string;
	}

	let metrics = $state<Metrics | null>(null);
	let metricsError = $state('');
	let metricsLoading = $state(false);

	let recentItems = $state<ActivityEvent[]>([]);
	let recentError = $state('');
	let recentLoading = $state(false);

	let fetchedForUserId = $state<string | null>(null);

	$effect(() => {
		if (active && user && fetchedForUserId !== user.id) {
			loadAll();
		}
	});

	async function loadAll() {
		const userId = user.id;
		// Claim this userId BEFORE the await so a second hydration trigger
		// (e.g. parent re-passes user with same id but new object ref) can't
		// race in and fire a duplicate concurrent fetch. Cleared on error
		// so retries via re-activation still work.
		fetchedForUserId = userId;
		metricsLoading = true;
		recentLoading = true;
		metricsError = '';
		recentError = '';
		metrics = null;
		recentItems = [];
		// Fire both in parallel; commit independently so a slow metrics
		// fetch doesn't block the recent-items list (or vice versa).
		const metricsP = adminFetch(`/admin/users/${userId}/metrics`)
			.then((m: Metrics) => {
				if (user.id !== userId) return;
				metrics = m;
			})
			.catch((e) => {
				if (user.id !== userId) return;
				metricsError = e instanceof Error ? e.message : 'Failed to load metrics';
			})
			.finally(() => {
				if (user.id === userId) metricsLoading = false;
			});

		// Recent items: pull 20 from the activity feed, narrow to item-write
		// actions, slice 5. Comments/admin actions are out — that's what the
		// dedicated Activity tab is for. PLAN-1542 / TASK-1553.
		const recentP = adminFetch(`/admin/users/${userId}/activity?limit=20`)
			.then((r: { events?: ActivityEvent[] }) => {
				if (user.id !== userId) return;
				const writes = (r.events ?? []).filter((e) =>
					['created', 'updated', 'archived', 'restored', 'moved'].includes(e.action)
				);
				recentItems = writes.slice(0, 5);
			})
			.catch((e) => {
				if (user.id !== userId) return;
				recentError = e instanceof Error ? e.message : 'Failed to load activity';
			})
			.finally(() => {
				if (user.id === userId) recentLoading = false;
			});

		await Promise.all([metricsP, recentP]);
		// fetchedForUserId stays set on failure. Clearing it while the
		// tab is still active would re-trigger the gating $effect and
		// fire loadAll() again immediately — under a persistent outage
		// that's an infinite fetch loop. Retry is explicit via the
		// inline retry buttons below.
	}

	function recencyBucket(days: number | null): 'recent' | 'stale' | 'cold' | 'never' {
		if (days === null) return 'never';
		if (days < 7) return 'recent';
		if (days <= 30) return 'stale';
		return 'cold';
	}

	function accountAge(createdAt: string): string {
		const then = new Date(createdAt).getTime();
		const days = Math.floor((Date.now() - then) / (1000 * 60 * 60 * 24));
		if (days < 1) return 'today';
		if (days < 30) return `${days} day${days === 1 ? '' : 's'} ago`;
		if (days < 365) return `${Math.floor(days / 30)} month${Math.floor(days / 30) === 1 ? '' : 's'} ago`;
		return `${Math.floor(days / 365)} year${Math.floor(days / 365) === 1 ? '' : 's'} ago`;
	}

	function relativeTime(dateStr: string | null | undefined): string {
		if (!dateStr) return '';
		const seconds = Math.floor((Date.now() - new Date(dateStr).getTime()) / 1000);
		if (seconds < 60) return 'Just now';
		const minutes = Math.floor(seconds / 60);
		if (minutes < 60) return `${minutes}m ago`;
		const hours = Math.floor(minutes / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		if (days < 30) return `${days}d ago`;
		return new Date(dateStr).toLocaleDateString();
	}
</script>

<!-- Vitals header — name + email + role + plan + age. Disabled badge is
     already on the modal header; we don't repeat it here. -->
<header class="vitals">
	<div class="vitals-name">
		<h3>{user.name || user.username || user.email}</h3>
		<div class="vitals-email">{user.email}</div>
	</div>
	<div class="vitals-badges">
		<span class="badge badge-role badge-{user.role || 'member'}">{user.role || 'member'}</span>
		<span class="badge badge-plan badge-{user.plan || 'free'}">{user.plan || 'free'}</span>
		<span class="vitals-age">Joined {accountAge(user.created_at)}</span>
	</div>
</header>

<!-- Metric tiles. Failure → "—" placeholders so the tab doesn't block
     on a metrics outage. PLAN-1542 / TASK-1553. -->
<section class="tiles" aria-label="Engagement metrics">
	<div class="tile tile-{metrics ? recencyBucket(metrics.days_since_write) : 'loading'}">
		<div class="tile-value">
			{#if metricsLoading}
				…
			{:else if metricsError || !metrics}
				—
			{:else if metrics.days_since_write === null}
				Never
			{:else if metrics.days_since_write === 0}
				Today
			{:else}
				{metrics.days_since_write}d ago
			{/if}
		</div>
		<div class="tile-label">Last write</div>
	</div>

	<div class="tile">
		<div class="tile-value">
			{#if metricsLoading}…{:else if metricsError || !metrics}—{:else}{metrics.writes_7d}{/if}
		</div>
		<div class="tile-label">7d writes</div>
		<div class="tile-sub">items + comments</div>
	</div>

	<div class="tile">
		<div class="tile-value">
			{#if metricsLoading}…{:else if metricsError || !metrics}—{:else}{metrics.collections_touched_30d}{/if}
		</div>
		<div class="tile-label">30d breadth</div>
		<div class="tile-sub">collections touched</div>
	</div>
</section>

{#if metricsError}
	<div class="state-msg error">
		{metricsError}
		<button class="btn-retry" type="button" disabled={metricsLoading} onclick={loadAll}
			>{metricsLoading ? 'Retrying…' : 'Retry'}</button
		>
	</div>
{/if}

<!-- Recent items — filtered from /activity to item-write actions only.
     Full activity feed (incl. comments and admin actions) lives in the
     Activity tab. PLAN-1542 / TASK-1553. -->
<section class="recent" aria-label="Recent items">
	<h4>Recent items</h4>
	{#if recentLoading}
		<div class="state-msg">Loading…</div>
	{:else if recentError}
		<div class="state-msg error">
			{recentError}
			<button class="btn-retry" type="button" disabled={recentLoading} onclick={loadAll}
				>{recentLoading ? 'Retrying…' : 'Retry'}</button
			>
		</div>
	{:else if recentItems.length === 0}
		<div class="state-msg empty">No recent item writes by this user.</div>
	{:else}
		<ul class="recent-list">
			{#each recentItems as ev (ev.id)}
				<li class="recent-row">
					<span class="recent-action">{ev.action}</span>
					<span class="recent-source">via {ev.source}</span>
					<span class="recent-time" title={ev.created_at}>{relativeTime(ev.created_at)}</span>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.vitals {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: var(--space-3);
		flex-wrap: wrap;
		margin-bottom: var(--space-4);
	}
	.vitals-name h3 {
		margin: 0;
		font-size: 1.1rem;
	}
	.vitals-email {
		color: var(--text-muted);
		font-size: 0.85rem;
		margin-top: 2px;
	}
	.vitals-badges {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.vitals-age {
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.badge {
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
		text-transform: lowercase;
	}
	.badge.badge-admin {
		background: color-mix(in srgb, var(--accent-orange, #f59e0b) 15%, transparent);
		color: var(--accent-orange, #f59e0b);
	}
	.badge.badge-pro {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	.tiles {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
		gap: var(--space-3);
		margin-bottom: var(--space-4);
	}
	.tile {
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-3);
		background: var(--bg-secondary);
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.tile-value {
		font-size: 1.4rem;
		font-weight: 600;
		font-variant-numeric: tabular-nums;
	}
	.tile-recent .tile-value {
		color: #10b981;
	}
	.tile-stale .tile-value {
		color: #f59e0b;
	}
	.tile-cold .tile-value {
		color: var(--accent-red);
	}
	.tile-never .tile-value {
		color: var(--text-muted);
		font-style: italic;
	}
	.tile-label {
		font-size: 0.75rem;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}
	.tile-sub {
		font-size: 0.7rem;
		color: var(--text-muted);
	}
	.recent h4 {
		margin: 0 0 var(--space-2);
		font-size: 0.9rem;
	}
	.recent-list {
		list-style: none;
		padding: 0;
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.recent-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.85rem;
	}
	.recent-action {
		font-weight: 500;
		color: var(--text-primary);
	}
	.recent-source {
		color: var(--text-muted);
		font-size: 0.75rem;
	}
	.recent-time {
		margin-left: auto;
		color: var(--text-muted);
		font-size: 0.75rem;
	}
	.state-msg {
		padding: var(--space-3);
		color: var(--text-muted);
		font-size: 0.85rem;
		text-align: center;
	}
	.state-msg.error {
		color: var(--accent-red);
	}
	.state-msg.empty {
		font-style: italic;
	}
	.btn-retry {
		margin-left: var(--space-2);
		padding: 4px 10px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		cursor: pointer;
		font-size: 0.8rem;
	}
	.btn-retry:hover {
		background: var(--bg-tertiary);
	}
	.btn-retry[disabled] {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
