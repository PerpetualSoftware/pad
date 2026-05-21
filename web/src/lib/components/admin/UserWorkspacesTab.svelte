<!--
  Workspaces tab — per-workspace breakdown of the user's footprint.
  Consumes GET /api/v1/admin/users/{id}/detail (TASK-1545):
  name, slug, role, collections (excluding system: playbooks +
  conventions), items (open/total), members, storage, last_activity_at.

  PLAN-1542 / TASK-1552.
-->
<script lang="ts">
	import { adminFetch, type AdminUser } from '$lib/stores/admin.svelte';

	interface Props {
		user: AdminUser;
		/** Set to true when the parent has opened this tab — triggers the
		 *  lazy fetch on first activation rather than on mount of the
		 *  whole modal. PLAN-1542 / TASK-1552. */
		active: boolean;
	}

	let { user, active }: Props = $props();

	interface WorkspaceDetail {
		workspace_id: string;
		workspace_name: string;
		workspace_slug: string;
		owner_username: string;
		role: string;
		joined_at: string;
		collections_count: number;
		items_open: number;
		items_total: number;
		members_count: number;
		storage_bytes: number;
		last_activity_at?: string;
	}

	let workspaces = $state<WorkspaceDetail[]>([]);
	let loading = $state(false);
	let loadError = $state('');
	let fetchedForUserId = $state<string | null>(null);

	// Cap visible rows at 20 (server caps at 50). "Show all" expands to
	// the full set without re-fetching.
	const VISIBLE_CAP = 20;
	let showAll = $state(false);

	// Lazy load on first activation; refetch when the user changes mid-modal.
	$effect(() => {
		if (active && user && fetchedForUserId !== user.id) {
			loadDetail();
		}
	});

	async function loadDetail() {
		const userId = user.id;
		// Claim this userId BEFORE the await so a re-trigger of the
		// gating effect (parent passes a new object ref with the same id)
		// can't race in and fire a duplicate fetch.
		fetchedForUserId = userId;
		loading = true;
		loadError = '';
		workspaces = [];
		showAll = false;
		try {
			const data = await adminFetch(`/admin/users/${userId}/detail`);
			// Defensive: only commit if the modal hasn't swapped underneath us.
			if (user.id !== userId) return;
			workspaces = (data.workspaces ?? []) as WorkspaceDetail[];
		} catch (e) {
			if (user.id !== userId) return;
			loadError = e instanceof Error ? e.message : 'Failed to load workspaces';
			// fetchedForUserId stays set on failure. Clearing it while
			// the tab is still active would re-trigger the gating
			// $effect immediately and fire loadDetail() again — under
			// a persistent outage that's an infinite fetch loop.
			// Retry is explicit via the Retry button below.
		} finally {
			if (user.id === userId) loading = false;
		}
	}

	let visible = $derived(showAll ? workspaces : workspaces.slice(0, VISIBLE_CAP));
	let overflow = $derived(Math.max(0, workspaces.length - VISIBLE_CAP));

	function formatStorageBytes(n: number): string {
		if (n === -1) return 'Unlimited';
		if (n < 0) return String(n);
		const KB = 1024;
		const MB = KB * 1024;
		const GB = MB * 1024;
		const TB = GB * 1024;
		if (n >= TB) return `${(n / TB).toFixed(2).replace(/\.?0+$/, '')} TB`;
		if (n >= GB) return `${(n / GB).toFixed(2).replace(/\.?0+$/, '')} GB`;
		if (n >= MB) return `${(n / MB).toFixed(1).replace(/\.0$/, '')} MB`;
		if (n >= KB) return `${(n / KB).toFixed(1).replace(/\.0$/, '')} KB`;
		return `${n} B`;
	}

	function relativeTime(dateStr: string | null | undefined): string {
		if (!dateStr) return 'Never';
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		const seconds = Math.floor((now - then) / 1000);
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

{#if loading}
	<div class="state-msg">Loading workspaces…</div>
{:else if loadError}
	<div class="state-msg error">{loadError}
		<button class="btn" type="button" onclick={loadDetail}>Retry</button>
	</div>
{:else if workspaces.length === 0}
	<div class="state-msg empty">This user has no workspaces.</div>
{:else}
	<div class="ws-table-wrap">
		<table class="ws-table">
			<thead>
				<tr>
					<th>Workspace</th>
					<th>Role</th>
					<th class="num">Collections</th>
					<th class="num">Items (open / total)</th>
					<th class="num">Members</th>
					<th class="num">Storage</th>
					<th>Last activity</th>
				</tr>
			</thead>
			<tbody>
				{#each visible as ws (ws.workspace_id)}
					<tr>
						<td>
							<a
								class="ws-link"
								href={'/' + ws.owner_username + '/' + ws.workspace_slug}
								target="_blank"
								rel="noopener"
							>
								{ws.workspace_name}
							</a>
							<div class="ws-slug">{ws.workspace_slug}</div>
						</td>
						<td><span class="badge role-{ws.role}">{ws.role}</span></td>
						<td class="num">{ws.collections_count}</td>
						<td class="num">{ws.items_open} / {ws.items_total}</td>
						<td class="num">{ws.members_count}</td>
						<td class="num">{formatStorageBytes(ws.storage_bytes)}</td>
						<td>{relativeTime(ws.last_activity_at)}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
	{#if overflow > 0 && !showAll}
		<button class="btn show-all-btn" type="button" onclick={() => (showAll = true)}
			>Show all {workspaces.length} workspaces</button
		>
	{/if}
{/if}

<style>
	.state-msg {
		padding: var(--space-4);
		color: var(--text-muted);
		text-align: center;
	}
	.state-msg.error {
		color: #ef4444;
	}
	.state-msg.empty {
		font-style: italic;
	}
	.ws-table-wrap {
		overflow-x: auto;
	}
	.ws-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.85rem;
	}
	.ws-table th,
	.ws-table td {
		text-align: left;
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border);
		vertical-align: middle;
	}
	.ws-table th {
		font-weight: 600;
		color: var(--text-muted);
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}
	.ws-table .num {
		text-align: right;
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}
	.ws-link {
		color: var(--accent-blue);
		text-decoration: none;
	}
	.ws-link:hover {
		text-decoration: underline;
	}
	.ws-slug {
		font-size: 0.75rem;
		color: var(--text-muted);
	}
	.badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
		text-transform: lowercase;
	}
	.badge.role-owner {
		background: color-mix(in srgb, var(--accent-orange, #f59e0b) 15%, transparent);
		color: var(--accent-orange, #f59e0b);
	}
	.badge.role-editor {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	.show-all-btn {
		display: block;
		margin: var(--space-3) auto 0;
		padding: 6px 14px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		font-size: 0.85rem;
		cursor: pointer;
	}
	.show-all-btn:hover {
		background: var(--bg-tertiary);
	}
	.btn {
		padding: 4px 10px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		cursor: pointer;
		font-size: 0.8rem;
		margin-left: var(--space-2);
	}
</style>
