<script lang="ts">
	import { api } from '$lib/api/client';
	import { goto } from '$app/navigation';

	let password = $state('');
	let error = $state('');
	let loading = $state(false);

	async function handleSubmit() {
		error = '';
		if (!password) {
			error = 'Please enter a password.';
			return;
		}

		loading = true;
		try {
			await api.auth.login(password);
			await goto('/', { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Invalid password.';
			} else {
				error = 'Invalid password.';
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
</style>
