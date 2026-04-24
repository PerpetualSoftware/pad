<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import { goto } from '$app/navigation';
	import SetupRequiredNotice from '$lib/components/auth/SetupRequiredNotice.svelte';
	import LegalFooter from '$lib/components/auth/LegalFooter.svelte';
	import SupportFooter from '$lib/components/auth/SupportFooter.svelte';
	import { authStore } from '$lib/stores/auth.svelte';

	let name = $state('');
	let username = $state('');
	let email = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let error = $state('');
	let setupRequired = $state(false);
	let setupMethod = $state<'local_cli' | 'docker_exec' | 'cloud' | undefined>(undefined);
	let loading = $state(false);

	let usernameManuallyEdited = $state(false);
	let usernameChecking = $state(false);
	let usernameAvailable = $state<boolean | null>(null);
	let usernameError = $state('');
	let checkTimeout: ReturnType<typeof setTimeout> | null = null;

	onMount(async () => {
		try {
			// Route session fetch through authStore so authStore.cloudMode is
			// populated after a logout → /register navigation (the root layout's
			// authStore.load() only runs once, so the store can be cleared and
			// never re-filled without this). ensureLoaded is a no-op when the
			// store already has a session.
			const session = await authStore.ensureLoaded();
			if (session?.setup_required) {
				setupRequired = true;
				setupMethod = session.setup_method;
				return;
			}
			if (session?.authenticated) {
				goto('/console', { replaceState: true });
				return;
			}
		} catch {}
	});

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

	function validate(): string | null {
		if (!name.trim()) return 'Please enter your name.';
		if (username && username.length < 3) return 'Username must be at least 3 characters.';
		if (usernameAvailable === false) return usernameError || 'Username is not available.';
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
			await api.auth.register(email, name, password, username || undefined);
			await goto('/console', { replaceState: true });
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
					oninput={handleNameInput}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="name"
				/>

				<div class="username-field">
					<input
						type="text"
						placeholder="Username"
						bind:value={username}
						oninput={handleUsernameInput}
						onkeydown={handleKeydown}
						disabled={loading}
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

				{#if authStore.cloudMode}
					<p class="consent">
						By creating an account, you agree to our
						<a href="https://getpad.dev/terms" target="_blank" rel="noopener noreferrer">Terms</a>
						and
						<a href="https://getpad.dev/privacy" target="_blank" rel="noopener noreferrer">Privacy Policy</a>.
					</p>
				{/if}
			</div>

			<p class="login-link">
				Already have an account? <a href="/login">Sign in</a>
			</p>
		{/if}
	</div>

	<LegalFooter cloudMode={authStore.cloudMode} />
	<SupportFooter cloudMode={authStore.cloudMode} />
</div>

<style>
	.register-page {
		display: flex;
		flex-direction: column;
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

	.consent {
		margin-top: var(--space-2);
		color: var(--text-muted);
		font-size: 0.78rem;
		line-height: 1.4;
		text-align: center;
	}

	.consent a {
		color: var(--text-secondary);
		text-decoration: underline;
	}

	.consent a:hover {
		color: var(--text-primary);
	}

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
		color: #ef4444;
	}
</style>
