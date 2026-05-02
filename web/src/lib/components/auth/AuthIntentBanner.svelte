<script lang="ts">
	// Contextual banner shown on /login and /register when the user landed
	// there mid-OAuth-flow, e.g. by clicking "Connect to Claude" on the
	// marketing site → an OAuth client redirected them to
	// /login?redirect=/oauth/authorize?... → and the user is now staring at
	// a sign-in form for a tool they may have never used.
	//
	// Without this banner, first-touch users get a fairly steep "wait, why am
	// I being asked to sign up to a new tool?" moment. The banner tells them
	// what they're in the middle of so the form doesn't feel like a non
	// sequitur.
	//
	// Detection is purely a heuristic on the validated `redirect` target —
	// if it begins with `/oauth/authorize`, we assume the user is mid-flow.
	// We don't need to be exact: false positives only mean a slightly more
	// specific banner; false negatives leave the user with the same UX they
	// had before this banner existed.
	//
	// FUTURE (TASK-951 / follow-up): once `/api/v1/oauth/clients/{id}/
	// public-info` ships from TASK-951, parse `client_id` out of the inner
	// query string and fetch the client's display name so the banner can
	// say "connect Claude Desktop" instead of the generic "connect an AI
	// agent." The component contract is shaped to allow that extension
	// without consumer changes.

	let {
		redirectTarget,
		mode
	}: {
		/** Already-validated redirect target (output of validateRedirect from $lib/auth/redirect). */
		redirectTarget: string;
		/** Drives the verb: signin = "signing in", signup = "creating an account". */
		mode: 'signin' | 'signup';
	} = $props();

	const isOAuthFlow = $derived(redirectTarget.startsWith('/oauth/authorize'));
	const verb = $derived(mode === 'signin' ? 'signing in' : 'creating an account');
</script>

{#if isOAuthFlow}
	<div class="intent-banner" role="status" aria-live="polite">
		<span class="intent-emoji" aria-hidden="true">🔌</span>
		<span class="intent-text">
			You're <strong>{verb}</strong> to connect an AI agent to your Pad workspaces.
		</span>
	</div>
{/if}

<style>
	/* Same visual family as the OAuth-error banner on /login (blue info tone)
	   but always informational, never dismissible. Sits above the form so
	   the user reads context first, form second. */
	.intent-banner {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-5);
		padding: var(--space-3) var(--space-4);
		border-radius: var(--radius);
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-blue) 35%, transparent);
		color: var(--text-primary);
		font-size: 0.85rem;
		line-height: 1.4;
		text-align: left;
	}

	.intent-emoji {
		font-size: 1rem;
		flex-shrink: 0;
	}

	.intent-text {
		flex: 1;
		min-width: 0;
	}
</style>
