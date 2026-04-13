<script lang="ts">
	import { onMount } from 'svelte';
	import { authStore } from '$lib/stores/auth.svelte';

	const BASE = '/api/v1';

	function getCSRFToken(): string | null {
		// Check __Host- prefixed cookie first (secure/TLS mode), fall back to unprefixed
		const hostMatch = document.cookie.match(/(?:^|;\s*)__Host-pad_csrf=([^;]+)/);
		if (hostMatch) return hostMatch[1];
		const match = document.cookie.match(/(?:^|;\s*)pad_csrf=([^;]+)/);
		return match ? match[1] : null;
	}

	async function adminFetch(path: string, opts?: RequestInit) {
		const resp = await fetch(BASE + path, { credentials: 'same-origin', ...opts });
		if (!resp.ok) throw new Error(`${resp.status}`);
		return resp.json();
	}

	async function adminPatch(path: string, body: unknown) {
		const headers: Record<string, string> = { 'Content-Type': 'application/json' };
		const csrf = getCSRFToken();
		if (csrf) headers['X-CSRF-Token'] = csrf;
		return adminFetch(path, {
			method: 'PATCH',
			headers,
			body: JSON.stringify(body)
		});
	}

	interface AdminUser {
		id: string; email: string; username: string; name: string;
		role: string; plan: string; plan_expires_at: string | null;
		plan_overrides: Record<string, number> | null; totp_enabled: boolean; created_at: string;
	}

	interface Stats { users: number; users_by_plan: Record<string, number>; workspaces: number; cloud_mode: boolean; }
	interface LimitTiers { free: Record<string, number>; pro: Record<string, number>; }

	const LIMIT_FEATURES = [
		'workspaces', 'items_per_workspace', 'members_per_workspace',
		'api_tokens', 'storage_bytes', 'webhooks', 'automated_backups'
	] as const;

	let isAdmin = $derived(authStore.user?.role === 'admin');

	let loading = $state(true);
	let error = $state('');
	let stats = $state<Stats | null>(null);
	let users = $state<AdminUser[]>([]);
	let limits = $state<LimitTiers | null>(null);
	let search = $state('');
	let selectedId = $state<string | null>(null);
	let editPlan = $state('free');
	let editOverrides = $state('');
	let saving = $state(false);
	let savingLimits = $state(false);
	let saveMsg = $state('');
	let limitMsg = $state('');

	// Email settings
	let platformSettings = $state<Record<string, string>>({});
	let savingPlatform = $state(false);
	let platformStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let testingEmail = $state(false);
	let testEmailResult = $state<{ message: string; type: 'success' | 'error' } | null>(null);

	function formatDate(d: string): string {
		return new Date(d).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
	}

	async function loadData() {
		loading = true;
		error = '';
		try {
			const [s, u, l] = await Promise.all([
				adminFetch('/admin/stats'),
				adminFetch('/admin/users'),
				adminFetch('/admin/limits')
			]);
			stats = s; users = u.users ?? u; limits = l;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load';
		} finally {
			loading = false;
		}
	}

	async function searchUsers() {
		try {
			const result = await adminFetch(`/admin/users?q=${encodeURIComponent(search)}`);
			users = result.users ?? result;
		} catch { /* keep existing */ }
	}

	function selectUser(u: AdminUser) {
		if (selectedId === u.id) { selectedId = null; return; }
		selectedId = u.id;
		editPlan = u.plan || 'free';
		editOverrides = u.plan_overrides ? JSON.stringify(u.plan_overrides, null, 2) : '';
		saveMsg = '';
	}

	async function saveUser() {
		if (!selectedId) return;
		saving = true;
		saveMsg = '';
		try {
			// Validate JSON syntax before sending
			if (editOverrides.trim()) {
				JSON.parse(editOverrides); // throws if invalid
			}
			// Backend expects plan_overrides as a JSON string, not an object
			await adminPatch(`/admin/users/${selectedId}`, {
				plan: editPlan,
				plan_overrides: editOverrides.trim() || null
			});
			const updated = await adminFetch(`/admin/users/${selectedId}`);
			users = users.map((u) => u.id === selectedId ? { ...u, ...updated } : u);
			saveMsg = 'Saved';
		} catch (e) {
			saveMsg = e instanceof Error ? e.message : 'Save failed';
		} finally {
			saving = false;
		}
	}

	async function saveLimits() {
		if (!limits) return;
		savingLimits = true;
		limitMsg = '';
		try {
			await adminPatch('/admin/limits', limits);
			limitMsg = 'Saved';
		} catch (e) {
			limitMsg = e instanceof Error ? e.message : 'Save failed';
		} finally {
			savingLimits = false;
		}
	}

	function updateLimit(tier: 'free' | 'pro', key: string, val: string) {
		if (!limits) return;
		const num = val.trim() === '' ? 0 : Number(val);
		limits[tier] = { ...limits[tier], [key]: isNaN(num) ? 0 : num };
	}

	async function loadEmailSettings() {
		try {
			const resp = await fetch(BASE + '/admin/settings', { credentials: 'same-origin' });
			if (resp.ok) platformSettings = await resp.json();
		} catch {}
	}

	async function savePlatformSettings() {
		if (savingPlatform) return;
		savingPlatform = true;
		platformStatus = 'idle';
		try {
			const headers: Record<string, string> = { 'Content-Type': 'application/json' };
			const csrf = getCSRFToken();
			if (csrf) headers['X-CSRF-Token'] = csrf;
			await fetch(BASE + '/admin/settings', {
				method: 'PATCH',
				credentials: 'same-origin',
				headers,
				body: JSON.stringify(platformSettings)
			});
			platformStatus = 'saved';
			setTimeout(() => (platformStatus = 'idle'), 2000);
		} catch {
			platformStatus = 'error';
		} finally {
			savingPlatform = false;
		}
	}

	async function sendTestEmail() {
		testingEmail = true;
		testEmailResult = null;
		try {
			const headers: Record<string, string> = { 'Content-Type': 'application/json' };
			const csrf = getCSRFToken();
			if (csrf) headers['X-CSRF-Token'] = csrf;
			const resp = await fetch(BASE + '/admin/test-email', {
				method: 'POST',
				credentials: 'same-origin',
				headers
			});
			if (!resp.ok) throw new Error('Failed to send test email');
			const result = await resp.json();
			testEmailResult = { message: `Test email sent to ${result.sent_to}`, type: 'success' };
		} catch (err: unknown) {
			testEmailResult = {
				message: err instanceof Error ? err.message : 'Failed to send test email',
				type: 'error'
			};
		} finally {
			testingEmail = false;
		}
	}

	onMount(() => {
		if (isAdmin) {
			loadData();
			loadEmailSettings();
		}
	});
