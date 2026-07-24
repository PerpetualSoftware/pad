<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import Modal from '$lib/components/common/Modal.svelte';
	import type { ConnectedApp, Workspace } from '$lib/types';

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

	// PLAN-1519 / TASK-1524 / IDEA-1517 §3: per-card edit panel state.
	// editingId tracks which card has its panel open (one at a time);
	// the per-app errors / pending flags hang off the id so multiple
	// concurrent mutations across cards don't cross-talk.
	let editingId = $state<string | null>(null);
	let nameDrafts = $state<Record<string, string>>({});
	let editErrors = $state<Record<string, string>>({});
	let savingFlag = $state<Record<string, boolean>>({});
	let userWorkspaces = $state<Workspace[] | null>(null);
	let workspacesError = $state('');
	let addPickerSlug = $state<Record<string, string>>({});

	async function loadWorkspaces() {
		if (userWorkspaces !== null) return;
		try {
			const list = await api.workspaces.list();
			userWorkspaces = Array.isArray(list) ? list : [];
		} catch (e) {
			workspacesError = e instanceof Error ? e.message : 'Failed to load workspaces';
			userWorkspaces = [];
		}
	}

	function openEdit(app: ConnectedApp) {
		editingId = app.id;
		nameDrafts[app.id] = app.name ?? '';
		editErrors[app.id] = '';
		addPickerSlug[app.id] = '';
		loadWorkspaces();
	}

	function closeEdit() {
		editingId = null;
	}

	function setError(id: string, message: string) {
		editErrors = { ...editErrors, [id]: message };
	}

	function clearError(id: string) {
		if (editErrors[id]) setError(id, '');
	}

	function replaceApp(updated: ConnectedApp) {
		apps = apps.map((a) => (a.id === updated.id ? { ...a, ...updated } : a));
	}

	async function saveName(app: ConnectedApp) {
		const draft = (nameDrafts[app.id] ?? '').trim();
		if (draft === (app.name ?? '')) return; // no-op
		savingFlag[app.id] = true;
		clearError(app.id);
		try {
			const updated = await api.connectedApps.rename(app.id, draft);
			replaceApp(updated);
		} catch (e) {
			setError(app.id, e instanceof Error ? e.message : 'Failed to rename');
		} finally {
			savingFlag[app.id] = false;
		}
	}

	async function toggleFlag(
		app: ConnectedApp,
		key: 'may_create_workspaces' | 'all_current_workspaces' | 'include_future_workspaces',
		next: boolean
	) {
		savingFlag[app.id] = true;
		clearError(app.id);
		const flags = {
			may_create_workspaces: app.may_create_workspaces ?? true,
			all_current_workspaces: app.all_current_workspaces ?? true,
			include_future_workspaces: app.include_future_workspaces ?? true,
			[key]: next
		};
		try {
			const updated = await api.connectedApps.updateFlags(app.id, flags);
			replaceApp(updated);
		} catch (e) {
			setError(app.id, e instanceof Error ? e.message : 'Failed to update flags');
		} finally {
			savingFlag[app.id] = false;
		}
	}

	async function addWorkspaceToApp(app: ConnectedApp) {
		const slug = (addPickerSlug[app.id] ?? '').trim();
		if (!slug) return;
		savingFlag[app.id] = true;
		clearError(app.id);
		try {
			const updated = await api.connectedApps.addWorkspace(app.id, slug);
			replaceApp(updated);
			addPickerSlug[app.id] = '';
		} catch (e) {
			setError(app.id, e instanceof Error ? e.message : 'Failed to add workspace');
		} finally {
			savingFlag[app.id] = false;
		}
	}

	async function removeWorkspaceFromApp(app: ConnectedApp, slug: string) {
		savingFlag[app.id] = true;
		clearError(app.id);
		try {
			const updated = await api.connectedApps.removeWorkspace(app.id, slug);
			replaceApp(updated);
		} catch (e) {
			setError(app.id, e instanceof Error ? e.message : 'Failed to remove workspace');
		} finally {
			savingFlag[app.id] = false;
		}
	}

	// Available workspaces to add: any membership the user has that
	// isn't already in this connection's allow-list.
	function pickableFor(app: ConnectedApp): Workspace[] {
		if (!userWorkspaces) return [];
		const inList = new Set((app.allowed_workspaces ?? []).filter((s) => s !== '*'));
		return userWorkspaces.filter((ws) => !inList.has(ws.slug));
	}

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

	// Returns true iff the user should see the "Any workspace" badge
	// in the card's summary view. Post-Codex-#585-round-2 the wire
	// shape sends BOTH the wildcard flag AND the staged slug list,
	// so the badge is driven by the flag (the slug list may carry
	// pre-staged rows that are inert until the user flips the flag).
	// Legacy fallback for older wire shapes (no all_current flag,
	// nil/empty list, ["*"] sentinel) preserved so a stale frontend
	// against a fresh backend doesn't lose the badge.
	function isAnyWorkspace(app: ConnectedApp): boolean {
		if (app.all_current_workspaces === true) return true;
		if (app.all_current_workspaces === false) return false;
		// Older wire shape: infer from the slug list.
		const ws = app.allowed_workspaces;
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
				See the
				<a href="https://getpad.dev/connect" target="_blank" rel="noopener noreferrer"
					>connect guide</a
				>
				to set up Claude Desktop, Cursor, or another MCP client.
			</p>
		</div>
	{:else}
		<div class="apps-list">
			{#each apps as app (app.id)}
				{@const anyWs = isAnyWorkspace(app)}
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
						<button
							class="btn"
							onclick={() => (editingId === app.id ? closeEdit() : openEdit(app))}
							aria-expanded={editingId === app.id}
						>
							{editingId === app.id ? 'Done' : 'Edit'}
						</button>
						<button class="btn btn-danger" onclick={() => openConfirm(app)}>Revoke</button>
					</div>

					{#if editingId === app.id}
						{@const eaWsList = (app.allowed_workspaces ?? []).filter((s) => s !== '*')}
						{@const pickable = pickableFor(app)}
						{@const isAll = app.all_current_workspaces ?? true}
						<div class="edit-panel" role="region" aria-label="Edit {app.client_name}">
							{#if editErrors[app.id]}
								<p class="edit-error">{editErrors[app.id]}</p>
							{/if}

							<div class="edit-field">
								<label class="edit-label" for="name-{app.id}">Connection name</label>
								<div class="edit-row">
									<input
										id="name-{app.id}"
										class="edit-input"
										type="text"
										maxlength="120"
										placeholder="e.g. Cursor on MacBook"
										bind:value={nameDrafts[app.id]}
										disabled={!!savingFlag[app.id]}
									/>
									<button
										class="btn"
										onclick={() => saveName(app)}
										disabled={!!savingFlag[app.id] ||
											(nameDrafts[app.id] ?? '').trim() === (app.name ?? '')}
									>
										Save
									</button>
								</div>
								{#if !(app.name ?? '')}
									<p class="edit-hint">
										Name your connection so you can tell it apart from other apps.
									</p>
								{/if}
							</div>

							<div class="edit-field">
								<span class="edit-label">Scope flags</span>
								<label class="edit-toggle">
									<input
										type="checkbox"
										checked={app.may_create_workspaces ?? true}
										disabled={!!savingFlag[app.id]}
										onchange={(e) =>
											toggleFlag(
												app,
												'may_create_workspaces',
												(e.currentTarget as HTMLInputElement).checked
											)}
									/>
									<span>Let this app create new workspaces</span>
								</label>
								<label class="edit-toggle">
									<input
										type="checkbox"
										checked={app.all_current_workspaces ?? true}
										disabled={!!savingFlag[app.id]}
										onchange={(e) =>
											toggleFlag(
												app,
												'all_current_workspaces',
												(e.currentTarget as HTMLInputElement).checked
											)}
									/>
									<span>Cover all my current workspaces (wildcard)</span>
								</label>
								<label class="edit-toggle">
									<input
										type="checkbox"
										checked={app.include_future_workspaces ?? true}
										disabled={!!savingFlag[app.id]}
										onchange={(e) =>
											toggleFlag(
												app,
												'include_future_workspaces',
												(e.currentTarget as HTMLInputElement).checked
											)}
									/>
									<span>Auto-add new workspaces I create</span>
								</label>
							</div>

							<!-- Workspace allow-list editor (TASK-1524 / Codex review
								 #585 round 1): always rendered so users in wildcard
								 mode can pre-stage workspaces before flipping
								 all_current_workspaces=off. The backend's
								 empty_allowlist guard rejects the toggle when the
								 join table is empty; pre-staging in wildcard mode
								 is the mechanism that lets a user transition
								 through that state cleanly. -->
							<div class="edit-field">
								<div class="edit-label-row">
									<span class="edit-label">Workspace allow-list</span>
									{#if isAll}
										<span class="edit-badge">Inert while wildcard is on</span>
									{/if}
								</div>
								<div class="ws-chips">
									{#each eaWsList as ws (ws)}
										<span class="chip">
											{ws}
											<button
												type="button"
												class="chip-remove"
												aria-label="Remove {ws}"
												disabled={!!savingFlag[app.id] || (!isAll && eaWsList.length <= 1)}
												onclick={() => removeWorkspaceFromApp(app, ws)}
											>
												×
											</button>
										</span>
									{/each}
									{#if eaWsList.length === 0}
										<span class="ws-empty">
											{#if isAll}
												No workspaces staged — add one if you plan to switch off
												"Cover all my current workspaces".
											{:else}
												No workspaces — add one below.
											{/if}
										</span>
									{/if}
								</div>
								{#if !isAll && eaWsList.length <= 1}
									<p class="edit-hint">
										You can't remove the last workspace — switch to "Cover all my current
										workspaces" first or revoke the connection.
									</p>
								{/if}
								<div class="edit-row add-row">
									<select
										class="edit-input"
										bind:value={addPickerSlug[app.id]}
										disabled={!!savingFlag[app.id]}
									>
										<option value="">Add a workspace…</option>
										{#each pickable as ws (ws.slug)}
											<option value={ws.slug}>{ws.name}</option>
										{/each}
									</select>
									<button
										class="btn"
										onclick={() => addWorkspaceToApp(app)}
										disabled={!!savingFlag[app.id] || !addPickerSlug[app.id]}
									>
										Add
									</button>
								</div>
								{#if workspacesError}
									<p class="edit-hint edit-hint-error">{workspacesError}</p>
								{/if}
							</div>
						</div>
					{/if}
				</article>
			{/each}
		</div>
	{/if}
</div>

<Modal
	open={!!confirmTarget}
	onclose={closeConfirm}
	labelledby="revoke-title"
	maxWidth="420px"
	placement="center"
	--modal-bg="var(--bg-primary)"
	--modal-radius="var(--radius)"
	--modal-shadow="0 20px 60px rgba(0, 0, 0, 0.3)"
>
	{#if confirmTarget}
		<div class="revoke-modal">
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
	{/if}
</Modal>

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
		color: var(--accent-red);
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
		color: var(--accent-blue);
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
		color: var(--accent-blue);
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
		color: var(--accent-blue);
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
		color: var(--accent-red);
		border-color: rgba(239, 68, 68, 0.4);
	}

	.btn-danger:hover:not(:disabled) {
		background: rgba(239, 68, 68, 0.1);
		border-color: var(--accent-red);
	}

	/* Modal — surface/backdrop/Escape come from the shared <Modal> primitive
	   (TASK-2083); this wrapper just restores the inner padding + column layout. */
	.revoke-modal {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
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
		color: var(--accent-red);
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

	/* PLAN-1519 / TASK-1524 edit panel — appears below the card body
	   when the user clicks Edit. Inline rather than modal so two
	   cards can stay readable side-by-side; the existing card layout
	   already accommodates an expanded "Details" block below the
	   actions row. */
	.edit-panel {
		grid-column: 1 / -1;
		margin-top: 1rem;
		padding: 1rem;
		border-top: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.edit-field {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.edit-label {
		font-weight: 600;
		font-size: 0.85rem;
		color: var(--text-secondary, #555);
	}

	.edit-label-row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}

	.edit-badge {
		font-size: 0.75rem;
		padding: 0.15rem 0.5rem;
		border-radius: 4px;
		background: var(--bg-muted, #f0f0f0);
		color: var(--text-tertiary, #666);
		font-weight: normal;
	}

	.edit-row {
		display: flex;
		gap: 0.5rem;
		align-items: center;
	}

	.edit-input {
		flex: 1 1 auto;
		padding: 0.45rem 0.65rem;
		font-size: 0.9rem;
		border: 1px solid var(--border);
		border-radius: 6px;
		background: var(--bg-input, var(--bg));
		color: inherit;
	}

	.edit-toggle {
		display: flex;
		gap: 0.5rem;
		align-items: center;
		font-size: 0.9rem;
		cursor: pointer;
	}

	.edit-toggle input {
		margin: 0;
	}

	.edit-hint {
		font-size: 0.8rem;
		color: var(--text-tertiary, #777);
		margin: 0;
	}

	.edit-hint-error {
		color: var(--danger, #b00);
	}

	.edit-error {
		padding: 0.5rem 0.75rem;
		background: var(--bg-error, #fee);
		color: var(--danger, #b00);
		border-radius: 6px;
		font-size: 0.85rem;
		margin: 0;
	}

	.ws-chips {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}

	.ws-empty {
		font-size: 0.85rem;
		color: var(--text-tertiary, #777);
		font-style: italic;
	}

	.chip-remove {
		margin-left: 0.3rem;
		border: none;
		background: transparent;
		color: inherit;
		font-size: 1rem;
		line-height: 1;
		cursor: pointer;
		opacity: 0.6;
	}

	.chip-remove:hover:not(:disabled) {
		opacity: 1;
	}

	.chip-remove:disabled {
		cursor: not-allowed;
		opacity: 0.3;
	}

	.add-row {
		margin-top: 0.5rem;
	}

	.add-row .edit-input {
		min-width: 0;
	}
</style>
