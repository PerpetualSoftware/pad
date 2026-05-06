<script lang="ts">
	// TASK-1167 / PLAN-1166 — first-run bootstrap page.
	//
	// This is the browser surface for the one-time logs-token bootstrap that
	// lets a Docker / Unraid operator claim the first admin without
	// `docker exec`. The server prints a token to its container logs, the
	// operator copies it into either:
	//
	//   1. the URL fragment (`/setup#token=<TOKEN>`) — convenient deep link;
	//      we scrub the fragment from the address bar on mount so the secret
	//      doesn't linger in browser history / screenshots (F10), or
	//   2. the paste-prompt input on this page when they navigate here
	//      without a fragment.
	//
	// The token never goes in a query string, never in a request URL — only
	// the X-Bootstrap-Token header on POST /api/v1/auth/bootstrap (see
	// api.auth.bootstrap()). If the token is rejected (403 — expired, already
	// used, or wrong) we clear the in-memory copy and revert to the paste
	// prompt so the operator can grab a fresh token from the logs and recover
	// in place (F12).
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';

	// ── Token state ──────────────────────────────────────────────────────────
	//
	// `token` is the only place the bootstrap secret lives in component
	// state. It's never written to a URL, query, or any storage.
	//
	// Read the fragment SYNCHRONOUSLY at component init (gated for SSR) and
	// scrub it from the address bar before the first paint. onMount runs
	// after paint, which would leave a window for the user to copy or
	// screenshot the URL.
	let token = $state('');
	let pastedToken = $state('');

	if (typeof window !== 'undefined') {
		const hash = window.location.hash;
		if (hash.startsWith('#token=')) {
			const raw = hash.slice('#token='.length);
			// decodeURIComponent so an operator who URL-encoded the token
			// (e.g. via a copy/paste tool that escapes special chars) still
			// gets a usable string. Falls back to the raw value if decoding
			// throws on a malformed sequence.
			try {
				token = decodeURIComponent(raw);
			} catch {
				token = raw;
			}
			// Scrub the fragment from the URL bar before paint so the
			// secret doesn't survive in browser history, screen recordings,
			// or screenshots (F10). replaceState keeps the navigation
			// entry — we just rewrite its URL.
			history.replaceState({}, '', '/setup');
		}
	}

	// ── Form state ───────────────────────────────────────────────────────────
	let email = $state('');
	let name = $state('');
	let password = $state('');
	let confirmPassword = $state('');
	let error = $state('');
	let loading = $state(false);
	let sessionChecked = $state(false);

	onMount(async () => {
		// If the user already has a session, /setup is a no-op for them —
		// route to the app root. Mirrors the register page's pattern of
		// going through authStore so a logout → /setup navigation gets
		// fresh session data instead of a stale cached value.
		try {
			const session = await authStore.ensureLoaded();
			if (session?.authenticated) {
				await goto('/');
				return;
			}
		} catch {
			// Network / parse error — treat as unauthenticated and let the
			// user proceed. The bootstrap POST will surface a real error if
			// the server is actually unhealthy.
		} finally {
			sessionChecked = true;
		}
	});

	function handlePasteSubmit() {
		error = '';
		const trimmed = pastedToken.trim();
		if (!trimmed) {
			error = 'Please paste your bootstrap token.';
			return;
		}
		token = trimmed;
		pastedToken = '';
	}

	function validate(): string | null {
		if (!email.trim()) return 'Please enter your email.';
		const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
		if (!emailRegex.test(email)) return 'Please enter a valid email address.';
		if (!name.trim()) return 'Please enter your name.';
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
			await api.auth.bootstrap(email, name, password, token);
			// Bootstrap success — server has set the session cookie. Reload
			// the auth store so subsequent navigations see the new session.
			await authStore.load();
			await goto('/');
		} catch (err: unknown) {
			// 403: token rejected (expired / already used / wrong). Clear
			// the in-memory token, drop back to the paste prompt, and
			// surface an actionable message so the operator can grab a
			// fresh token from the container logs (F12).
			if (err instanceof PadApiError && err.code === 'forbidden') {
				token = '';
				password = '';
				confirmPassword = '';
				error =
					'This token is invalid, expired, or already used. Get a fresh token from your container logs and paste it below.';
			} else if (err instanceof Error) {
				error = err.message || 'Bootstrap failed.';
			} else {
				error = 'Bootstrap failed.';
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

	function handlePasteKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter') {
			handlePasteSubmit();
		}
	}
</script>

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="setup-page" class:cloud-mode={authStore.cloudMode}>
	<div class="setup-card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

		{#if !sessionChecked}
			<p class="subtitle">Loading…</p>
		{:else if !token}
			<p class="subtitle">Paste your bootstrap token</p>
			<p class="hint">
				Look for the token printed in your container logs when the server
				started. It looks like a long random string.
			</p>

			<div class="form">
				<input
					type="text"
					placeholder="Bootstrap token"
					bind:value={pastedToken}
					onkeydown={handlePasteKeydown}
					disabled={loading}
					autocomplete="off"
					spellcheck="false"
				/>

				{#if error}
					<p class="error">{error}</p>
				{/if}

				<button onclick={handlePasteSubmit} disabled={loading}>Continue</button>
			</div>
		{:else}
			<p class="subtitle">Create the first admin account</p>
			<p class="hint">
				This account will own the workspace and can invite other users.
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

				<input
					type="text"
					placeholder="Name"
					bind:value={name}
					onkeydown={handleKeydown}
					disabled={loading}
					autocomplete="name"
				/>

				<input
					type="password"
					placeholder="Password (min. 8 characters)"
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
						Creating admin account…
					{:else}
						Create admin account
					{/if}
				</button>
			</div>
		{/if}
	</div>

	<AuthFooter cloudMode={authStore.cloudMode} />
</div>

<style>
	.setup-page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	.setup-page.cloud-mode {
		padding-top: 4rem;
	}

	.setup-card {
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

	.hint {
		color: var(--text-muted);
		font-size: 0.8rem;
		margin-bottom: var(--space-6);
		line-height: 1.5;
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
