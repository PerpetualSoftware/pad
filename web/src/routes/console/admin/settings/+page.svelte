<script lang="ts">
	import { onMount } from 'svelte';
	import {
		adminFetch,
		adminPatch,
		adminPost,
		getCSRFToken,
		adminStore,
		type LimitTiers
	} from '$lib/stores/admin.svelte';

	// Plan limits
	let limits = $state<LimitTiers | null>(null);
	let savingLimits = $state(false);
	let limitMsg = $state('');

	const LIMIT_FEATURES = [
		'workspaces',
		'items_per_workspace',
		'members_per_workspace',
		'api_tokens',
		'storage_bytes',
		'webhooks',
		'automated_backups'
	] as const;

	// Email settings
	let platformSettings = $state<Record<string, string>>({});
	// GET /admin/settings returns the Maileroo key masked (abcd...wxyz / ****).
	// Track whether the admin actually re-typed it so we never persist the mask
	// back over the real stored key (BUG-1890).
	let apiKeyEdited = $state(false);
	let savingPlatform = $state(false);
	let platformStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let savingIntegrations = $state(false);
	let integrationsStatus = $state<'idle' | 'saved' | 'error'>('idle');
	let testingEmail = $state(false);
	let testEmailResult = $state<{ message: string; type: 'success' | 'error' } | null>(null);

	let loading = $state(true);

	async function loadSettings() {
		try {
			const [limitsData, settingsData] = await Promise.all([
				adminFetch('/admin/limits').catch(() => null),
				adminFetch('/admin/settings').catch(() => ({}))
			]);
			limits = limitsData;
			platformSettings = settingsData ?? {};
		} finally {
			loading = false;
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
			limitMsg = e instanceof Error ? e.message : 'Failed to save';
		} finally {
			savingLimits = false;
		}
	}

	function updateLimit(tier: 'free' | 'pro', key: string, val: string) {
		if (!limits) return;
		const num = parseInt(val, 10);
		if (Number.isNaN(num)) return;
		limits[tier] = { ...limits[tier], [key]: num };
	}

	async function savePlatformSettings() {
		savingPlatform = true;
		platformStatus = 'idle';
		try {
			// Scope the PATCH to the fields this email form owns (mirrors the
			// Integrations save). Only include the API key when the admin actually
			// edited it — otherwise the field still holds the masked placeholder
			// from GET /admin/settings, and saving it would corrupt the real
			// stored key, silently breaking email (BUG-1890).
			const payload: Record<string, string> = {
				email_provider: platformSettings.email_provider ?? '',
				email_from: platformSettings.email_from ?? '',
				email_from_name: platformSettings.email_from_name ?? ''
			};
			if (apiKeyEdited) {
				payload.maileroo_api_key = platformSettings.maileroo_api_key ?? '';
			}
			await adminPatch('/admin/settings', payload);
			platformStatus = 'saved';
		} catch {
			platformStatus = 'error';
		} finally {
			savingPlatform = false;
		}
	}

	async function saveIntegrations() {
		savingIntegrations = true;
		integrationsStatus = 'idle';
		try {
			await adminPatch('/admin/settings', { webmcp_enabled: platformSettings.webmcp_enabled ?? 'false' });
			integrationsStatus = 'saved';
		} catch {
			integrationsStatus = 'error';
		} finally {
			savingIntegrations = false;
		}
	}

	async function sendTestEmail() {
		testingEmail = true;
		testEmailResult = null;
		try {
			const result = await adminPost('/admin/test-email');
			testEmailResult = { message: result.message ?? 'Test email sent', type: 'success' };
		} catch (e) {
			testEmailResult = {
				message: e instanceof Error ? e.message : 'Failed to send test email',
				type: 'error'
			};
		} finally {
			testingEmail = false;
		}
	}

	onMount(() => {
		loadSettings();
	});
</script>

