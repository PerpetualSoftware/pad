<script lang="ts">
	import { onMount } from 'svelte';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { exportAndDownloadAccountData } from '$lib/utils/artifacts';
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

	// OAuth providers shown in Linked Accounts. `webLinkable` is whether the
	// provider can be LINKED from the browser: GitHub/Google have a
	// /auth/<provider>/link redirect flow; Apple does NOT — Sign in with
	// Apple is native-iOS-only (PLAN-1772), so there's no web link route to
	// point a button at. Apple still appears here (badge + Unlink) once it's
	// linked from the app; it's just never offered a "Link" button.
	const linkProviders = [
		{ id: 'github', name: 'GitHub', webLinkable: true },
		{ id: 'google', name: 'Google', webLinkable: true },
		{ id: 'apple', name: 'Apple', webLinkable: false }
	];

	function providerName(provider: string): string {
		return linkProviders.find((p) => p.id === provider)?.name ?? 'the provider';
	}

	async function unlinkProvider(provider: string) {
		providerMsg = '';
		providerError = '';
		try {
			await api.auth.unlinkProvider(provider);
			// Refresh profile to get updated providers list
			profile = await api.auth.me();
			providerMsg = `${providerName(provider)} unlinked.`;
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

	// Danger Zone — export my data (TASK-1961)
	let exportSaving = $state(false);
	let exportMsg = $state('');
	let exportError = $state('');

	// Danger Zone — delete my account (TASK-1962). Own $state trio, distinct from
	// the export trio above (per-action state, mirroring the file's convention).
	let showDeleteConfirm = $state(false);
	let deletePassword = $state('');
	let deleteConfirmText = $state('');
	let deleteTotpCode = $state('');
	let deleteSaving = $state(false);
	let deleteError = $state('');

	// Which identity check the confirm block demands. Email/password accounts
	// (and every self-host account) re-enter their password; cloud OAuth-only
	// accounts have no password, so they type a confirmation string instead.
	// `?? true` makes the password branch the SAFE FALLBACK while `password_set`
	// is still loading — an unknown password state must never downgrade to the
	// weaker typed-confirm path.
	const usePasswordBranch = $derived(!authStore.cloudMode || (profile?.password_set ?? true));

	// Focus the first confirm input the moment the block reveals (a11y). Same
	// requestAnimationFrame action the quick-capture sheet uses.
	function autofocus(node: HTMLElement) {
		requestAnimationFrame(() => node.focus());
	}

	let loading = $state(true);

	// Map pad-cloud's /console/settings?error= / ?linked= redirect codes to
	// the Linked Accounts section's provider banner. Kept near onMount so
	// the full list of codes the UI handles is easy to audit against
	// pad-cloud/oauth.go.
	function readOAuthQueryStatus() {
		if (typeof window === 'undefined') return;
		const url = new URL(window.location.href);
		const linked = url.searchParams.get('linked');
		const errCode = url.searchParams.get('error');
		const provider = url.searchParams.get('provider'); // optional hint

		if (linked) {
			providerMsg = `${providerName(linked)} linked.`;
		} else if (errCode) {
			const name = providerName(provider ?? '');
			switch (errCode) {
				case 'not_logged_in':
					providerError = 'Your session expired while linking. Sign in and try again.';
					break;
				case 'email_mismatch':
					providerError = `The ${name} account uses a different email than your Pad account. Sign into ${name} as your Pad email, then retry.`;
					break;
				case 'link_failed':
					providerError = `Couldn't link ${name}. Try again in a moment.`;
					break;
				default:
					// Unknown code — never break the page.
					providerError = 'Linking failed. Try again.';
					break;
			}
		} else {
			return;
		}

		// Strip ?linked / ?error / ?provider so a refresh / back-button
		// doesn't re-show the banner.
		url.searchParams.delete('linked');
		url.searchParams.delete('error');
		url.searchParams.delete('provider');
		history.replaceState(history.state, '', url.pathname + (url.search || '') + url.hash);
	}

	onMount(async () => {
		readOAuthQueryStatus();
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
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				tokenError = err instanceof Error ? err.message : 'Failed to create token';
			}
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

	// Export the current user's account data as a single JSON download
	// (TASK-1961). The stream runs under the server's 60s deadline, so keep
	// exportSaving true for the whole request to hold the button disabled +
	// in-flight. Surfaces the restricted-owner 403 (PadApiError.message) and
	// network/timeout failures inline.
	async function exportData() {
		exportError = '';
		exportMsg = '';
		exportSaving = true;
		try {
			await exportAndDownloadAccountData();
			exportMsg = 'Your data export has downloaded.';
		} catch (err) {
			exportError = err instanceof Error ? err.message : 'Failed to export your data. Please try again.';
		} finally {
			exportSaving = false;
		}
	}

	// Reveal the delete confirm block. Clears the action's own stale error so a
	// prior failed attempt doesn't linger over a fresh reveal.
	function revealDeleteConfirm() {
		showDeleteConfirm = true;
		deleteError = '';
	}

	// Cancel out of the confirm block (also the Escape-key target). Wipes every
	// secret the user may have typed so nothing lingers in memory / the DOM.
	function cancelDelete() {
		showDeleteConfirm = false;
		deletePassword = '';
		deleteConfirmText = '';
		deleteTotpCode = '';
		deleteError = '';
	}

	// Shared keydown for the confirm inputs: Escape cancels, Enter submits.
	function deleteKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			cancelDelete();
		} else if (e.key === 'Enter') {
			deleteAccount();
		}
	}

	// Permanently delete the account (TASK-1962). Builds the request from the
	// account's auth method: password for email/password + self-host users, a
	// typed confirmation for cloud OAuth-only users, plus a TOTP code whenever
	// 2FA is on (the server re-verifies it — TASK-1958). On success the server
	// has already cleared the session + CSRF cookies, so we HARD-reset to /login
	// (authStore.clear() then window.location) and make NO further API calls —
	// a goto() to an authed route would 401 against a now-deleted account.
	async function deleteAccount() {
		deleteError = '';

		const opts: { password?: string; confirm?: boolean; totp_code?: string } = {};
		if (usePasswordBranch) {
			if (!deletePassword) {
				deleteError = 'Please enter your password to delete your account.';
				return;
			}
			opts.password = deletePassword;
		} else {
			const typed = deleteConfirmText.trim();
			const matchesEmail = profile?.email ? typed === profile.email : false;
			const matchesWord = typed.toUpperCase() === 'DELETE';
			if (!matchesEmail && !matchesWord) {
				deleteError = `Type your email (${profile?.email ?? ''}) or the word DELETE to confirm.`;
				return;
			}
			opts.confirm = true;
		}

		if (profile?.totp_enabled) {
			const code = deleteTotpCode.trim();
			if (!code) {
				deleteError = 'Enter your 6-digit 2FA code to delete your account.';
				return;
			}
			opts.totp_code = code;
		}

		deleteSaving = true;
		try {
			await api.auth.deleteAccount(opts);
			// Success: hard reset, no further API calls.
			authStore.clear();
			window.location.href = '/login';
		} catch (err) {
			// Render the server message VERBATIM. billing_cancel_failed /
			// partial_delete carry account-state truth ("your account was NOT
			// deleted", "your billing was cancelled but…") that must NOT be
			// swallowed behind a generic retry line.
			deleteError = err instanceof Error ? err.message : 'Failed to delete your account. Please try again.';
			deleteSaving = false;
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
					{#each linkProviders as p (p.id)}
						{@const linked = profile?.oauth_providers?.includes(p.id) ?? false}
						<!-- Apple (and any non-web-linkable provider) only appears
						     once linked — there's no web flow to link it, so an
						     unlinked row with no action would just be noise. -->
						{#if p.webLinkable || linked}
							<div class="provider-row">
								<div class="provider-info">
									<span class="provider-name">{p.name}</span>
									{#if linked}
										<span class="provider-badge linked">Linked</span>
									{:else}
										<span class="provider-badge">Not linked</span>
									{/if}
								</div>
								{#if linked}
									<button class="delete-btn" onclick={() => unlinkProvider(p.id)}>Unlink</button>
								{:else if p.webLinkable}
									<a href="/auth/{p.id}/link" data-sveltekit-reload class="primary-btn small">Link {p.name}</a>
								{/if}
							</div>
						{/if}
					{/each}
					{#if providerMsg}<p class="success" role="status" aria-live="polite">{providerMsg}</p>{/if}
					{#if providerError}<p class="error" role="alert" aria-live="assertive">{providerError}</p>{/if}
				</div>
			</section>
		{/if}

		<!-- Plan (cloud mode only) — TASK-800 -->
		{#if authStore.cloudMode}
			<section class="card">
				<h2 class="card-title">Plan</h2>
				<div class="card-body">
					<div class="plan-row">
						<div class="plan-info">
							<span class="plan-name">
								{authStore.user?.plan === 'pro' ? 'Pro' : 'Free'}
							</span>
							<span class="plan-badge" class:pro={authStore.user?.plan === 'pro'} class:free={authStore.user?.plan !== 'pro'}>
								{authStore.user?.plan === 'pro' ? 'Active' : 'Current'}
							</span>
						</div>
						<p class="plan-desc">
							{#if authStore.user?.plan === 'pro'}
								You have full access to all Pad features.
							{:else}
								The free plan includes basic workspace management with limited features.
							{/if}
						</p>
					</div>
					{#if authStore.user?.plan !== 'pro'}
						<a href="/console/billing" class="primary-btn">
							{authStore.billingAvailable ? 'Upgrade to Pro' : 'View Plans'}
						</a>
					{:else}
						<a href="/console/billing" class="secondary-btn">Manage Billing</a>
					{/if}
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

		<!-- Danger Zone (TASK-1961) — ungated: data export works self-host too -->
		<section class="card danger-card">
			<h2 class="card-title danger-title">Danger Zone</h2>
			<div class="card-body">
				<div class="danger-row">
					<div class="danger-info">
						<span class="danger-heading">Export my data</span>
						<p class="danger-desc">
							Download all of your account data as a single JSON file. This may
							take a moment for large accounts.
						</p>
					</div>
					<button class="danger-btn" onclick={exportData} disabled={exportSaving}>
						{exportSaving ? 'Exporting...' : 'Export my data'}
					</button>
				</div>
				{#if exportMsg}
					<p class="success" role="status" aria-live="polite">{exportMsg}</p>
				{/if}
				{#if exportError}
					<p class="error" role="alert" aria-live="assertive">{exportError}</p>
				{/if}

				<hr class="danger-divider" />

				<!-- Delete my account (TASK-1962) -->
				<div class="danger-row">
					<div class="danger-info">
						<span class="danger-heading">Delete my account</span>
						<p class="danger-desc">
							Permanently delete your account and everything you own. This cannot be undone.
						</p>
					</div>
					{#if !showDeleteConfirm}
						<button class="danger-btn" onclick={revealDeleteConfirm}>Delete my account</button>
					{/if}
				</div>

				{#if showDeleteConfirm}
					<div class="disable-confirm">
						<ul id="delete-warnings" class="delete-warnings">
							<li>This is <strong>permanent and irreversible</strong>.</li>
							<li>It cancels any active paid subscription immediately, with no refund.</li>
							<li>
								Any workspaces you own that are shared with others are
								<strong>deleted</strong>, and every member loses access. There is no
								ownership transfer.
							</li>
						</ul>
						<p class="section-desc">
							Want a copy first? Use the
							<button type="button" class="inline-export-link" onclick={exportData} disabled={exportSaving || deleteSaving}>
								{exportSaving ? 'exporting your data…' : 'Export my data'}
							</button>
							button above before you delete.
						</p>

						{#if usePasswordBranch}
							<div class="field">
								<label for="delete-password">Enter your password to confirm</label>
								<input
									id="delete-password"
									type="password"
									bind:value={deletePassword}
									disabled={deleteSaving}
									autocomplete="current-password"
									aria-describedby="delete-warnings"
									onkeydown={deleteKeydown}
									use:autofocus
								/>
							</div>
						{:else}
							<div class="field">
								<label for="delete-confirm">
									Type your email ({profile?.email ?? ''}) or the word <strong>DELETE</strong> to confirm
								</label>
								<input
									id="delete-confirm"
									type="text"
									placeholder="DELETE"
									bind:value={deleteConfirmText}
									disabled={deleteSaving}
									autocomplete="off"
									aria-describedby="delete-warnings"
									onkeydown={deleteKeydown}
									use:autofocus
								/>
							</div>
						{/if}

						{#if profile?.totp_enabled}
							<div class="field">
								<label for="delete-totp">2FA code</label>
								<input
									id="delete-totp"
									type="text"
									placeholder="6-digit code"
									bind:value={deleteTotpCode}
									disabled={deleteSaving}
									autocomplete="one-time-code"
									inputmode="numeric"
									maxlength="6"
									aria-describedby="delete-warnings"
									onkeydown={deleteKeydown}
								/>
							</div>
						{/if}

						{#if deleteError}
							<p class="error" role="alert" aria-live="assertive">{deleteError}</p>
						{/if}

						<div class="btn-row">
							<button class="danger-btn" onclick={deleteAccount} disabled={deleteSaving}>
								{deleteSaving ? 'Deleting...' : 'Permanently delete my account'}
							</button>
							<button class="secondary-btn" onclick={cancelDelete} disabled={deleteSaving}>Cancel</button>
						</div>
					</div>
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
		color: var(--accent-red);
		font-size: 0.85rem;
	}

	.success {
		color: var(--accent-green);
		font-size: 0.85rem;
	}

	/* Plan section (TASK-800) */
	.plan-row {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.plan-info {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.plan-name {
		font-size: 1rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.plan-badge {
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
	}

	.plan-badge.pro {
		background: color-mix(in srgb, var(--accent-green) 15%, transparent);
		color: var(--accent-green);
	}

	.plan-badge.free {
		background: color-mix(in srgb, var(--accent-gray, #6b7280) 15%, transparent);
		color: var(--text-secondary);
	}

	.plan-desc {
		color: var(--text-secondary);
		font-size: 0.85rem;
		line-height: 1.4;
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
		color: var(--accent-red);
		border-color: var(--accent-red);
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
		border: 1px solid var(--accent-red);
		border-radius: var(--radius);
		color: var(--accent-red);
		font-size: 0.85rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: background 0.15s;
	}

	.danger-btn:hover:not(:disabled) {
		background: color-mix(in srgb, var(--accent-red) 10%, transparent);
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

	/* Danger Zone (TASK-1961) — red header/border per the danger palette */
	.danger-card {
		border-color: color-mix(in srgb, var(--accent-red) 40%, var(--border));
	}

	.danger-title {
		color: var(--accent-red);
		border-bottom-color: color-mix(in srgb, var(--accent-red) 40%, var(--border));
	}

	.danger-row {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		flex-wrap: wrap;
	}

	.danger-info {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.danger-heading {
		font-size: 0.9rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.danger-desc {
		font-size: 0.85rem;
		color: var(--text-muted);
	}

	/* Divider between the export row and the delete-account block (TASK-1962) */
	.danger-divider {
		border: none;
		border-top: 1px solid var(--border);
		margin: 0;
	}

	.delete-warnings {
		margin: 0;
		padding-left: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: 0.85rem;
		color: var(--text-secondary);
		line-height: 1.4;
	}

	.delete-warnings strong {
		color: var(--accent-red);
	}

	/* Inline "Export my data" link inside the delete confirm nudge (TASK-1962) */
	.inline-export-link {
		background: none;
		border: none;
		padding: 0;
		font: inherit;
		color: var(--accent-blue);
		cursor: pointer;
		text-decoration: underline;
	}

	.inline-export-link:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}
</style>
