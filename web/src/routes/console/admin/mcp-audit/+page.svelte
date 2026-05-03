<script lang="ts">
	import { onMount } from 'svelte';
	import { adminFetch } from '$lib/stores/admin.svelte';

	// Admin MCP audit log page (PLAN-943 TASK-960).
	//
	// Full-table view of every request that hit /mcp. Per-user
	// drilldown (the same data filtered to one connection) lives on
	// the connected-apps page (TASK-954) — this page is the
	// operations view for ops + on-call.
	//
	// No filtering UI in v1 beyond pagination — at <= 200 rows per
	// page client-side find-as-you-type is enough; structured
	// filters land if/when ops asks for them. The drop counter at
	// the top tells operators when audit writes are being shed under
	// backpressure (which would mean follow up on the writer
	// goroutine / DB throughput).

	interface MCPAuditEntry {
		id: string;
		timestamp: string;
		user_id: string;
		workspace_id?: string;
		token_kind: 'oauth' | 'pat';
		connection_id: string;
		tool_name: string;
		args_hash?: string;
		result_status: 'ok' | 'error' | 'denied';
		error_kind?: string;
		latency_ms: number;
		request_id: string;
	}

	const LIMIT = 100;

	let entries = $state<MCPAuditEntry[]>([]);
	let dropped = $state<number>(0);
	let loading = $state(true);
	let error = $state('');
	let offset = $state(0);
	let hasMore = $state(false);
	let loadingMore = $state(false);

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

	function statusColor(status: string): string {
		if (status === 'ok') return 'success';
		if (status === 'denied') return 'warning';
		if (status === 'error') return 'danger';
		return '';
	}

	function shortRef(s: string): string {
		if (!s) return '—';
		return s.length > 12 ? s.slice(0, 12) + '…' : s;
	}

	async function loadEntries(append = false) {
		if (append) {
			loadingMore = true;
		} else {
			loading = true;
			error = '';
			offset = 0;
		}

		try {
			const params = new URLSearchParams();
			params.set('limit', String(LIMIT));
			params.set('offset', String(append ? offset : 0));
			const result = await adminFetch(`/admin/mcp-audit?${params}`);
			const items: MCPAuditEntry[] = Array.isArray(result?.items) ? result.items : [];
			dropped = typeof result?.dropped === 'number' ? result.dropped : 0;
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
			if (!append) {
				error = e instanceof Error ? e.message : 'Failed to load MCP audit log';
			}
		} finally {
			loading = false;
			loadingMore = false;
		}
	}

	function loadMore() {
		loadEntries(true);
	}

	onMount(() => {
		loadEntries();
	});
</script>

<svelte:head>
	<title>MCP Audit Log - Pad Admin</title>
</svelte:head>

<div class="audit-page">
	{#if dropped > 0}
		<div class="drop-warning">
			<strong>{dropped}</strong> audit
			{dropped === 1 ? 'entry' : 'entries'} dropped due to writer backpressure since this server started.
			Investigate the audit writer goroutine or database throughput if this counter keeps growing.
		</div>
	{/if}

	{#if loading}
		<div class="loading-msg">Loading MCP audit log&hellip;</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={() => loadEntries()}>Retry</button>
		</div>
	{:else if entries.length === 0}
		<div class="empty-state">
			<p class="empty-title">No MCP audit entries</p>
			<p class="empty-desc">Audit rows appear here as soon as the first /mcp request lands.</p>
		</div>
	{:else}
		<div class="table-wrap">
			<table class="table">
				<thead>
					<tr>
						<th>Time</th>
						<th>User</th>
						<th>Connection</th>
						<th>Kind</th>
						<th>Tool</th>
						<th>Status</th>
						<th>Latency</th>
					</tr>
				</thead>
				<tbody>
					{#each entries as entry (entry.id)}
						<tr>
							<td class="time-cell" title={new Date(entry.timestamp).toISOString()}>
								{relativeTime(entry.timestamp)}
							</td>
							<td title={entry.user_id}>{shortRef(entry.user_id)}</td>
							<td title={entry.connection_id}>{shortRef(entry.connection_id)}</td>
							<td>
								<span class="badge {entry.token_kind === 'oauth' ? 'success' : ''}">
									{entry.token_kind}
								</span>
							</td>
							<td class="tool-cell">{entry.tool_name}</td>
							<td>
								<span class="badge {statusColor(entry.result_status)}">
									{entry.result_status}
								</span>
								{#if entry.error_kind}
									<span class="error-kind">{entry.error_kind}</span>
								{/if}
							</td>
							<td class="latency-cell">{entry.latency_ms}ms</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		{#if hasMore}
			<div class="load-more-row">
				<button class="btn" onclick={loadMore} disabled={loadingMore}>
					{loadingMore ? 'Loading…' : 'Load more'}
				</button>
			</div>
		{/if}
	{/if}
</div>

<style>
	.audit-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.drop-warning {
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		border: 1px solid var(--accent-orange, #d97706);
		background: rgba(217, 119, 6, 0.08);
		color: var(--text-primary);
		font-size: 0.9rem;
	}

	.loading-msg,
	.error-msg,
	.empty-state {
		padding: var(--space-6);
		text-align: center;
		color: var(--text-muted);
	}

	.empty-title {
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: var(--space-1);
	}

	.empty-desc {
		font-size: 0.9rem;
	}

	.table-wrap {
		overflow-x: auto;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
	}

	.table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.85rem;
	}

	.table th,
	.table td {
		padding: var(--space-2) var(--space-3);
		text-align: left;
		border-bottom: 1px solid var(--border);
	}

	.table th {
		font-weight: 600;
		font-size: 0.78rem;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		background: var(--bg-tertiary);
	}

	.table tbody tr:last-child td {
		border-bottom: none;
	}

	.time-cell {
		color: var(--text-secondary);
		white-space: nowrap;
	}

	.tool-cell {
		font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace);
		font-size: 0.8rem;
	}

	.latency-cell {
		font-variant-numeric: tabular-nums;
		text-align: right;
		color: var(--text-secondary);
	}

	.badge {
		display: inline-block;
		padding: 2px 8px;
		border-radius: 999px;
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		font-size: 0.72rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.badge.success {
		background: rgba(34, 197, 94, 0.15);
		color: #16a34a;
	}

	.badge.warning {
		background: rgba(217, 119, 6, 0.15);
		color: #b45309;
	}

	.badge.danger {
		background: rgba(239, 68, 68, 0.15);
		color: #dc2626;
	}

	.error-kind {
		display: inline-block;
		margin-left: var(--space-2);
		font-size: 0.75rem;
		color: var(--text-muted);
		font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace);
	}

	.load-more-row {
		display: flex;
		justify-content: center;
		padding: var(--space-3);
	}

	.btn {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9rem;
		cursor: pointer;
		transition: background 0.15s;
	}

	.btn:hover:not(:disabled) {
		background: var(--bg-tertiary);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}
</style>
