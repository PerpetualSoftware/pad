<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';
	import LegalFooter from '$lib/components/auth/LegalFooter.svelte';
	import SupportFooter from '$lib/components/auth/SupportFooter.svelte';

	let email = $state('');
	let password = $state('');
	let error = $state('');
	let setupRequired = $state(false);
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | undefined>(undefined);
	let loading = $state(false);

	let cloudMode = $state(false);
	let step = $state<'credentials' | '2fa'>('credentials');
	let challengeToken = $state('');
	let totpCode = $state('');

	// Banner for ?error= redirects coming back from pad-cloud's OAuth
	// handlers. Distinct from `error` above (which is driven by form
	// submissions) so a stray form-error assignment doesn't clobber the
	// OAuth banner and vice versa.
	let oauthBanner = $state<
		| { kind: 'not_linked'; provider: string | null }
		| { kind: 'generic'; message: string; tone: 'error' | 'info' }
		| null
	>(null);

	function getRedirectTarget(): string {
		const redirect = $page.url.searchParams.get('redirect');
		// Only allow relative redirects (prevent open redirect). A bare
		// `startsWith('/')` is NOT enough: protocol-relative URLs like
		// `//evil.example` and `/\evil.example` pass that check but are
		// treated by browsers (and most server-side redirect handlers) as
		// cross-origin destinations. Reject those explicitly.
		if (
			redirect &&
			redirect.startsWith('/') &&
			!redirect.startsWith('//') &&
			!redirect.startsWith('/\\')
		) {
			return redirect;
		}
		return '/console';
	}

	const oauthRedirectQuery = $derived.by(() => {
		const target = getRedirectTarget();
		if (target === '/console') return '';
		return `?redirect=${encodeURIComponent(target)}`;
	});

	// Same target appended with `&` so it composes with `?force=1` on
	// the "Use a different <provider> account" banner buttons. Empty
	// when redirect is the default destination so we don't append a
	// redundant `&redirect=%2Fconsole`.
	const oauthRedirectAmpQuery = $derived.by(() => {
		const target = getRedirectTarget();
		if (target === '/console') return '';
		return `&redirect=${encodeURIComponent(target)}`;
	});

	// Map pad-cloud's /login?error=... redirect codes to a friendly banner
	// plus optional CTAs. Kept near onMount so the full list of codes the
	// frontend handles is easy to audit against pad-cloud/oauth.go.
	function readOAuthErrorFromQuery() {
		if (typeof window === 'undefined') return;
		const url = new URL(window.location.href);
		const code = url.searchParams.get('error');
		const provider = url.searchParams.get('provider'); // optional hint from pad-cloud
		if (!code) return;

		switch (code) {
			case 'oauth_provider_not_linked':
				oauthBanner = {
					kind: 'not_linked',
					provider: provider === 'github' || provider === 'google' ? provider : null
				};
				break;
			case 'oauth_failed':
				oauthBanner = {
					kind: 'generic',
					tone: 'error',
					message: "We couldn't finish the sign-in. Please try again."
				};
				break;
			case 'no_email':
				oauthBanner = {
					kind: 'generic',
					tone: 'error',
					message:
						"Your OAuth provider didn't return a verified email. Verify your email with the provider, then try again."
				};
				break;
			case 'too_many_attempts':
				oauthBanner = {
					kind: 'generic',
					tone: 'error',
					message: 'Too many sign-in attempts. Wait a few minutes, then try again.'
				};
				break;
			case 'account_disabled':
				oauthBanner = {
					kind: 'generic',
					tone: 'error',
					message: 'Your account is disabled. Contact an administrator to restore access.'
				};
				break;
			default:
				// Unknown code — never break the page, just surface a safe fallback.
				oauthBanner = {
					kind: 'generic',
					tone: 'error',
					message: 'Sign-in failed. Please try again.'
				};
				break;
		}

		// Strip ?error= (and ?provider=) from the URL so a refresh or back-
		// button doesn't re-show the banner.
		url.searchParams.delete('error');
		url.searchParams.delete('provider');
		history.replaceState(history.state, '', url.pathname + (url.search || '') + url.hash);
	}

	function dismissOAuthBanner() {
		oauthBanner = null;
	}

	onMount(async () => {
		readOAuthErrorFromQuery();
		try {
			const session = await api.auth.session();
			cloudMode = session.cloud_mode ?? false;
			if (session.setup_required) {
				setupRequired = true;
				setupMethod = session.setup_method;
				return;
			}
			if (session.authenticated) {
				goto(getRedirectTarget(), { replaceState: true });
				return;
			}
		} catch {}
	});

	async function handleSubmit() {
		error = '';
		if (!email) {
			error = 'Please enter your email.';
			return;
		}
		if (!password) {
			error = 'Please enter your password.';
			return;
		}

		loading = true;
		try {
			const response = await api.auth.login(email, password);

			if (response.requires_2fa && response.challenge_token) {
				challengeToken = response.challenge_token;
				step = '2fa';
				error = '';
				return;
			}

			await authStore.load();
			await goto(getRedirectTarget(), { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Invalid email or password.';
			} else {
				error = 'Invalid email or password.';
			}
		} finally {
			loading = false;
		}
	}

	async function handleVerify2FA() {
		error = '';
		const code = totpCode.trim();

		if (!code) {
			error = 'Please enter your authentication code.';
			return;
		}

		loading = true;
		try {
			const isTotp = /^\d{6}$/.test(code);

			if (isTotp) {
				await api.auth.verify2FA(challengeToken, code, undefined);
			} else {
				await api.auth.verify2FA(challengeToken, undefined, code);
			}

			await authStore.load();
			await goto(getRedirectTarget(), { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Invalid code. Please try again.';
			} else {
				error = 'Invalid code. Please try again.';
			}
		} finally {
			loading = false;
		}
	}

	function handleBack() {
		step = 'credentials';
		challengeToken = '';
		totpCode = '';
		error = '';
	}

	function handleKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter') {
			if (step === 'credentials') {
				handleSubmit();
			} else {
				handleVerify2FA();
			}
		}
	}
