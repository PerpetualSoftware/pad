<script lang="ts">
	// Root-level error page. Renders for any unhandled error or unmatched
	// route (404) anywhere in the SvelteKit tree.
	//
	// On Pad Cloud (cloudMode=true) we wrap the error in marketing chrome —
	// AuthHeader + AuthFooter — so a 404 doesn't drop the user out of the
	// brand. Body carries a friendly message + escape CTAs (back to home,
	// docs, marketing site) so they always have somewhere to go.
	//
	// On self-hosted (cloudMode=false) we render a minimal centered card
	// with no marketing branding — operators ship Pad under their own brand
	// and getpad.dev chrome would be wrong there. This matches the same
	// gating philosophy as the auth-page header/footer (TASK-902/903).
	//
	// Visual contract: docs/brand.md sections 5–7 via the reused
	// AuthHeader / AuthFooter components.

	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthHeader from '$lib/components/auth/AuthHeader.svelte';
	import AuthFooter from '$lib/components/auth/AuthFooter.svelte';

	onMount(() => {
		// Hydrate authStore so cloudMode resolves on first paint. Same pattern
		// as the auth-page family. Fire-and-forget; errors during the session
		// fetch are non-fatal — we'll just render the self-hosted variant.
		authStore.ensureLoaded().catch(() => {});
	});

	// SvelteKit guarantees page.status and page.error are populated when this
	// component renders. We surface a friendly title for the common codes and
	// fall back to the framework's error message otherwise.
	const status = $derived(page.status);
	const message = $derived(page.error?.message ?? '');

	const friendlyTitle = $derived.by(() => {
		if (status === 404) return 'Page not found';
		if (status >= 500) return 'Something went wrong';
		if (status === 403) return 'Access denied';
		if (status === 401) return 'Authentication required';
		return 'An error occurred';
	});

	const friendlyHint = $derived.by(() => {
		if (status === 404) return "The page you were looking for doesn't exist or has moved.";
		if (status >= 500) return "We're having trouble right now. Please try again in a moment.";
		if (status === 403) return "You don't have permission to view this page.";
		if (status === 401) return 'Please sign in to continue.';
		return message || 'Something unexpected happened.';
	});
</script>

<svelte:head>
	<title>{status} · {friendlyTitle} · Pad</title>
</svelte:head>

<AuthHeader cloudMode={authStore.cloudMode} />

<div class="error-page" class:cloud-mode={authStore.cloudMode}>
	<div class="error-card">
		{#if !authStore.cloudMode}
			<h1 class="logo">Pad</h1>
		{/if}

		<p class="status-code" aria-hidden="true">{status}</p>
		<h2 class="title">{friendlyTitle}</h2>
		<p class="hint">{friendlyHint}</p>

		{#if status === 404 && message && message !== 'Not Found'}
			<!-- Surface the framework-provided message only when it's specific
			     enough to be useful (i.e. not the literal "Not Found" stub). -->
			<p class="detail">{message}</p>
		{/if}

		<div class="actions">
			<a href="/" class="primary">Go to home</a>
			{#if authStore.cloudMode}
				<a
					href="https://getpad.dev/"
					target="_blank"
					rel="noopener noreferrer"
					class="secondary"
				>
					Back to getpad.dev
				</a>
				<a
					href="https://getpad.dev/docs"
					target="_blank"
					rel="noopener noreferrer"
					class="secondary"
				>
					Open docs
				</a>
			{/if}
		</div>
	</div>

	<AuthFooter cloudMode={authStore.cloudMode} />
</div>

<style>
	.error-page {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		min-height: 100vh;
		background: var(--bg-primary);
		padding: var(--space-4);
	}

	/* Reserve room for the fixed AuthHeader on Cloud. Matches the pattern
	   established by the auth-page family in TASK-902. */
	.error-page.cloud-mode {
		padding-top: 4rem;
	}

	.error-card {
		width: 100%;
		max-width: 480px;
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

	.status-code {
		font-family: var(--font-mono);
		font-size: 0.85rem;
		color: var(--text-muted);
		letter-spacing: 0.1em;
		margin: 0 0 var(--space-2);
	}

	.title {
		font-size: 1.5rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0 0 var(--space-3);
	}

	.hint {
		color: var(--text-secondary);
		font-size: 0.95rem;
		line-height: 1.5;
		margin: 0 0 var(--space-2);
	}

	.detail {
		color: var(--text-muted);
		font-size: 0.85rem;
		font-family: var(--font-mono);
		word-break: break-word;
		margin: 0 0 var(--space-2);
	}

	.actions {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: var(--space-3);
		margin-top: var(--space-8);
	}

	.actions a {
		display: inline-block;
		padding: var(--space-3) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.9rem;
		font-weight: 500;
		text-decoration: none;
		transition: opacity 150ms ease, background 150ms ease;
	}

	.primary {
		background: var(--accent-blue);
		color: #fff;
	}

	.primary:hover {
		opacity: 0.9;
	}

	.secondary {
		background: var(--bg-tertiary);
		color: var(--text-secondary);
		border: 1px solid var(--border);
	}

	.secondary:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
</style>
