<!--
  Activity tab — chronological feed of activities originated by this user.
  Consumes GET /admin/users/{id}/activity (T1546) with offset pagination
  via the next_offset field.

  Items written + comments authored come through; admin actions where
  the user is the SUBJECT are server-filtered out (the endpoint scopes
  to user_id = X, which is the actor not the target). That's documented
  on T1546's PR and noted on this tab so admins know what they're
  looking at.

  PLAN-1542 / TASK-1554.
-->
<script lang="ts">
	import { adminFetch, type AdminUser } from '$lib/stores/admin.svelte';

	interface Props {
		user: AdminUser;
		active: boolean;
	}

	let { user, active }: Props = $props();

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
		ip_address?: string;
		user_agent?: string;
	}

	let events = $state<ActivityEvent[]>([]);
	let loading = $state(false);
	let loadingMore = $state(false);
	// loadError is the top-of-tab error used when the INITIAL fetch fails
	// and there's nothing to show. loadMoreError is a separate inline
	// message for append failures so an already-rendered feed doesn't
	// flip back to a "Retry" full-tab state (Codex review on PR #609).
	let loadError = $state('');
	let loadMoreError = $state('');
	let nextOffset = $state<number | null>(0);
	let fetchedForUserId = $state<string | null>(null);

	const PAGE_SIZE = 20;

	$effect(() => {
		if (active && user && fetchedForUserId !== user.id) {
			loadInitial();
		}
	});

	async function loadInitial() {
		const userId = user.id;
		// Claim this userId BEFORE the await so a re-trigger of the
		// gating effect (parent passes a new object ref with the same id)
		// can't race in and fire a duplicate fetch.
		fetchedForUserId = userId;
		loading = true;
		loadError = '';
		events = [];
		nextOffset = 0;
		try {
			const result = await fetchPage(userId, 0);
			if (user.id !== userId) return;
			events = result.events;
			nextOffset = result.next_offset;
		} catch (e) {
			if (user.id !== userId) return;
			loadError = e instanceof Error ? e.message : 'Failed to load activity';
			// fetchedForUserId stays set on failure. Clearing it while
			// the tab is still active would re-trigger the gating
			// $effect immediately and fire loadInitial() again — under
			// a persistent outage that's an infinite fetch loop.
			// Retry is explicit via the Retry button below.
		} finally {
			if (user.id === userId) loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || nextOffset === null) return;
		const userId = user.id;
		const offset = nextOffset;
		loadingMore = true;
		loadMoreError = '';
		try {
			const result = await fetchPage(userId, offset);
			if (user.id !== userId) return;
			events = [...events, ...result.events];
			nextOffset = result.next_offset;
		} catch (e) {
			if (user.id !== userId) return;
			// Append failure — keep the already-loaded feed visible and
			// surface an inline error near the Load more button so the
			// admin can retry without losing context (Codex review on
			// PR #609).
			loadMoreError = e instanceof Error ? e.message : 'Failed to load more activity';
		} finally {
			if (user.id === userId) loadingMore = false;
		}
	}

	async function fetchPage(
		userId: string,
		offset: number
	): Promise<{ events: ActivityEvent[]; next_offset: number | null }> {
		const params = new URLSearchParams({ limit: String(PAGE_SIZE), offset: String(offset) });
		const r = await adminFetch(`/admin/users/${userId}/activity?` + params.toString());
		return {
			events: (r.events ?? []) as ActivityEvent[],
			next_offset: typeof r.next_offset === 'number' ? r.next_offset : null
		};
	}

	// Action → icon. SVG-free for now; emoji glyphs are good enough as a
	// chronological scan aid. The Activity tab is a triage view, not a
	// design showpiece. Coverage: every action constant in
	// internal/models/activity.go that can land in this endpoint's user-
	// authored feed (PLAN-1542 / TASK-1554, expanded per Codex review on
	// PR #609).
	function iconFor(action: string): string {
		switch (action) {
			// Item / comment writes
			case 'created':
				return '📄';
			case 'updated':
				return '✏️';
			case 'commented':
				return '💬';
			case 'archived':
				return '🗄️';
			case 'restored':
				return '↩️';
			case 'moved':
				return '➡️';
			// Auth events
			case 'login':
				return '🔑';
			case 'login_failed':
				return '🚫';
			case 'logout':
				return '🚪';
			case 'register':
				return '🆕';
			case 'bootstrap':
				return '🌱';
			case 'password_changed':
			case 'password_reset':
				return '🔒';
			// API tokens
			case 'token_created':
			case 'token_revoked':
			case 'token_rotated':
				return '🎟️';
			// TOTP
			case 'totp_enabled':
			case 'totp_disabled':
				return '🔐';
			// OAuth
			case 'oauth_login':
			case 'oauth_login_failed':
				return '🔗';
			// Membership / role admin actions the user themselves authored
			case 'member_invited':
			case 'member_removed':
				return '👥';
			case 'role_changed':
				return '🛡️';
			case 'settings_changed':
				return '⚙️';
			// Admin-on-user audit events. When the actor is an admin
			// modifying another user, the row's user_id is the admin's;
			// so these CAN appear in this feed and need icons.
			case 'plan_changed':
			case 'plan_overrides_changed':
				return '💳';
			case 'password_reset_by_admin':
				return '🛠️';
			case 'user_disabled':
				return '⛔';
			case 'user_enabled':
				return '✅';
			case 'session_ip_changed':
				return '🌐';
			case 'account_deleted':
				return '❌';
			default:
				return '·';
		}
	}

	function describe(ev: ActivityEvent): string {
		switch (ev.action) {
			case 'created':
				return 'Created an item';
			case 'updated':
				return 'Updated an item';
			case 'commented':
				return 'Posted a comment';
			case 'archived':
				return 'Archived an item';
			case 'restored':
				return 'Restored an item';
			case 'moved':
				return 'Moved an item';
			case 'login':
				return 'Logged in';
			case 'login_failed':
				return 'Login failed';
			case 'logout':
				return 'Logged out';
			case 'register':
				return 'Registered account';
			case 'bootstrap':
				return 'First admin bootstrap';
			case 'password_changed':
				return 'Changed password';
			case 'password_reset':
				return 'Reset password';
			case 'token_created':
				return 'Created an API token';
			case 'token_revoked':
				return 'Revoked an API token';
			case 'token_rotated':
				return 'Rotated an API token';
			case 'totp_enabled':
				return 'Enabled two-factor auth';
			case 'totp_disabled':
				return 'Disabled two-factor auth';
			case 'oauth_login':
				return 'Logged in via OAuth';
			case 'oauth_login_failed':
				return 'OAuth login failed';
			case 'member_invited':
				return 'Invited a workspace member';
			case 'member_removed':
				return 'Removed a workspace member';
			case 'role_changed':
				return 'Changed a user role';
			case 'settings_changed':
				return 'Updated settings';
			case 'session_ip_changed':
				return 'Session IP changed';
			case 'plan_changed':
				return 'Changed a user’s plan';
			case 'plan_overrides_changed':
				return 'Changed plan overrides for a user';
			case 'password_reset_by_admin':
				return 'Reset a user’s password (admin)';
			case 'user_disabled':
				return 'Disabled a user account';
			case 'user_enabled':
				return 'Re-enabled a user account';
			case 'account_deleted':
				return 'Deleted account';
			default:
				// Fallback for any new event type we haven't taught the UI
				// about yet — render the raw action with snake_case
				// flattened to spaces.
				return ev.action.replace(/_/g, ' ');
		}
	}

	function relativeTime(dateStr: string): string {
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

<p class="hint">
	Activities authored by this user. Admin actions targeting the user (e.g.
	role changes by another admin) aren’t shown here yet — pending follow-up.
</p>

{#if loading}
	<div class="state-msg">Loading activity…</div>
{:else if loadError}
	<div class="state-msg error">{loadError}
		<button class="btn-retry" type="button" onclick={loadInitial}>Retry</button>
	</div>
{:else if events.length === 0}
	<div class="state-msg empty">No activity recorded for this user yet.</div>
{:else}
	<ul class="activity-list" aria-label="Activity feed">
		{#each events as ev (ev.id)}
			<li class="activity-row">
				<span class="activity-icon" aria-hidden="true">{iconFor(ev.action)}</span>
				<div class="activity-main">
					<div class="activity-summary">
						<span class="activity-action">{describe(ev)}</span>
						<span class="activity-source">via {ev.source}</span>
					</div>
					{#if ev.workspace_id}
						<div class="activity-meta">workspace: {ev.workspace_id.slice(0, 8)}…</div>
					{/if}
				</div>
				<time class="activity-time" datetime={ev.created_at} title={ev.created_at}
					>{relativeTime(ev.created_at)}</time
				>
			</li>
		{/each}
	</ul>
	{#if nextOffset !== null || loadMoreError}
		<div class="load-more-row">
			{#if loadMoreError}
				<span class="load-more-error">{loadMoreError}</span>
			{/if}
			{#if nextOffset !== null}
				<button class="btn" type="button" disabled={loadingMore} onclick={loadMore}
					>{loadingMore ? 'Loading…' : loadMoreError ? 'Retry' : 'Load more'}</button
				>
			{/if}
		</div>
	{/if}
{/if}

<style>
	.hint {
		font-size: 0.8rem;
		color: var(--text-muted);
		margin: 0 0 var(--space-3);
	}
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
	.activity-list {
		list-style: none;
		padding: 0;
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.activity-row {
		display: flex;
		align-items: flex-start;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}
	.activity-icon {
		font-size: 1rem;
		line-height: 1.4;
		width: 24px;
		text-align: center;
		flex-shrink: 0;
	}
	.activity-main {
		flex: 1;
		min-width: 0;
	}
	.activity-summary {
		display: flex;
		gap: var(--space-2);
		align-items: baseline;
		flex-wrap: wrap;
	}
	.activity-action {
		font-size: 0.85rem;
		color: var(--text-primary);
	}
	.activity-source {
		font-size: 0.7rem;
		color: var(--text-muted);
	}
	.activity-meta {
		margin-top: 2px;
		font-size: 0.7rem;
		color: var(--text-muted);
		font-family: var(--font-mono, monospace);
	}
	.activity-time {
		font-size: 0.75rem;
		color: var(--text-muted);
		flex-shrink: 0;
	}
	.load-more-row {
		display: flex;
		justify-content: center;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) 0 0;
	}
	.load-more-error {
		color: #ef4444;
		font-size: 0.8rem;
	}
	.btn {
		padding: 6px 14px;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-primary);
		cursor: pointer;
		font-size: 0.85rem;
	}
	.btn:hover {
		background: var(--bg-tertiary);
	}
	.btn[disabled] {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