</script>

<div class="login-page">
	<div class="login-card">
		<h1 class="logo">Pad</h1>

		{#if oauthBanner}
			<div
				class="oauth-banner"
				class:error-tone={oauthBanner.kind === 'not_linked' || (oauthBanner.kind === 'generic' && oauthBanner.tone === 'error')}
				role="alert"
				aria-live="polite"
			>
				<button type="button" class="oauth-banner-close" onclick={dismissOAuthBanner} aria-label="Dismiss">&times;</button>
				{#if oauthBanner.kind === 'not_linked'}
					<p class="oauth-banner-msg">
						{#if oauthBanner.provider === 'github'}
							That GitHub account isn't linked to a Pad account.
						{:else if oauthBanner.provider === 'google'}
							That Google account isn't linked to a Pad account.
						{:else}
							That OAuth account isn't linked to a Pad account.
						{/if}
						Sign in with your password below, or retry with a different account.
					</p>
					<div class="oauth-banner-actions">
						{#if oauthBanner.provider === 'github' || oauthBanner.provider === null}
							<a
								href="/auth/github?force=1{oauthRedirectAmpQuery}"
								data-sveltekit-reload
								class="oauth-banner-btn"
							>
								Use a different GitHub account
							</a>
						{/if}
						{#if oauthBanner.provider === 'google' || oauthBanner.provider === null}
							<a
								href="/auth/google?force=1{oauthRedirectAmpQuery}"
								data-sveltekit-reload
								class="oauth-banner-btn"
							>
								Use a different Google account
							</a>
						{/if}
					</div>
				{:else}
					<p class="oauth-banner-msg">{oauthBanner.message}</p>
				{/if}
			</div>
		{/if}

		{#if setupRequired}
			<SetupRequiredNotice
				{setupMethod}
				nextStep="Once setup is complete, return here to sign in."
			/>
		{:else if step === '2fa'}
			<p class="subtitle">Two-factor authentication</p>

			<div class="form">
				<p class="hint">Enter the 6-digit code from your authenticator app, or a recovery code.</p>

				<input
					type="text"
					placeholder="Authentication code"
					bind:value={totpCode}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="one-time-code"
					inputmode="numeric"
				/>

				{#if error}
					<p class="error">{error}</p>
				{/if}

				<button onclick={handleVerify2FA} disabled={loading}>
					{#if loading}
						Verifying...
					{:else}
						Verify
					{/if}
				</button>

				<button class="back-button" onclick={handleBack} disabled={loading} type="button">
					Back to sign in
				</button>
			</div>
		{:else}
			<p class="subtitle">Sign in to continue</p>

			<div class="form">
				<input
					type="email"
					placeholder="Email"
					bind:value={email}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="email"
				/>

				<input
					type="password"
					placeholder="Password"
					bind:value={password}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="current-password"
				/>

				{#if error}
					<p class="error">{error}</p>
				{/if}

				<button onclick={handleSubmit} disabled={loading}>
					{#if loading}
						Signing in...
					{:else}
						Sign in
					{/if}
				</button>
			</div>

			{#if cloudMode}
				<div class="oauth-divider">
					<span>or</span>
				</div>

				<div class="oauth-buttons">
					<a href="/auth/github{oauthRedirectQuery}" data-sveltekit-reload class="oauth-btn oauth-github">
						<svg width="18" height="18" viewBox="0 0 16 16" fill="currentColor"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>
						Continue with GitHub
					</a>
					<a href="/auth/google{oauthRedirectQuery}" data-sveltekit-reload class="oauth-btn oauth-google">
						<svg width="18" height="18" viewBox="0 0 18 18" fill="none"><path d="M17.64 9.2c0-.637-.057-1.251-.164-1.84H9v3.481h4.844a4.14 4.14 0 01-1.796 2.716v2.259h2.908c1.702-1.567 2.684-3.875 2.684-6.615z" fill="#4285F4"/><path d="M9 18c2.43 0 4.467-.806 5.956-2.18l-2.908-2.259c-.806.54-1.837.86-3.048.86-2.344 0-4.328-1.584-5.036-3.711H.957v2.332A8.997 8.997 0 009 18z" fill="#34A853"/><path d="M3.964 10.71A5.41 5.41 0 013.682 9c0-.593.102-1.17.282-1.71V4.958H.957A8.996 8.996 0 000 9c0 1.452.348 2.827.957 4.042l3.007-2.332z" fill="#FBBC05"/><path d="M9 3.58c1.321 0 2.508.454 3.44 1.345l2.582-2.58C13.463.891 11.426 0 9 0A8.997 8.997 0 00.957 4.958L3.964 7.29C4.672 5.163 6.656 3.58 9 3.58z" fill="#EA4335"/></svg>
						Continue with Google
					</a>
				</div>
			{/if}

			<p class="register-link">
				<a href="/forgot-password">Forgot password?</a>
			</p>
			{#if cloudMode}
				<p class="register-link">
					Don't have an account? <a href="/register">Sign up</a>
				</p>
			{:else}
				<p class="register-link">
					Need an account? Ask your admin for an invitation link.
				</p>
			{/if}
		{/if}
	</div>

	<LegalFooter {cloudMode} />
	<SupportFooter {cloudMode} />
</div>

<style>
	.login-page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.login-card {
		width: 100%;
		max-width: 360px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		padding: var(--space-10) var(--space-8);
		text-align: center;
	}

	.logo {
		font-size: 2rem;
		font-weight: 700;
		color: var(--text-primary);
		letter-spacing: -0.02em;
		margin-bottom: var(--space-2);
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.9rem;
		margin-bottom: var(--space-8);
	}

	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.hint {
		color: var(--text-muted);
		font-size: 0.85rem;
		text-align: left;
		line-height: 1.4;
	}

	input {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-size: 0.95rem;
		font-family: var(--font-ui);
		outline: none;
		transition: border-color 0.15s;
	}

	input::placeholder {
		color: var(--text-muted);
	}

	input:focus {
		border-color: var(--accent-blue);
	}

	input:disabled {
		opacity: 0.6;
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
		text-align: left;
	}

	button {
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-size: 0.95rem;
		font-weight: 500;
		font-family: var(--font-ui);
		cursor: pointer;
		transition: opacity 0.15s;
	}

	button:hover:not(:disabled) {
		opacity: 0.9;
	}

	button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.back-button {
		background: transparent;
		color: var(--text-muted);
		font-size: 0.85rem;
		font-weight: 400;
		padding: var(--space-2) var(--space-4);
	}

	.back-button:hover:not(:disabled) {
		color: var(--text-primary);
		opacity: 1;
	}

	.register-link {
		margin-top: var(--space-6);
		color: var(--text-muted);
		font-size: 0.85rem;
	}

	.register-link a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.register-link a:hover {
		text-decoration: underline;
	}

	.oauth-divider {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		margin: var(--space-6) 0;
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.oauth-divider::before,
	.oauth-divider::after {
		content: '';
		flex: 1;
		height: 1px;
		background: var(--border);
	}

	.oauth-buttons {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.oauth-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.9rem;
		font-weight: 500;
		font-family: var(--font-ui);
		text-decoration: none;
		cursor: pointer;
		transition: background 0.15s, border-color 0.15s;
	}

	.oauth-github {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.oauth-github:hover {
		background: var(--bg-hover);
		border-color: var(--text-muted);
	}

	.oauth-google {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.oauth-google:hover {
		background: var(--bg-hover);
		border-color: var(--text-muted);
	}

	.oauth-banner {
		position: relative;
		margin-bottom: var(--space-5);
		padding: var(--space-3) var(--space-8) var(--space-3) var(--space-4);
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-blue) 35%, transparent);
		color: var(--text-primary);
		text-align: left;
	}

	.oauth-banner.error-tone {
		background: color-mix(in srgb, var(--accent-yellow, #eab308) 12%, transparent);
		border-color: color-mix(in srgb, var(--accent-yellow, #eab308) 35%, transparent);
	}

	.oauth-banner-msg {
		margin: 0;
		font-size: 0.88rem;
		line-height: 1.45;
	}

	.oauth-banner-close {
		position: absolute;
		top: var(--space-2);
		right: var(--space-2);
		width: 24px;
		height: 24px;
		padding: 0;
		background: transparent;
		border: none;
		color: var(--text-muted);
		font-size: 1.1rem;
		line-height: 1;
		cursor: pointer;
		border-radius: var(--radius-sm);
	}

	.oauth-banner-close:hover {
		color: var(--text-primary);
		background: color-mix(in srgb, var(--text-primary) 8%, transparent);
	}

	.oauth-banner-actions {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-top: var(--space-3);
	}

	.oauth-banner-btn {
		display: inline-block;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.82rem;
		text-decoration: none;
		text-align: center;
		transition: background 0.15s, border-color 0.15s;
	}

	.oauth-banner-btn:hover {
		background: var(--bg-hover);
		border-color: var(--text-muted);
		text-decoration: none;
	}
</style>
