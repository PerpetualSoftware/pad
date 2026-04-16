<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import type { User, APIToken, APITokenWithSecret, TOTPSetupResponse } from '$lib/types';

	// Profile
	let profile = $state<User | null>(null);
	let profileName = $state('');
	let profileUsername = $state('');
	let profileSaving = $state(false);
	let profileMsg = $state('');
	let profileError = $state('');

	// Password
	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let passwordSaving = $state(false);
	let passwordMsg = $state('');
	let passwordError = $state('');

	// OAuth providers
	let providerMsg = $state('');
	let providerError = $state('');

	// 2FA
	let totpStep = $state<'idle' | 'setup' | 'verify' | 'recovery'>('idle');
	let totpSetup = $state<TOTPSetupResponse | null>(null);
	let totpCode = $state('');
	let totpSaving = $state(false);
	let totpMsg = $state('');
	let totpError = $state('');
	let recoveryCodes = $state<string[]>([]);
	let disablePassword = $state('');
	let showDisableConfirm = $state(false);

	async function unlinkProvider(provider: string) {
		providerMsg = '';
		providerError = '';
		try {
			await api.auth.unlinkProvider(provider);
			// Refresh profile to get updated providers list
			profile = await api.auth.me();
			providerMsg = `${provider === 'github' ? 'GitHub' : 'Google'} unlinked.`;
		} catch (err) {
			providerError = err instanceof Error ? err.message : 'Failed to unlink provider';
		}
	}

	// Tokens
	let tokens = $state<APIToken[]>([]);
	let newTokenName = $state('');
	let createdToken = $state<APITokenWithSecret | null>(null);
	let tokenCreating = $state(false);
	let tokenError = $state('');

	let loading = $state(true);

	onMount(async () => {
		try {
			const [me, tokenList] = await Promise.all([
				api.auth.me(),
				api.auth.tokens.list()
			]);
			profile = me;
			profileName = me.name;
			profileUsername = me.username;
			tokens = tokenList;
		} catch {
			profileError = 'Failed to load profile';
		} finally {
			loading = false;
		}
	});

	async function saveProfile() {
		profileError = '';
		profileMsg = '';
		profileSaving = true;
		try {
			const updated = await api.auth.updateProfile({
				name: profileName.trim(),
				username: profileUsername.trim()
			});
			profile = updated;
			profileMsg = 'Profile updated.';
			await authStore.load();
		} catch (err) {
			profileError = err instanceof Error ? err.message : 'Failed to update profile';
		} finally {
			profileSaving = false;
		}
	}

	async function changePassword() {
		passwordError = '';
		passwordMsg = '';
		if (!currentPassword || !newPassword) {
			passwordError = 'Please fill in all password fields.';
			return;
		}
		if (newPassword !== confirmPassword) {
			passwordError = 'New passwords do not match.';
			return;
		}
		if (newPassword.length < 8) {
			passwordError = 'Password must be at least 8 characters.';
			return;
		}

		passwordSaving = true;
		try {
			await api.auth.updateProfile({
				current_password: currentPassword,
				new_password: newPassword
			});
			passwordMsg = 'Password changed successfully.';
			currentPassword = '';
			newPassword = '';
			confirmPassword = '';
		} catch (err) {
			passwordError = err instanceof Error ? err.message : 'Failed to change password';
		} finally {
			passwordSaving = false;
		}
	}

	async function startTOTPSetup() {
		totpError = '';
		totpMsg = '';
		totpSaving = true;
		try {
			totpSetup = await api.auth.totp.setup();
			totpStep = 'setup';
		} catch (err) {
			totpError = err instanceof Error ? err.message : 'Failed to start 2FA setup';
			totpStep = 'idle';
			return;
		} finally {
			totpSaving = false;
		}

		// Render QR code separately — failure here should not abort setup
		// since the user can still enter the secret manually
		try {
			const QRCode = (await import('qrcode')).default;
			await new Promise(r => setTimeout(r, 50));
			const el = document.getElementById('qr-canvas') as HTMLCanvasElement;
			if (el) {
				await QRCode.toCanvas(el, totpSetup.url, { width: 200, margin: 2 });
			}
		} catch {
			// QR rendering failed — manual entry still available
		}
	}

	async function renderQRCode() {
		if (!totpSetup) return;
		try {
			const QRCode = (await import('qrcode')).default;
			const el = document.getElementById('qr-canvas') as HTMLCanvasElement;
			if (el) {
				await QRCode.toCanvas(el, totpSetup.url, { width: 200, margin: 2 });
			}
		} catch {}
	}

	async function verifyTOTP() {
		totpError = '';
		const code = totpCode.trim();
		if (!code || !totpSetup) {
			totpError = 'Please enter the 6-digit code from your authenticator app.';
			return;
		}

		totpSaving = true;
		try {
			const result = await api.auth.totp.verify(code, totpSetup.secret);
			recoveryCodes = result.recovery_codes;
			totpStep = 'recovery';
			totpCode = '';
			// Refresh profile to get updated totp_enabled
			profile = await api.auth.me();
		} catch (err) {
			totpError = err instanceof Error ? err.message : 'Invalid code. Please try again.';
		} finally {
			totpSaving = false;
		}
	}

	async function disableTOTP() {
		totpError = '';
		if (!disablePassword) {
			totpError = 'Please enter your password to disable 2FA.';
			return;
		}

		totpSaving = true;
		try {
			await api.auth.totp.disable(disablePassword);
			totpMsg = 'Two-factor authentication has been disabled.';
			showDisableConfirm = false;
			disablePassword = '';
			totpStep = 'idle';
			totpSetup = null;
			recoveryCodes = [];
			// Refresh profile
			profile = await api.auth.me();
		} catch (err) {
			totpError = err instanceof Error ? err.message : 'Failed to disable 2FA';
		} finally {
			totpSaving = false;
		}
	}

	function finishSetup() {
		totpStep = 'idle';
		totpSetup = null;
		recoveryCodes = [];
		totpMsg = 'Two-factor authentication is now enabled.';
	}

	function cancelSetup() {
		totpStep = 'idle';
		totpSetup = null;
		totpCode = '';
		totpError = '';
		recoveryCodes = [];
	}

	async function copyRecoveryCodes() {
		const text = recoveryCodes.join('\n');
		const ok = await copyToClipboard(text);
		if (ok) {
			totpMsg = 'Recovery codes copied to clipboard.';
		} else {
			totpError = 'Failed to copy — please select and copy the codes manually.';
		}
	}

	function downloadRecoveryCodes() {
		const text = recoveryCodes.join('\n');
		const blob = new Blob([text], { type: 'text/plain' });
		const url = URL.createObjectURL(blob);
		const a = document.createElement('a');
		a.href = url;
		a.download = 'pad-recovery-codes.txt';
		a.click();
		URL.revokeObjectURL(url);
	}

	async function createToken() {
		tokenError = '';
		createdToken = null;
		if (!newTokenName.trim()) {
			tokenError = 'Please enter a token name.';
			return;
		}

		tokenCreating = true;
		try {
			const token = await api.auth.tokens.create(newTokenName.trim());
			createdToken = token;
			tokens = [...tokens, token];
			newTokenName = '';
		} catch (err) {
			tokenError = err instanceof Error ? err.message : 'Failed to create token';
		} finally {
			tokenCreating = false;
		}
	}

	async function deleteToken(tokenId: string) {
		try {
			await api.auth.tokens.delete(tokenId);
			tokens = tokens.filter((t) => t.id !== tokenId);
		} catch {
			// Silent failure acceptable for delete
		}
	}

	function formatDate(dateStr: string): string {
		return new Date(dateStr).toLocaleDateString('en-US', {
			month: 'short', day: 'numeric', year: 'numeric'
		});
	}
