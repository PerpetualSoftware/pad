<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import type { ConnectedApp } from '$lib/types';

	// Connected Apps page (TASK-954). Lists every active OAuth grant
	// chain the user has authorized via the MCP API and lets them
	// revoke any of them. Revocation is immediate and irreversible
	// (the request_id chain is dropped server-side), so the confirm
	// modal exists to make that point obvious.

	let apps = $state<ConnectedApp[]>([]);
	let loading = $state(true);
	let error = $state('');

	let expanded = $state<Record<string, boolean>>({});
	let chipsExpanded = $state<Record<string, boolean>>({});

	// Modal state — single modal at a time, keyed by the target app id.
	let confirmTarget = $state<ConnectedApp | null>(null);
	let revoking = $state(false);
	let revokeError = $state('');

	const TIER_LABELS: Record<ConnectedApp['capability_tier'], string> = {
		read_only: 'Read only',
		read_write: 'Read & write',
		full_access: 'Full access',
		unknown: 'Unknown'
	};

	function tierClass(tier: ConnectedApp['capability_tier']): string {
		if (tier === 'read_write') return 'tier-blue';
		if (tier === 'full_access') return 'tier-orange';
		return 'tier-gray';
	}

	function relativeTime(dateStr: string | undefined): string {
		if (!dateStr) return '—';
		const now = Date.now();
		const then = new Date(dateStr).getTime();
		if (Number.isNaN(then)) return '—';
		const diffMs = Math.max(0, now - then);
		const diffSec = Math.floor(diffMs / 1000);
		const diffMin = Math.floor(diffSec / 60);
		const diffHr = Math.floor(diffMin / 60);
		const diffDay = Math.floor(diffHr / 24);

		if (diffSec < 60) return 'Just now';
		if (diffMin < 60) return `${diffMin} minute${diffMin === 1 ? '' : 's'} ago`;
		if (diffHr < 24) return `${diffHr} hour${diffHr === 1 ? '' : 's'} ago`;
		if (diffDay < 30) return `${diffDay} day${diffDay === 1 ? '' : 's'} ago`;

		return new Date(dateStr).toLocaleDateString('en-US', {
			month: 'short',
			day: 'numeric',
			year: 'numeric'
		});
	}

	function initials(name: string): string {
		const parts = (name || '?').trim().split(/\s+/).filter(Boolean);
		if (parts.length === 0) return '?';
		if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
		return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
	}

	function isAnyWorkspace(ws: string[] | null | undefined): boolean {
		if (ws == null) return true;
		if (ws.length === 0) return true;
		if (ws.length === 1 && ws[0] === '*') return true;
		return false;
	}

	function callsLabel(n: number): string {
		if (!n || n <= 0) return 'No activity in 30d';
		return `${n} ${n === 1 ? 'call' : 'calls'} in 30d`;
	}

	async function loadApps() {
		loading = true;
		error = '';
		try {
			const result = await api.connectedApps.list();
			apps = Array.isArray(result?.items) ? result.items : [];
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load connected apps';
		} finally {
			loading = false;
		}
	}

	function toggleDetails(id: string) {
		expanded[id] = !expanded[id];
	}

	function toggleChips(id: string) {
		chipsExpanded[id] = !chipsExpanded[id];
	}

	function openConfirm(app: ConnectedApp) {
		confirmTarget = app;
		revokeError = '';
	}

	function closeConfirm() {
		if (revoking) return;
		confirmTarget = null;
		revokeError = '';
	}

	async function confirmRevoke() {
		if (!confirmTarget) return;
		revoking = true;
		revokeError = '';
		const targetId = confirmTarget.id;
		try {
			await api.connectedApps.revoke(targetId);
			confirmTarget = null;
			await loadApps();
		} catch (e) {
			revokeError = e instanceof Error ? e.message : 'Failed to revoke app';
		} finally {
			revoking = false;
		}
	}

	function onBackdropClick(e: MouseEvent) {
		if (e.target === e.currentTarget) closeConfirm();
	}

	$effect(() => {
		if (!confirmTarget) return;
		function onKey(e: KeyboardEvent) {
			if (e.key === 'Escape') closeConfirm();
		}
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});

	onMount(() => {
		loadApps();
	});
</script>

