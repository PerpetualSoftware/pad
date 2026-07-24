<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { adminFetch, formatDate, adminStore, type AdminUser } from '$lib/stores/admin.svelte';
	import UserModal from '$lib/components/admin/UserModal.svelte';
	import Chip from '$lib/components/common/Chip.svelte';

	// --- List + pagination + filter + sort state (PLAN-1542 / TASK-1549) ---
	// All filter state is reactive $state; we rebuild the query string on
	// every loadList() call and reset offset to 0 on any filter/sort change.
	// URL params reflect the filter/sort set so admins can paste a link
	// (e.g. "all disabled pro users sorted by storage desc"). The search
	// input is deliberately NOT in the URL — it changes on every keystroke
	// and would clutter history.
	type SortKey = 'email' | 'last_write' | 'last_active' | 'storage' | 'workspaces' | 'created';
	type SortOrder = 'asc' | 'desc';
	const PAGE_LIMIT = 50;

	let users = $state<AdminUser[]>([]);
	let total = $state(0);
	let offset = $state(0);
	let loadingMore = $state(false);
	let search = $state('');
	let planFilter = $state('');
	let roleFilter = $state('');
	let statusFilter = $state(''); // disabled / no-workspace / inactive / active / ''
	let activeWithinDaysFilter = $state(''); // '7' | '30' | '90' | ''
	let hasWorkspacesFilter = $state(''); // 'true' | 'false' | ''
	let sortKey = $state<SortKey>('created');
	let sortOrder = $state<SortOrder>('desc');

	// Per-user modal state. The modal owns every detail surface;
	// the inline-expand was removed in TASK-1555.
	let modalUser = $state<AdminUser | null>(null);
	let modalOpen = $state(false);
	function openUserModal(u: AdminUser) {
		modalUser = u;
		modalOpen = true;
	}
	function closeUserModal() {
		modalOpen = false;
		// Keep modalUser around so the closing animation (if any) doesn't
		// blank the modal mid-fade; cleared on next open.
	}
	// Called by the modal's Settings tab after any save — merges the
	// refetched user into the table row and the bound modalUser so the
	// list stays in sync without a full reload. PLAN-1542 / TASK-1551.
	function onModalUserUpdated(updated: AdminUser) {
		users = users.map((u) => (u.id === updated.id ? { ...u, ...updated } : u));
		if (modalUser && modalUser.id === updated.id) modalUser = { ...modalUser, ...updated };
	}
	let loading = $state(true);
	let error = $state('');

	// buildQueryParams renders the current filter/sort/pagination state
	// into a URLSearchParams. Sort and filter values are always present
	// when non-default so the server resolves them consistently.
	function buildQueryParams(opts: { offset?: number; includeSearch?: boolean } = {}): URLSearchParams {
		const p = new URLSearchParams();
		if (opts.includeSearch && search) p.set('q', search);
		if (planFilter) p.set('plan', planFilter);
		if (roleFilter) p.set('role', roleFilter);
		// statusFilter and hasWorkspacesFilter are independent; we used to
		// auto-map statusFilter=no-workspace to has_workspaces=false but
		// that conflicted with the explicit hasWorkspacesFilter and broke
		// the server-side status precedence (disabled > no-workspace).
		// Now: statusFilter is always applied client-side via
		// applyClientStatusFilter; only the "disabled" case gets a server
		// hint so the result set is smaller for the common case (Codex
		// review on PR #604, finding 1+2+3).
		if (statusFilter === 'disabled') p.set('disabled', 'true');
		if (activeWithinDaysFilter) p.set('active_within_days', activeWithinDaysFilter);
		if (hasWorkspacesFilter) p.set('has_workspaces', hasWorkspacesFilter);
		if (sortKey !== 'created' || sortOrder !== 'desc') {
			p.set('sort', sortKey);
			p.set('order', sortOrder);
		}
		p.set('limit', String(PAGE_LIMIT));
		p.set('offset', String(opts.offset ?? offset));
		return p;
	}

	// applyClientStatusFilter narrows the response to the selected status
	// bucket using the server-computed `status` field on each row. Server
	// precedence (disabled > no-workspace > inactive > active) is preserved
	// because we filter on the exact value, not approximations.
	function applyClientStatusFilter(rows: AdminUser[]): AdminUser[] {
		if (!statusFilter) return rows;
		return rows.filter((u) => u.status === statusFilter);
	}

	async function loadList(reset: boolean = true) {
		if (reset) {
			loading = true;
			offset = 0;
		} else {
			loadingMore = true;
		}
		error = '';
		try {
			const params = buildQueryParams({ offset: reset ? 0 : offset, includeSearch: true });
			const result = await adminFetch('/admin/users?' + params.toString());
			const rows: AdminUser[] = applyClientStatusFilter(result.users ?? result);
			users = reset ? rows : [...users, ...rows];
			total = typeof result.total === 'number' ? result.total : users.length;
			// Sync filter/sort state back to URL (skip on reset=false to
			// avoid spamming history during paginated "load more").
			if (reset) syncURL();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load users';
		} finally {
			loading = false;
			loadingMore = false;
		}
	}

	// loadMore appends the next page without resetting offset. On failure,
	// rolls back the offset so a retry doesn't skip a page (Codex review
	// on PR #604). Also guards against re-entrant calls while a page is
	// in flight.
	async function loadMore() {
		if (loadingMore || loading) return;
		const prevOffset = offset;
		offset += PAGE_LIMIT;
		try {
			await loadList(false);
		} catch {
			offset = prevOffset;
		}
		// Belt-and-braces: if loadList caught the error internally and set
		// `error`, also roll back so the next click retries the same page.
		if (error) offset = prevOffset;
	}

	// onFilterChange resets pagination + reloads. Bound to every filter
	// input + the sort header clicks.
	function onFilterChange() {
		offset = 0;
		loadList(true);
	}

	function setSort(key: SortKey) {
		if (sortKey === key) {
			sortOrder = sortOrder === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			// Default direction: desc for time/numeric, asc for email.
			sortOrder = key === 'email' ? 'asc' : 'desc';
		}
		onFilterChange();
	}

	// Search button (or Enter in search input) — same as a filter change.
	function searchUsers() {
		onFilterChange();
	}

	// syncURL pushes filter/sort state into the URL via SvelteKit's goto,
	// replaceState so we don't litter browser history. Search query is
	// excluded (changes per keystroke). Status is its own URL param
	// (rather than smuggled through has_workspaces/disabled) so all four
	// buckets — active/inactive/disabled/no-workspace — round-trip
	// losslessly (Codex review on PR #604).
	function syncURL() {
		const p = buildQueryParams({ offset: 0, includeSearch: false });
		p.delete('limit');
		p.delete('offset');
		// buildQueryParams stamps disabled=true for statusFilter=disabled
		// (server hint). Strip it from the URL; statusFilter is the
		// canonical representation in the URL.
		p.delete('disabled');
		if (statusFilter) p.set('status', statusFilter);
		const qs = p.toString();
		const target = qs ? `?${qs}` : page.url.pathname;
		goto(target, { replaceState: true, noScroll: true, keepFocus: true });
	}

	// hydrateFromURL pulls filter/sort state off the URL on first load
	// so a pasted link restores the view.
	function hydrateFromURL() {
		const p = page.url.searchParams;
		planFilter = p.get('plan') ?? '';
		roleFilter = p.get('role') ?? '';
		hasWorkspacesFilter = p.get('has_workspaces') ?? '';
		activeWithinDaysFilter = p.get('active_within_days') ?? '';
		const s = p.get('sort');
		if (s === 'email' || s === 'last_write' || s === 'last_active' || s === 'storage' || s === 'workspaces' || s === 'created') {
			sortKey = s;
		}
		const o = p.get('order');
		if (o === 'asc' || o === 'desc') sortOrder = o;
		const st = p.get('status');
		if (st === 'active' || st === 'inactive' || st === 'disabled' || st === 'no-workspace') {
			statusFilter = st;
		}
	}

	// formatStorageBytes renders a raw byte count in the largest unit
	// that keeps the number readable (-1 → "-1" unlimited). Used by the
	// Storage cell in the user table. The parse-side of this helper
	// lives in UserSettingsForm.svelte (lifted in T1551). PLAN-1542 /
	// TASK-1555.
	function formatStorageBytes(n: number): string {
		if (n === -1) return '-1';
		if (n < 0) return String(n);
		const KB = 1024;
		const MB = KB * 1024;
		const GB = MB * 1024;
		const TB = GB * 1024;
		if (n >= TB && n % TB === 0) return `${n / TB} TB`;
		if (n >= GB && n % GB === 0) return `${n / GB} GB`;
		if (n >= MB && n % MB === 0) return `${n / MB} MB`;
		if (n >= KB && n % KB === 0) return `${n / KB} KB`;
		return String(n);
	}

	function relativeTime(dateStr: string | null): string {
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
		return formatDate(dateStr);
	}

	// writeRecency buckets the last-write timestamp into a CSS class:
	//   recent: < 7 days (green)
	//   stale:  < 30 days (yellow)
	//   cold:   ≥ 30 days (red)
	//   never:  null (gray)
	// Buckets are the visual half of the API's "status" pill — the pill
	// names the overall user state, this cell colors the write-age cell
	// at a glance. PLAN-1542 / TASK-1548.
	function writeRecency(dateStr: string | null): 'recent' | 'stale' | 'cold' | 'never' {
		if (!dateStr) return 'never';
		const days = (Date.now() - new Date(dateStr).getTime()) / (1000 * 60 * 60 * 24);
		if (days < 7) return 'recent';
		// 30 days inclusive is "stale" to match server-side
		// computeAdminUserStatus which only flips to inactive on > 30 days
		// (Codex review on PR #603).
		if (days <= 30) return 'stale';
		return 'cold';
	}

	// Status pill colors — these are admin *user* states (disabled /
	// no-workspace / inactive), not item statuses, so they map locally
	// rather than through fieldColors.statusColor. "active" never renders.
	function userStatusColor(status: string): string {
		if (status === 'disabled') return 'var(--accent-red)';
		if (status === 'inactive') return 'var(--accent-amber)';
		return 'var(--accent-gray)'; // no-workspace
	}

	onMount(() => {
		hydrateFromURL();
		loadList(true);
	});
