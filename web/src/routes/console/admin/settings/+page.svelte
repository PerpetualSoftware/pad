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
	import { api } from '$lib/api/client';
	import type { DecisionSettings, DecisionSettingsInput, MCPSettings } from '$lib/types';

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

	// Decision provider (TASK-3121). The server never returns the API key, only
	// whether one is set, so the key input starts empty and is sent only when the
	// admin types into it. Its own endpoint rather than /admin/settings: the key
	// is encrypted at rest and the environment can override each field.
	let decision = $state<DecisionSettings | null>(null);
	let decisionLoadError = $state('');
	let decisionEnabled = $state(false);
	let decisionProvider = $state<'typesafe' | 'none'>('typesafe');
	let decisionModel = $state('');
	let decisionKey = $state('');
	let decisionClearKey = $state(false);
	let savingDecision = $state(false);
	let decisionStatus = $state<{ message: string; type: 'saved' | 'error' } | null>(null);

	function applyDecision(d: DecisionSettings) {
		decision = d;
		// Seed from what is in force for env-set fields (the input is disabled and
		// shows that value) and from the stored setting otherwise.
		decisionEnabled = d.env.provider ? d.effective.enabled : (d.stored.enabled ?? d.effective.enabled);
		// "none" only when that is what is saved (or what the environment
		// forces); an unset provider shows the one real choice.
		const shown = d.env.provider ? d.effective.provider : d.stored.provider;
		decisionProvider = shown === 'none' ? 'none' : 'typesafe';
		decisionModel = d.env.model ? d.effective.model : d.stored.model;
		decisionKey = '';
		decisionClearKey = false;
	}

	// MCP over HTTP (PLAN-2310 DR-7). The toggle writes at once; the readout
	// is what GET /admin/mcp reports, which is configuration, not an
	// observation of the deployment (the server cannot see TLS a proxy
	// terminates).
	let mcp = $state<MCPSettings | null>(null);
	let mcpLoadError = $state('');
	let savingMcp = $state(false);
	let mcpStatus = $state<{ message: string; type: 'saved' | 'error' } | null>(null);

	async function loadMcp() {
		try {
			mcp = await api.admin.getMCPSettings();
		} catch (e) {
			mcpLoadError = e instanceof Error ? e.message : 'Failed to load';
		}
	}

	async function setMcpEnabled(enabled: boolean) {
		savingMcp = true;
		mcpStatus = null;
		try {
			mcp = await api.admin.updateMCPSettings(enabled);
			mcpStatus = { message: 'Saved', type: 'saved' };
		} catch (e) {
			mcpStatus = { message: e instanceof Error ? e.message : 'Failed to save', type: 'error' };
		} finally {
			savingMcp = false;
		}
	}

	const mcpAuthLabel = (methods: string[]) =>
		methods.includes('oauth') ? 'OAuth and API tokens' : methods.includes('pat') ? 'API tokens only' : 'None';

	async function loadDecision() {
		try {
			applyDecision(await api.admin.getDecisionSettings());
		} catch (e) {
			decisionLoadError = e instanceof Error ? e.message : 'Failed to load';
		}
	}

	async function saveDecision() {
		if (!decision) return;
		savingDecision = true;
		decisionStatus = null;
		// Only fields the environment does not set: the server refuses the rest.
		const input: DecisionSettingsInput = {};
		if (!decision.env.provider) {
			input.enabled = decisionEnabled;
			input.provider = decisionProvider;
		}
		if (!decision.env.model) input.model = decisionModel.trim();
		if (!decision.env.api_key) {
			if (decisionKey.trim()) input.api_key = decisionKey.trim();
			else if (decisionClearKey) input.clear_api_key = true;
		}
		try {
			applyDecision(await api.admin.updateDecisionSettings(input));
			decisionStatus = { message: 'Saved', type: 'saved' };
		} catch (e) {
			decisionStatus = { message: e instanceof Error ? e.message : 'Failed to save', type: 'error' };
		} finally {
			savingDecision = false;
		}
	}

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
			// Integrations save).
			const payload: Record<string, string> = {
				email_provider: platformSettings.email_provider ?? '',
				email_from: platformSettings.email_from ?? '',
				email_from_name: platformSettings.email_from_name ?? ''
			};
			if (payload.email_provider !== 'maileroo') {
				// Disabling the provider: clear the stored key so email actually
				// turns off. The server keys email enablement off the presence of
				// maileroo_api_key (reconfigureEmail ignores email_provider), so
				// leaving a stale key would keep sending mail after "None".
				payload.maileroo_api_key = '';
			} else if (apiKeyEdited) {
				// Provider is Maileroo: only send the key when the admin actually
				// edited it — otherwise the field still holds the masked placeholder
				// from GET /admin/settings, and saving it would corrupt the real
				// stored key, silently breaking email (BUG-1890).
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
		loadDecision();
		loadMcp();
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

		<section class="section" data-testid="decision-provider-section">
			<h2 class="section-title">Decision provider</h2>
			<p class="section-desc">
				Scores open items for attention (needs a human decision, blocked, waiting on someone
				outside the team) using a hosted model.
			</p>

			<div class="email-card">
				{#if decisionLoadError}
					<p class="save-msg error-msg">Could not load: {decisionLoadError}</p>
				{:else if !decision}
					<p class="save-msg">Loading…</p>
				{:else if decision.read_only}
					<p class="save-msg" data-testid="decision-operator-note">
						Configured by the operator.
						{decision.effective.enabled
							? `Enabled: ${decision.effective.provider} (${decision.effective.model}).`
							: 'Disabled.'}
					</p>
				{:else}
					<p class="privacy-line" data-testid="decision-privacy-line">
						When enabled, item titles, bodies and comments are sent to the provider.
					</p>

					<div class="email-field">
						<label for="decision-provider">
							Provider
							{#if decision.env.provider}<span class="env-note">set by environment</span>{/if}
						</label>
						<select
							id="decision-provider"
							value={decisionProvider}
							disabled={decision.env.provider}
							onchange={(e) => (decisionProvider = e.currentTarget.value as 'typesafe' | 'none')}
						>
							<option value="none">None (disabled)</option>
							<option value="typesafe">typesafe</option>
						</select>
					</div>

					<label class="toggle-row">
						<input
							type="checkbox"
							data-testid="decision-enabled"
							checked={decisionEnabled}
							disabled={decision.env.provider}
							onchange={(e) => (decisionEnabled = e.currentTarget.checked)}
						/>
						<span>Enable decisions</span>
						{#if decision.env.provider}<span class="env-note">set by environment</span>{/if}
					</label>

					<div class="email-field">
						<label for="decision-api-key">
							API key
							{#if decision.env.api_key}<span class="env-note">set by environment</span>{/if}
						</label>
						<input
							id="decision-api-key"
							type="password"
							autocomplete="off"
							disabled={decision.env.api_key}
							value={decisionKey}
							oninput={(e) => {
								decisionKey = e.currentTarget.value;
								if (decisionKey) decisionClearKey = false;
							}}
							placeholder={decision.stored.api_key_set
								? 'A key is saved — type to replace it'
								: 'typesafe API key'}
						/>
						{#if decision.stored_key_error}
							<span class="save-msg error-msg" data-testid="decision-stored-key-error"
								>{decision.stored_key_error}</span
							>
						{/if}
						{#if decision.stored.api_key_set && !decision.env.api_key}
							<label class="toggle-row small">
								<input
									type="checkbox"
									checked={decisionClearKey}
									disabled={decisionKey !== ''}
									onchange={(e) => (decisionClearKey = e.currentTarget.checked)}
								/>
								<span>Remove the saved key</span>
							</label>
						{/if}
					</div>

					<div class="email-field">
						<label for="decision-model">
							Model
							{#if decision.env.model}<span class="env-note">set by environment</span>{/if}
						</label>
						<input
							id="decision-model"
							type="text"
							disabled={decision.env.model}
							value={decisionModel}
							oninput={(e) => (decisionModel = e.currentTarget.value)}
							placeholder="jev-1.13.0 (default)"
						/>
					</div>

					<div class="edit-actions">
						<button
							class="btn primary"
							data-testid="decision-save"
							disabled={savingDecision}
							onclick={saveDecision}
						>
							{savingDecision ? 'Saving...' : 'Save Decision Settings'}
						</button>
						{#if decisionStatus}
							<span
								class="save-msg"
								class:error-msg={decisionStatus.type === 'error'}
								data-testid="decision-status">{decisionStatus.message}</span
							>
						{/if}
					</div>
				{/if}

				{#if decision}
					<p class="save-msg" data-testid="decision-effective">
						{#if decision.effective.enabled && !decision.error}
							Running: {decision.effective.provider} ({decision.effective.model}).
						{:else if decision.error}
							<span class="error-msg">Not running: {decision.error}</span>
						{:else}
							Off.
						{/if}
					</p>
				{/if}
			</div>
		</section>

		<section class="section" data-testid="mcp-section">
			<h2 class="section-title">MCP for agents</h2>
			<p class="section-desc">
				Lets remote agents (Claude.ai, ChatGPT, Cursor and other MCP clients) use this server at
				its MCP URL, without installing the Pad CLI. Off by default.
			</p>

			<div class="email-card">
				{#if mcpLoadError}
					<p class="save-msg error-msg">Could not load: {mcpLoadError}</p>
				{:else if !mcp}
					<p class="save-msg">Loading…</p>
				{:else}
					{@const rd = mcp.readiness}
					<label class="toggle-row">
						<input
							type="checkbox"
							data-testid="mcp-enabled"
							checked={mcp.enabled}
							disabled={mcp.locked || savingMcp}
							onchange={(e) => setMcpEnabled(e.currentTarget.checked)}
						/>
						<span>Enable MCP</span>
						{#if mcp.source === 'environment'}
							<span class="env-note" data-testid="mcp-lock-note">set by environment (PAD_MCP_ENABLED)</span>
						{:else if mcp.source === 'cloud'}
							<span class="env-note" data-testid="mcp-lock-note">managed by the operator</span>
						{/if}
					</label>

					<p class="mcp-state" data-testid="mcp-state" data-state={rd.state}>
						{#if rd.state === 'on'}
							<span class="mcp-dot on" aria-hidden="true"></span>On: agents can connect.
						{:else if rd.state === 'blocked'}
							<span class="mcp-dot blocked" aria-hidden="true"></span>On, but blocked: {rd.blocked}
						{:else}
							<span class="mcp-dot" aria-hidden="true"></span>Off: every MCP and OAuth route answers 404.
						{/if}
					</p>

					<dl class="mcp-readout" data-testid="mcp-readout">
						<dt>Public origin</dt>
						<dd>
							{#if rd.origin}
								<code>{rd.origin}</code>{#if rd.origin_var}<span class="env-note">from {rd.origin_var}</span>{/if}
							{:else if rd.problems.length > 0}
								not usable <span class="env-note">see above</span>
							{:else}
								not set <span class="env-note">set PAD_URL to the URL this server is reached at</span>
							{/if}
						</dd>
						<dt>MCP URL</dt>
						<dd>{#if rd.mcp_url}<code>{rd.mcp_url}</code>{:else}—{/if}</dd>
						<dt>https</dt>
						<dd>{rd.origin ? (rd.https ? 'Yes' : 'No') : '—'}</dd>
						<dt>Agents sign in with</dt>
						<dd data-testid="mcp-auth">{mcpAuthLabel(rd.auth_methods)}</dd>
					</dl>

					{#each rd.problems.filter((p) => p !== rd.blocked) as problem (problem)}
						<p class="save-msg error-msg" data-testid="mcp-problem">{problem}</p>
					{/each}

					{#if rd.state === 'off' && (rd.resume.oauth_connections > 0 || rd.resume.pats > 0)}
						<p class="privacy-line" data-testid="mcp-resume">
							Turning it on resumes {rd.resume.oauth_connections}
							{rd.resume.oauth_connections === 1 ? 'OAuth connection' : 'OAuth connections'} and {rd.resume.pats}
							{rd.resume.pats === 1 ? 'API token' : 'API tokens'}. Turning it off revoked nothing.
						</p>
					{/if}

					{#if rd.origin}
					<p class="privacy-line">
						This reports configuration, not an observation: the server cannot see TLS a proxy
						terminates. {rd.https ? 'An https URL on a server actually reached over http advertises OAuth that cannot complete.' : 'Over http, agents use API tokens only.'}
					</p>
					{/if}

					{#if mcpStatus}
						<span class="save-msg" class:error-msg={mcpStatus.type === 'error'}>{mcpStatus.message}</span>
					{/if}
				{/if}
			</div>
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
	.toggle-row.small {
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.env-note {
		font-size: 0.75rem;
		color: var(--text-muted);
		font-style: italic;
		margin-left: var(--space-2);
	}
	.privacy-line {
		font-size: 0.8rem;
		color: var(--text-secondary);
		margin: 0;
	}
	.error-msg {
		color: var(--accent-red, #d33);
	}
	.mcp-state {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85rem;
		color: var(--text-primary);
		margin: 0;
	}
	.mcp-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--text-muted);
		flex-shrink: 0;
	}
	.mcp-dot.on {
		background: var(--accent-green);
	}
	.mcp-dot.blocked {
		background: var(--accent-orange, var(--accent-red));
	}
	.mcp-readout {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: var(--space-1) var(--space-4);
		margin: 0;
		font-size: 0.82rem;
	}
	.mcp-readout dt {
		color: var(--text-secondary);
	}
	.mcp-readout dd {
		margin: 0;
		color: var(--text-primary);
		min-width: 0;
		overflow-wrap: anywhere;
	}
</style>