<div class="page">
	<header class="page-header">
		<h1>Connected apps</h1>
		<p class="page-desc">
			Apps and assistants that can act on your behalf via the MCP API. Revoking an app
			immediately invalidates its access.
		</p>
	</header>

	{#if loading}
		<div class="loading-msg">Loading&hellip;</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={loadApps}>Retry</button>
		</div>
	{:else if apps.length === 0}
		<div class="empty-state">
			<p class="empty-title">No connected apps yet.</p>
			<p class="empty-desc">
				See the <a href="/connect">connect guide</a> to set up Claude Desktop, Cursor,
				or another MCP client.
			</p>
		</div>
	{:else}
		<div class="apps-list">
			{#each apps as app (app.id)}
				{@const anyWs = isAnyWorkspace(app.allowed_workspaces)}
				{@const wsList = anyWs ? [] : (app.allowed_workspaces ?? [])}
				{@const showAllChips = chipsExpanded[app.id]}
				{@const visibleChips = showAllChips ? wsList : wsList.slice(0, 3)}
				{@const extraChips = Math.max(0, wsList.length - 3)}
				<article class="app-card">
					<div class="app-main">
						<div class="app-logo">
							{#if app.logo_uri}
								<img src={app.logo_uri} alt="" />
							{:else}
								<span class="logo-initials">{initials(app.client_name)}</span>
							{/if}
						</div>
						<div class="app-body">
							<div class="app-title-row">
								<h2 class="app-title">{app.client_name}</h2>
								<span class="badge {tierClass(app.capability_tier)}">
									{TIER_LABELS[app.capability_tier]}
								</span>
							</div>

							<div class="chip-row">
								{#if anyWs}
									<span class="chip chip-any">Any workspace</span>
								{:else}
									{#each visibleChips as ws (ws)}
										<span class="chip">{ws}</span>
									{/each}
									{#if !showAllChips && extraChips > 0}
										<button
											type="button"
											class="chip chip-more"
											onclick={() => toggleChips(app.id)}
										>
											+{extraChips}
										</button>
									{:else if showAllChips && wsList.length > 3}
										<button
											type="button"
											class="chip chip-more"
											onclick={() => toggleChips(app.id)}
										>
											Show less
										</button>
									{/if}
								{/if}
							</div>

							<dl class="meta">
								<div class="meta-item">
									<dt>Connected</dt>
									<dd title={new Date(app.connected_at).toISOString()}>
										{relativeTime(app.connected_at)}
									</dd>
								</div>
								<div class="meta-item">
									<dt>Last used</dt>
									<dd title={app.last_used_at ? new Date(app.last_used_at).toISOString() : ''}>
										{relativeTime(app.last_used_at)}
									</dd>
								</div>
								<div class="meta-item">
									<dt>Activity</dt>
									<dd>{callsLabel(app.calls_30d)}</dd>
								</div>
							</dl>

							<button
								type="button"
								class="details-toggle"
								onclick={() => toggleDetails(app.id)}
								aria-expanded={!!expanded[app.id]}
							>
								{expanded[app.id] ? 'Hide details' : 'Details'}
							</button>

							{#if expanded[app.id]}
								<div class="details">
									<div class="detail-row">
										<span class="detail-label">Scopes</span>
										<code class="detail-value">{app.scope_string || '—'}</code>
									</div>
									<div class="detail-row">
										<span class="detail-label">Allowed workspaces</span>
										<span class="detail-value">
											{#if anyWs}
												Any workspace
											{:else}
												{wsList.join(', ')}
											{/if}
										</span>
									</div>
									<div class="detail-row">
										<span class="detail-label">Redirect URIs</span>
										<div class="detail-value">
											{#if app.redirect_uris && app.redirect_uris.length > 0}
												{#each app.redirect_uris as uri (uri)}
													<code class="uri">{uri}</code>
												{/each}
											{:else}
												&mdash;
											{/if}
										</div>
									</div>
								</div>
							{/if}
						</div>
					</div>

					<div class="app-actions">
						<button class="btn btn-danger" onclick={() => openConfirm(app)}>Revoke</button>
					</div>
				</article>
			{/each}
		</div>
	{/if}
</div>

{#if confirmTarget}
	<div
		class="modal-backdrop"
		role="presentation"
		onclick={onBackdropClick}
	>
		<div class="modal" role="dialog" aria-modal="true" aria-labelledby="revoke-title">
			<h3 id="revoke-title" class="modal-title">Revoke {confirmTarget.client_name}?</h3>
			<p class="modal-body">
				The app will lose access immediately. This can&rsquo;t be undone.
			</p>
			{#if revokeError}
				<p class="modal-error">{revokeError}</p>
			{/if}
			<div class="modal-actions">
				<button class="btn" onclick={closeConfirm} disabled={revoking}>Cancel</button>
				<button class="btn btn-danger" onclick={confirmRevoke} disabled={revoking}>
					{revoking ? 'Revoking…' : 'Revoke'}
				</button>
			</div>
		</div>
	</div>
{/if}

<style>
	.page {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.page-header h1 {
		margin: 0 0 var(--space-2) 0;
		font-size: 1.5rem;
		font-weight: 700;
		color: var(--text-primary);
		letter-spacing: -0.01em;
	}

	.page-desc {
		margin: 0;
		font-size: 0.9rem;
		color: var(--text-secondary);
		max-width: 60ch;
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
		padding: var(--space-4) var(--space-6);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
	}

	.error-msg p {
		margin: 0;
		font-size: 0.85rem;
	}

	.empty-state {
		padding: var(--space-8) var(--space-6);
		text-align: center;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.empty-title {
		margin: 0 0 var(--space-2) 0;
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.empty-desc {
		margin: 0;
		font-size: 0.9rem;
		color: var(--text-secondary);
	}

	/* List */
	.apps-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.app-card {
		display: flex;
		gap: var(--space-4);
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		align-items: flex-start;
	}

	.app-main {
		display: flex;
		gap: var(--space-4);
		flex: 1;
		min-width: 0;
	}

	.app-logo {
		flex-shrink: 0;
		width: 48px;
		height: 48px;
		border-radius: var(--radius);
		background: var(--bg-tertiary);
		display: flex;
		align-items: center;
		justify-content: center;
		overflow: hidden;
	}

	.app-logo img {
		width: 100%;
		height: 100%;
		object-fit: cover;
	}

	.logo-initials {
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-secondary);
	}

	.app-body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.app-title-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}

	.app-title {
		margin: 0;
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.chip-row {
		display: flex;
		gap: var(--space-1);
		flex-wrap: wrap;
	}

	.chip {
		display: inline-block;
		padding: 2px 8px;
		border-radius: 999px;
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		font-size: 0.75rem;
		font-weight: 500;
		font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace);
		border: 1px solid var(--border);
	}

	.chip-any {
		font-family: inherit;
		background: rgba(59, 130, 246, 0.1);
		color: var(--accent-blue, #3b82f6);
		border-color: rgba(59, 130, 246, 0.3);
	}

	button.chip-more {
		cursor: pointer;
		font-family: inherit;
	}

	button.chip-more:hover {
		color: var(--text-primary);
	}

	.meta {
		display: flex;
		gap: var(--space-4);
		flex-wrap: wrap;
		margin: 0;
		padding: 0;
	}

	.meta-item {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.meta-item dt {
		font-size: 0.7rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
		margin: 0;
	}

	.meta-item dd {
		font-size: 0.85rem;
		color: var(--text-secondary);
		margin: 0;
	}

	.details-toggle {
		align-self: flex-start;
		background: none;
		border: none;
		padding: 0;
		color: var(--accent-blue, #3b82f6);
		font-size: 0.8rem;
		cursor: pointer;
		font-weight: 500;
	}

	.details-toggle:hover {
		text-decoration: underline;
	}

	.details {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-top: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		border: 1px solid var(--border);
	}

	.detail-row {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	.detail-label {
		font-size: 0.7rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
	}

	.detail-value {
		font-size: 0.85rem;
		color: var(--text-secondary);
		display: flex;
		flex-direction: column;
		gap: 2px;
		word-break: break-all;
	}

	code.detail-value,
	code.uri {
		font-family: var(--font-mono, ui-monospace, SFMono-Regular, monospace);
		font-size: 0.8rem;
	}

	.app-actions {
		flex-shrink: 0;
	}

	/* Badges */
	.badge {
		display: inline-block;
		padding: 2px 8px;
		border-radius: 999px;
		font-size: 0.72rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		background: var(--bg-tertiary);
		color: var(--text-secondary);
	}

	.badge.tier-gray {
		background: rgba(148, 163, 184, 0.15);
		color: var(--text-secondary);
	}

	.badge.tier-blue {
		background: rgba(59, 130, 246, 0.15);
		color: var(--accent-blue, #3b82f6);
	}

	.badge.tier-orange {
		background: rgba(217, 119, 6, 0.15);
		color: var(--accent-orange, #d97706);
	}

	/* Buttons */
	.btn {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		font-weight: 500;
		cursor: pointer;
		transition: background 0.15s, color 0.15s, border-color 0.15s;
	}

	.btn:hover:not(:disabled) {
		background: var(--bg-tertiary);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.btn-danger {
		color: #ef4444;
		border-color: rgba(239, 68, 68, 0.4);
	}

	.btn-danger:hover:not(:disabled) {
		background: rgba(239, 68, 68, 0.1);
		border-color: #ef4444;
	}

	/* Modal */
	.modal-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-4);
		z-index: 100;
	}

	.modal {
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-5);
		max-width: 420px;
		width: 100%;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
	}

	.modal-title {
		margin: 0;
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.modal-body {
		margin: 0;
		font-size: 0.9rem;
		color: var(--text-secondary);
	}

	.modal-error {
		margin: 0;
		font-size: 0.85rem;
		color: #ef4444;
		padding: var(--space-2) var(--space-3);
		background: rgba(239, 68, 68, 0.08);
		border: 1px solid rgba(239, 68, 68, 0.3);
		border-radius: var(--radius);
	}

	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}

	@media (max-width: 640px) {
		.app-card {
			flex-direction: column;
		}

		.app-actions {
			align-self: stretch;
		}

		.app-actions .btn {
			width: 100%;
		}
	}
</style>
