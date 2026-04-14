<script lang="ts">
	import { onMount } from 'svelte';
	import { adminFetch, adminPatch, adminPost, formatDate, adminStore, type AdminUser } from '$lib/stores/admin.svelte';

	let users = $state<AdminUser[]>([]);
	let search = $state('');
	let selectedId = $state<string | null>(null);
	let editPlan = $state('free');
	let editRole = $state('member');
	// Plan override fields — empty string means "use plan default", a number overrides it
	const overrideFields = [
		{ key: 'workspaces', label: 'Workspaces', hint: 'Max workspaces owned' },
		{ key: 'items_per_workspace', label: 'Items per workspace', hint: 'Max items in each workspace' },
		{ key: 'members_per_workspace', label: 'Members per workspace', hint: 'Max members per workspace' },
		{ key: 'api_tokens', label: 'API tokens', hint: 'Max API tokens' },
		{ key: 'webhooks', label: 'Webhooks', hint: 'Max webhooks per workspace' },
	];
	const overrideFieldKeys = new Set(overrideFields.map(f => f.key));
	let editOverrides = $state<Record<string, string>>({});
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

	async function loadUsers() {
		loading = true;
		error = '';
		try {
			const result = await adminFetch('/admin/users');
			users = result.users ?? result;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load users';
		} finally {
			loading = false;
		}
	}

	async function searchUsers() {
		try {
			const result = await adminFetch(`/admin/users?q=${encodeURIComponent(search)}`);
			users = result.users ?? result;
		} catch {
			/* keep existing */
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
		// Populate override fields from the user's plan_overrides object
		const ov = u.plan_overrides ?? {};
		editOverrides = {};
		extraOverrides = {};
		for (const f of overrideFields) {
			editOverrides[f.key] = f.key in ov ? String(ov[f.key]) : '';
		}
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
			const overridesJSON = Object.keys(overrides).length > 0 ? JSON.stringify(overrides) : null;
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

	onMount(() => {
		loadUsers();
	});
</script>

<div class="users-page">
	{#if loading}
		<div class="loading-msg">Loading users...</div>
	{:else if error}
		<div class="error-msg">
			<p>{error}</p>
			<button class="btn" onclick={loadUsers}>Retry</button>
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

		<div class="table-wrap">
			<table class="table">
				<thead>
					<tr>
						<th>Name</th>
						<th>Role</th>
						<th>Email</th>
						{#if adminStore.stats?.cloud_mode}
							<th>Plan</th>
						{/if}
						<th>Last Active</th>
						<th>Created</th>
					</tr>
				</thead>
				<tbody>
					{#each users as user (user.id)}
						<tr
							class="user-row"
							class:selected={selectedId === user.id}
							class:disabled-row={!!user.disabled_at}
							onclick={() => selectUser(user)}
						>
							<td>
								{user.name || user.username}
								{#if user.disabled_at}
									<span class="badge disabled">disabled</span>
								{/if}
							</td>
							<td
								><span class="badge" class:admin={user.role === 'admin'}
									>{user.role || 'member'}</span
								></td
							>
							<td>{user.email}</td>
							{#if adminStore.stats?.cloud_mode}
								<td
									><span class="badge" class:pro={user.plan === 'pro'}
										>{user.plan || 'free'}</span
									></td
								>
							{/if}
							<td class="date-cell muted"
								title={user.last_active_at || ''}
								>{relativeTime(user.last_active_at)}</td>
							<td class="date-cell">{formatDate(user.created_at)}</td>
						</tr>
						{#if selectedId === user.id}
							<tr class="edit-row">
								<td colspan={adminStore.stats?.cloud_mode ? 6 : 5}>
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
		</div>
	{/if}
</div>

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
