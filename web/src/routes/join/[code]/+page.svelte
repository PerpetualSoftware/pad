<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, type InvitationPreview } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';
	import AuthOAuthButtons from '$lib/components/auth/AuthOAuthButtons.svelte';
	import { recordAuthMethod, getLastAuthMethod, type AuthMethod } from '$lib/auth/lastMethod';
	import { validateRedirect } from '$lib/auth/redirect';

	let code = $derived(page.params.code ?? '');
	// OAuth completes outside the SPA and returns via a full-page navigation to
	// this same /join/<code> URL, where onMount's session probe sees
	// `authenticated` and calls acceptInvitation. Thread the code through the
	// SSO link's ?redirect= so that round trip lands back here to finish
	// accepting the invite (BUG-1931 / DR-8). validateRedirect keeps this to a
	// same-origin relative path — no open redirect.
	let oauthRedirectTarget = $derived(validateRedirect(`/join/${code}`));
	let status = $state<'loading' | 'login' | 'register' | 'accepting' | 'error' | 'setup' | '2fa'>('loading');
	let errorMsg = $state('');
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | 'logs_token' | 'open' | undefined>(undefined);

	// Auth form state
	let mode = $state<'login' | 'register'>('register');
	let email = $state('');
	// When the non-consuming preview (BUG-1934) resolves the invited address,
	// we prefill `email` and lock the field: the backend binds the invite to
	// this exact address (register 403s on mismatch; accept requires the
	// signed-in account to match), so letting the invitee retype it only
	// invites the confusing invitation_email_mismatch rejection this fixes.
	let invitedEmail = $state<string | null>(null);
	let name = $state('');
	let username = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let formError = $state('');
	let submitting = $state(false);
	let challengeToken = $state('');
	let totpCode = $state('');

	// Last-used auth method (github/google/password) → drives the "Last used"
	// pill on the matching OAuth button, mirroring /login and /register.
	let lastMethod = $state<AuthMethod | null>(null);

	let usernameManuallyEdited = $state(false);
	let usernameChecking = $state(false);
	let usernameAvailable = $state<boolean | null>(null);
	let usernameError = $state('');
	let checkTimeout: ReturnType<typeof setTimeout> | null = null;

	onMount(async () => {
		// Read the last-used auth method so the OAuth buttons can paint the
		// "Last used" pill on first render. Fails silent in SSR / private mode.
		const last = getLastAuthMethod();
		if (last) lastMethod = last.method;

		// Hydrate authStore so AuthHeader can branch on cloudMode (matching the
		// pattern in /forgot-password and /reset-password). Fire-and-forget so
		// it does not delay the join-flow logic below — the header just won't
		// render its Cloud branch on the very first paint if the session
		// fetch is mid-flight, and it lights up once authStore resolves.
		authStore.ensureLoaded().catch(() => {});

		// Kick off the non-consuming invitation preview (BUG-1934) in parallel
		// with the session probe. It's always-200 and never accepts the invite;
		// it just tells us the invited email (to prefill read-only) and whether
		// an account already exists (to pick login vs register). Best-effort: on
		// failure we fall back to a blank, editable email field.
		const previewPromise = api.members.previewInvitation(code).catch(() => null);

		try {
			const session = await api.auth.session();
			if (session.authenticated) {
				// Already logged in — try to accept directly
				await acceptInvitation();
				return;
			}
			if (session.setup_required) {
				setupMethod = session.setup_method;
				status = 'setup';
				return;
			}
			// Logged out — show the auth form.
			await applyPreview(previewPromise);
			status = 'login';
		} catch {
			// Session probe itself failed — still show the form (defaulting to
			// register unless the preview says an account exists). See BUG-1930.
			await applyPreview(previewPromise);
			status = 'login';
		}
	});

	// Apply the invitation preview to the auth form: prefill + lock the invited
	// email, and default the mode by whether an account already exists. When
	// the preview is unavailable (invalid/expired code, or the request failed)
	// we keep the register default — a never-registered invitee has no account
	// to sign into, and register passes the code to auto-accept in one step
	// (BUG-1930). The "already have an account? sign in" switch still lets a
	// returning user flip to login.
	async function applyPreview(previewPromise: Promise<InvitationPreview | null>) {
		const preview = await previewPromise;
		if (preview?.found && preview.email) {
			email = preview.email;
			invitedEmail = preview.email;
			mode = preview.has_account ? 'login' : 'register';
		} else {
			mode = 'register';
		}
	}

	// OAuth completes entirely outside the SPA (provider → pad-cloud callback →
	// server-set session cookie → server-driven redirect back to /join/<code>),
	// so there's no JS success callback to hang the record on. Mirror
	// /login and /register: record speculatively on click. See lastMethod.ts.
	function handleOAuthClick(provider: AuthMethod) {
		recordAuthMethod(provider);
	}

	async function acceptInvitation() {
		status = 'accepting';
		try {
			const result = await api.members.acceptInvitation(code);
			// Find the workspace slug to redirect to
			// The API returns workspace_id, but we need the slug — redirect to root and let the app figure it out
			await goto('/console', { replaceState: true });
		} catch (err: unknown) {
			errorMsg = err instanceof Error ? err.message : 'Failed to accept invitation';
			status = 'error';
		}
	}

	function generateUsername(name: string): string {
		let u = name.toLowerCase().trim();
		u = u.replace(/[^a-z0-9-]+/g, '-');
		u = u.replace(/-{2,}/g, '-');
		u = u.replace(/^-|-$/g, '');
		if (u.length > 39) u = u.substring(0, 39).replace(/-$/, '');
		return u;
	}

	function handleNameInput() {
		if (!usernameManuallyEdited) {
			username = generateUsername(name);
			checkUsernameAvailability();
		}
	}

	function handleUsernameInput() {
		usernameManuallyEdited = username !== '' && username !== generateUsername(name);
		checkUsernameAvailability();
	}

	function checkUsernameAvailability() {
		if (checkTimeout) clearTimeout(checkTimeout);
		usernameAvailable = null;
		usernameError = '';

		if (!username || username.length < 3) {
			usernameChecking = false;
			return;
		}

		usernameChecking = true;
		checkTimeout = setTimeout(async () => {
			try {
				const result = await api.auth.checkUsername(username);
				usernameAvailable = result.available;
				usernameError = result.message || '';
			} catch {
				usernameError = '';
				usernameAvailable = null;
			} finally {
				usernameChecking = false;
			}
		}, 400);
	}

	async function handleSubmit() {
		formError = '';
		submitting = true;

		try {
			if (mode === 'register') {
				if (!name.trim()) { formError = 'Name is required'; submitting = false; return; }
				if (username && username.length < 3) { formError = 'Username must be at least 3 characters'; submitting = false; return; }
				if (usernameAvailable === false) { formError = usernameError || 'Username is not available'; submitting = false; return; }
				if (!email.trim()) { formError = 'Email is required'; submitting = false; return; }
				if (password.length < 8) { formError = 'Password must be at least 8 characters'; submitting = false; return; }
				if (password !== confirmPassword) { formError = 'Passwords do not match'; submitting = false; return; }
				// Pass the invitation code so the backend allows registration
				// and auto-accepts the invitation in one step.
				await api.auth.register(email.trim(), name.trim(), password, username || undefined, code);
				// Registration with invitation_code already accepted the invite,
				// so redirect directly instead of calling acceptInvitation().
				await goto('/console', { replaceState: true });
				return;
			} else {
				if (!email.trim()) { formError = 'Email is required'; submitting = false; return; }
				if (!password) { formError = 'Password is required'; submitting = false; return; }
				const response = await api.auth.login(email.trim(), password);

				if (response.requires_2fa && response.challenge_token) {
					challengeToken = response.challenge_token;
					status = '2fa';
					submitting = false;
					return;
				}
			}
			// Logged in via login — now accept the invitation
			await acceptInvitation();
		} catch (err: unknown) {
			formError = err instanceof Error ? err.message : 'Authentication failed';
			submitting = false;
		}
	}

	async function handleVerify2FA() {
		formError = '';
		const code = totpCode.trim();

		if (!code) {
			formError = 'Please enter your authentication code.';
			return;
		}

		submitting = true;
		try {
			const isTotp = /^\d{6}$/.test(code);

			if (isTotp) {
				await api.auth.verify2FA(challengeToken, code, undefined);
			} else {
				await api.auth.verify2FA(challengeToken, undefined, code);
			}

			// 2FA verified — now accept the invitation
			await acceptInvitation();
		} catch (err: unknown) {
			formError = err instanceof Error ? err.message : 'Invalid code. Please try again.';
			submitting = false;
		}
	}

	function handleBack2FA() {
		status = 'login';
		challengeToken = '';
		totpCode = '';
		formError = '';
		submitting = false;
	}

	function handleKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter') {
			if (status === '2fa') {
				handleVerify2FA();
			} else {
				handleSubmit();
			}
		}
	}
