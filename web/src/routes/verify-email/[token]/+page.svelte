<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';

	let token = $derived(page.params.token ?? '');

	// verifying → success (terminal, redirects) | error (terminal, offers resend).
	let status = $state<'verifying' | 'success' | 'error'>('verifying');
	let error = $state('');

	// Mirrors VerifyEmailBanner's resend flow: idle → sending → sent (terminal) ;
	// error is retryable.
	let resendState = $state<'idle' | 'sending' | 'sent' | 'error'>('idle');

	onMount(() => {
		// Hydrate authStore so AuthHeader can branch on cloudMode, matching the
		// reset-password analog — without this a user landing here after logout
		// sees the self-hosted layout even on Pad Cloud. Swallow fetch errors so
		// verification still runs if the session endpoint is unreachable.
		authStore.ensureLoaded().catch(() => {});
		verify();
	});

	async function verify() {
		if (!token) {
			// Missing/empty token param — treat as an invalid link.
			error = 'This verification link is invalid or has expired. Request a new one.';
			status = 'error';
			return;
		}
		try {
			await api.auth.verifyEmailToken(token);
			// Refresh the session so emailVerified flips (the server already
			// flipped the DB row; load() re-reads /auth/me) and the
			// VerifyEmailBanner clears. Swallow errors — a logged-out consumer
			// has no session to refresh, and /console bounces them to /login
			// where they sign in already-verified.
			await authStore.load().catch(() => {});
			status = 'success';
			await goto('/console', { replaceState: true });
		} catch (err: unknown) {
			if (err instanceof Error) {
				error = err.message || 'This verification link is invalid or has expired.';
			} else {
				error = 'This verification link is invalid or has expired.';
			}
			status = 'error';
		}
	}

	async function resend() {
		const email = authStore.user?.email;
		if (!email || resendState === 'sending') return;
		resendState = 'sending';
		try {
			// Enumeration-safe (always 200); a resolved promise is the only
			// success signal, so treat any non-throw as "sent".
			await api.auth.resendVerification(email);
			resendState = 'sent';
		} catch {
			resendState = 'error';
		}
	}
</script>

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="page" class:cloud-mode={authStore.cloudMode}>
	<div class="card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

		{#if status === 'verifying'}
			<p class="subtitle">Verifying your email…</p>
			<div class="status" role="status">
				<span class="spinner" aria-hidden="true"></span>
				<span>Just a moment while we confirm your email address.</span>
			</div>
		{:else if status === 'success'}
			<p class="subtitle">Email verified</p>
			<div class="status" role="status">
				<span>Email verified — redirecting…</span>
			</div>
		{:else}
			<p class="subtitle">This verification link is invalid or expired.</p>
			<div class="form">
				<p class="error">{error}</p>

				{#if authStore.user?.email}
					{#if resendState === 'sent'}
						<p class="sent" role="status">
							If your account still needs verification, a new link has been sent.
						</p>
					{:else}
						<button onclick={resend} disabled={resendState === 'sending'}>
							{#if resendState === 'sending'}
								Sending…
							{:else if resendState === 'error'}
								Retry sending verification email
							{:else}
								Resend verification email
							{/if}
						</button>
					{/if}
				{:else}
					<a class="signin-link" href="/login">Sign in to resend</a>
				{/if}
			</div>
		{/if}

		<p class="back-link">
			<a href="/login">Back to sign in</a>
		</p>
	</div>

	<AuthFooter cloudMode={authStore.cloudMode} />
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

	.status {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		color: var(--text-secondary);
		font-size: 0.9rem;
	}

	.spinner {
		width: 1.5rem;
		height: 1.5rem;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.7s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.error {
		color: #ef4444;
		font-size: 0.85rem;
		text-align: left;
	}

	.sent {
		color: var(--accent-blue);
		font-size: 0.85rem;
		font-weight: 500;
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

	.signin-link {
		display: inline-block;
		color: var(--accent-blue);
		text-decoration: none;
		font-size: 0.9rem;
	}

	.signin-link:hover { text-decoration: underline; }

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
