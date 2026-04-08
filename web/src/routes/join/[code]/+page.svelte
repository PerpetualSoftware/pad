<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';

	let code = $derived(page.params.code ?? '');
	let status = $state<'loading' | 'login' | 'register' | 'accepting' | 'error' | 'setup' | '2fa'>('loading');
	let errorMsg = $state('');
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | undefined>(undefined);

	// Auth form state
	let mode = $state<'login' | 'register'>('register');
	let email = $state('');
	let name = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let formError = $state('');
	let submitting = $state(false);
	let challengeToken = $state('');
	let totpCode = $state('');

	onMount(async () => {
		try {
			const session = await api.auth.session();
			if (session.authenticated) {
				// Already logged in — try to accept directly
				await acceptInvitation();
			} else if (session.setup_required) {
				setupMethod = session.setup_method;
				status = 'setup';
			} else {
				// Users exist, not logged in — show login (with option to register)
				mode = 'login';
				status = 'login';
			}
		} catch {
			status = 'login';
			mode = 'login';
		}
	});

	async function acceptInvitation() {
		status = 'accepting';
		try {
			const result = await api.members.acceptInvitation(code);
			// Find the workspace slug to redirect to
			// The API returns workspace_id, but we need the slug — redirect to root and let the app figure it out
			await goto('/', { replaceState: true });
		} catch (err: unknown) {
			errorMsg = err instanceof Error ? err.message : 'Failed to accept invitation';
			status = 'error';
		}
	}

	async function handleSubmit() {
		formError = '';
		submitting = true;

		try {
			if (mode === 'register') {
				if (!name.trim()) { formError = 'Name is required'; submitting = false; return; }
				if (!email.trim()) { formError = 'Email is required'; submitting = false; return; }
				if (password.length < 8) { formError = 'Password must be at least 8 characters'; submitting = false; return; }
				if (password !== confirmPassword) { formError = 'Passwords do not match'; submitting = false; return; }
				// Pass the invitation code so the backend allows registration
				// and auto-accepts the invitation in one step.
				await api.auth.register(email.trim(), name.trim(), password, code);
				// Registration with invitation_code already accepted the invite,
				// so redirect directly instead of calling acceptInvitation().
				await goto('/', { replaceState: true });
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

<div class="join-page">
	<div class="join-card">
		<h1 class="logo">Pad</h1>

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
						onkeydown={handleKeydown}
						disabled={submitting}
						autocomplete="name"
					/>
				{/if}
				<input
					type="email"
					placeholder="Email"
					bind:value={email}
					onkeydown={handleKeydown}
					disabled={submitting}
					autocomplete="email"
				/>
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

			<p class="switch-mode">
				{#if mode === 'login'}
					Don't have an account? <button class="link-btn" onclick={() => { mode = 'register'; formError = ''; }}>Create one</button>
				{:else}
					Already have an account? <button class="link-btn" onclick={() => { mode = 'login'; formError = ''; }}>Sign in</button>
				{/if}
			</p>
		{/if}
	</div>
</div>

<style>
	.join-page {
		display: flex;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
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
		color: #ef4444;
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

	.error {
		color: #ef4444;
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
</style>
