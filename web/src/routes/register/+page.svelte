<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { goto } from '$app/navigation';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';

	let name = $state('');
	let email = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let error = $state('');
	let setupRequired = $state(false);
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | undefined>(undefined);
	let loading = $state(false);

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

	function validate(): string | null {
		if (!name.trim()) return 'Please enter your name.';
		if (!email.trim()) return 'Please enter your email.';
		const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
		if (!emailRegex.test(email)) return 'Please enter a valid email address.';
		if (password.length < 8) return 'Password must be at least 8 characters.';
		if (password !== confirmPassword) return 'Passwords do not match.';
		return null;
	}

	async function handleSubmit() {
		error = '';
		const validationError = validate();
		if (validationError) {
			error = validationError;
			return;
		}

		loading = true;
		try {
			await api.auth.register(email, name, password);
			await goto('/', { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'Registration failed.';
			} else {
				error = 'Registration failed.';
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

<div class="register-page">
	<div class="register-card">
		<h1 class="logo">Pad</h1>
		{#if setupRequired}
			<SetupRequiredNotice
				{setupMethod}
				nextStep="After the first admin is created, invitation-based registration will work here."
				actionHref="/login"
				actionLabel="Back to login"
			/>
		{:else}
			<p class="subtitle">Create your account</p>

			<div class="form">
				<input
					type="text"
					placeholder="Name"
					bind:value={name}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="name"
				/>

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
					autocomplete="new-password"
				/>

				<input
					type="password"
					placeholder="Confirm password"
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
						Creating account...
					{:else}
						Create account
					{/if}
				</button>
			</div>

			<p class="login-link">
				Already have an account? <a href="/login">Sign in</a>
			</p>
		{/if}
	</div>
</div>

<style>
	.register-page {
		display: flex;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.register-card {
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

	.login-link {
		margin-top: var(--space-6);
		color: var(--text-muted);
		font-size: 0.85rem;
	}

	.login-link a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.login-link a:hover {
		text-decoration: underline;
	}
</style>