</script>

<svelte:head>
	<title>Settings - Pad</title>
</svelte:head>

<div class="settings-page">
	<h1 class="page-title">Account Settings</h1>

	{#if loading}
		<div class="loading">Loading...</div>
	{:else}
		<!-- Profile -->
		<section class="card">
			<h2 class="card-title">Profile</h2>
			<div class="card-body">
				<div class="field">
					<label for="profile-name">Name</label>
					<input id="profile-name" type="text" bind:value={profileName} disabled={profileSaving} />
				</div>
				<div class="field">
					<label for="profile-username">Username</label>
					<input id="profile-username" type="text" bind:value={profileUsername} disabled={profileSaving} />
				</div>
				<div class="field">
					<label for="profile-email">Email</label>
					<input id="profile-email" type="email" value={profile?.email ?? ''} disabled readonly />
				</div>
				{#if profileError}
					<p class="error">{profileError}</p>
				{/if}
				{#if profileMsg}
					<p class="success">{profileMsg}</p>
				{/if}
				<button class="primary-btn" onclick={saveProfile} disabled={profileSaving}>
					{profileSaving ? 'Saving...' : 'Save Changes'}
				</button>
			</div>
		</section>

		<!-- Password -->
		<section class="card">
			<h2 class="card-title">Password</h2>
			<div class="card-body">
				<div class="field">
					<label for="current-pw">Current password</label>
					<input id="current-pw" type="password" bind:value={currentPassword} disabled={passwordSaving} autocomplete="current-password" />
				</div>
				<div class="field">
					<label for="new-pw">New password</label>
					<input id="new-pw" type="password" bind:value={newPassword} disabled={passwordSaving} autocomplete="new-password" />
				</div>
				<div class="field">
					<label for="confirm-pw">Confirm new password</label>
					<input id="confirm-pw" type="password" bind:value={confirmPassword} disabled={passwordSaving} autocomplete="new-password" />
				</div>
				{#if passwordError}
					<p class="error">{passwordError}</p>
				{/if}
				{#if passwordMsg}
					<p class="success">{passwordMsg}</p>
				{/if}
				<button class="primary-btn" onclick={changePassword} disabled={passwordSaving}>
					{passwordSaving ? 'Changing...' : 'Change Password'}
				</button>
			</div>
		</section>

		<!-- Two-Factor Authentication -->
		<section class="card">
			<h2 class="card-title">Two-Factor Authentication</h2>
			<div class="card-body">
				{#if totpStep === 'idle'}
					<div class="totp-status-row">
						<div class="totp-status-info">
							<span class="totp-label">Status</span>
							{#if profile?.totp_enabled}
								<span class="totp-badge enabled">Enabled</span>
							{:else}
								<span class="totp-badge">Disabled</span>
							{/if}
						</div>
						{#if profile?.totp_enabled}
							{#if showDisableConfirm}
								<div class="disable-confirm">
									<p class="section-desc">Enter your password to disable 2FA.</p>
									<div class="field">
										<input
											type="password"
											placeholder="Current password"
											bind:value={disablePassword}
											disabled={totpSaving}
											autocomplete="current-password"
										/>
									</div>
									{#if totpError}
										<p class="error">{totpError}</p>
									{/if}
									<div class="btn-row">
										<button class="danger-btn" onclick={disableTOTP} disabled={totpSaving}>
											{totpSaving ? 'Disabling...' : 'Confirm Disable'}
										</button>
										<button class="secondary-btn" onclick={() => { showDisableConfirm = false; disablePassword = ''; totpError = ''; }} disabled={totpSaving}>
											Cancel
										</button>
									</div>
								</div>
							{:else}
								<button class="danger-btn" onclick={() => { showDisableConfirm = true; totpError = ''; totpMsg = ''; }}>
									Disable 2FA
								</button>
							{/if}
						{:else}
							<button class="primary-btn" onclick={startTOTPSetup} disabled={totpSaving}>
								{totpSaving ? 'Setting up...' : 'Enable 2FA'}
							</button>
						{/if}
					</div>
					{#if totpMsg}
						<p class="success">{totpMsg}</p>
					{/if}
					{#if totpError && !showDisableConfirm}
						<p class="error">{totpError}</p>
					{/if}

				{:else if totpStep === 'setup'}
					<p class="section-desc">Scan this QR code with your authenticator app (Google Authenticator, Authy, 1Password, etc.)</p>
					<div class="qr-container">
						<canvas id="qr-canvas"></canvas>
					</div>
					{#if totpSetup}
						<div class="manual-entry">
							<p class="section-desc">Or enter this code manually:</p>
							<code class="secret-code">{totpSetup.secret}</code>
						</div>
					{/if}
					<div class="field">
						<label for="totp-verify">Verification code</label>
						<input
							id="totp-verify"
							type="text"
							placeholder="Enter 6-digit code"
							bind:value={totpCode}
							disabled={totpSaving}
							autocomplete="one-time-code"
							inputmode="numeric"
							maxlength="6"
							onkeydown={(e) => { if (e.key === 'Enter') verifyTOTP(); }}
						/>
					</div>
					{#if totpError}
						<p class="error">{totpError}</p>
					{/if}
					<div class="btn-row">
						<button class="primary-btn" onclick={verifyTOTP} disabled={totpSaving || !totpCode.trim()}>
							{totpSaving ? 'Verifying...' : 'Verify & Enable'}
						</button>
						<button class="secondary-btn" onclick={cancelSetup} disabled={totpSaving}>Cancel</button>
					</div>

				{:else if totpStep === 'recovery'}
					<div class="recovery-section">
						<p class="section-desc"><strong>Save your recovery codes.</strong> Each code can only be used once. Store them in a safe place — you will not see them again.</p>
						<div class="recovery-codes">
							{#each recoveryCodes as code, i (i)}
								<code class="recovery-code">{code}</code>
							{/each}
						</div>
						<div class="btn-row">
							<button class="secondary-btn" onclick={copyRecoveryCodes}>Copy</button>
							<button class="secondary-btn" onclick={downloadRecoveryCodes}>Download</button>
						</div>
						{#if totpMsg}
							<p class="success">{totpMsg}</p>
						{/if}
						<button class="primary-btn" onclick={finishSetup}>Done</button>
					</div>
				{/if}
			</div>
		</section>

		<!-- Linked Accounts (cloud mode only) -->
		{#if authStore.cloudMode}
			<section class="card">
				<h2 class="card-title">Linked Accounts</h2>
				<div class="card-body">
					<p class="section-desc">Link OAuth providers for single sign-on. You can sign in with any linked provider.</p>
					{#each ['github', 'google'] as provider (provider)}
						{@const linked = profile?.oauth_providers?.includes(provider) ?? false}
						<div class="provider-row">
							<div class="provider-info">
								<span class="provider-name">{provider === 'github' ? 'GitHub' : 'Google'}</span>
								{#if linked}
									<span class="provider-badge linked">Linked</span>
								{:else}
									<span class="provider-badge">Not linked</span>
								{/if}
							</div>
							{#if linked}
								<button class="delete-btn" onclick={() => unlinkProvider(provider)}>Unlink</button>
							{:else}
								<a href="/auth/{provider}/link" data-sveltekit-reload class="primary-btn small">Link {provider === 'github' ? 'GitHub' : 'Google'}</a>
							{/if}
						</div>
					{/each}
					{#if providerMsg}<p class="success">{providerMsg}</p>{/if}
					{#if providerError}<p class="error">{providerError}</p>{/if}
				</div>
			</section>
		{/if}

		<!-- API Tokens -->
		<section class="card">
			<h2 class="card-title">API Tokens</h2>
			<div class="card-body">
				{#if createdToken}
					<div class="token-created">
						<p class="token-warning">Copy this token now. It will not be shown again.</p>
						<code class="token-value">{createdToken.token}</code>
					</div>
				{/if}

				<div class="token-create-row">
					<input
						type="text"
						placeholder="Token name"
						bind:value={newTokenName}
						disabled={tokenCreating}
					/>
					<button class="primary-btn" onclick={createToken} disabled={tokenCreating || !newTokenName.trim()}>
						{tokenCreating ? 'Creating...' : 'Create'}
					</button>
				</div>

				{#if tokenError}
					<p class="error">{tokenError}</p>
				{/if}

				{#if tokens.length > 0}
					<div class="token-list">
						{#each tokens as token (token.id)}
							<div class="token-row">
								<div class="token-info">
									<span class="token-name">{token.name}</span>
									<span class="token-meta">
										{token.prefix}... &middot; Created {formatDate(token.created_at)}
										{#if token.expires_at}
											&middot; Expires {formatDate(token.expires_at)}
										{/if}
									</span>
								</div>
								<button class="delete-btn" onclick={() => deleteToken(token.id)}>Delete</button>
							</div>
						{/each}
					</div>
				{:else}
					<p class="empty-text">No API tokens yet.</p>
				{/if}
			</div>
		</section>
	{/if}
</div>

<style>
	.settings-page {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
		max-width: 600px;
	}

	.page-title {
		font-size: 1.4rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.loading {
		color: var(--text-muted);
		padding: var(--space-10) 0;
		text-align: center;
	}

	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		overflow: hidden;
	}

	.card-title {
		font-size: 0.95rem;
		font-weight: 600;
		color: var(--text-primary);
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}

	.card-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	label {
		font-size: 0.8rem;
		font-weight: 500;
		color: var(--text-muted);
	}

	input {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.9rem;
		font-family: var(--font-ui);
		outline: none;
		transition: border-color 0.15s;
	}

	input:focus {
		border-color: var(--accent-blue);
	}

	input:disabled {
		opacity: 0.6;
	}

	input[readonly] {
		color: var(--text-muted);
		cursor: not-allowed;
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
	}

	.success {
		color: var(--accent-green);
		font-size: 0.85rem;
	}

	.primary-btn {
		align-self: flex-start;
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.85rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}

	.primary-btn:hover:not(:disabled) {
		opacity: 0.9;
	}

	.primary-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.token-created {
		padding: var(--space-3) var(--space-4);
		background: color-mix(in srgb, var(--accent-green) 10%, var(--bg-tertiary));
		border: 1px solid color-mix(in srgb, var(--accent-green) 30%, transparent);
		border-radius: var(--radius);
	}

	.token-warning {
		font-size: 0.8rem;
		font-weight: 500;
		color: var(--accent-green);
		margin-bottom: var(--space-2);
	}

	.token-value {
		display: block;
		font-family: var(--font-mono);
		font-size: 0.8rem;
		color: var(--text-primary);
		word-break: break-all;
	}

	.token-create-row {
		display: flex;
		gap: var(--space-2);
	}

	.token-create-row input {
		flex: 1;
	}

	.token-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.token-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}

	.token-info {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.token-name {
		font-weight: 500;
		font-size: 0.85rem;
		color: var(--text-primary);
	}

	.token-meta {
		font-size: 0.75rem;
		color: var(--text-muted);
	}

	.delete-btn {
		padding: var(--space-1) var(--space-3);
		background: transparent;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.75rem;
		cursor: pointer;
		flex-shrink: 0;
		transition: color 0.15s, border-color 0.15s;
	}

	.delete-btn:hover {
		color: #ef4444;
		border-color: #ef4444;
	}

	.empty-text {
		color: var(--text-muted);
		font-size: 0.85rem;
	}

	.provider-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
	}

	.provider-info {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.provider-name {
		font-weight: 500;
		font-size: 0.9rem;
		color: var(--text-primary);
	}

	.provider-badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
	}

	.provider-badge.linked {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.section-desc {
		font-size: 0.8rem;
		color: var(--text-muted);
		margin-top: calc(-1 * var(--space-2));
	}

	.primary-btn.small {
		padding: var(--space-1) var(--space-3);
		font-size: 0.8rem;
		text-decoration: none;
	}

	.totp-status-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.totp-status-info {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.totp-label {
		font-size: 0.85rem;
		color: var(--text-muted);
	}

	.totp-badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		background: color-mix(in srgb, var(--accent-gray, #888) 15%, transparent);
		color: var(--text-muted);
	}

	.totp-badge.enabled {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.qr-container {
		display: flex;
		justify-content: center;
		padding: var(--space-4) 0;
	}

	.qr-container canvas {
		border-radius: var(--radius);
	}

	.manual-entry {
		text-align: center;
	}

	.secret-code {
		display: inline-block;
		font-family: var(--font-mono);
		font-size: 0.85rem;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		letter-spacing: 0.05em;
		word-break: break-all;
		user-select: all;
	}

	.recovery-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.recovery-codes {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-2);
	}

	.recovery-code {
		font-family: var(--font-mono);
		font-size: 0.85rem;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-sm);
		text-align: center;
	}

	.btn-row {
		display: flex;
		gap: var(--space-2);
	}

	.secondary-btn {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.85rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: background 0.15s;
	}

	.secondary-btn:hover:not(:disabled) {
		background: var(--bg-hover);
	}

	.danger-btn {
		align-self: flex-start;
		padding: var(--space-2) var(--space-4);
		background: transparent;
		border: 1px solid #ef4444;
		border-radius: var(--radius);
		color: #ef4444;
		font-size: 0.85rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: background 0.15s;
	}

	.danger-btn:hover:not(:disabled) {
		background: color-mix(in srgb, #ef4444 10%, transparent);
	}

	.danger-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.disable-confirm {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
</style>
