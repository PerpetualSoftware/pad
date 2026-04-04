<script lang="ts">
	import { page } from '$app/state';
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { parseFields, formatItemRef, itemUrlId } from '$lib/types';
	import type { Item, RoleBoardLane, AgentRole } from '$lib/types';
	import { dndzone, TRIGGERS, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';

	let wsSlug = $derived(page.params.workspace ?? '');

	// Data
	let lanes = $state<RoleBoardLane[]>([]);
	let loading = $state(true);
	let error = $state('');

	// Highlight: dim cards not assigned to current user
	let highlightMine = $state(false);

	// Role editing modal state
	let dialogEl = $state<HTMLDialogElement | null>(null);
	let dialogMode = $state<'edit' | 'create'>('create');
	let editingRoleId = $state<string | null>(null);
	let editName = $state('');
	let editDescription = $state('');
	let editIcon = $state('');
	let editTools = $state('');

	function openEditModal(role: AgentRole) {
		dialogMode = 'edit';
		editingRoleId = role.id;
		editName = role.name;
		editDescription = role.description;
		editIcon = role.icon;
		editTools = role.tools;
		dialogEl?.showModal();
	}

	function openCreateModal() {
		dialogMode = 'create';
		editingRoleId = null;
		editName = '';
		editDescription = '';
		editIcon = '';
		editTools = '';
		dialogEl?.showModal();
	}

	function closeModal() {
		dialogEl?.close();
	}
	let currentUserId = $state('');

	// Reorder lanes: unassigned first, then roles in order
	let orderedLanes = $derived.by(() => {
		const unassigned = lanes.filter((l) => !l.role);
		const assigned = lanes.filter((l) => l.role);
		return [...unassigned, ...assigned];
	});

	let totalItems = $derived(orderedLanes.reduce((sum, lane) => sum + lane.items.length, 0));

	// Drag-and-drop state
	const flipDurationMs = 200;
	const touchDragDelayMs = 500;
	let isDragging = $state(false);

	// Mutable lane data for DnD — keyed by role ID (or '__unassigned')
	let laneData = $state<Record<string, Item[]>>({});

	// Sync from orderedLanes when not dragging
	$effect(() => {
		if (!isDragging) {
			const data: Record<string, Item[]> = {};
			for (const lane of orderedLanes) {
				const key = lane.role?.id ?? '__unassigned';
				data[key] = [...lane.items];
			}
			laneData = data;
		}
	});

	function laneKey(lane: RoleBoardLane): string {
		return lane.role?.id ?? '__unassigned';
	}

	function handleDndConsider(key: string, e: CustomEvent<DndEvent<Item>>) {
		laneData[key] = e.detail.items;
		if (!isDragging && e.detail.info.trigger === TRIGGERS.DRAG_STARTED) {
			if (typeof navigator !== 'undefined' && navigator.vibrate) {
				navigator.vibrate(50);
			}
		}
		isDragging = true;
	}

	async function handleDndFinalize(key: string, e: CustomEvent<DndEvent<Item>>) {
		const finalItems = e.detail.items.filter((i: any) => !i[SHADOW_ITEM_MARKER_PROPERTY_NAME]);
		laneData[key] = finalItems;

		// Keep isDragging true until lanes state is updated,
		// so the $effect doesn't overwrite laneData from stale orderedLanes.

		const { id: itemId, trigger } = e.detail.info;

		// Check for cross-lane move
		if (trigger === TRIGGERS.DROPPED_INTO_ZONE) {
			const originalItem = orderedLanes.flatMap((l) => l.items).find((i) => i.id === itemId);
			const oldKey = originalItem ? (originalItem.agent_role_id ?? '__unassigned') : key;

			if (originalItem && oldKey !== key) {
				// Cross-lane: update the item's role
				const newRoleId = key === '__unassigned' ? null : key;
				const targetRole = orderedLanes.find((l) => laneKey(l) === key)?.role ?? null;

				const updatedItem = { ...originalItem,
					agent_role_id: newRoleId,
					agent_role_name: targetRole?.name ?? '',
					agent_role_slug: targetRole?.slug ?? '',
					agent_role_icon: targetRole?.icon ?? '',
				};

				if (!originalItem.assigned_user_id && currentUserId && newRoleId) {
					updatedItem.assigned_user_id = currentUserId;
				}

				lanes = lanes.map((lane) => {
					const lk = lane.role?.id ?? '__unassigned';
					if (lk === oldKey) {
						return { ...lane, items: lane.items.filter((i) => i.id !== itemId) };
					}
					if (lk === key) {
						return { ...lane, items: [...lane.items.filter((i) => i.id !== itemId), updatedItem] };
					}
					return lane;
				});

				try {
					const update: Record<string, any> = {};
					if (key === '__unassigned') {
						update.clear_agent_role = true;
					} else {
						update.agent_role_id = key;
						if (!originalItem.assigned_user_id && currentUserId) {
							update.assigned_user_id = currentUserId;
						}
					}
					await api.items.update(wsSlug, originalItem.id, update);
				} catch (err) {
					console.error('Failed to update role:', err);
					await loadData();
					isDragging = false;
					return;
				}
			}
		}

		// Always persist sort order for all items in this lane (covers both
		// within-lane reorder and cross-lane moves)
		const reorderUpdates = finalItems.map((item, index) => ({
			item_id: item.id,
			role_sort_order: index
		}));

		// Optimistic: update lanes state with new sort orders BEFORE releasing isDragging
		lanes = lanes.map((lane) => {
			if (laneKey(lane) !== key) return lane;
			return { ...lane, items: finalItems.map((item, index) => ({ ...item, role_sort_order: index })) };
		});

		// Now safe to release — lanes has the correct data for the $effect to sync from
		isDragging = false;

		try {
			await api.agentRoles.reorder(wsSlug, reorderUpdates);
		} catch (err) {
			console.error('Failed to persist sort order:', err);
		}
	}

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

	async function saveRole() {
		if (!editName.trim()) return;
		try {
			if (dialogMode === 'edit' && editingRoleId) {
				await api.agentRoles.update(wsSlug, editingRoleId, {
					name: editName.trim(),
					description: editDescription.trim(),
					icon: editIcon.trim(),
					tools: editTools.trim()
				});
			} else {
				await api.agentRoles.create(wsSlug, {
					name: editName.trim(),
					description: editDescription.trim(),
					icon: editIcon.trim(),
					tools: editTools.trim()
				});
			}
			closeModal();
			await loadData();
		} catch (e) {
			console.error('Failed to save role:', e);
		}
	}

	async function deleteRole() {
		if (!editingRoleId) return;
		if (!confirm(`Delete role "${editName}"? Items assigned to this role will become unassigned.`)) return;
		try {
			await api.agentRoles.delete(wsSlug, editingRoleId);
			closeModal();
			await loadData();
		} catch (e) {
			console.error('Failed to delete role:', e);
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
				class:active={highlightMine}
				onclick={() => highlightMine = !highlightMine}
			>
				Highlight Mine
			</button>
		</div>
	</header>

	<!-- Role edit/create modal -->
<dialog class="roles-dialog" bind:this={dialogEl} onclick={(e) => { if (e.target === dialogEl) closeModal(); }}>
	<div class="dialog-content">
		<div class="dialog-header">
			<h2>{dialogMode === 'edit' ? 'Edit Role' : 'New Role'}</h2>
			<button class="dialog-close" onclick={closeModal}>✕</button>
		</div>

		<div class="dialog-body">
			<div class="role-edit-form">
				<div class="role-field-group">
					<label class="role-field-label">Icon & Name</label>
					<div class="role-edit-row">
						<input class="role-input role-input-icon" type="text" bind:value={editIcon} placeholder="🔨" />
						<input class="role-input" type="text" bind:value={editName} placeholder="Role name" />
					</div>
				</div>
				<div class="role-field-group">
					<label class="role-field-label">Description</label>
					<input class="role-input" type="text" bind:value={editDescription} placeholder="What does this role do?" />
				</div>
				<div class="role-field-group">
					<label class="role-field-label">Tools</label>
					<input class="role-input" type="text" bind:value={editTools} placeholder="e.g. Claude Code + Sonnet 4.6" />
				</div>
			</div>
		</div>

		<div class="dialog-footer">
			{#if dialogMode === 'edit'}
				<button class="role-btn role-btn-danger" onclick={deleteRole}>Delete Role</button>
				<div class="dialog-footer-right">
					<button class="role-btn" onclick={closeModal}>Cancel</button>
					<button class="role-btn role-btn-save" disabled={!editName.trim()} onclick={saveRole}>Save</button>
				</div>
			{:else}
				<div></div>
				<div class="dialog-footer-right">
					<button class="role-btn" onclick={closeModal}>Cancel</button>
					<button class="role-btn role-btn-save" disabled={!editName.trim()} onclick={saveRole}>Create</button>
				</div>
			{/if}
		</div>
	</div>
</dialog>

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
	{:else if orderedLanes.length === 0}
		<div class="empty-state">
			{#if highlightMine}
				<div class="empty-icon">&#128100;</div>
				<p class="empty-title">No items assigned to you</p>
				<p class="empty-desc">
					Turn off "My Work" to see all items, or assign items to yourself from the item detail page.
				</p>
			{:else}
				<div class="empty-icon">&#127917;</div>
				<p class="empty-title">No roles configured</p>
				<p class="empty-desc">
					Agent roles let you organize work by what kind of thinking it requires — planning, implementing, reviewing, etc.
				</p>
				<button class="retry-btn" onclick={openCreateModal}>Create your first role</button>
			{/if}
		</div>
	{:else}
		<div class="lanes-container">
			{#each orderedLanes as lane (lane.role?.id ?? '__unassigned')}
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
							{#if lane.role}
								<button class="lane-edit-btn" title="Edit role" onclick={() => openEditModal(lane.role)}>✎</button>
							{/if}
						</div>
						{#if lane.role?.tools}
							<div class="lane-tools">{lane.role.tools}</div>
						{/if}
						</div>

					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="lane-items"
						use:dndzone={{
							items: laneData[laneKey(lane)] ?? [],
							flipDurationMs,
							type: 'role-board-card',
							dropTargetClasses: ['drop-target'],
							delayTouchStart: touchDragDelayMs
						}}
						onconsider={(e) => handleDndConsider(laneKey(lane), e)}
						onfinalize={(e) => handleDndFinalize(laneKey(lane), e)}
						oncontextmenu={(e) => e.preventDefault()}
					>
						{#each (laneData[laneKey(lane)] ?? []) as item (item.id)}
							{@const fields = parseFields(item)}
							{@const ref = formatItemRef(item)}
							{@const status = fields.status ?? ''}
							{@const priority = fields.priority ?? ''}
							<div class="card-wrapper" class:dimmed={highlightMine && currentUserId && item.assigned_user_id !== currentUserId}>
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
							</div>
						{/each}
						{#if (laneData[laneKey(lane)] ?? []).length === 0 && !isDragging}
							<div class="lane-empty">No items</div>
						{/if}
					</div>
				</div>
			{/each}

			<!-- Add role column -->
			<div class="lane lane-add">
				<button class="add-role-btn" onclick={openCreateModal}>
					<span class="add-role-icon">+</span>
					<span class="add-role-label">Add Role</span>
				</button>
			</div>
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
		align-items: stretch;
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

	/* ── Lane Items ───────────────────────────────────────────────────── */
	.lane-items {
		padding: var(--space-2);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		overflow-y: auto;
		flex: 1;
	}

	.lane-items:global(.drop-target) {
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}
	.card-wrapper {
		cursor: grab;
		-webkit-touch-callout: none;
		-webkit-user-select: none;
		user-select: none;
	}
	.card-wrapper:active {
		cursor: grabbing;
	}
	.card-wrapper.dimmed {
		opacity: 0.35;
		transition: opacity 0.15s;
	}
	.card-wrapper.dimmed:hover {
		opacity: 0.7;
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
			padding: 0;
		}
		.page-header {
			padding: var(--space-3) var(--space-4);
		}
		.lanes-container {
			overflow-x: auto;
			scroll-snap-type: x proximity;
			-webkit-overflow-scrolling: touch;
			gap: var(--space-3);
			padding: 0 var(--space-4) var(--space-3);
		}
		.lane {
			min-width: 75vw;
			max-width: 75vw;
			scroll-snap-align: center;
			flex-shrink: 0;
			max-height: none;
		}
	}

	/* ── Lane edit button ─────────────────────────────── */
	.lane-edit-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		padding: 2px 6px;
		border-radius: var(--radius-sm);
		opacity: 0;
		transition: opacity 0.15s;
		margin-left: auto;
	}
	.lane-title-row:hover .lane-edit-btn {
		opacity: 1;
	}
	.lane-edit-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/* ── Add role column ─────────────────────────────── */
	.lane-add {
		background: transparent;
		border: 2px dashed var(--border);
		display: flex;
		align-items: center;
		justify-content: center;
		align-self: flex-start;
		min-height: 0;
		flex: 0 0 auto;
		min-width: auto;
		width: auto;
		padding: var(--space-3);
	}
	.add-role-btn {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-2);
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: var(--space-4);
		border-radius: var(--radius);
		transition: color 0.15s, background 0.15s;
	}
	.add-role-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.add-role-icon {
		font-size: 1.8em;
		font-weight: 300;
		line-height: 1;
	}
	.add-role-label {
		font-size: 0.85em;
		font-weight: 500;
	}

	/* ── Roles Dialog ─────────────────────────────────── */
	.roles-dialog {
		border: none;
		border-radius: var(--radius-lg);
		padding: 0;
		max-width: 520px;
		width: 90vw;
		max-height: 80vh;
		background: var(--bg-primary);
		color: var(--text-primary);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
		position: fixed;
		top: 50%;
		left: 50%;
		transform: translate(-50%, -50%);
		margin: 0;
	}
	.roles-dialog::backdrop {
		background: rgba(0, 0, 0, 0.5);
	}
	.dialog-content {
		display: flex;
		flex-direction: column;
		max-height: 80vh;
	}
	.dialog-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}
	.dialog-header h2 {
		margin: 0;
		font-size: 1.1em;
		font-weight: 600;
	}
	.dialog-close {
		background: none;
		border: none;
		font-size: 1.2em;
		color: var(--text-muted);
		cursor: pointer;
		padding: 4px 8px;
		border-radius: var(--radius-sm);
	}
	.dialog-close:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.dialog-body {
		padding: var(--space-4) var(--space-5);
		overflow-y: auto;
	}
	.dialog-footer {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-5);
		border-top: 1px solid var(--border);
	}
	.dialog-footer-right {
		display: flex;
		gap: var(--space-2);
	}

	/* ── Shared form elements ─────────────────────────── */
	.role-edit-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.role-field-group {
		display: flex;
		flex-direction: column;
		gap: 4px;
	}
	.role-field-label {
		font-size: 0.78em;
		font-weight: 500;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.03em;
	}
	.role-edit-row {
		display: flex;
		gap: var(--space-2);
	}
	.role-input {
		width: 100%;
		padding: 7px 10px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}
	.role-input:focus {
		outline: 2px solid var(--accent-blue);
		outline-offset: -1px;
	}
	.role-input-icon {
		width: 48px;
		flex-shrink: 0;
		text-align: center;
	}
	.role-edit-actions {
		display: flex;
		gap: var(--space-2);
	}
	.role-btn {
		padding: 5px 12px;
		font-size: 0.82em;
		font-family: inherit;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		cursor: pointer;
	}
	.role-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.role-btn-save {
		background: var(--accent-blue);
		color: white;
		border-color: var(--accent-blue);
	}
	.role-btn-save:hover {
		filter: brightness(1.1);
	}
	.role-btn-create {
		width: 100%;
		padding: 8px;
		background: var(--accent-blue);
		color: white;
		border-color: var(--accent-blue);
		font-weight: 500;
	}
	.role-btn-create:hover:not(:disabled) {
		filter: brightness(1.1);
	}
	.role-btn-create:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.role-btn-cancel {
		color: var(--text-muted);
	}
	.role-btn-danger {
		color: var(--accent-orange);
	}
	.role-btn-danger:hover {
		background: color-mix(in srgb, var(--accent-orange) 15%, transparent);
	}
</style>
