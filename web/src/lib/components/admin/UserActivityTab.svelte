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
	let loadError = $state('');
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
		loading = true;
		loadError = '';
		events = [];
		nextOffset = 0;
		try {
			const result = await fetchPage(userId, 0);
			if (user.id !== userId) return;
			events = result.events;
			nextOffset = result.next_offset;
			fetchedForUserId = userId;
		} catch (e) {
			if (user.id !== userId) return;
			loadError = e instanceof Error ? e.message : 'Failed to load activity';
		} finally {
			if (user.id === userId) loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || nextOffset === null) return;
		const userId = user.id;
		const offset = nextOffset;
		loadingMore = true;
		try {
			const result = await fetchPage(userId, offset);
			if (user.id !== userId) return;
			events = [...events, ...result.events];
			nextOffset = result.next_offset;
		} catch (e) {
			if (user.id !== userId) return;
			loadError = e instanceof Error ? e.message : 'Failed to load activity';
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
	// design showpiece. PLAN-1542 / TASK-1554.
	function iconFor(action: string): string {
		switch (action) {
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
			case 'login':
				return '🔑';
			case 'logout':
				return '🚪';
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
			case 'logout':
				return 'Logged out';
			case 'bootstrap':
				return 'First admin bootstrap';
			default:
				return ev.action;
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
	{#if nextOffset !== null}
		<div class="load-more-row">
			<button class="btn" type="button" disabled={loadingMore} onclick={loadMore}
				>{loadingMore ? 'Loading…' : 'Load more'}</button
			>
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
		padding: var(--space-3) 0 0;
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
