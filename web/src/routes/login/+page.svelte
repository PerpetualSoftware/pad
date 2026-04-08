<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';

	let email = $state('');
	let password = $state('');
	let error = $state('');
	let setupRequired = $state(false);
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | undefined>(undefined);
	let loading = $state(false);

	let step = $state<'credentials' | '2fa'>('credentials');
	let challengeToken = $state('');
	let totpCode = $state('');

	onMount(async () => {
		try {
			const session = await api.auth.session();
			if (session.setup_required) {
				setupRequired = true;
				setupMethod = session.setup_method;
				return;
			}
			if (session.authenticated) {
				goto('/', { replaceState: true });
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
			await goto('/', { replaceState: true });
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
			await goto('/', { replaceState: true });
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

			<p class="register-link">
				<a href="/forgot-password">Forgot password?</a>
			</p>
			<p class="register-link">
				Need an account? Ask your admin for an invitation link.
			</p>
		{/if}
	</div>
</div>

<style>
	.login-page {
		display: flex;
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
</style>
