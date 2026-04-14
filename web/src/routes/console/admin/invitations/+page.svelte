<script lang="ts">
	import { onMount } from 'svelte';
	import { adminFetch, adminPost, getCSRFToken, formatDate } from '$lib/stores/admin.svelte';

	interface AdminInvitation {
		id: string;
		email: string;
		role: string;
		workspace_id: string;
		workspace_name: string;
		workspace_slug: string;
		invited_by_id: string;
		invited_by_name: string;
		created_at: string;
	}

	let invitations = $state<AdminInvitation[]>([]);
	let search = $state('');
	let loading = $state(true);
	let error = $state('');
	let actionId = $state<string | null>(null);
	let actionType = $state<'resend' | 'revoke' | null>(null);
	let actionSaving = $state(false);
	let actionMsg = $state<Record<string, string>>({});

	async function adminDelete(path: string) {
		const headers: Record<string, string> = {};
		const csrf = getCSRFToken();
		if (csrf) headers['X-CSRF-Token'] = csrf;
		const resp = await fetch('/api/v1' + path, {
			method: 'DELETE',
			credentials: 'same-origin',
			headers
		});
		if (!resp.ok) throw new Error(`${resp.status}`);
		return resp.json();
	}

	async function loadInvitations() {
		loading = true;
		error = '';
		try {
			const result = await adminFetch('/admin/invitations');
			invitations = result.invitations ?? [];
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load invitations';
		} finally {
			loading = false;
		}
	}

	async function searchInvitations() {
		try {
			const result = await adminFetch(`/admin/invitations?q=${encodeURIComponent(search)}`);
			invitations = result.invitations ?? [];
		} catch {
			/* keep existing */
		}
	}

	function startAction(id: string, type: 'resend' | 'revoke') {
		actionId = id;
		actionType = type;
		actionSaving = false;
		// Clear any previous message for this invitation
		actionMsg = { ...actionMsg, [id]: '' };
	}

	function cancelAction() {
		actionId = null;
		actionType = null;
		actionSaving = false;
	}

	async function confirmResend(id: string) {
		actionSaving = true;
		try {
			const result = await adminPost(`/admin/invitations/${id}/resend`);
			actionMsg = { ...actionMsg, [id]: result.message || 'Invitation resent' };
			cancelAction();
		} catch (e) {
			actionMsg = { ...actionMsg, [id]: e instanceof Error ? e.message : 'Resend failed' };
			cancelAction();
		}
	}

	async function confirmRevoke(id: string) {
		actionSaving = true;
		try {
			const result = await adminDelete(`/admin/invitations/${id}`);
			actionMsg = { ...actionMsg, [id]: result.message || 'Invitation revoked' };
			invitations = invitations.filter((inv) => inv.id !== id);
			cancelAction();
		} catch (e) {
			actionMsg = { ...actionMsg, [id]: e instanceof Error ? e.message : 'Revoke failed' };
			cancelAction();
		}
	}

	onMount(() => {
		loadInvitations();
	});
</script>

<div class="invitations-page">
	{#if loading}
		<div class="loading-msg">Loading invitations...</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={loadInvitations}>Retry</button>
		</div>
	{:else}
		<div class="search-row">
			<input
				type="text"
				class="search-input"
				placeholder="Search by email or workspace..."
				bind:value={search}
				onkeydown={(e) => {
					if (e.key === 'Enter') searchInvitations();
				}}
			/>
			<button class="btn" onclick={searchInvitations}>Search</button>
		</div>

		{#if invitations.length === 0}
			<div class="empty-state">
				<p class="empty-title">No pending invitations</p>
				<p class="empty-desc">Workspace invitations will appear here once users are invited.</p>
			</div>
		{:else}
			<div class="table-wrap">
				<table class="table">
					<thead>
						<tr>
							<th>Email</th>
							<th>Workspace</th>
							<th>Role</th>
							<th>Invited By</th>
							<th>Created</th>
							<th>Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each invitations as inv (inv.id)}
							<tr>
								<td>{inv.email}</td>
								<td>{inv.workspace_name}</td>
								<td>
									<span class="badge" class:owner={inv.role === 'owner'}>
										{inv.role}
									</span>
								</td>
								<td>{inv.invited_by_name}</td>
								<td class="date-cell">{formatDate(inv.created_at)}</td>
								<td class="actions-cell">
									{#if actionId === inv.id && actionType === 'resend'}
										<div class="action-confirm">
											<span class="action-confirm-msg">Resend invitation?</span>
											<button
												class="btn primary btn-sm"
												onclick={() => confirmResend(inv.id)}
												disabled={actionSaving}
											>
												{actionSaving ? 'Sending...' : 'Confirm'}
											</button>
											<button class="btn btn-sm" onclick={cancelAction}>Cancel</button>
										</div>
									{:else if actionId === inv.id && actionType === 'revoke'}
										<div class="action-confirm">
											<span class="action-confirm-msg">Revoke invitation?</span>
											<button
												class="btn danger btn-sm"
												onclick={() => confirmRevoke(inv.id)}
												disabled={actionSaving}
											>
												{actionSaving ? 'Revoking...' : 'Confirm'}
											</button>
											<button class="btn btn-sm" onclick={cancelAction}>Cancel</button>
										</div>
									{:else}
										<div class="action-buttons">
											<button class="btn btn-sm" onclick={() => startAction(inv.id, 'resend')}>
												Resend
											</button>
											<button class="btn danger btn-sm" onclick={() => startAction(inv.id, 'revoke')}>
												Revoke
											</button>
										</div>
									{/if}
									{#if actionMsg[inv.id]}
										<span class="action-msg">{actionMsg[inv.id]}</span>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	{/if}
</div>

<style>
	.invitations-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

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
	.btn.danger {
		border-color: #ef4444;
		color: #ef4444;
	}
	.btn.danger:hover {
		background: color-mix(in srgb, #ef4444 10%, transparent);
	}
	.btn:disabled {
		opacity: 0.5;
		cursor: default;
	}
	.btn-sm {
		font-size: 0.8rem;
		padding: var(--space-1) var(--space-3);
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
	.date-cell {
		white-space: nowrap;
	}

	/* Badge */
	.badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
	}
	.badge.owner {
		background: color-mix(in srgb, var(--accent-green, #22c55e) 15%, transparent);
		color: var(--accent-green, #22c55e);
	}

	/* Actions */
	.actions-cell {
		white-space: nowrap;
	}
	.action-buttons {
		display: flex;
		gap: var(--space-2);
	}
	.action-confirm {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}
	.action-confirm-msg {
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.action-msg {
		display: block;
		font-size: 0.75rem;
		color: var(--text-muted);
		margin-top: var(--space-1);
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
