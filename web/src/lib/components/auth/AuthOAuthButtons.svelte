<script lang="ts">
	// Cloud-mode SSO buttons (Continue with GitHub / Continue with Google),
	// shared between /login and /register so both pages offer the same
	// one-click sign-in/sign-up affordance and pass `redirect=` through
	// identically.
	//
	// Self-hosted (cloudMode === false) renders nothing — pad-cloud owns the
	// OAuth provider integrations. Same gate-on-cloudMode pattern as the
	// companion AuthHeader / AuthFooter components in this directory.
	//
	// Layout-wise this component owns the "or" divider above its buttons,
	// so callers can drop it directly under their primary submit button
	// without needing an extra spacer wrapper.

	import type { AuthMethod } from '$lib/auth/lastMethod';
	import { redirectQueryFragment } from '$lib/auth/redirect';

	let {
		cloudMode = false,
		redirectTarget,
		lastMethod = null,
		onProviderClick
	}: {
		cloudMode?: boolean;
		/** Already-validated redirect target (use validateRedirect from $lib/auth/redirect). */
		redirectTarget: string;
		lastMethod?: AuthMethod | null;
		onProviderClick: (provider: AuthMethod) => void;
	} = $props();

	const oauthRedirectQuery = $derived(redirectQueryFragment(redirectTarget, '?'));
</script>

{#if cloudMode}
	<div class="oauth-divider">
		<span>or</span>
	</div>

	<div class="oauth-buttons">
		<a
			href="/auth/github{oauthRedirectQuery}"
			data-sveltekit-reload
			class="oauth-btn oauth-github"
			class:last-used={lastMethod === 'github'}
			onclick={() => onProviderClick('github')}
		>
			<svg width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"
				><path
					d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"
				/></svg
			>
			Continue with GitHub
			{#if lastMethod === 'github'}
				<span class="last-used-pill">Last used</span>
			{/if}
		</a>
		<a
			href="/auth/google{oauthRedirectQuery}"
			data-sveltekit-reload
			class="oauth-btn oauth-google"
			class:last-used={lastMethod === 'google'}
			onclick={() => onProviderClick('google')}
		>
			<svg width="18" height="18" viewBox="0 0 18 18" fill="none" aria-hidden="true"
				><path
					d="M17.64 9.2c0-.637-.057-1.251-.164-1.84H9v3.481h4.844a4.14 4.14 0 01-1.796 2.716v2.259h2.908c1.702-1.567 2.684-3.875 2.684-6.615z"
					fill="#4285F4"
				/><path
					d="M9 18c2.43 0 4.467-.806 5.956-2.18l-2.908-2.259c-.806.54-1.837.86-3.048.86-2.344 0-4.328-1.584-5.036-3.711H.957v2.332A8.997 8.997 0 009 18z"
					fill="#34A853"
				/><path
					d="M3.964 10.71A5.41 5.41 0 013.682 9c0-.593.102-1.17.282-1.71V4.958H.957A8.996 8.996 0 000 9c0 1.452.348 2.827.957 4.042l3.007-2.332z"
					fill="#FBBC05"
				/><path
					d="M9 3.58c1.321 0 2.508.454 3.44 1.345l2.582-2.58C13.463.891 11.426 0 9 0A8.997 8.997 0 00.957 4.958L3.964 7.29C4.672 5.163 6.656 3.58 9 3.58z"
					fill="#EA4335"
				/></svg
			>
			Continue with Google
			{#if lastMethod === 'google'}
				<span class="last-used-pill">Last used</span>
			{/if}
		</a>
	</div>
{/if}

<style>
	.oauth-divider {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		margin: var(--space-6) 0;
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.oauth-divider::before,
	.oauth-divider::after {
		content: '';
		flex: 1;
		height: 1px;
		background: var(--border);
	}

	.oauth-buttons {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.oauth-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.9rem;
		font-weight: 500;
		font-family: var(--font-ui);
		text-decoration: none;
		cursor: pointer;
		transition:
			background 0.15s,
			border-color 0.15s;
	}

	.oauth-github,
	.oauth-google {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.oauth-github:hover,
	.oauth-google:hover {
		background: var(--bg-hover);
		border-color: var(--text-muted);
	}

	.oauth-btn.last-used {
		border-color: color-mix(in srgb, var(--accent-blue) 50%, var(--border));
		box-shadow: 0 0 0 1px color-mix(in srgb, var(--accent-blue) 20%, transparent);
	}

	.last-used-pill {
		margin-left: auto;
		padding: 2px var(--space-2);
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
		border-radius: var(--radius-sm);
		font-size: 0.7rem;
		font-weight: 500;
		letter-spacing: 0.02em;
	}
</style>