</script>

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="join-page" class:cloud-mode={authStore.cloudMode}>
	<div class="join-card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

		{#if status === 'loading'}
			<p class="subtitle">Checking invitation...</p>
		{:else if status === 'accepting'}
			<p class="subtitle">Joining workspace...</p>
		{:else if status === 'setup'}
			<SetupRequiredNotice
				{setupMethod}
				nextStep="An admin must finish setup before invitation links can be accepted."
				actionHref="/login"
				actionLabel="Go to login"
			/>
		{:else if status === 'error'}
			<p class="subtitle error-text">{errorMsg}</p>
			<a href="/login" class="link">Go to login</a>
		{:else if status === '2fa'}
			<p class="subtitle">Two-factor authentication</p>

			<div class="form">
				<p class="hint">Enter the 6-digit code from your authenticator app, or a recovery code.</p>

				<input
					type="text"
					placeholder="Authentication code"
					bind:value={totpCode}
					onkeydown={handleKeydown}
					disabled={submitting}
					autocomplete="one-time-code"
					inputmode="numeric"
				/>

				{#if formError}
					<p class="error">{formError}</p>
				{/if}

				<button onclick={handleVerify2FA} disabled={submitting}>
					{#if submitting}
						Verifying...
					{:else}
						Verify & join
					{/if}
				</button>

				<button class="back-button" onclick={handleBack2FA} disabled={submitting} type="button">
					Back to sign in
				</button>
			</div>
		{:else}
			<p class="subtitle">You've been invited to a workspace</p>
			<p class="hint">{mode === 'register' ? 'Create an account' : 'Sign in'} to accept</p>

			<div class="form">
				{#if mode === 'register'}
					<input
						type="text"
						placeholder="Name"
						bind:value={name}
						oninput={handleNameInput}
						onkeydown={handleKeydown}
						disabled={submitting}
						autocomplete="name"
					/>

					<div class="username-field">
						<input
							type="text"
							placeholder="Username"
							bind:value={username}
							oninput={handleUsernameInput}
							onkeydown={handleKeydown}
							disabled={submitting}
							autocomplete="username"
						/>
						{#if usernameChecking}
							<span class="username-status checking">checking...</span>
						{:else if usernameAvailable === true}
							<span class="username-status available">available</span>
						{:else if usernameAvailable === false}
							<span class="username-status taken">{usernameError || 'not available'}</span>
						{/if}
					</div>
				{/if}
				<input
					type="email"
					placeholder="Email"
					class:locked={invitedEmail !== null}
					bind:value={email}
					onkeydown={handleKeydown}
					disabled={submitting}
					readonly={invitedEmail !== null}
					autocomplete="email"
				/>
				{#if invitedEmail !== null}
					<p class="field-hint">This invitation was sent to this address.</p>
				{/if}
				<input
					type="password"
					placeholder="Password"
					bind:value={password}
					onkeydown={handleKeydown}
					disabled={submitting}
					autocomplete={mode === 'register' ? 'new-password' : 'current-password'}
				/>
				{#if mode === 'register'}
					<input
						type="password"
						placeholder="Confirm password"
						bind:value={confirmPassword}
						onkeydown={handleKeydown}
						disabled={submitting}
						autocomplete="new-password"
					/>
				{/if}

				{#if formError}
					<p class="error">{formError}</p>
				{/if}

				<button onclick={handleSubmit} disabled={submitting}>
					{#if submitting}
						{mode === 'register' ? 'Creating account...' : 'Signing in...'}
					{:else}
						{mode === 'register' ? 'Create account & join' : 'Sign in & join'}
					{/if}
				</button>
			</div>

			<AuthOAuthButtons
				cloudMode={authStore.cloudMode}
				redirectTarget={oauthRedirectTarget}
				{lastMethod}
				onProviderClick={handleOAuthClick}
			/>

			<p class="switch-mode">
				{#if mode === 'login'}
					Don't have an account? <button class="link-btn" onclick={() => { mode = 'register'; formError = ''; }}>Create one</button>
				{:else}
					Already have an account? <button class="link-btn" onclick={() => { mode = 'login'; formError = ''; }}>Sign in</button>
				{/if}
			</p>
		{/if}
	</div>

	<AuthFooter cloudMode={authStore.cloudMode} />
</div>

<style>
	.join-page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.join-page.cloud-mode {
		padding-top: 4rem;
	}

	.join-card {
		width: 100%;
		max-width: 380px;
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
		color: var(--text-secondary);
		font-size: 0.95rem;
		margin-bottom: var(--space-2);
	}

	.hint {
		color: var(--text-muted);
		font-size: 0.85rem;
		margin-bottom: var(--space-6);
		line-height: 1.4;
	}

	.error-text {
		color: var(--accent-red);
	}

	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		text-align: left;
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
	input::placeholder { color: var(--text-muted); }
	input:focus { border-color: var(--accent-blue); }
	input:disabled { opacity: 0.6; }
	input.locked {
		color: var(--text-secondary);
		cursor: not-allowed;
	}
	input.locked:focus { border-color: var(--border); }

	.field-hint {
		color: var(--text-muted);
		font-size: 0.75rem;
		margin: calc(-1 * var(--space-2)) 0 0;
	}

	.error {
		color: var(--accent-red);
		font-size: 0.85rem;
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
	button:hover:not(:disabled) { opacity: 0.9; }
	button:disabled { opacity: 0.6; cursor: not-allowed; }

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

	.switch-mode {
		margin-top: var(--space-6);
		font-size: 0.85rem;
		color: var(--text-muted);
	}

	.link-btn {
		background: none;
		border: none;
		color: var(--accent-blue);
		cursor: pointer;
		font-size: inherit;
		padding: 0;
		width: auto;
		font-weight: 500;
	}
	.link-btn:hover { text-decoration: underline; }

	.link {
		color: var(--accent-blue);
		font-size: 0.9rem;
		text-decoration: none;
	}
	.link:hover { text-decoration: underline; }

	.username-field {
		position: relative;
	}

	.username-status {
		position: absolute;
		right: var(--space-3);
		top: 50%;
		transform: translateY(-50%);
		font-size: 0.75rem;
		pointer-events: none;
	}

	.username-status.checking {
		color: var(--text-muted);
	}

	.username-status.available {
		color: #22c55e;
	}

	.username-status.taken {
		color: var(--accent-red);
	}
</style>
