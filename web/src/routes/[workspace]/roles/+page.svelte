<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { parseFields, formatItemRef, itemUrlId } from '$lib/types';
	import type { RoleBoardLane } from '$lib/types';

	let wsSlug = $derived(page.params.workspace ?? '');

	// Data
	let lanes = $state<RoleBoardLane[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Filter: "My Work" toggle
	let myWorkOnly = $state(false);
	let currentUserId = $state('');

	// Filtered lanes based on "My Work" toggle
	let filteredLanes = $derived.by(() => {
		if (!myWorkOnly || !currentUserId) return lanes;
		return lanes
			.map((lane) => ({
				...lane,
				items: lane.items.filter((item) => item.assigned_user_id === currentUserId)
			}))
			.filter((lane) => lane.items.length > 0);
	});

	// Total item count
	let totalItems = $derived(filteredLanes.reduce((sum, lane) => sum + lane.items.length, 0));

	onMount(() => {
		workspaceStore.setCurrent(wsSlug);
		uiStore.onNavigate();
		loadData();
	});

	async function loadData() {
		loading = true;
		error = '';
		try {
			const [boardResult, session] = await Promise.all([
				api.agentRoles.board(wsSlug),
				api.auth.session()
			]);
			lanes = boardResult.lanes;
			if (session.authenticated && session.user) {
				currentUserId = session.user.id;
			}
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to load role board';
		} finally {
			loading = false;
		}
	}

	function statusColor(status: string): string {
		const s = status.toLowerCase();
		if (s === 'done' || s === 'completed' || s === 'closed') return 'var(--accent-green)';
		if (s === 'in progress' || s === 'in_progress' || s === 'active') return 'var(--accent-blue)';
		if (s === 'blocked') return 'var(--accent-orange)';
		if (s === 'todo' || s === 'open' || s === 'backlog') return 'var(--text-muted)';
		return 'var(--text-secondary)';
	}

	function priorityColor(priority: string): string {
		const p = priority.toLowerCase();
		if (p === 'critical' || p === 'urgent') return 'var(--accent-orange)';
		if (p === 'high') return 'var(--accent-amber)';
		if (p === 'medium') return 'var(--accent-blue)';
		if (p === 'low') return 'var(--accent-teal)';
		return 'var(--text-muted)';
	}
</script>

<svelte:head>
	<title>Role Board - {workspaceStore.current?.name ?? wsSlug} | Pad</title>
</svelte:head>

<div class="role-board-page">
	<header class="page-header">
		<div class="page-header-left">
			<h1><span class="page-icon" aria-hidden="true">&#127917;</span> Role Board</h1>
			{#if !loading}
				<span class="item-count">{totalItems} item{totalItems === 1 ? '' : 's'}</span>
			{/if}
		</div>
		<div class="page-header-right">
			<button
				class="toggle-btn"
				class:active={myWorkOnly}
				onclick={() => (myWorkOnly = !myWorkOnly)}
			>
				My Work
			</button>
		</div>
	</header>

	{#if loading}
		<div class="skeleton-board">
			{#each Array(4) as _, i (i)}
				<div class="skeleton-lane">
					<div class="skeleton-lane-header"></div>
					{#each Array(3) as _, j (j)}
						<div class="skeleton-card"></div>
					{/each}
				</div>
			{/each}
		</div>
	{:else if error}
		<div class="empty-state">
			<div class="empty-icon">!</div>
			<p class="empty-title">Failed to load</p>
			<p class="empty-desc">{error}</p>
			<button class="retry-btn" onclick={loadData}>Retry</button>
		</div>
	{:else if filteredLanes.length === 0}
		<div class="empty-state">
			{#if myWorkOnly}
				<div class="empty-icon">&#128100;</div>
				<p class="empty-title">No items assigned to you</p>
				<p class="empty-desc">
					Turn off "My Work" to see all items, or assign items to yourself from the item detail page.
				</p>
			{:else}
				<div class="empty-icon">&#127917;</div>
				<p class="empty-title">No roles configured</p>
				<p class="empty-desc">
					Agent roles let you partition work across different AI agents or team members.
					Create roles in workspace settings, then assign items to roles from the item detail page.
				</p>
			{/if}
		</div>
	{:else}
		<div class="lanes-container">
			{#each filteredLanes as lane (lane.role?.id ?? '__unassigned')}
				{@const isUnassigned = !lane.role}
				<div class="lane" class:unassigned={isUnassigned}>
					<div class="lane-header">
						<div class="lane-title-row">
							{#if lane.role}
								<span class="lane-icon">{lane.role.icon || '&#129302;'}</span>
								<span class="lane-name">{lane.role.name}</span>
							{:else}
								<span class="lane-name unassigned-name">Unassigned</span>
							{/if}
							<span class="lane-count">{lane.items.length}</span>
						</div>
						{#if lane.role?.tools}
							<div class="lane-tools">{lane.role.tools}</div>
						{/if}
						{#if lane.assigned_users.length > 0}
							<div class="lane-users">
								{#each lane.assigned_users as user (user)}
									<span class="user-pill">{user}</span>
								{/each}
							</div>
						{/if}
					</div>

					<div class="lane-items">
						{#each lane.items as item (item.id)}
							{@const fields = parseFields(item)}
							{@const ref = formatItemRef(item)}
							{@const status = fields.status ?? ''}
							{@const priority = fields.priority ?? ''}
							<a
								href="/{wsSlug}/{item.collection_slug}/{itemUrlId(item)}"
								class="item-card"
							>
								<div class="card-top-row">
									{#if item.collection_icon || item.collection_name}
										<span class="collection-badge">
											{#if item.collection_icon}<span class="coll-icon">{item.collection_icon}</span>{/if}
											{#if item.collection_name}<span class="coll-name">{item.collection_name}</span>{/if}
										</span>
									{/if}
									{#if ref}
										<span class="item-ref">{ref}</span>
									{/if}
								</div>

								<div class="card-title">{item.title}</div>

								<div class="card-meta">
									{#if status}
										<span class="status-badge" style="color: {statusColor(status)}">
											{status}
										</span>
									{/if}
									{#if priority}
										<span class="priority-badge" style="color: {priorityColor(priority)}">
											{priority}
										</span>
									{/if}
									{#if item.assigned_user_name}
										<span class="assigned-user">{item.assigned_user_name}</span>
									{/if}
								</div>
							</a>
						{/each}
						{#if lane.items.length === 0}
							<div class="lane-empty">No items</div>
						{/if}
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

<style>
	/* ── Page Layout ──────────────────────────────────────────────────── */
	.role-board-page {
		padding: var(--space-6);
		height: 100%;
		display: flex;
		flex-direction: column;
	}

	/* ── Header ───────────────────────────────────────────────────────── */
	.page-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-5);
		flex-shrink: 0;
	}
	.page-header-left {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
	}
	.page-header h1 {
		font-size: 1.6em;
		font-weight: 700;
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.page-icon {
		font-size: 0.85em;
	}
	.item-count {
		font-size: 0.9em;
		color: var(--text-muted);
	}
	.page-header-right {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	/* ── Toggle Button ────────────────────────────────────────────────── */
	.toggle-btn {
		background: var(--bg-secondary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s, color 0.15s;
	}
	.toggle-btn:hover {
		border-color: var(--text-muted);
		color: var(--text-primary);
	}
	.toggle-btn.active {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
		border-color: var(--accent-blue);
	}

	/* ── Lanes Container ──────────────────────────────────────────────── */
	.lanes-container {
		display: flex;
		gap: var(--space-4);
		overflow-x: auto;
		flex: 1;
		align-items: flex-start;
		padding-bottom: var(--space-4);
	}

	/* ── Lane ─────────────────────────────────────────────────────────── */
	.lane {
		flex: 0 0 280px;
		min-width: 280px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		display: flex;
		flex-direction: column;
		max-height: 100%;
	}

	.lane-header {
		padding: var(--space-4) var(--space-4) var(--space-3);
		border-bottom: 1px solid var(--border);
		position: sticky;
		top: 0;
		background: var(--bg-secondary);
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		z-index: 1;
	}

	.lane-title-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.lane-icon {
		font-size: 1.1em;
		flex-shrink: 0;
	}
	.lane-name {
		font-weight: 700;
		font-size: 0.95em;
		color: var(--text-primary);
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.unassigned-name {
		color: var(--text-muted);
	}
	.lane-count {
		font-size: 0.75em;
		font-weight: 700;
		background: var(--bg-tertiary);
		color: var(--text-muted);
		padding: 1px 8px;
		border-radius: 10px;
		flex-shrink: 0;
	}

	.lane-tools {
		font-size: 0.75em;
		color: var(--text-muted);
		margin-top: var(--space-1);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.lane-users {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		margin-top: var(--space-2);
	}
	.user-pill {
		font-size: 0.7em;
		font-weight: 600;
		background: color-mix(in srgb, var(--accent-teal) 15%, transparent);
		color: var(--accent-teal);
		padding: 1px 8px;
		border-radius: 10px;
		white-space: nowrap;
	}

	/* ── Lane Items ───────────────────────────────────────────────────── */
	.lane-items {
		padding: var(--space-2);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		overflow-y: auto;
		flex: 1;
	}

	.lane-empty {
		text-align: center;
		padding: var(--space-4);
		color: var(--text-muted);
		font-size: 0.85em;
	}

	/* ── Item Card ────────────────────────────────────────────────────── */
	.item-card {
		display: block;
		padding: var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s, background 0.15s;
	}
	.item-card:hover {
		border-color: var(--text-muted);
		background: var(--bg-hover);
	}

	.card-top-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-1);
		flex-wrap: wrap;
	}

	.collection-badge {
		display: inline-flex;
		align-items: center;
		gap: 3px;
		font-size: 0.7em;
		background: var(--bg-tertiary);
		padding: 1px 7px;
		border-radius: 10px;
		color: var(--text-muted);
		white-space: nowrap;
	}
	.coll-icon {
		font-size: 1em;
	}
	.coll-name {
		font-weight: 600;
	}

	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.card-title {
		font-size: 0.875em;
		font-weight: 600;
		color: var(--text-primary);
		line-height: 1.35;
		overflow: hidden;
		text-overflow: ellipsis;
		display: -webkit-box;
		-webkit-line-clamp: 2;
		-webkit-box-orient: vertical;
	}

	.card-meta {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
		margin-top: var(--space-2);
	}

	.status-badge {
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.priority-badge {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: capitalize;
	}

	.assigned-user {
		font-size: 0.7em;
		color: var(--text-muted);
		margin-left: auto;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		max-width: 100px;
	}

	/* ── Empty State ──────────────────────────────────────────────────── */
	.empty-state {
		text-align: center;
		padding: var(--space-10) var(--space-4);
		color: var(--text-muted);
	}
	.empty-icon {
		font-size: 2em;
		margin-bottom: var(--space-3);
		opacity: 0.5;
	}
	.empty-title {
		font-size: 1.1em;
		font-weight: 600;
		color: var(--text-secondary);
		margin-bottom: var(--space-2);
	}
	.empty-desc {
		font-size: 0.9em;
		max-width: 400px;
		margin: 0 auto;
		line-height: 1.5;
	}
	.retry-btn {
		margin-top: var(--space-4);
		background: var(--bg-secondary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-5);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s;
	}
	.retry-btn:hover {
		border-color: var(--text-muted);
		background: var(--bg-hover);
	}

	/* ── Skeleton ─────────────────────────────────────────────────────── */
	.skeleton-board {
		display: flex;
		gap: var(--space-4);
		flex: 1;
	}
	.skeleton-lane {
		flex: 0 0 280px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.skeleton-lane-header {
		height: 24px;
		width: 60%;
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	.skeleton-card {
		height: 80px;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		animation: skeleton-pulse 1.5s ease-in-out infinite;
	}
	@keyframes skeleton-pulse {
		0%,
		100% {
			opacity: 0.5;
		}
		50% {
			opacity: 1;
		}
	}

	/* ── Responsive ───────────────────────────────────────────────────── */
	@media (max-width: 768px) {
		.role-board-page {
			padding: var(--space-4);
		}
		.lanes-container {
			flex-direction: column;
		}
		.lane {
			flex: 0 0 auto;
			min-width: unset;
			width: 100%;
			max-height: none;
		}
	}
</style>
