<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import LegalFooter from '$lib/components/auth/LegalFooter.svelte';

	let email = $state('');
	let error = $state('');
	let loading = $state(false);
	let sent = $state(false);
	let cloudMode = $state(false);

	onMount(async () => {
		try {
			const session = await api.auth.session();
			cloudMode = session.cloud_mode ?? false;
		} catch {}
	});

	async function handleSubmit() {
		error = '';
		if (!email) {
			error = 'Please enter your email.';
			return;
		}

		loading = true;
		try {
			await api.auth.forgotPassword(email);
			sent = true;
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Something went wrong.';
			} else {
				error = 'Something went wrong.';
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

<div class="page">
	<div class="card">
		<h1 class="logo">Pad</h1>

		{#if sent}
			<p class="subtitle">Check your email</p>
			<p class="message">
				If an account with that email exists, we've sent a password reset link. Check your inbox and spam folder.
			</p>
			<p class="back-link">
				<a href="/login">Back to sign in</a>
			</p>
		{:else}
			<p class="subtitle">Reset your password</p>
			<p class="message">
				Enter your email address and we'll send you a link to reset your password.
			</p>

			<div class="form">
				<input
					type="email"
					placeholder="Email"
					bind:value={email}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="email"
				/>

				{#if error}
					<p class="error">{error}</p>
				{/if}

				<button onclick={handleSubmit} disabled={loading}>
					{#if loading}
						Sending...
					{:else}
						Send reset link
					{/if}
				</button>
			</div>

			<p class="back-link">
				<a href="/login">Back to sign in</a>
			</p>
		{/if}
	</div>

	<LegalFooter {cloudMode} />
</div>

<style>
	.page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
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
		margin-bottom: var(--space-4);
	}

	.message {
		color: var(--text-secondary);
		font-size: 0.88rem;
		line-height: 1.5;
		margin-bottom: var(--space-6);
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
