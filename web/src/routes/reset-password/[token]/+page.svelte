<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';

	let token = $derived(page.params.token ?? '');

	onMount(() => {
		// Hydrate authStore so AuthHeader can branch on cloudMode. See the
		// matching pattern in /forgot-password — without this, a user landing
		// on a reset-password link after a logout sees the self-hosted layout
		// even on Pad Cloud. Swallow fetch errors so the reset flow stays
		// usable even if the session endpoint is unreachable.
		authStore.ensureLoaded().catch(() => {});
	});

	let password = $state('');
	let confirmPassword = $state('');
	let error = $state('');
	let loading = $state(false);

	async function handleSubmit() {
		error = '';

		if (password.length < 8) {
			error = 'Password must be at least 8 characters.';
			return;
		}
		if (password !== confirmPassword) {
			error = 'Passwords do not match.';
			return;
		}

		loading = true;
		try {
			await api.auth.resetPassword(token, password);
			await goto('/console', { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Failed to reset password.';
			} else {
				error = 'Failed to reset password.';
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

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="page" class:cloud-mode={authStore.cloudMode}>
	<div class="card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}
		<p class="subtitle">Set a new password</p>

		<div class="form">
			<input
				type="password"
				placeholder="New password"
				bind:value={password}
				onkeydown={handleKeydown}
				disabled={loading}
				autocomplete="new-password"
			/>

			<input
				type="password"
				placeholder="Confirm new password"
				bind:value={confirmPassword}
				onkeydown={handleKeydown}
				disabled={loading}
				autocomplete="new-password"
			/>

			{#if error}
				<p class="error">{error}</p>
			{/if}

			<button onclick={handleSubmit} disabled={loading}>
				{#if loading}
					Resetting...
				{:else}
					Reset password
				{/if}
			</button>
		</div>

		<p class="back-link">
			<a href="/login">Back to sign in</a>
		</p>
	</div>
</div>

<style>
	.page {
		display: flex;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.page.cloud-mode {
		padding-top: 4rem;
	}

	.card {
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

	input::placeholder { color: var(--text-muted); }
	input:focus { border-color: var(--accent-blue); }
	input:disabled { opacity: 0.6; }

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

	button:hover:not(:disabled) { opacity: 0.9; }
	button:disabled { opacity: 0.6; cursor: not-allowed; }

	.back-link {
		margin-top: var(--space-6);
		color: var(--text-muted);
		font-size: 0.85rem;
	}

	.back-link a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.back-link a:hover { text-decoration: underline; }
</style>
