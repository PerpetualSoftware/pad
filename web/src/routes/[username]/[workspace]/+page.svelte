<script lang="ts">
	import { page } from '$app/state';
	import { onMount, onDestroy, untrack } from 'svelte';
	import { browser } from '$app/environment';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import OnboardingLaunchpad from '$lib/components/OnboardingLaunchpad.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';
	import CreateCollectionModal from '$lib/components/collections/CreateCollectionModal.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { titleStore } from '$lib/stores/title.svelte';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import type { DashboardResponse, Collection } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');

	let loading = $state(true);
	let dashboard = $state<DashboardResponse | null>(null);
	// The workspace slug the current `dashboard` data was fetched for. A silent
	// reload leaves stale `dashboard` in place while `wsSlug` already changed on
	// a workspace switch, so route-param-keyed logic can read mismatched state.
	// Stamping the data's own slug lets consumers (the aha-highlight effect) gate
	// on data that actually belongs to the current route.
	let dashboardSlug = $state<string | null>(null);
	let collections = $state<Collection[]>([]);

	// Scroll position restoration (BUG-1425). Dashboard renders progressively
	// (active items, attention list, etc.) — wait for the initial dashboard
	// fetch before applying a saved offset so the document is tall enough.
	const scrollRestoration = createScrollRestoration({
		ready: () => !loading && dashboard !== null,
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;
	let pollTimer: ReturnType<typeof setInterval> | undefined;
	let onboardingDismissed = $state(false);
	let connectOpen = $state(false);
	let showCreateCollection = $state(false);

	// Owner-gate state for the New Collection trigger.
	//
	// Reading `workspaceStore.isOwner` directly would make the trigger button
	// flicker visibility every 30 s: the dashboard's silent poll (and sync
	// signals) call `load()` → `workspaceStore.setCurrent()`, which clears
	// `currentMembership` to null before `/me` resolves. During that window
	// `workspaceStore.isOwner` returns false even for owners, hiding the CTA
	// and dropping any focus on it.
	//
	// Two effects per CONVE-606 (split reactive-state sync from route-change
	// effects): one resets the cache on a real workspace switch, the other
	// updates it only when membership is definitively known (non-null).
	// Initial default is `false` so we never flash owner-only UI before /me
	// confirms ownership. Server enforcement (handlers_collections.go:48)
	// remains the security boundary; this is purely a stability fix for the
	// UX gate.
	let isOwner = $state(false);
	let lastOwnerSlug: string | null = null;
	$effect(() => {
		if (wsSlug !== lastOwnerSlug) {
			lastOwnerSlug = wsSlug;
			isOwner = false;
		}
	});
	$effect(() => {
		const mem = workspaceStore.currentMembership;
		if (mem !== null) isOwner = mem.role === 'owner';
	});

	// Post IDEA-1516 / TASK-1530: the canonical onboarding signal is
	// `dashboard.needs_onboarding` (mirrors AgentBootstrap.NeedsOnboarding
	// from PLAN-1496 / TASK-1504). The old `onboarding_seed` field still
	// rides on the dashboard response (its backend cleanup is out of
	// scope) but no longer has a consumer in this page — the
	// OnboardingIdeaBanner that read it was retired with this task.
	let needsOnboarding = $derived(dashboard?.needs_onboarding ?? false);

	// Sprint-to-aha (PLAN-1847 Phase 2 / TASK-1853). When needs_onboarding
	// flips true→false, the first real item(s) have just appeared — an agent
	// created them while the launchpad was showing. Capture their slugs so
	// their dashboard cards get a "✨ your agent just created this" highlight
	// for the rest of the session.
	//
	// Scoped tightly to the live transition: on a later page load
	// needs_onboarding is already false, so no transition fires and nothing
	// is highlighted — it only celebrates the launchpad→board moment, never
	// every subsequent create. The track is keyed on `dashboardSlug` (the
	// workspace the data belongs to), NOT the route `wsSlug`: on a switch,
	// `wsSlug` flips before the new dashboard arrives, so a route-keyed edge
	// would read the OLD dashboard's onboarding=true under the NEW slug and
	// then false-positive when fresh data lands. Keying on the data's own
	// slug means the true→false edge is only ever computed across two loads
	// of the SAME workspace's data.
	let justCreatedSlugs = $state<Set<string>>(new Set());
	let onboardingTrack: { slug: string; onboarding: boolean } | null = null;
	$effect(() => {
		const slug = dashboardSlug;
		const onboarding = needsOnboarding;
		untrack(() => {
			if (slug === null) return;
			const prev = onboardingTrack;
			if (!prev || prev.slug !== slug) {
				// First load of this workspace's data (or a switch) — clear stale highlight.
				justCreatedSlugs = new Set();
			} else if (prev.onboarding && !onboarding) {
				// Same workspace, onboarding just completed: these are the first items.
				justCreatedSlugs = new Set((dashboard?.active_items ?? []).map((i) => i.slug));
			}
			onboardingTrack = { slug, onboarding };
		});
	});

	// Sync dismissed state from localStorage when workspace changes
	$effect(() => {
		if (browser && wsSlug) {
			onboardingDismissed = localStorage.getItem(`pad-onboarding-dismissed-${wsSlug}`) === 'true';
		}
	});

	// Phase F (PLAN-1519 / TASK-1526): consume the post-create Connect-modal
	// auto-open signal staged by CreateWorkspaceModal via +layout.svelte.
	// Kept as its OWN effect per CONVE-606 — entangling it with the
	// dismissed-state sync (or with the load() effect below) would re-fire
	// on unrelated reactive churn. The consume helper is single-shot, so
	// re-runs after a workspace switch are harmless: any non-matching slug
	// is left in place for the right destination page to pick up.
	$effect(() => {
		if (!browser || !wsSlug) return;
		const requestedSlug = uiStore.connectAfterNavigateSlug;
		if (requestedSlug && requestedSlug === wsSlug) {
			uiStore.consumeConnectAfterNavigate();
			connectOpen = true;
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

	// `load(wsSlug)` calls `workspaceStore.setCurrent(slug)`, which
	// SYNCHRONOUSLY reads `workspaces.find(...)` before its first await.
	// That synchronous read would otherwise establish a reactive dependency
	// on `workspaceStore.workspaces`, so any reorder via the topbar (which
	// calls `workspaceStore.loadAll()`) would re-fire this effect and cause
	// the dashboard to refetch + re-render — a visible flicker. Wrap in
	// `untrack` so the only tracked dep is `wsSlug` from the if-check.
	$effect(() => {
		if (wsSlug) untrack(() => load(wsSlug));
	});

	// Workspace home shows only the workspace-level title — clear section/item.
	$effect(() => {
		titleStore.setPageTitle({ section: null, item: null });
	});

	let unsubscribeSync: (() => void) | null = null;

	onMount(() => {
		pollTimer = setInterval(() => {
			if (wsSlug) load(wsSlug, true);
		}, 30000);
		// Dashboard always does a full reload on any sync signal since it's
		// an aggregated view (counts, activity, suggestions change with any item update)
		unsubscribeSync = syncService.onSync((result) => {
			if (result.type !== 'caught_up' && wsSlug) {
				load(wsSlug, true);
			}
		});
		return () => clearInterval(pollTimer);
	});

	onDestroy(() => {
		unsubscribeSync?.();
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
			dashboardSlug = slug;
			collections = colls;
		} catch {
			// allow partial render
		} finally {
			loading = false;
		}
	}

	let totalItems = $derived(dashboard?.summary.total_items ?? 0);
	let firstCollection = $derived(collections.filter(c => !c.is_system && c.slug !== 'tasks').sort((a, b) => a.sort_order - b.sort_order)[0]);
	let hasTasksCollection = $derived(collections.some(c => c.slug === 'tasks'));
	// A workspace with no user-facing collections (only system ones, or none)
	// has no manual path to create anything from the board. Drives the
	// web-only escape hatch on the skipped-onboarding board (TASK-1856).
	let hasUserCollections = $derived(collections.some(c => !c.is_system));

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



	function attentionIcon(type: string): string {
		if (type === 'overdue') return '\u23f0';
		if (type === 'stalled') return '\u26a0';
		if (type === 'plan_complete' || type === 'plan_completion' || type === 'phase_complete' || type === 'phase_completion') return '\ud83c\udf89';
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
		{#if needsOnboarding && !onboardingDismissed}
			<!--
				Launchpad render-mode (PLAN-1847 Phase 2 / TASK-1852). While
				needs_onboarding is true and not dismissed, the workspace renders
				as a dedicated setup launchpad INSTEAD of the empty board (which
				reads as broken, not new). Dismissing ("Skip setup") falls through
				to the normal board with a reshow affordance. The flag flips to
				false on the first real item, swapping back to the board.
			-->
			<OnboardingLaunchpad
				workspaceName={workspaceStore.current?.name ?? wsSlug}
				onconnect={() => (connectOpen = true)}
				ondismiss={dismissOnboarding}
			/>
		{:else}
		<!-- 1. Header -->
		<header class="dash-header">
			<div class="dash-header-left">
				<h1>{workspaceStore.current?.name ?? wsSlug}</h1>
				<span class="item-count">{totalItems} item{totalItems !== 1 ? 's' : ''}</span>
			</div>
			<div class="dash-header-actions">
				{#if firstCollection}
					<button class="btn btn-secondary" onclick={() => uiStore.requestQuickAdd(firstCollection.slug)}>{firstCollection.icon} New {firstCollection.name.replace(/s$/, '')}</button>
				{/if}
				{#if hasTasksCollection}
					<button class="btn btn-primary" onclick={() => uiStore.requestQuickAdd('tasks')}>+ New Task</button>
				{/if}
			</div>
		</header>

		<!-- 2. Onboarding nudge (IDEA-1516 §3 / TASK-1530) -->
		<!--
			Single banner triggered by `dashboard.needs_onboarding`. The
			pre-IDEA-1516 design split this into two banners — an
			OnboardingIdeaBanner gated on the seeded primary entry
			(IDEA-1 / BACK-1 / FEAT-1) plus an OnboardingChecklist gated
			on totalItems === 0. Both were wired to retired signals
			(seed-item pattern from PLAN-1496, item-count heuristic
			predating needs_onboarding) and produced two competing CTAs
			in the same screen. The new banner reads the canonical
			AgentBootstrap.NeedsOnboarding signal (mirrored onto the
			dashboard response by TASK-1530's backend change) and
			delegates "Connect agent →" to the workspace's already-mounted
			ConnectWorkspaceModal — same modal the Phase F auto-open hook
			and ConnectBanner use.
		-->
		{#if needsOnboarding && onboardingDismissed}
			{#if isOwner && !hasUserCollections}
				<!--
					Web-only escape hatch (TASK-1856). A user who skipped the
					launchpad on a still-empty workspace has no manual path to
					create anything from the board (collections live behind
					Settings). Offer a direct "create a collection" affordance so
					declining an agent isn't a dead end — owner-gated to match the
					server's create-collection boundary, with a reshow link back to
					the launchpad. Hidden once any user collection exists.
				-->
				<div class="empty-escape">
					<span class="empty-escape-icon" aria-hidden="true">📋</span>
					<div class="empty-escape-body">
						<p class="empty-escape-title">Your workspace is empty</p>
						<p class="empty-escape-text">
							Connect an agent and say <strong>set up my workspace</strong> to
							have it set up for you — or create your first collection by hand.
						</p>
						<div class="empty-escape-actions">
							<button class="btn btn-primary" onclick={() => (showCreateCollection = true)}>+ Create a collection</button>
							<button class="btn btn-secondary" onclick={showOnboarding}>Show setup guide</button>
						</div>
					</div>
				</div>
			{:else}
				<!--
					Reshow affordance for users who skipped the launchpad. Clicking
					restores the launchpad render-mode (clears the dismissed flag).
				-->
				<div class="onboarding-reshow">
					<button class="reshow-btn" onclick={showOnboarding}>Show setup guide</button>
				</div>
			{/if}
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
						<a href="/{username}/{wsSlug}/{item.collection_slug}/{item.slug}" class="active-card" class:just-created={justCreatedSlugs.has(item.slug)}>
							{#if justCreatedSlugs.has(item.slug)}
								<span class="just-created-badge">✨ your agent just created this</span>
							{/if}
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

		<!-- Starred Items -->
		{#if dashboard.starred_items && dashboard.starred_items.length > 0}
			<section class="section">
				<div class="section-header">
					<span class="section-label">⭐ Starred</span>
					<span class="section-badge">{dashboard.starred_items.length}</span>
					<a href="/{username}/{wsSlug}/starred" class="section-link">View all</a>
				</div>
				<div class="active-grid">
					{#each dashboard.starred_items as item (item.slug)}
						<a href="/{username}/{wsSlug}/{item.collection_slug}/{item.slug}" class="active-card">
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
							</div>
						</a>
					{/each}
				</div>
			</section>
		{/if}

		<!-- 4. Active Plans -->
		{#if dashboard.active_plans.length > 0}
			<section class="section">
				<div class="section-header">
					<span class="section-label">Active Plans</span>
				</div>
				<div class="plan-list">
					{#each dashboard.active_plans as plan (plan.slug)}
						<a href="/{username}/{wsSlug}/plans/{plan.slug}" class="plan-row">
							<span class="plan-title">{plan.title}</span>
							<div class="progress-bar">
								<div class="progress-fill" style="width: {plan.progress}%"></div>
							</div>
							<span class="plan-meta">{plan.done_count}/{plan.task_count} &middot; {plan.progress}%</span>
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
					<a href="/{username}/{wsSlug}/{coll.slug}" class="coll-card">
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
				{#if isOwner}
					<button
						type="button"
						class="coll-card coll-card-new"
						onclick={() => { showCreateCollection = true; }}
					>
						<div class="coll-card-header">
							<span class="coll-card-name">
								<span class="coll-icon">+</span>
								New Collection
							</span>
						</div>
						<div class="coll-statuses">
							<span class="coll-status-empty">Create a custom collection</span>
						</div>
					</button>
				{/if}
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
										<a href="/{username}/{wsSlug}/{alert.collection}/{alert.item_slug}" class="attention-title">{alert.item_title}</a>
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
										<a href="/{username}/{wsSlug}/{sug.collection}/{sug.item_slug}" class="sug-title">{sug.item_title}</a>
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
					<a href="/{username}/{wsSlug}/activity" class="section-link">View all</a>
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
								<a href="/{username}/{wsSlug}/{activity.collection_slug}/{activity.item_slug}" class="activity-item">{activity.item_title}</a>
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
		{/if}
	{:else}
		<div class="loading">No dashboard data available.</div>
	{/if}
</div>

<!--
	Mount the connect modal unconditionally at the page root so it
	survives re-renders of the conditional onboarding block above —
	closing the modal must not be entangled with that branch's state.
-->
<ConnectWorkspaceModal
	bind:open={connectOpen}
	serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
	workspaceSlug={wsSlug}
	workspaceName={workspaceStore.current?.name ?? ''}
	mcpPublicUrl={authStore.mcpPublicUrl}
/>

{#if wsSlug}
	<!--
		Mount unconditionally (gated on wsSlug, not isOwner) so an owner editing
		the modal isn't unmounted mid-edit when the 30s dashboard poll or a sync
		signal calls load() → workspaceStore.setCurrent(), which transiently
		clears currentMembership and flips isOwner false until /me resolves.
		The trigger button is owner-gated above; this matches Sidebar.svelte's
		pattern, and the server-side owner check (handlers_collections.go:48)
		remains the enforcement boundary.
	-->
	<CreateCollectionModal
		open={showCreateCollection}
		{wsSlug}
		oncreated={() => {
			showCreateCollection = false;
			// Refresh dashboard-local data (summary + collections grid) AND
			// the shared collectionStore the Sidebar/quick-add read from,
			// matching Sidebar.svelte's create-flow pattern.
			load(wsSlug, true);
			collectionStore.loadCollections(wsSlug);
		}}
		onclose={() => { showCreateCollection = false; }}
	/>
{/if}

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
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
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

	/* Web-only escape hatch (TASK-1856) — manual start for users who skip the
	   agent flow on an empty workspace. */
	.empty-escape {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		margin-bottom: var(--space-4);
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.empty-escape-icon {
		font-size: 1.4em;
		line-height: 1.2;
		flex-shrink: 0;
	}
	.empty-escape-body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.empty-escape-title {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.empty-escape-text {
		margin: 0;
		font-size: 0.85em;
		line-height: 1.45;
		color: var(--text-secondary);
	}
	.empty-escape-actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-top: var(--space-1);
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
		color: var(--accent-blue);
		text-decoration: none;
		margin-left: auto;
	}
	.section-link:hover {
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
	/* Sprint-to-aha highlight (TASK-1853): the first agent-created item(s)
	   surfaced live as the launchpad handed off to the board. */
	.active-card.just-created {
		border-left-color: var(--accent-blue);
		border-color: color-mix(in srgb, var(--accent-blue) 45%, var(--border));
		background: color-mix(in srgb, var(--accent-blue) 6%, var(--bg-secondary));
	}
	.just-created-badge {
		display: inline-block;
		margin-bottom: var(--space-1);
		font-size: 0.72em;
		font-weight: 600;
		color: var(--accent-blue);
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

	/* ── Active Plans ──────────────────────────────────────────────────── */
	.plan-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.plan-row {
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
	.plan-row:hover {
		border-color: var(--accent-blue);
		text-decoration: none;
	}
	.plan-title {
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
	.plan-meta {
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
	/* Reset <button> defaults so the trigger card matches its sibling <a> cards. */
	button.coll-card-new {
		font: inherit;
		text-align: left;
		cursor: pointer;
		width: 100%;
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
