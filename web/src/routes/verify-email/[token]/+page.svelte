<script lang="ts">
	import { confirmSignOut } from '$lib/stores/signOutGuard.svelte';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, PadApiError } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';

	let token = $derived(page.params.token ?? '');

	// verifying → success (terminal, redirects) | error (terminal, offers resend)
	// | needs-session (BUG-3382: the link was opened without a session for the
	// account it names; offers sign-in, or a claim when it wasn't them).
	let status = $state<'verifying' | 'success' | 'error' | 'needs-session'>('verifying');
	let error = $state('');

	// needs-session details, from the 409's details.
	let pendingEmail = $state('');
	let canClaim = $state(false);
	// idle → confirming → claiming → claimed (terminal) | failed (retryable).
	let claimState = $state<'idle' | 'confirming' | 'claiming' | 'claimed' | 'failed'>('idle');
	let claimError = $state('');
	let claimed = $state<{ reset_path: string; stripped_workspaces: string[]; deleted_workspaces: string[] } | null>(null);
	let signInHref = $derived(`/login?redirect=${encodeURIComponent(`/verify-email/${token}`)}`);

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
			if (err instanceof PadApiError && err.code === 'verify_needs_session') {
				pendingEmail = typeof err.details?.email === 'string' ? err.details.email : '';
				canClaim = err.details?.can_claim === true;
				status = 'needs-session';
				return;
			}
			if (err instanceof Error) {
				error = err.message || 'This verification link is invalid or has expired.';
			} else {
				error = 'This verification link is invalid or has expired.';
			}
			status = 'error';
		}
	}

	// Signed in as a DIFFERENT account: /login would bounce straight back
	// here still signed in as it, so sign out first (the CLI-approval page's
	// switch-account pattern).
	let switching = $state(false);
	let switchError = $state('');
	async function switchAccount() {
		if (switching) return;
		// BUG-3571: unsaved edits in an open item are asked about first.
		if (!(await confirmSignOut())) return;
		switching = true;
		switchError = '';
		try {
			await api.auth.logout();
		} catch (err: unknown) {
			switching = false;
			switchError =
				err instanceof Error && err.message
					? `Couldn't sign you out: ${err.message}`
					: "Couldn't sign you out. Please try again.";
			return;
		}
		await goto(signInHref, { replaceState: true });
	}

	async function claim() {
		if (claimState === 'claiming') return;
		claimState = 'claiming';
		claimError = '';
		try {
			claimed = await api.auth.claimByVerification(token);
			claimState = 'claimed';
		} catch (err: unknown) {
			claimError =
				err instanceof Error && err.message ? err.message : 'Could not claim this address. Try again.';
			claimState = 'failed';
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
		{:else if status === 'needs-session'}
			{#if claimState === 'claimed' && claimed}
				<p class="subtitle">Address claimed</p>
				<div class="form claim">
					<p>
						Every password, session and token on this account has been reset. Set a password to
						sign in.
					</p>
					{#if claimed.deleted_workspaces.length > 0}
						<p>Workspaces created by whoever registered this address were deleted:</p>
						<ul>
							{#each claimed.deleted_workspaces as name, i (i)}<li>{name}</li>{/each}
						</ul>
					{/if}
					{#if claimed.stripped_workspaces.length > 0}
						<p>The account was removed from workspaces it had joined:</p>
						<ul>
							{#each claimed.stripped_workspaces as name, i (i)}<li>{name}</li>{/each}
						</ul>
					{/if}
					{#if claimed.reset_path}
						<a class="button-link" href={claimed.reset_path}>Set a password</a>
					{:else}
						<a class="button-link" href="/forgot-password">Set a password</a>
					{/if}
				</div>
			{:else}
				<p class="subtitle">Sign in to verify</p>
				<div class="form claim">
					<p>
						This link verifies {pendingEmail || 'this address'}. Sign in to that account to finish
						verifying.
					</p>
					{#if authStore.user}
						<p>You're signed in as {authStore.user.email}.</p>
						{#if switchError}
							<p class="error" role="alert">{switchError}</p>
						{/if}
						<button onclick={switchAccount} disabled={switching}>
							{switching ? 'Signing out…' : 'Switch account to verify'}
						</button>
					{:else}
						<a class="button-link" href={signInHref}>Sign in to verify</a>
					{/if}
					{#if canClaim}
						{#if claimState === 'idle'}
							<button class="secondary" onclick={() => (claimState = 'confirming')}>
								I didn't register this account
							</button>
						{:else}
							<p>
								If someone else registered your address, you can claim it. This resets every
								password, session and token on the account, deletes workspaces it created, and
								removes it from workspaces it joined. You then set your own password.
							</p>
							{#if claimState === 'failed'}
								<p class="error" role="alert">{claimError}</p>
							{/if}
							<button onclick={claim} disabled={claimState === 'claiming'}>
								{claimState === 'claiming' ? 'Claiming…' : 'Claim this address'}
							</button>
						{/if}
					{/if}
				</div>
			{/if}
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
		color: var(--accent-red);
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
		background: var(--accent-primary-strong); /* white text: AA in both themes (TASK-3509) */
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

	.claim {
		text-align: left;
		color: var(--text-secondary);
		font-size: 0.85rem;
	}

	.claim ul {
		margin: 0;
		padding-left: var(--space-5);
	}

	.button-link {
		display: block;
		text-align: center;
		padding: var(--space-3) var(--space-4);
		background: var(--accent-primary-strong); /* white text: AA in both themes (TASK-3509) */
		color: #fff;
		border-radius: var(--radius);
		font-size: 0.95rem;
		font-weight: 500;
		text-decoration: none;
	}

	button.secondary {
		background: transparent;
		color: var(--accent-blue);
		border: 1px solid var(--border);
	}

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
