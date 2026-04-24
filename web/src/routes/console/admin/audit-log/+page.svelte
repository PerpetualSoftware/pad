<script lang="ts">
	import { onMount } from 'svelte';
	import { adminFetch } from '$lib/stores/admin.svelte';

	interface Activity {
		id: string;
		workspace_id?: string;
		document_id?: string;
		action: string;
		actor: string;
		source: string;
		metadata?: string;
		user_id?: string;
		ip_address?: string;
		user_agent?: string;
		created_at: string;
		actor_name?: string;
	}

	const ACTION_TYPES = [
		'login', 'login_failed', 'logout', 'bootstrap', 'register',
		'password_changed', 'password_reset', 'token_created', 'token_revoked',
		'token_rotated', 'totp_enabled', 'totp_disabled', 'member_invited',
		'member_removed', 'role_changed', 'settings_changed', 'oauth_login',
		'oauth_login_failed', 'plan_changed', 'password_reset_by_admin',
		'user_disabled', 'user_enabled', 'account_deleted'
	];

	const ACTION_LABELS: Record<string, string> = {
		login: 'Login',
		login_failed: 'Login Failed',
		logout: 'Logout',
		bootstrap: 'Bootstrap',
		register: 'Register',
		password_changed: 'Password Changed',
		password_reset: 'Password Reset',
		token_created: 'Token Created',
		token_revoked: 'Token Revoked',
		token_rotated: 'Token Rotated',
		totp_enabled: 'TOTP Enabled',
		totp_disabled: 'TOTP Disabled',
		member_invited: 'Member Invited',
		member_removed: 'Member Removed',
		role_changed: 'Role Changed',
		settings_changed: 'Settings Changed',
		oauth_login: 'OAuth Login',
		oauth_login_failed: 'OAuth Login Failed',
		plan_changed: 'Plan Changed',
		password_reset_by_admin: 'Password Reset (Admin)',
		user_disabled: 'User Disabled',
		user_enabled: 'User Enabled',
		account_deleted: 'Account Deleted'
	};

	const LIMIT = 50;

	let entries = $state<Activity[]>([]);
	let loading = $state(true);
	let error = $state('');
	let filterAction = $state('');
	let filterDays = $state(30);
	let offset = $state(0);
	let hasMore = $state(false);
	let loadingMore = $state(false);
	let requestCounter = 0;

	function formatAction(action: string): string {
		return ACTION_LABELS[action] ?? action.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	function actionColor(action: string): string {
		if (['login', 'register', 'oauth_login'].includes(action)) return 'success';
		if (['login_failed', 'oauth_login_failed', 'user_disabled', 'account_deleted'].includes(action)) return 'danger';
		if (['password_reset_by_admin', 'user_enabled', 'plan_changed', 'role_changed', 'settings_changed'].includes(action)) return 'warning';
		return '';
	}

	function relativeTime(dateStr: string): string {
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		const diffMs = now - then;
		const diffSec = Math.floor(diffMs / 1000);
		const diffMin = Math.floor(diffSec / 60);
		const diffHr = Math.floor(diffMin / 60);
		const diffDay = Math.floor(diffHr / 24);

		if (diffSec < 60) return 'Just now';
		if (diffMin < 60) return `${diffMin}m ago`;
		if (diffHr < 24) return `${diffHr}h ago`;
		if (diffDay < 30) return `${diffDay}d ago`;

		return new Date(dateStr).toLocaleDateString('en-US', {
			month: 'short',
			day: 'numeric',
			year: 'numeric'
		});
	}

	function formatMetadata(metadata: string | undefined, action: string): string {
		if (!metadata) return '\u2014';
		try {
			const data = JSON.parse(metadata);
			switch (action) {
				case 'role_changed':
					if (data.old_role && data.new_role) return `${data.old_role} \u2192 ${data.new_role}`;
					break;
				case 'member_invited':
					if (data.email) return `${data.email}${data.role ? ` (${data.role})` : ''}`;
					break;
				case 'member_removed':
					if (data.email) return data.email;
					break;
				case 'plan_changed':
					if (data.old_plan && data.new_plan) return `${data.old_plan} \u2192 ${data.new_plan}`;
					break;
				case 'settings_changed':
					if (data.setting) return data.setting;
					if (data.keys) return Array.isArray(data.keys) ? data.keys.join(', ') : String(data.keys);
					break;
				case 'token_created':
				case 'token_revoked':
					if (data.name) return data.name;
					break;
				case 'login_failed':
				case 'oauth_login_failed':
					if (data.reason) return data.reason;
					if (data.email) return data.email;
					break;
				case 'register':
					if (data.email) return data.email;
					break;
				case 'payment_failed_email_sent': {
					// Differentiate operationally distinct outcomes: a genuine delivery
					// failure (Maileroo 5xx) should not read the same as a pre-send
					// skip (unknown customer, no email on file, provider not wired up).
					// Surface admin_actor_id when present so manual operator calls
					// show which admin triggered the send; sidecar calls omit that
					// field and the User column's target user tells the story.
					const cus = data.stripe_customer_id ? ` (${data.stripe_customer_id})` : '';
					const by = data.admin_actor_id ? ` by admin:${data.admin_actor_id}` : '';
					if (data.sent === 'true') return `sent${cus}${by}`;
					if (data.reason === 'send_failed') return `send failed${cus}${by}`;
					if (data.reason) return `skipped (${data.reason})${cus}${by}`;
					break;
				}
				default:
					break;
			}
			// Fallback: show all keys briefly
			const keys = Object.keys(data);
			if (keys.length === 0) return '\u2014';
			const parts = keys.slice(0, 3).map((k) => `${k}: ${data[k]}`);
			return parts.join(', ');
		} catch {
			return '\u2014';
		}
	}

	function displayUser(entry: Activity): string {
		if (entry.actor_name) return entry.actor_name;
		if (entry.actor === 'system') return 'System';
		if (entry.user_id) return entry.user_id.length > 12 ? entry.user_id.slice(0, 12) + '\u2026' : entry.user_id;
		return 'Unknown';
	}

	async function loadEntries(append = false) {
		const thisRequest = ++requestCounter;

		if (append) {
			loadingMore = true;
		} else {
			loading = true;
			error = '';
			offset = 0;
		}

		try {
			const params = new URLSearchParams();
			if (filterAction) params.set('action', filterAction);
			params.set('days', String(filterDays));
			params.set('limit', String(LIMIT));
			params.set('offset', String(append ? offset : 0));
			const result = await adminFetch(`/audit-log?${params}`);

			// Discard stale responses from superseded requests
			if (thisRequest !== requestCounter) return;

			const items: Activity[] = Array.isArray(result) ? result : [];
			if (append) {
				entries = [...entries, ...items];
			} else {
				entries = items;
			}
			hasMore = items.length >= LIMIT;
			if (append) {
				offset += items.length;
			} else {
				offset = items.length;
			}
		} catch (e) {
			if (thisRequest !== requestCounter) return;
			if (!append) {
				error = e instanceof Error ? e.message : 'Failed to load audit log';
			}
		} finally {
			if (thisRequest === requestCounter) {
				loading = false;
				loadingMore = false;
			}
		}
	}

	function applyFilters() {
		loadEntries(false);
	}

	function loadMore() {
		loadEntries(true);
	}

	onMount(() => {
		loadEntries();
	});
</script>

<div class="audit-page">
	<div class="filter-row">
		<select class="filter-select" bind:value={filterAction}>
			<option value="">All actions</option>
			{#each ACTION_TYPES as action (action)}
				<option value={action}>{formatAction(action)}</option>
			{/each}
		</select>
		<select class="filter-select" bind:value={filterDays}>
			<option value={7}>Last 7 days</option>
			<option value={14}>Last 14 days</option>
			<option value={30}>Last 30 days</option>
			<option value={60}>Last 60 days</option>
			<option value={90}>Last 90 days</option>
		</select>
		<button class="btn" onclick={applyFilters}>Filter</button>
	</div>

	{#if loading}
		<div class="loading-msg">Loading audit log...</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={() => loadEntries()}>Retry</button>
		</div>
	{:else if entries.length === 0}
		<div class="empty-state">
			<p class="empty-title">No audit log entries</p>
			<p class="empty-desc">No events match the selected filters.</p>
		</div>
	{:else}
		<div class="table-wrap">
			<table class="table">
				<thead>
					<tr>
						<th>Time</th>
						<th>User</th>
						<th>Action</th>
						<th>Details</th>
						<th>IP Address</th>
					</tr>
				</thead>
				<tbody>
					{#each entries as entry (entry.id)}
						<tr>
							<td class="time-cell" title={new Date(entry.created_at).toISOString()}>
								{relativeTime(entry.created_at)}
							</td>
							<td>{displayUser(entry)}</td>
							<td>
								<span class="badge {actionColor(entry.action)}">
									{formatAction(entry.action)}
								</span>
							</td>
							<td class="details-cell">{formatMetadata(entry.metadata, entry.action)}</td>
							<td class="ip-cell">{entry.ip_address || '\u2014'}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>

		<div class="load-more">
			{#if hasMore}
				<button class="btn" onclick={loadMore} disabled={loadingMore}>
					{loadingMore ? 'Loading...' : 'Load more'}
				</button>
			{:else}
				<span class="no-more">No more entries</span>
			{/if}
		</div>
	{/if}
</div>

<style>
	.audit-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	/* Filter row */
	.filter-row {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.filter-select {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		outline: none;
	}
	.filter-select:focus {
		border-color: var(--accent-blue);
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
	.time-cell {
		white-space: nowrap;
	}
	.details-cell {
		max-width: 300px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.ip-cell {
		font-family: 'SF Mono', 'Fira Code', 'Fira Mono', Menlo, Consolas, monospace;
		font-size: 0.8rem;
		white-space: nowrap;
	}

	/* Badge */
	.badge {
		display: inline-block;
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
		white-space: nowrap;
	}
	.badge.success {
		background: color-mix(in srgb, var(--accent-green, #22c55e) 15%, transparent);
		color: var(--accent-green, #22c55e);
	}
	.badge.danger {
		background: color-mix(in srgb, #ef4444 15%, transparent);
		color: #ef4444;
	}
	.badge.warning {
		background: color-mix(in srgb, #f59e0b 15%, transparent);
		color: #f59e0b;
	}

	/* Load more */
	.load-more {
		display: flex;
		justify-content: center;
		padding: var(--space-4) 0;
	}
	.no-more {
		font-size: 0.8rem;
		color: var(--text-muted);
	}

	/* Empty state */
	.empty-state {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		padding: var(--space-10) 0;
		gap: var(--space-2);
		text-align: center;
	}
	.empty-title {
		font-size: 0.95rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}
	.empty-desc {
		font-size: 0.85rem;
		color: var(--text-muted);
		margin: 0;
	}

	/* States */
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