</script>

<div class="users-page">
	{#if loading}
		<div class="loading-msg">Loading users...</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={() => loadList(true)}>Retry</button>
		</div>
	{:else}
		<div class="search-row">
			<input
				type="text"
				class="search-input"
				placeholder="Search users..."
				bind:value={search}
				onkeydown={(e) => {
					if (e.key === 'Enter') searchUsers();
				}}
			/>
			<button class="btn" onclick={searchUsers}>Search</button>
		</div>

		<!-- Filter bar (PLAN-1542 / TASK-1549). Each control fires
		     onFilterChange so the list reloads from offset=0 and the
		     URL updates. -->
		<div class="filter-bar">
			<label class="filter-field">
				<span class="filter-label">Role</span>
				<select bind:value={roleFilter} onchange={onFilterChange}>
					<option value="">All</option>
					<option value="admin">admin</option>
					<option value="member">member</option>
				</select>
			</label>

			{#if adminStore.stats?.cloud_mode}
				<label class="filter-field">
					<span class="filter-label">Plan</span>
					<select bind:value={planFilter} onchange={onFilterChange}>
						<option value="">All</option>
						<option value="free">free</option>
						<option value="pro">pro</option>
						<option value="self-hosted">self-hosted</option>
					</select>
				</label>
			{/if}

			<label class="filter-field">
				<span class="filter-label">Status</span>
				<select bind:value={statusFilter} onchange={onFilterChange}>
					<option value="">All</option>
					<option value="active">active</option>
					<option value="inactive">inactive</option>
					<option value="disabled">disabled</option>
					<option value="no-workspace">no-workspace</option>
				</select>
			</label>

			<label class="filter-field">
				<span class="filter-label">Active within</span>
				<select bind:value={activeWithinDaysFilter} onchange={onFilterChange}>
					<option value="">Any</option>
					<option value="7">7 days</option>
					<option value="30">30 days</option>
					<option value="90">90 days</option>
				</select>
			</label>

			<label class="filter-field">
				<span class="filter-label">Has workspaces</span>
				<select bind:value={hasWorkspacesFilter} onchange={onFilterChange}>
					<option value="">All</option>
					<option value="true">Yes</option>
					<option value="false">No</option>
				</select>
			</label>

			{#if planFilter || roleFilter || statusFilter || activeWithinDaysFilter || hasWorkspacesFilter}
				<button
					class="btn btn-sub"
					onclick={() => {
						planFilter = '';
						roleFilter = '';
						statusFilter = '';
						activeWithinDaysFilter = '';
						hasWorkspacesFilter = '';
						onFilterChange();
					}}>Clear filters</button
				>
			{/if}
		</div>

		<div class="table-wrap">
			<table class="table">
				<thead>
					<!-- Sortable headers are keyboard-accessible buttons inside
					     the cell so they participate in tab order; aria-sort
					     conveys current state to screen readers. PLAN-1542 /
					     TASK-1549. -->
					<tr>
						<th>Name</th>
						<th>Role</th>
						<th aria-sort={sortKey === 'workspaces' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('workspaces')}
								>Workspaces <span class="sort-ind">{sortKey === 'workspaces' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
						<th aria-sort={sortKey === 'email' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('email')}
								>Email <span class="sort-ind">{sortKey === 'email' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
						{#if adminStore.stats?.cloud_mode}
							<th>Plan</th>
						{/if}
						<th aria-sort={sortKey === 'storage' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('storage')}
								>Storage <span class="sort-ind">{sortKey === 'storage' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
						<th aria-sort={sortKey === 'last_write' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('last_write')}
								>Last Write <span class="sort-ind">{sortKey === 'last_write' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
						<th aria-sort={sortKey === 'last_active' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('last_active')}
								>Last Active <span class="sort-ind">{sortKey === 'last_active' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
						<th aria-sort={sortKey === 'created' ? (sortOrder === 'asc' ? 'ascending' : 'descending') : 'none'}>
							<button type="button" class="sort-btn" onclick={() => setSort('created')}
								>Created <span class="sort-ind">{sortKey === 'created' ? (sortOrder === 'asc' ? '▲' : '▼') : ''}</span></button>
						</th>
					</tr>
				</thead>
				<tbody>
					{#each users as user (user.id)}
						<tr
							class="user-row"
							class:disabled-row={!!user.disabled_at}
							onclick={() => openUserModal(user)}
						>
							<td>
								{user.name || user.username}
								<!-- Status pill replaces the legacy "disabled" badge.
								     "active" is the common case; omit the pill to avoid
								     visual noise. Other states call out problems. -->
								{#if user.status && user.status !== 'active'}
									<span class="status-chip"><Chip size="sm" color={userStatusColor(user.status)}>{user.status}</Chip></span>
								{/if}
							</td>
							<td
								><Chip size="sm" color={user.role === 'admin' ? 'var(--accent-orange)' : 'var(--accent-gray)'}
									>{user.role || 'member'}</Chip
								></td
							>
							<td class="num-cell">{user.workspace_count ?? 0}</td>
							<td>{user.email}</td>
							{#if adminStore.stats?.cloud_mode}
								<td
									><Chip size="sm" color={user.plan === 'pro' ? 'var(--status-blue)' : 'var(--accent-gray)'}
										>{user.plan || 'free'}</Chip
									></td
								>
							{/if}
							<td class="num-cell">{formatStorageBytes(user.storage_bytes ?? 0)}</td>
							<td
								class="date-cell write-{writeRecency(user.last_write_at)}"
								title={user.last_write_at || ''}
								aria-label={`Last write: ${relativeTime(user.last_write_at)} (${writeRecency(user.last_write_at)})`}
								>{relativeTime(user.last_write_at)}</td>
							<td class="date-cell muted"
								title={user.last_active_at || ''}
								>{relativeTime(user.last_active_at)}</td>
							<td class="date-cell">{formatDate(user.created_at)}</td>
						</tr>
					{/each}
				</tbody>
			</table>
			{#if total > 0}
				<div class="pager">
					<span class="pager-info"
						>Showing <strong>{users.length}</strong> of <strong>{total}</strong>{#if statusFilter === 'active' || statusFilter === 'inactive'}
							(client-filtered){/if}</span
					>
					{#if users.length < total}
						<button class="btn" disabled={loadingMore} onclick={loadMore}
							>{loadingMore ? 'Loading…' : 'Load more'}</button
						>
					{/if}
				</div>
			{:else if !loading}
				<div class="pager pager-empty">No users match the current filters.</div>
			{/if}
		</div>
	{/if}
</div>

<!-- Per-user detail modal — Overview, Workspaces, Activity, Settings. -->
<UserModal
	bind:open={modalOpen}
	user={modalUser}
	onClose={closeUserModal}
	onUserUpdated={onModalUserUpdated}
/>

<style>
	/* Search */
	.search-row {
		display: flex;
		gap: var(--space-2);
	}
	.search-input {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		outline: none;
	}
	.search-input:focus {
		border-color: var(--accent-blue);
	}

	/* Filter bar (PLAN-1542 / TASK-1549) */
	.filter-bar {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
		align-items: end;
		margin-top: var(--space-3);
	}
	.filter-field {
		display: flex;
		flex-direction: column;
		gap: 4px;
		font-size: 0.8rem;
	}
	.filter-label {
		color: var(--text-muted);
		font-size: 0.75rem;
	}
	.filter-field select {
		padding: 4px var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.85rem;
		outline: none;
	}
	.btn-sub {
		background: transparent;
		color: var(--text-muted);
		border: 1px dashed var(--border);
		align-self: end;
	}

	/* Sortable column headers — button-inside-th pattern so they're
	   keyboard-focusable and announce as buttons. */
	.sort-btn {
		display: inline-flex;
		align-items: center;
		gap: 4px;
		background: transparent;
		border: 0;
		padding: 0;
		font: inherit;
		color: inherit;
		cursor: pointer;
		user-select: none;
	}
	.sort-btn:hover {
		color: var(--accent-blue);
	}
	.sort-btn:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
		border-radius: 2px;
	}
	.sort-ind {
		display: inline-block;
		min-width: 10px;
		color: var(--accent-blue);
		font-size: 0.7rem;
	}

	/* Pager */
	.pager {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: var(--space-3) 0;
		gap: var(--space-3);
		font-size: 0.85rem;
	}
	.pager-info {
		color: var(--text-muted);
	}
	.pager-empty {
		justify-content: center;
		font-style: italic;
		color: var(--text-muted);
	}

	/* Buttons */
	.btn {
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-secondary);
		font-size: 0.85rem;
		font-weight: 500;
		cursor: pointer;
		transition:
			border-color 0.15s,
			color 0.15s;
	}
	.btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}
	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}

	/* Table */
	.table-wrap {
		overflow-x: auto;
	}
	.table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.85rem;
	}
	.table th {
		text-align: left;
		padding: var(--space-2) var(--space-3);
		color: var(--text-muted);
		font-weight: 500;
		border-bottom: 1px solid var(--border);
		font-size: 0.8rem;
	}
	.table td {
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border);
		color: var(--text-secondary);
	}
	.user-row {
		cursor: pointer;
		transition: background 0.1s;
	}
	.user-row:hover {
		background: var(--bg-hover);
	}
	.date-cell {
		white-space: nowrap;
	}
	.date-cell.muted {
		color: var(--text-muted);
		font-size: 0.8rem;
	}
	/* Status pill wrapper — keeps the pill offset from the user name.
	   Colors live in userStatusColor(); the pill itself is the shared
	   <Chip> primitive. PLAN-1542 / TASK-1548 / TASK-2292. */
	.status-chip {
		margin-left: var(--space-2);
	}
	/* Numeric cells (workspace_count, storage_bytes) — right-aligned and
	   tabular figures so digits line up vertically across rows. */
	.num-cell {
		text-align: right;
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}
	/* Last-write recency coloring — see writeRecency() in the script. */
	.date-cell.write-recent { color: #10b981; }
	.date-cell.write-stale { color: #f59e0b; }
	.date-cell.write-cold { color: var(--accent-red); }
	.date-cell.write-never {
		color: var(--text-muted);
		font-style: italic;
	}
	.disabled-row {
		opacity: 0.6;
	}

	/* Edit-panel + plan-override + ws-list CSS lived here until TASK-1555.
	   All inline-expand styles were dropped with the inline-expand DOM
	   itself; the modal (UserSettingsForm + UserWorkspacesTab) owns
	   styling for those surfaces now. */

	.users-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.loading-msg {
		color: var(--text-muted);
		padding: var(--space-6) 0;
		text-align: center;
		font-size: 0.9rem;
	}
	.error-msg {
		color: var(--accent-red);
		padding: var(--space-6);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.error-msg p {
		margin: 0;
		font-size: 0.85rem;
	}
</style>
