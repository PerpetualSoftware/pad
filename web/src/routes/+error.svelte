<script lang="ts">
	// Root-level error page. Renders for any unhandled error or unmatched
	// route (404) anywhere in the SvelteKit tree.
	//
	// Context-aware chrome:
	//
	// - "Marketing context" (auth pages, share pages, console pages — the
	//   paths where the root +layout.svelte renders children bare without
	//   the workspace app shell): on Pad Cloud we wrap the error in
	//   AuthHeader + AuthFooter so a 404 doesn't drop the user out of the
	//   brand; on self-hosted we render a bare centered card with the
	//   inline Pad wordmark.
	//
	// - "App-shell context" (workspace pages /[username]/[workspace]/...):
	//   the root layout already wraps us in Sidebar / TopBar / main-content,
	//   so we MUST NOT also render fixed-position marketing chrome — that
	//   would stack two header bars on top of each other. Render a minimal
	//   in-shell error instead (no AuthHeader, no AuthFooter, no body-level
	//   sizing — just a centered block inside the existing main-content).
	//
	// On self-hosted (cloudMode=false) the marketing-context branch still
	// renders no getpad.dev chrome — same gating philosophy as TASK-902/903.
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

	// Mirror the bare-render condition from web/src/routes/+layout.svelte —
	// these are the paths where the root layout renders children directly,
	// without wrapping them in the workspace app shell. Errors on those
	// paths get the marketing chrome treatment (Cloud) or a minimal
	// centered card (self-hosted). Errors on any other path (workspace
	// pages) render inside the app shell, so we render only the inner
	// block without any header/footer of our own.
	const isMarketingContext = $derived.by(() => {
		const pathname = page.url.pathname;
		return (
			pathname === '/login' ||
			pathname === '/register' ||
			pathname === '/forgot-password' ||
			pathname.startsWith('/reset-password/') ||
			pathname.startsWith('/join/') ||
			pathname.startsWith('/auth/cli/') ||
			pathname.startsWith('/s/') ||
			pathname.startsWith('/console')
		);
	});

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

{#if isMarketingContext}
	<!-- Marketing-context error: full-viewport centered card. Cloud renders
	     the AuthHeader/AuthFooter chrome around it; self-hosted renders the
	     bare card with the inline Pad wordmark. -->
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
{:else}
	<!-- App-shell-context error: rendered inside the workspace shell that
	     already provides Sidebar / TopBar / main-content. Render a centered
	     block within the available main-content area only — no fixed-position
	     header/footer of our own. -->
	<div class="error-inline">
		<div class="error-card">
			<p class="status-code" aria-hidden="true">{status}</p>
			<h2 class="title">{friendlyTitle}</h2>
			<p class="hint">{friendlyHint}</p>

			{#if status === 404 && message && message !== 'Not Found'}
				<p class="detail">{message}</p>
			{/if}

			<div class="actions">
				<a href="/" class="primary">Go to home</a>
			</div>
		</div>
	</div>
{/if}

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

	/* In-shell variant: render inside the existing main-content area without
	   stretching to full viewport. The parent .main-content already handles
	   scroll/sizing; we just center within it. */
	.error-inline {
		display: flex;
		align-items: center;
		justify-content: center;
		min-height: 100%;
		padding: var(--space-8) var(--space-4);
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
