<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { adminFetch, adminPatch, adminPost, formatDate, adminStore, type AdminUser } from '$lib/stores/admin.svelte';
	import UserModal from '$lib/components/admin/UserModal.svelte';

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

	// Modal state. T1550 lands the shell; T1551–T1554 fill the tabs;
	// T1555 then deletes the inline-expand block below. During T1550–T1554
	// both UIs coexist — clicking a row opens the modal AND toggles the
	// inline expand. This is the explicit broken state from the plan.
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
	// Called by the modal's Settings tab after any save. Mirror the same
	// merge pattern used by the inline-expand handlers so both UIs see
	// the same row state until T1555 removes the inline expand.
	// PLAN-1542 / TASK-1551.
	function onModalUserUpdated(updated: AdminUser) {
		users = users.map((u) => (u.id === updated.id ? { ...u, ...updated } : u));
		if (modalUser && modalUser.id === updated.id) modalUser = { ...modalUser, ...updated };
	}
	let selectedId = $state<string | null>(null);
	let editPlan = $state('free');
	let editRole = $state('member');
	// Plan override fields — empty string means "use plan default", a number overrides it.
	// storage_bytes is omitted from this list because it has its own
	// dedicated input (parses "500 MB" / "10 GB" shorthand into a byte
	// count rather than forcing the admin to type 536870912 by hand).
	const overrideFields = [
		{ key: 'workspaces', label: 'Workspaces', hint: 'Max workspaces owned' },
		{ key: 'items_per_workspace', label: 'Items per workspace', hint: 'Max items in each workspace' },
		{ key: 'members_per_workspace', label: 'Members per workspace', hint: 'Max members per workspace' },
		{ key: 'api_tokens', label: 'API tokens', hint: 'Max API tokens' },
		{ key: 'webhooks', label: 'Webhooks', hint: 'Max webhooks per workspace' },
	];
	// Keys the UI explicitly knows how to render (overrideFields plus
	// the dedicated storage_bytes input). Anything else is preserved
	// as-is in extraOverrides so an unrelated override key set via the
	// API doesn't get clobbered when an admin saves the structured form.
	const overrideFieldKeys = new Set([...overrideFields.map(f => f.key), 'storage_bytes']);
	let editOverrides = $state<Record<string, string>>({});
	// Storage override input: text rather than number so the admin can
	// type "10 GB" / "500MB" / "-1" (unlimited) / "" (clear, falls back
	// to plan default). Parsed by parseStorageInput on save.
	let editStorageOverride = $state('');
	let storageOverrideError = $state('');
	let extraOverrides = $state<Record<string, number>>({});
	let saving = $state(false);
	let saveMsg = $state('');
	let loading = $state(true);
	let error = $state('');
	let roleConfirm = $state(false);
	let roleSaving = $state(false);
	let roleMsg = $state('');
	let resetConfirm = $state(false);
	let resetSaving = $state(false);
	let resetResult = $state<{ method: string; temp_password?: string; message: string } | null>(null);
	let resetError = $state('');
	let disableConfirm = $state(false);
	let disableSaving = $state(false);
	let disableMsg = $state('');
	let userWorkspaces = $state<{ workspace_name: string; workspace_slug: string; owner_username: string; role: string; joined_at: string }[]>([]);
	let workspacesLoading = $state(false);

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

	function selectUser(u: AdminUser) {
		if (selectedId === u.id) {
			selectedId = null;
			return;
		}
		selectedId = u.id;
		editPlan = u.plan || 'free';
		editRole = u.role || 'member';
		// plan_overrides is a raw JSON string from the API. Parse it
		// once here; downstream code reads keys / Object.entries on
		// the resulting object. Tolerant of malformed JSON (returns
		// {} instead of throwing) so a corrupt row doesn't lock the
		// user out of the admin form — the save path will overwrite
		// it cleanly.
		const ov = parsePlanOverrides(u.plan_overrides);
		editOverrides = {};
		extraOverrides = {};
		for (const f of overrideFields) {
			editOverrides[f.key] = f.key in ov ? String(ov[f.key]) : '';
		}
		// storage_bytes has its own dedicated input. Format the raw
		// byte count as human-readable so an admin who already set
		// "10 GB" doesn't see "10737418240" on the next visit.
		editStorageOverride =
			'storage_bytes' in ov ? formatStorageBytes(ov.storage_bytes) : '';
		storageOverrideError = '';
		// Preserve any override keys not in our UI fields
		for (const [k, v] of Object.entries(ov)) {
			if (!overrideFieldKeys.has(k)) {
				extraOverrides[k] = v;
			}
		}
		saveMsg = '';
		roleConfirm = false;
		roleMsg = '';
		resetConfirm = false;
		resetResult = null;
		resetError = '';
		disableConfirm = false;
		disableMsg = '';
		userWorkspaces = [];
		loadUserWorkspaces(u.id);
	}

	async function loadUserWorkspaces(userId: string) {
		workspacesLoading = true;
		try {
			const result = await adminFetch(`/admin/users/${userId}/workspaces`);
			if (selectedId === userId) {
				userWorkspaces = result.workspaces ?? [];
			}
		} catch {
			if (selectedId === userId) {
				userWorkspaces = [];
			}
		} finally {
			if (selectedId === userId) {
				workspacesLoading = false;
			}
		}
	}

	function selectedUser(): AdminUser | undefined {
		return users.find((u) => u.id === selectedId);
	}

	function roleAction(): string {
		const user = selectedUser();
		if (!user) return '';
		return editRole === 'admin' ? 'Promote' : 'Demote';
	}

	async function changeRole() {
		const userId = selectedId;
		if (!userId) return;
		roleSaving = true;
		roleMsg = '';
		try {
			await adminPatch(`/admin/users/${userId}`, { role: editRole });
			const updated = await adminFetch(`/admin/users/${userId}`);
			users = users.map((u) => (u.id === userId ? { ...u, ...updated } : u));
			roleMsg = 'Role updated';
			roleConfirm = false;
		} catch (e) {
			roleMsg = e instanceof Error ? e.message : 'Role change failed';
		} finally {
			roleSaving = false;
		}
	}

	async function resetPassword() {
		const userId = selectedId;
		if (!userId) return;
		resetSaving = true;
		resetError = '';
		resetResult = null;
		try {
			const result = await adminPost(`/admin/users/${userId}/reset-password`);
			resetResult = result;
		} catch (e) {
			resetError = e instanceof Error ? e.message : 'Password reset failed';
		} finally {
			resetSaving = false;
			resetConfirm = false;
		}
	}

	async function toggleDisable() {
		const userId = selectedId;
		if (!userId) return;
		const user = selectedUser();
		if (!user) return;
		const wasDisabled = !!user.disabled_at;
		disableSaving = true;
		disableMsg = '';
		try {
			const action = wasDisabled ? 'enable' : 'disable';
			await adminPost(`/admin/users/${userId}/${action}`);
			const updated = await adminFetch(`/admin/users/${userId}`);
			users = users.map((u) => (u.id === userId ? { ...u, ...updated } : u));
			disableMsg = wasDisabled ? 'User re-enabled' : 'User disabled';
			disableConfirm = false;
		} catch (e) {
			disableMsg = e instanceof Error ? e.message : 'Action failed';
		} finally {
			disableSaving = false;
		}
	}

	async function saveUser() {
		if (!selectedId) return;
		saving = true;
		saveMsg = '';
		storageOverrideError = '';
		try {
			// Build plan_overrides JSON from structured fields, preserving unknown keys
			const overrides: Record<string, number> = { ...extraOverrides };
			for (const f of overrideFields) {
				const val = editOverrides[f.key]?.trim();
				if (val !== '' && val !== undefined) {
					const num = Number(val);
					if (isNaN(num) || !Number.isInteger(num)) {
						saveMsg = `"${f.label}" must be a whole number`;
						saving = false;
						return;
					}
					overrides[f.key] = num;
				}
			}
			// Storage quota override: empty input clears the override
			// (falls back to plan default); otherwise parse "500 MB" /
			// "10 GB" / "-1" (unlimited) / a raw byte count.
			const rawStorage = editStorageOverride.trim();
			if (rawStorage !== '') {
				const parsed = parseStorageInput(rawStorage);
				if (parsed === null) {
					storageOverrideError =
						'Storage override must be bytes (1024), shorthand (500 MB / 10 GB), or -1 for unlimited';
					saveMsg = '';
					saving = false;
					return;
				}
				overrides['storage_bytes'] = parsed;
			}
			// Empty overrides object → send "" so the backend's
			// nil-vs-non-nil pointer logic actually runs the update
			// path (SetUserPlanOverrides("") clears the column).
			// Sending null would JSON-decode to a nil *string and
			// the handler would skip the update entirely — clearing
			// the form would silently no-op. Codex caught this on
			// PR #304 round 1.
			const overridesJSON =
				Object.keys(overrides).length > 0 ? JSON.stringify(overrides) : '';
			await adminPatch(`/admin/users/${selectedId}`, {
				plan: editPlan,
				plan_overrides: overridesJSON
			});
			const updated = await adminFetch(`/admin/users/${selectedId}`);
			users = users.map((u) => (u.id === selectedId ? { ...u, ...updated } : u));
			saveMsg = 'Saved';
		} catch (e) {
			saveMsg = e instanceof Error ? e.message : 'Save failed';
		} finally {
			saving = false;
		}
	}

	// Reset clears the storage override field. The actual save still
	// runs through saveUser so an admin can review the change before
	// committing — matches the flow for the other override fields.
	function resetStorageOverride() {
		editStorageOverride = '';
		storageOverrideError = '';
	}

	// parsePlanOverrides decodes the raw JSON string the API returns
	// for `plan_overrides` into a Record<string, number>. Tolerant of
	// malformed / null / empty input so the UI doesn't crash on a
	// row that somehow has bad JSON in the column.
	function parsePlanOverrides(raw: unknown): Record<string, number> {
		if (raw == null || raw === '') return {};
		// Defensive: if a future API revision starts returning the
		// decoded object directly, accept it without breaking.
		if (typeof raw === 'object') return raw as Record<string, number>;
		if (typeof raw !== 'string') return {};
		try {
			const parsed = JSON.parse(raw);
			if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
				return parsed as Record<string, number>;
			}
		} catch {
			/* fall through to empty */
		}
		return {};
	}

	// parseStorageInput accepts (in order of priority):
	//   • "-1" → -1 (unlimited)
	//   • Raw byte count: "1024", "536870912"
	//   • IEC shorthand: "500 KB", "10MB", "10GB", "1.5gb"
	// Returns the byte count as an integer, or null on parse failure.
	// Mirrors the server-side hardcodedLimit byte values (KB=1024 etc).
	function parseStorageInput(input: string): number | null {
		const trimmed = input.trim();
		if (trimmed === '-1') return -1;
		// Allow plain integers up to JS safe-integer range.
		if (/^\d+$/.test(trimmed)) {
			const n = Number(trimmed);
			return Number.isSafeInteger(n) && n >= 0 ? n : null;
		}
		// Shorthand: <number> [unit]. Unit is optional (treated as B).
		const m = trimmed.match(/^(\d+(?:\.\d+)?)\s*([KMGT]?B?)$/i);
		if (!m) return null;
		const value = parseFloat(m[1]);
		if (isNaN(value) || value < 0) return null;
		const unit = m[2].toUpperCase().replace(/B$/, '');
		const mult: Record<string, number> = {
			'': 1,
			'K': 1024,
			'M': 1024 * 1024,
			'G': 1024 * 1024 * 1024,
			'T': 1024 * 1024 * 1024 * 1024,
		};
		const factor = mult[unit];
		if (factor === undefined) return null;
		const bytes = Math.round(value * factor);
		return Number.isSafeInteger(bytes) ? bytes : null;
	}

	// formatStorageBytes is the inverse of parseStorageInput: render a
	// raw byte count as the admin would have typed it. -1 → "-1"
	// (unlimited); other values pick the largest unit that keeps the
	// number readable.
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

	// storageOverridePreview renders the current input as the admin
	// would see it after parsing — gives instant feedback on whether
	// "10 gb" was understood as 10 GiB. Empty/invalid input → "" so
	// the helper text doesn't flicker as the admin types.
	let storageOverridePreview = $derived.by(() => {
		const trimmed = editStorageOverride.trim();
		if (trimmed === '') return '';
		const parsed = parseStorageInput(trimmed);
		if (parsed === null) return 'Invalid input';
		if (parsed === -1) return 'Unlimited';
		return `= ${formatStorageBytes(parsed)} (${parsed.toLocaleString()} bytes)`;
	});

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
							class:selected={selectedId === user.id}
							class:disabled-row={!!user.disabled_at}
							onclick={() => { selectUser(user); openUserModal(user); }}
						>
							<td>
								{user.name || user.username}
								<!-- Status pill replaces the legacy "disabled" badge.
								     "active" is the common case; omit the pill to avoid
								     visual noise. Other states call out problems. -->
								{#if user.status && user.status !== 'active'}
									<span class="badge status-{user.status}">{user.status}</span>
								{/if}
							</td>
							<td
								><span class="badge" class:admin={user.role === 'admin'}
									>{user.role || 'member'}</span
								></td
							>
							<td class="num-cell">{user.workspace_count ?? 0}</td>
							<td>{user.email}</td>
							{#if adminStore.stats?.cloud_mode}
								<td
									><span class="badge" class:pro={user.plan === 'pro'}
										>{user.plan || 'free'}</span
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
						{#if selectedId === user.id}
							<tr class="edit-row">
								<td colspan={adminStore.stats?.cloud_mode ? 9 : 8}>
									<div class="edit-panel">
										<div class="edit-field">
											<label for="edit-role">Role</label>
											<div class="role-row">
												<select id="edit-role" bind:value={editRole}>
													<option value="member">member</option>
													<option value="admin">admin</option>
												</select>
												{#if editRole !== (user.role || 'member')}
													{#if !roleConfirm}
														<button
															class="btn role-btn"
															onclick={() => {
																roleConfirm = true;
																roleMsg = '';
															}}
														>
															Change Role
														</button>
													{:else}
														<div class="role-confirm">
															<span class="role-confirm-msg">
																{roleAction()}
																<strong>{user.name || user.username}</strong> to {editRole}?
															</span>
															<button
																class="btn primary"
																onclick={changeRole}
																disabled={roleSaving}
															>
																{roleSaving ? 'Saving...' : 'Confirm'}
															</button>
															<button
																class="btn"
																onclick={() => {
																	roleConfirm = false;
																	roleMsg = '';
																}}
															>
																Cancel
															</button>
														</div>
													{/if}
												{/if}
												{#if roleMsg}<span class="save-msg">{roleMsg}</span>{/if}
											</div>
										</div>
										<div class="edit-field">
											<span class="field-label">Password</span>
											<div class="role-row">
												{#if !resetConfirm && !resetResult}
													<button class="btn role-btn" onclick={() => { resetConfirm = true; resetError = ''; }}>
														Reset Password
													</button>
												{/if}
												{#if resetConfirm && !resetResult}
													<div class="role-confirm">
														<span class="role-confirm-msg">
															Send password reset for <strong>{user.name || user.username}</strong>?
														</span>
														<button class="btn primary" onclick={resetPassword} disabled={resetSaving}>
															{resetSaving ? 'Resetting...' : 'Confirm'}
														</button>
														<button class="btn" onclick={() => { resetConfirm = false; }}>
															Cancel
														</button>
													</div>
												{/if}
												{#if resetResult}
													<div class="reset-result">
														{#if resetResult.method === 'email'}
															<span class="reset-success">{resetResult.message}</span>
														{:else}
															<div class="temp-password-result">
																<span class="reset-success">Temporary password generated:</span>
																<code class="temp-password">{resetResult.temp_password}</code>
																<span class="reset-note">User's sessions have been invalidated. Share this password securely.</span>
															</div>
														{/if}
													</div>
												{/if}
												{#if resetError}<span class="save-msg" style="color: #ef4444">{resetError}</span>{/if}
											</div>
										</div>
										<div class="edit-field">
											<span class="field-label">Account Status</span>
											<div class="role-row">
												{#if !disableConfirm}
													<button
														class="btn role-btn"
														class:danger={!user.disabled_at}
														onclick={() => { disableConfirm = true; disableMsg = ''; }}
													>
														{user.disabled_at ? 'Enable Account' : 'Disable Account'}
													</button>
												{:else}
													<div class="role-confirm">
														<span class="role-confirm-msg">
															{#if user.disabled_at}
																Re-enable <strong>{user.name || user.username}</strong>?
															{:else}
																Disable <strong>{user.name || user.username}</strong>? Their sessions will be invalidated.
															{/if}
														</span>
														<button
															class="btn primary"
															class:danger={!user.disabled_at}
															onclick={toggleDisable}
															disabled={disableSaving}
														>
															{disableSaving ? 'Saving...' : 'Confirm'}
														</button>
														<button class="btn" onclick={() => { disableConfirm = false; }}>
															Cancel
														</button>
													</div>
												{/if}
												{#if disableMsg}<span class="save-msg">{disableMsg}</span>{/if}
											</div>
										</div>
										{#if adminStore.stats?.cloud_mode}
											<div class="edit-field">
												<label for="edit-plan">Plan</label>
												<select id="edit-plan" bind:value={editPlan}>
													<option value="free">free</option>
													<option value="pro">pro</option>
												</select>
											</div>
											<div class="edit-field">
												<span class="field-label">Plan overrides</span>
												<p class="field-hint">Override individual limits for this user. Leave blank to use plan defaults. Use -1 for unlimited.</p>
												<div class="overrides-grid">
													{#each overrideFields as field (field.key)}
														<div class="override-field">
															<label for="override-{field.key}">{field.label}</label>
															<input
																id="override-{field.key}"
																type="number"
																bind:value={editOverrides[field.key]}
																placeholder="default"
															/>
														</div>
													{/each}
												</div>
												<!-- Dedicated storage override row.
												     Storage is byte-counted, not row-counted, so a number
												     input forcing the admin to type 536870912 for 512MB
												     would be hostile. Accept "10 GB" / "500MB" / "-1"
												     (unlimited) / a raw byte count and show the parsed
												     value live.
												     Effective limit cascades:
												       per-user override → platform setting → hardcoded plan default.
												     The Settings → Storage page (TASK-882) reflects the
												     effective limit immediately after save.
												-->
												<div class="storage-override-row">
													<div class="override-field storage-override-field">
														<label for="override-storage_bytes">Storage quota override</label>
														<div class="storage-input-group">
															<input
																id="override-storage_bytes"
																type="text"
																class="storage-input"
																bind:value={editStorageOverride}
																placeholder="default (e.g. 10 GB, 500 MB, -1 for unlimited)"
															/>
															<button
																type="button"
																class="btn btn-small reset-storage-btn"
																onclick={resetStorageOverride}
																disabled={editStorageOverride.trim() === ''}
															>
																Reset to plan default
															</button>
														</div>
														{#if storageOverridePreview}
															<p
																class="storage-preview"
																class:storage-preview-error={storageOverridePreview === 'Invalid input'}
															>
																{storageOverridePreview}
															</p>
														{/if}
														{#if storageOverrideError}
															<p class="storage-error">{storageOverrideError}</p>
														{/if}
													</div>
												</div>
											</div>
											<div class="edit-actions">
												<button class="btn primary" onclick={saveUser} disabled={saving}>
													{saving ? 'Saving...' : 'Save Plan'}
												</button>
												{#if saveMsg}<span class="save-msg">{saveMsg}</span>{/if}
											</div>
										{/if}
										<div class="edit-field">
											<span class="field-label">Workspaces</span>
											{#if workspacesLoading}
												<span class="ws-loading">Loading...</span>
											{:else if userWorkspaces.length === 0}
												<span class="ws-empty">No workspace memberships</span>
											{:else}
												<div class="ws-list">
													{#each userWorkspaces as ws}
														<div class="ws-item">
															<a class="ws-name" href="/{ws.owner_username}/{ws.workspace_slug}">{ws.workspace_name}</a>
															<span class="badge" class:owner={ws.role === 'owner'}>{ws.role}</span>
															<span class="ws-joined">joined {formatDate(ws.joined_at)}</span>
														</div>
													{/each}
												</div>
											{/if}
										</div>
									</div>
								</td>
							</tr>
						{/if}
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

<!-- Per-user detail modal — shell only as of TASK-1550; tab content
     arrives across T1551–T1554, and T1555 deletes the parallel inline-
     expand block above. -->
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
	.btn.primary {
		background: var(--accent-blue);
		color: #fff;
		border-color: transparent;
	}
	.btn.primary:hover {
		opacity: 0.9;
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
	.user-row.selected {
		background: var(--bg-tertiary);
	}
	.date-cell {
		white-space: nowrap;
	}
	.date-cell.muted {
		color: var(--text-muted);
		font-size: 0.8rem;
	}
	.badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
	}
	.badge.pro {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	.badge.admin {
		background: color-mix(in srgb, var(--accent-orange, #f59e0b) 15%, transparent);
		color: var(--accent-orange, #f59e0b);
	}
	.badge.disabled {
		background: color-mix(in srgb, #ef4444 15%, transparent);
		color: #ef4444;
		margin-left: var(--space-2);
	}
	/* Status pill — server-side computed (disabled / no-workspace / inactive /
	   active). "active" never renders; the other three call out something
	   actionable. PLAN-1542 / TASK-1548. */
	.badge.status-disabled {
		background: color-mix(in srgb, #ef4444 15%, transparent);
		color: #ef4444;
		margin-left: var(--space-2);
	}
	.badge.status-no-workspace {
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
		margin-left: var(--space-2);
	}
	.badge.status-inactive {
		background: color-mix(in srgb, #f59e0b 15%, transparent);
		color: #f59e0b;
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
	.date-cell.write-cold { color: #ef4444; }
	.date-cell.write-never {
		color: var(--text-muted);
		font-style: italic;
	}
	.disabled-row {
		opacity: 0.6;
	}
	.btn.danger {
		border-color: #ef4444;
		color: #ef4444;
	}
	.btn.danger:hover {
		background: color-mix(in srgb, #ef4444 10%, transparent);
	}
	.btn.primary.danger {
		background: #ef4444;
		color: #fff;
		border-color: transparent;
	}
	.btn.primary.danger:hover {
		opacity: 0.9;
	}

	/* Edit panel */
	.edit-row td {
		padding: 0;
		border-bottom: 1px solid var(--border);
	}
	.edit-panel {
		padding: var(--space-4) var(--space-3);
		background: var(--bg-tertiary);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.edit-field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.edit-field label,
	.edit-field .field-label {
		font-size: 0.8rem;
		color: var(--text-muted);
		font-weight: 500;
	}
	.edit-field select,
	.edit-field textarea,
	.edit-field input[type='number'] {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		font-family: inherit;
	}
	.edit-field textarea {
		resize: vertical;
		font-family: monospace;
		font-size: 0.8rem;
	}
	.field-hint {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin: 0;
	}
	.overrides-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-2) var(--space-4);
	}
	.override-field {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.override-field label {
		font-size: 0.75rem;
		color: var(--text-muted);
		font-weight: 500;
	}
	.override-field input {
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8rem;
		font-family: var(--font-mono, monospace);
		outline: none;
	}
	.override-field input:focus {
		border-color: var(--accent-blue);
	}
	.override-field input::placeholder {
		color: var(--text-muted);
		font-family: inherit;
	}
	/* Storage override row: dedicated row outside the 2-column grid
	   because it has unique controls (preview, reset button, units). */
	.storage-override-row {
		margin-top: var(--space-3);
		padding-top: var(--space-3);
		border-top: 1px solid var(--border);
	}
	.storage-override-field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.storage-input-group {
		display: flex;
		gap: var(--space-2);
		align-items: stretch;
		flex-wrap: wrap;
	}
	.storage-input {
		flex: 1;
		min-width: 220px;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.85rem;
		font-family: var(--font-mono, monospace);
		outline: none;
	}
	.storage-input:focus {
		border-color: var(--accent-blue);
	}
	.btn-small {
		padding: var(--space-1) var(--space-3);
		font-size: 0.78rem;
	}
	.reset-storage-btn {
		white-space: nowrap;
	}
	.storage-preview {
		margin: 0;
		font-size: 0.78rem;
		color: var(--text-muted);
		font-family: var(--font-mono, monospace);
	}
	.storage-preview-error {
		color: #ef4444;
	}
	.storage-error {
		margin: 0;
		font-size: 0.82rem;
		color: #ef4444;
	}
	.edit-actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}
	.save-msg {
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.role-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.role-btn {
		font-size: 0.8rem;
		padding: var(--space-1) var(--space-3);
	}
	.role-confirm {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.role-confirm-msg {
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.role-confirm .btn {
		font-size: 0.8rem;
		padding: var(--space-1) var(--space-3);
	}
	.reset-result {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.reset-success {
		font-size: 0.8rem;
		color: var(--accent-green, #22c55e);
	}
	.temp-password-result {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.temp-password {
		font-family: monospace;
		font-size: 0.85rem;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		user-select: all;
	}
	.reset-note {
		font-size: 0.75rem;
		color: var(--text-muted);
	}

	.ws-loading,
	.ws-empty {
		font-size: 0.8rem;
		color: var(--text-muted);
	}
	.ws-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.ws-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8rem;
		padding: var(--space-1) 0;
	}
	.ws-name {
		color: var(--accent-blue);
		text-decoration: none;
		font-weight: 500;
	}
	.ws-name:hover {
		text-decoration: underline;
	}
	.badge.owner {
		background: color-mix(in srgb, var(--accent-green, #22c55e) 15%, transparent);
		color: var(--accent-green, #22c55e);
	}
	.ws-joined {
		color: var(--text-muted);
		font-size: 0.75rem;
	}

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
		color: #ef4444;
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
