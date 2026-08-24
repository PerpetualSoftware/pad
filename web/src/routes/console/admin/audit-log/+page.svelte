<script lang="ts">
	import { onMount } from 'svelte';
	import { adminFetch } from '$lib/stores/admin.svelte';
	import { agentNameOf } from '$lib/utils/agentActor';
	import Chip from '$lib/components/common/Chip.svelte';
	import EmptyState from '$lib/components/common/EmptyState.svelte';

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
		'user_disabled', 'user_enabled', 'account_deleted',
		'payment_failed_email_sent'
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
		account_deleted: 'Account Deleted',
		payment_failed_email_sent: 'Payment Failed Email'
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

	// Chip color for an audit action — success/danger/warning semantics
	// mapped onto the shared accent tokens (TASK-2292).
	function actionColor(action: string): string {
		if (['login', 'register', 'oauth_login'].includes(action)) return 'var(--accent-green)';
		if (['login_failed', 'oauth_login_failed', 'user_disabled', 'account_deleted'].includes(action)) return 'var(--accent-red)';
		if (['password_reset_by_admin', 'user_enabled', 'plan_changed', 'role_changed', 'settings_changed'].includes(action)) return 'var(--accent-amber)';
		return 'var(--accent-gray)';
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

	/** One parse per row, shared by every consumer below — the row's metadata
	 *  is read by both the User and Details columns, and parsing it twice on a
	 *  table that grows through "Load more" is pure waste. Returns null when
	 *  there is nothing parseable, which both callers treat as "no data". */
	function parseMetadata(metadata: string | undefined): Record<string, any> | null {
		if (!metadata) return null;
		try {
			return JSON.parse(metadata);
		} catch {
			return null;
		}
	}

	function formatMetadata(data: Record<string, any> | null, action: string): string {
		if (!data) return '\u2014';
		// The try still wraps the FORMATTERS, not just the parse it used to
		// share with. Hoisting the parse out narrowed this guard to nothing,
		// and the branches below can genuinely throw on well-formed JSON —
		// `String(data.keys)` on `{"keys":{"toString":null}}` cannot convert
		// to a primitive. That used to render an em dash; it must not take
		// the whole audit page down instead (codex round 6).
		try {
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

	// An agent's write is authenticated with a HUMAN's credentials, so
	// `actor_name` — joined from user_id — used to be the only thing this
	// column showed for it: the agent's work rendered under the name of the
	// person whose token it borrowed. Both facts are real and an audit
	// surface needs both, so they render together rather than one replacing
	// the other: the agent acted, that account is who it acted as. The name
	// is self-declared (see agentActor.ts) and this column is the last place
	// that should be implied otherwise, hence "via" rather than a merge.
	//
	// Returns the PARTS, never a joined string. The agent half is
	// attacker-chosen text and the human half is not, so building
	// `${agent} (via ${human})` would let a writer pick a name that forges the
	// construction (`admin (via root)`), or that carries a bidi control
	// character and visually reorders the suffix it was appended to — the
	// audited party editing how the audit reads. Kept apart, the template can
	// isolate each one, which bounds a hostile name to its own element instead
	// of letting it rewrite its neighbours (codex round 8).
	//
	// This is not a rendering quirk, it is ResolveAgentName's documented
	// attribution-honesty problem arriving through the renderer. That contract
	// (internal/cli/agent_identity.go, "WHAT THIS IS NOT") says the header
	// records honesty rather than identity, because the actor authors it. A
	// surface that COMPOSES with an authored value inherits that: it hands the
	// author influence over the parts they did not write. So the rule for any
	// future edit here — including a new column, a tooltip, an export or a
	// search-result summary — is that a self-declared value is a leaf, never a
	// fragment something else is built around.
	function displayUser(
		entry: Activity,
		meta: Record<string, any> | null
	): { agent?: string; account?: string } {
		const agent = entry.actor === 'agent' ? agentNameOf(meta) : undefined;
		if (agent) return { agent, account: entry.actor_name || undefined };
		if (entry.actor_name) return { account: entry.actor_name };
		if (entry.actor === 'system') return { account: 'System' };
		if (entry.user_id) {
			return {
				account:
					entry.user_id.length > 12 ? entry.user_id.slice(0, 12) + '\u2026' : entry.user_id
			};
		}
		return { account: 'Unknown' };
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
		<EmptyState title="No audit log entries" message="No events match the selected filters." />
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
						{@const meta = parseMetadata(entry.metadata)}
						{@const who = displayUser(entry, meta)}
						<tr>
							<td class="time-cell" title={new Date(entry.created_at).toISOString()}>
								{relativeTime(entry.created_at)}
							</td>
							<td class="user-cell">
								<!-- <bdi> per name: an agent's is self-declared text and may
								     contain bidi controls; isolation stops it reordering the
								     " (via " literal or the account name beside it. -->
								{#if who.agent}
									<bdi class="agent-name" title={who.agent}>{who.agent}</bdi>
									{#if who.account}<span class="via"
											>(via <bdi>{who.account}</bdi>)</span
										>{/if}
								{:else}<bdi>{who.account}</bdi>{/if}
							</td>
							<td>
								<Chip size="sm" color={actionColor(entry.action)}>
									{formatAction(entry.action)}
								</Chip>
							</td>
							<td class="details-cell">{formatMetadata(meta, entry.action)}</td>
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
	/* Bounded for the same reason .details-cell is: an agent name is arbitrary
	   client-supplied text, and one very long or combining-heavy name must not
	   be able to widen the table for everyone reading it. The full value stays
	   on the title attribute. */
	.user-cell {
		max-width: 260px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.agent-name {
		font-weight: 600;
	}
	/* Visually distinct from the self-declared half, so a name that spells out
	   "(via someone)" cannot pass itself off as this part of the cell. */
	.via {
		color: var(--text-muted);
		font-size: 0.85em;
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

	/* States */
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
