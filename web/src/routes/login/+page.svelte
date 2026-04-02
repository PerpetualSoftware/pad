<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { goto } from '$app/navigation';

	let email = $state('');
	let password = $state('');
	let error = $state('');
	let loading = $state(false);

	onMount(async () => {
		try {
			const session = await api.auth.session();
			if (session.setup_required && session.setup_method === 'open_register') {
				goto('/register', { replaceState: true });
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
			await api.auth.login(email, password);
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

	function handleKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter') {
			handleSubmit();
		}
	}
</script>

<div class="login-page">
	<div class="login-card">
		<h1 class="logo">Pad</h1>
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
			First time? <a href="/register">Create an account</a>
		</p>
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
