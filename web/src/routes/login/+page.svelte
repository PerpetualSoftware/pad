<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';
	import AuthOAuthButtons from '$lib/components/auth/AuthOAuthButtons.svelte';
	import AuthIntentBanner from '$lib/components/auth/AuthIntentBanner.svelte';
	import {
		recordAuthMethod,
		getLastAuthMethod,
		type AuthMethod
	} from '$lib/auth/lastMethod';
	import { validateRedirect, redirectQueryFragment } from '$lib/auth/redirect';

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

	// Last-used auth method (TASK-923) — reads from localStorage on mount,
	// drives a soft "you used X last time" banner and a visual lift on the
	// matching CTA. Never blocks any flow; first-time visitors see no banner.
	let lastMethod = $state<AuthMethod | null>(null);

	// Validation + query-string helpers live in $lib/auth/redirect so /register
	// can share them. `redirectTarget` is the validated target (or '/console'
	// default); `oauthRedirectAmpQuery` composes the same encoding behind a
	// '&' separator so it can append onto banner URLs that already carry
	// `?force=1`.
	const redirectTarget = $derived(validateRedirect($page.url.searchParams.get('redirect')));
	const oauthRedirectQuery = $derived(redirectQueryFragment(redirectTarget, '?'));
	const oauthRedirectAmpQuery = $derived(redirectQueryFragment(redirectTarget, '&'));

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
		// Read the last-used method before the session check so the banner
		// can render on the same paint as the form (no flash on slow
		// networks). Suppress the banner if pad-cloud just bounced us back
		// with an error tied to that exact provider — surfacing both at
		// once muddles the message.
		const last = getLastAuthMethod();
		if (last) {
			const errorTiedToLastProvider =
				oauthBanner?.kind === 'not_linked' && oauthBanner.provider === last.method;
			if (!errorTiedToLastProvider) {
				lastMethod = last.method;
			}
		}
		try {
			const session = await api.auth.session();
			cloudMode = session.cloud_mode ?? false;
			if (session.setup_required) {
				setupRequired = true;
				setupMethod = session.setup_method;
				return;
			}
			if (session.authenticated) {
				goto(redirectTarget, { replaceState: true });
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

			recordAuthMethod('password');
			await authStore.load();
			await goto(redirectTarget, { replaceState: true });
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

			recordAuthMethod('password');
			await authStore.load();
			await goto(redirectTarget, { replaceState: true });
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

	// OAuth completion happens entirely outside the SPA — the user leaves
	// pad, signs in with the provider, pad-cloud creates the session, and
	// the browser is redirected back to the redirect target. There is no
	// JS callback we can hang the localStorage write on, so we record the
	// chosen method *speculatively* on click. If the user bails at the
	// provider's consent screen the value still reflects "what the user
	// tried last", which is the right answer for the next-visit banner.
	function handleOAuthClick(provider: AuthMethod) {
		recordAuthMethod(provider);
	}
</script>

<AuthHeader {cloudMode} />

<div class="login-page" class:cloud-mode={cloudMode}>
	<div class="login-card">
		{#if !cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

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

		{#if !setupRequired && step !== '2fa' && lastMethod && !oauthBanner}
			<div class="last-used-banner" role="status" aria-live="polite">
				<span class="last-used-emoji" aria-hidden="true">👋</span>
				<span class="last-used-text">
					{#if lastMethod === 'github'}
						Last time, you used <strong>GitHub</strong> to sign in.
					{:else if lastMethod === 'google'}
						Last time, you used <strong>Google</strong> to sign in.
					{:else}
						Last time, you signed in with your <strong>password</strong>.
					{/if}
				</span>
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

			<AuthIntentBanner {redirectTarget} mode="signin" />

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

			<AuthOAuthButtons
				{cloudMode}
				{redirectTarget}
				{lastMethod}
				onProviderClick={handleOAuthClick}
			/>

			<p class="register-link">
				<a href="/forgot-password">Forgot password?</a>
			</p>
			{#if cloudMode}
				<p class="register-link">
					Don't have an account? <a href="/register{oauthRedirectQuery}">Sign up</a>
				</p>
			{:else}
				<p class="register-link">
					Need an account? Ask your admin for an invitation link.
				</p>
			{/if}
		{/if}
	</div>

	<AuthFooter {cloudMode} />
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

	/* On Cloud the AuthHeader is fixed at the top; reserve room so the auth
	   card does not slide under it. 4rem matches the header's effective
	   height (text-lg wordmark + py-4 padding ≈ 56–64px depending on line
	   metrics; we round up to the brand-spec value of pt-16 = 4rem). */
	.login-page.cloud-mode {
		padding-top: 4rem;
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

	/* OAuth provider button styles (.oauth-divider, .oauth-buttons, .oauth-btn,
	   .oauth-github, .oauth-google, .oauth-btn.last-used, .last-used-pill)
	   live in $lib/components/auth/AuthOAuthButtons.svelte. The styles below
	   are the OAuth-error banner and the welcome "last used" banner, both
	   page-local. */

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

	/*
	 * Last-used auth method banner + CTA emphasis (TASK-923).
	 * Soft hint above the form for returning users; matching OAuth button
	 * gets a subtle border lift + "Last used" pill so the recommended
	 * action reads at a glance without overwhelming first-time visitors
	 * who still see all methods equally.
	 */
	.last-used-banner {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-5);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--accent-blue) 8%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-blue) 25%, transparent);
		color: var(--text-primary);
		font-size: 0.85rem;
		line-height: 1.4;
		text-align: left;
	}

	.last-used-emoji {
		font-size: 1rem;
		flex-shrink: 0;
	}

	.last-used-text {
		flex: 1;
		min-width: 0;
	}
</style>