</script>

<svelte:head>
	<title>Admin - Pad</title>
</svelte:head>

<div class="admin-page">
	{#if !isAdmin}
		<div class="denied">Admin access required</div>
	{:else if loading}
		<div class="loading-msg">Loading admin data...</div>
	{:else if error}
		<div class="error-msg">{error}</div>
	{:else}
		<!-- Stats bar -->
		{#if stats}
			<div class="stats-bar">
				<div class="stat">
					<span class="stat-value">{stats.users}</span>
					<span class="stat-label">Users</span>
				</div>
				{#each Object.entries(stats.users_by_plan) as [plan, count] (plan)}
					<div class="stat">
						<span class="stat-value">{count}</span>
						<span class="stat-label">{plan}</span>
					</div>
				{/each}
				<div class="stat">
					<span class="stat-value">{stats.workspaces}</span>
					<span class="stat-label">Workspaces</span>
				</div>
			</div>
		{/if}

		<!-- Users section -->
		<section class="section">
			<h2 class="section-title">Users</h2>
			<div class="search-row">
				<input
					type="text"
					class="search-input"
					placeholder="Search users..."
					bind:value={search}
					onkeydown={(e) => { if (e.key === 'Enter') searchUsers(); }}
				/>
				<button class="btn" onclick={searchUsers}>Search</button>
			</div>

			<div class="table-wrap">
				<table class="table">
					<thead>
						<tr>
							<th>Name</th><th>Email</th><th>Plan</th><th>Created</th>
						</tr>
					</thead>
					<tbody>
						{#each users as user (user.id)}
							<tr
								class="user-row"
								class:selected={selectedId === user.id}
								onclick={() => selectUser(user)}
							>
								<td>{user.name || user.username}</td>
								<td>{user.email}</td>
								<td><span class="badge" class:pro={user.plan === 'pro'}>{user.plan || 'free'}</span></td>
								<td class="date-cell">{formatDate(user.created_at)}</td>
							</tr>
							{#if selectedId === user.id}
								<tr class="edit-row">
									<td colspan="4">
										<div class="edit-panel">
											<div class="edit-field">
												<label for="edit-plan">Plan</label>
												<select id="edit-plan" bind:value={editPlan}>
													<option value="free">free</option>
													<option value="pro">pro</option>
												</select>
											</div>
											<div class="edit-field">
												<label for="edit-overrides">Plan overrides (JSON)</label>
												<textarea id="edit-overrides" bind:value={editOverrides} rows="3" placeholder={'{"workspaces": 10}'}></textarea>
											</div>
											<div class="edit-actions">
												<button class="btn primary" onclick={saveUser} disabled={saving}>
													{saving ? 'Saving...' : 'Save'}
												</button>
												{#if saveMsg}<span class="save-msg">{saveMsg}</span>{/if}
											</div>
										</div>
									</td>
								</tr>
							{/if}
						{/each}
					</tbody>
				</table>
			</div>
		</section>

		<!-- Plan Limits section (cloud mode only) -->
		{#if limits && stats?.cloud_mode}
			<section class="section">
				<h2 class="section-title">Plan Limits</h2>
				<p class="section-desc">Use -1 for unlimited.</p>
				<div class="table-wrap">
					<table class="table">
						<thead>
							<tr><th>Feature</th><th>Free</th><th>Pro</th></tr>
						</thead>
						<tbody>
							{#each LIMIT_FEATURES as feat (feat)}
								<tr>
									<td class="feat-label">{feat.replace(/_/g, ' ')}</td>
									<td>
										<input
											type="number"
											class="limit-input"
											value={limits.free[feat] ?? 0}
											oninput={(e) => updateLimit('free', feat, e.currentTarget.value)}
										/>
									</td>
									<td>
										<input
											type="number"
											class="limit-input"
											value={limits.pro[feat] ?? 0}
											oninput={(e) => updateLimit('pro', feat, e.currentTarget.value)}
										/>
									</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
				<div class="edit-actions">
					<button class="btn primary" onclick={saveLimits} disabled={savingLimits}>
						{savingLimits ? 'Saving...' : 'Save Limits'}
					</button>
					{#if limitMsg}<span class="save-msg">{limitMsg}</span>{/if}
				</div>
			</section>
		{/if}

		<!-- Email Configuration -->
		<section class="section">
			<h2 class="section-title">Email Configuration</h2>
			<p class="section-desc">Configure transactional email for invitations, password resets, and notifications.</p>
			<div class="email-card">
				<div class="email-field">
					<label for="email-provider">Provider</label>
					<select id="email-provider" bind:value={platformSettings['email_provider']}
						onchange={() => { if (!platformSettings['email_provider']) platformSettings['email_provider'] = ''; }}>
						<option value="">Disabled</option>
						<option value="maileroo">Maileroo</option>
					</select>
				</div>
				{#if platformSettings['email_provider'] === 'maileroo'}
					<div class="email-field">
						<label for="maileroo-key">API Key</label>
						<input id="maileroo-key" type="password" bind:value={platformSettings['maileroo_api_key']}
							placeholder="Enter Maileroo sending key" />
					</div>
				{/if}
				<div class="email-field">
					<label for="email-from">From Address</label>
					<input id="email-from" type="email" bind:value={platformSettings['email_from']}
						placeholder="noreply@yourdomain.com" />
				</div>
				<div class="email-field">
					<label for="email-name">From Name</label>
					<input id="email-name" type="text" bind:value={platformSettings['email_from_name']}
						placeholder="Pad" />
				</div>
				<div class="edit-actions">
					<button class="btn primary" onclick={savePlatformSettings} disabled={savingPlatform}>
						{savingPlatform ? 'Saving...' : 'Save Email Settings'}
					</button>
					{#if platformStatus === 'saved'}
						<span class="save-msg" style="color: var(--accent-green);">Saved</span>
					{:else if platformStatus === 'error'}
						<span class="save-msg" style="color: #ef4444;">Error</span>
					{/if}
				</div>
			</div>

			{#if platformSettings['email_provider']}
				<div class="email-card">
					<h3 class="email-subheading">Test Email</h3>
					<p class="section-desc">Send a test email to verify your configuration is working.</p>
					<div class="edit-actions">
						<button class="btn" onclick={sendTestEmail} disabled={testingEmail}>
							{testingEmail ? 'Sending...' : 'Send Test Email'}
						</button>
						{#if testEmailResult}
							<span class="save-msg" style="color: {testEmailResult.type === 'success' ? 'var(--accent-green)' : '#ef4444'};">
								{testEmailResult.message}
							</span>
						{/if}
					</div>
				</div>
			{/if}
		</section>
	{/if}
</div>

<style>
	.admin-page { display: flex; flex-direction: column; gap: var(--space-8); }

	.denied, .loading-msg {
		color: var(--text-muted); padding: var(--space-10) 0; text-align: center; font-size: 0.95rem;
	}
	.error-msg {
		color: #ef4444; padding: var(--space-6); background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius);
	}

	/* Stats */
	.stats-bar {
		display: flex; gap: var(--space-4); flex-wrap: wrap;
	}
	.stat {
		flex: 1; min-width: 120px; padding: var(--space-4) var(--space-5);
		background: var(--bg-secondary); border: 1px solid var(--border);
		border-radius: var(--radius-lg); display: flex; flex-direction: column; gap: var(--space-1);
	}
	.stat-value { font-size: 1.5rem; font-weight: 700; color: var(--text-primary); }
	.stat-label { font-size: 0.8rem; color: var(--text-muted); text-transform: capitalize; }

	/* Section */
	.section { display: flex; flex-direction: column; gap: var(--space-4); }
	.section-title { font-size: 1.1rem; font-weight: 600; color: var(--text-primary); }
	.section-desc { font-size: 0.8rem; color: var(--text-muted); margin-top: calc(-1 * var(--space-2)); }

	/* Search */
	.search-row { display: flex; gap: var(--space-2); }
	.search-input {
		flex: 1; padding: var(--space-2) var(--space-3); background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary);
		font-size: 0.85rem; outline: none;
	}
	.search-input:focus { border-color: var(--accent-blue); }

	/* Buttons */
	.btn {
		padding: var(--space-2) var(--space-4); border-radius: var(--radius);
		border: 1px solid var(--border); background: var(--bg-secondary); color: var(--text-secondary);
		font-size: 0.85rem; font-weight: 500; cursor: pointer; transition: border-color 0.15s, color 0.15s;
	}
	.btn:hover { color: var(--text-primary); border-color: var(--text-muted); }
	.btn.primary { background: var(--accent-blue); color: #fff; border-color: transparent; }
	.btn.primary:hover { opacity: 0.9; }
	.btn:disabled { opacity: 0.5; cursor: default; }

	/* Table */
	.table-wrap { overflow-x: auto; }
	.table {
		width: 100%; border-collapse: collapse; font-size: 0.85rem;
	}
	.table th {
		text-align: left; padding: var(--space-2) var(--space-3); color: var(--text-muted);
		font-weight: 500; border-bottom: 1px solid var(--border); font-size: 0.8rem;
	}
	.table td {
		padding: var(--space-2) var(--space-3); border-bottom: 1px solid var(--border);
		color: var(--text-secondary);
	}
	.user-row { cursor: pointer; transition: background 0.1s; }
	.user-row:hover { background: var(--bg-hover); }
	.user-row.selected { background: var(--bg-tertiary); }
	.date-cell { white-space: nowrap; }

	.badge {
		padding: 2px var(--space-2); border-radius: var(--radius-sm); font-size: 0.75rem; font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent); color: var(--text-muted);
	}
	.badge.pro {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent); color: var(--accent-blue);
	}

	/* Edit panel */
	.edit-row td { padding: 0; border-bottom: 1px solid var(--border); }
	.edit-panel {
		padding: var(--space-4) var(--space-3); background: var(--bg-tertiary);
		display: flex; flex-direction: column; gap: var(--space-3);
	}
	.edit-field { display: flex; flex-direction: column; gap: var(--space-1); }
	.edit-field label { font-size: 0.8rem; color: var(--text-muted); font-weight: 500; }
	.edit-field select, .edit-field textarea {
		padding: var(--space-2) var(--space-3); background: var(--bg-secondary); border: 1px solid var(--border);
		border-radius: var(--radius); color: var(--text-primary); font-size: 0.85rem; font-family: inherit;
	}
	.edit-field textarea { resize: vertical; font-family: monospace; font-size: 0.8rem; }
	.edit-actions { display: flex; align-items: center; gap: var(--space-3); }
	.save-msg { font-size: 0.8rem; color: var(--text-muted); }

	/* Limits */
	.feat-label { text-transform: capitalize; color: var(--text-primary); }
	.limit-input {
		width: 100px; padding: var(--space-1) var(--space-2); background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius); color: var(--text-primary);
		font-size: 0.85rem; outline: none;
	}
	.limit-input:focus { border-color: var(--accent-blue); }

	/* Email config */
	.email-card {
		padding: var(--space-4) var(--space-5); background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius);
		display: flex; flex-direction: column; gap: var(--space-3);
	}
	.email-card + .email-card { margin-top: var(--space-3); }
	.email-field { display: flex; flex-direction: column; gap: var(--space-1); }
	.email-field label { font-size: 0.8rem; color: var(--text-muted); font-weight: 500; }
	.email-field select, .email-field input {
		padding: var(--space-2) var(--space-3); background: var(--bg-tertiary); border: 1px solid var(--border);
		border-radius: var(--radius); color: var(--text-primary); font-size: 0.85rem; max-width: 400px;
	}
	.email-field select:focus, .email-field input:focus { outline: none; border-color: var(--accent-blue); }
	.email-subheading { font-size: 0.95rem; font-weight: 600; color: var(--text-primary); margin: 0; }
</style>