<div class="settings-page">
	{#if loading}
		<p class="loading-msg">Loading settings...</p>
	{:else}
		{#if adminStore.stats?.cloud_mode}
			<section class="section">
				<h2 class="section-title">Plan Limits</h2>
				<p class="section-desc">Use -1 for unlimited.</p>

				{#if limits}
					<div class="table-wrap">
						<table class="table">
							<thead>
								<tr>
									<th>Feature</th>
									<th>Free</th>
									<th>Pro</th>
								</tr>
							</thead>
							<tbody>
								{#each LIMIT_FEATURES as feat (feat)}
									<tr>
										<td class="feat-label">{feat.replace(/_/g, ' ')}</td>
										<td>
											<input
												class="limit-input"
												type="number"
												value={limits.free[feat] ?? 0}
												onchange={(e) => updateLimit('free', feat, e.currentTarget.value)}
											/>
										</td>
										<td>
											<input
												class="limit-input"
												type="number"
												value={limits.pro[feat] ?? 0}
												onchange={(e) => updateLimit('pro', feat, e.currentTarget.value)}
											/>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>

					<div class="edit-actions">
						<button class="btn primary" disabled={savingLimits} onclick={saveLimits}>
							{savingLimits ? 'Saving...' : 'Save Limits'}
						</button>
						{#if limitMsg}
							<span class="save-msg">{limitMsg}</span>
						{/if}
					</div>
				{/if}
			</section>
		{/if}

		<section class="section">
			<h2 class="section-title">Email Configuration</h2>
			<p class="section-desc">
				Configure transactional email for invitations, password resets, and notifications.
			</p>

			<div class="email-card">
				<div class="email-field">
					<label for="email-provider">Provider</label>
					<select
						id="email-provider"
						value={platformSettings.email_provider ?? ''}
						onchange={(e) => {
							platformSettings = { ...platformSettings, email_provider: e.currentTarget.value };
						}}
					>
						<option value="">None (disabled)</option>
						<option value="maileroo">Maileroo</option>
					</select>
				</div>

				{#if platformSettings.email_provider === 'maileroo'}
					<div class="email-field">
						<label for="email-api-key">API Key</label>
						<input
							id="email-api-key"
							type="password"
							value={platformSettings.maileroo_api_key ?? ''}
							oninput={(e) => {
								apiKeyEdited = true;
								platformSettings = {
									...platformSettings,
									maileroo_api_key: e.currentTarget.value
								};
							}}
							placeholder="Maileroo sending key"
						/>
					</div>
				{/if}

				<div class="email-field">
					<label for="email-from">From Address</label>
					<input
						id="email-from"
						type="email"
						value={platformSettings.email_from ?? ''}
						oninput={(e) => {
							platformSettings = { ...platformSettings, email_from: e.currentTarget.value };
						}}
						placeholder="noreply@yourdomain.com"
					/>
				</div>

				<div class="email-field">
					<label for="email-from-name">From Name</label>
					<input
						id="email-from-name"
						type="text"
						value={platformSettings.email_from_name ?? ''}
						oninput={(e) => {
							platformSettings = { ...platformSettings, email_from_name: e.currentTarget.value };
						}}
						placeholder="Pad"
					/>
				</div>

				<div class="edit-actions">
					<button class="btn primary" disabled={savingPlatform} onclick={savePlatformSettings}>
						{savingPlatform ? 'Saving...' : 'Save Email Settings'}
					</button>
					{#if platformStatus === 'saved'}
						<span class="save-msg">Saved</span>
					{:else if platformStatus === 'error'}
						<span class="save-msg">Failed to save</span>
					{/if}
				</div>
			</div>

			{#if platformSettings.email_provider}
				<div class="email-card">
					<p class="email-subheading">Test Email</p>
					<div class="edit-actions">
						<button class="btn" disabled={testingEmail} onclick={sendTestEmail}>
							{testingEmail ? 'Sending...' : 'Send Test Email'}
						</button>
						{#if testEmailResult}
							<span class="save-msg" class:error-msg={testEmailResult.type === 'error'}>
								{testEmailResult.message}
							</span>
						{/if}
					</div>
				</div>
			{/if}
		</section>

		<section class="section">
			<h2 class="section-title">Integrations</h2>
			<p class="section-desc">
				WebMCP lets browser-based AI agents invoke Pad tools — including item create, update,
				delete, and import — under your full logged-in web session. Per-invocation browser consent is
				the only WebMCP-specific guard on destructive actions, and platform-admin sessions reach all
				workspaces. Disabled by default; enable only if you understand the risk.
			</p>

			<div class="email-card">
				<label class="toggle-row">
					<input
						type="checkbox"
						checked={platformSettings.webmcp_enabled === 'true'}
						onchange={(e) => {
							platformSettings = {
								...platformSettings,
								webmcp_enabled: e.currentTarget.checked ? 'true' : 'false'
							};
						}}
					/>
					<span>Enable WebMCP (browser AI agent tools)</span>
				</label>

				<div class="edit-actions">
					<button class="btn primary" disabled={savingIntegrations} onclick={saveIntegrations}>
						{savingIntegrations ? 'Saving...' : 'Save Integrations'}
					</button>
					{#if integrationsStatus === 'saved'}
						<span class="save-msg">Saved</span>
					{:else if integrationsStatus === 'error'}
						<span class="save-msg">Failed to save</span>
					{/if}
				</div>
			</div>
		</section>
	{/if}
</div>

<style>
	.settings-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-8);
	}
	.loading-msg {
		color: var(--text-muted);
		padding: var(--space-6) 0;
		text-align: center;
		font-size: 0.9rem;
	}

	.section {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.section-title {
		font-size: 1.1rem;
		font-weight: 600;
		color: var(--text-primary);
	}
	.section-desc {
		font-size: 0.8rem;
		color: var(--text-muted);
		margin-top: calc(-1 * var(--space-2));
	}

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
	.feat-label {
		text-transform: capitalize;
		color: var(--text-primary);
	}
	.limit-input {
		width: 100px;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		outline: none;
	}
	.limit-input:focus {
		border-color: var(--accent-blue);
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

	.email-card {
		padding: var(--space-4) var(--space-5);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.email-card + .email-card {
		margin-top: var(--space-3);
	}
	.email-field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}
	.email-field label {
		font-size: 0.8rem;
		color: var(--text-muted);
		font-weight: 500;
	}
	.email-field select,
	.email-field input {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		max-width: 400px;
	}
	.email-field select:focus,
	.email-field input:focus {
		outline: none;
		border-color: var(--accent-blue);
	}
	.email-subheading {
		font-size: 0.95rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.toggle-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85rem;
		color: var(--text-primary);
		cursor: pointer;
	}
	.toggle-row input[type='checkbox'] {
		cursor: pointer;
	}
</style>
