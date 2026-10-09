<script lang="ts">
	// Top header for the auth-page family on Pad Cloud installs (login, register,
	// forgot-password, reset-password, join, OAuth-error landings).
	//
	// Renders the same visual contract as getpad.dev's marketing header so the
	// marketing → product handoff (clicking "Login" / "Sign Up" on getpad.dev)
	// does not reset the user's sense of being on the same property.
	//
	// Visual contract: docs/brand.md, sections 5–6. The structural source of
	// truth is pad-web/src/routes/+layout.svelte. When changing tokens, header
	// structure, or link list here, update that file (and the brand doc) too.
	//
	// Self-hosted (cloudMode === false) renders a slim header instead: the Pad
	// mark, the host being signed in to, and the theme toggle. No getpad.dev
	// links, since operators ship Pad under their own name (TASK-3509; the
	// companion AuthFooter.svelte still renders nothing there).
	//
	// Both variants carry the light/dark toggle, which writes the preference
	// the app reads, so the choice survives signing in (TASK-3509).

	import { DOCS_URL, GITHUB_REPO_URL } from '$lib/brand/links';
	import { authStore } from '$lib/stores/auth.svelte';
	import AuthBrand from './AuthBrand.svelte';
	import AuthThemeToggle from './AuthThemeToggle.svelte';

	// Self-hosted: say which server this is (TASK-3509).
	const host = typeof window !== 'undefined' ? window.location.host : '';

	let { cloudMode = false }: { cloudMode?: boolean } = $props();

	let mobileMenuOpen = $state(false);

	// Link list mirrors pad-web's marketing nav (Docs / Blog / GitHub) but
	// points at absolute marketing URLs explicitly — auth pages are pre-login,
	// the user has no workspace context, and these are "go back to the
	// marketing site" links rather than in-app nav. The Docs and GitHub
	// addresses come from $lib/brand/links (single source, guarded by
	// Sidebar.svelte.test.ts); Blog has no constant yet.
	// Blog is marketing and is hidden in the mobile apps (PLAN-3291 DR-1,
	// TASK-3299); Docs and GitHub stay.
	const allNavLinks: Array<{ label: string; href: string; marketing?: boolean }> = [
		{ label: 'Docs', href: DOCS_URL },
		{ label: 'Blog', href: 'https://getpad.dev/blog', marketing: true },
		{ label: 'GitHub', href: GITHUB_REPO_URL }
	];
	const navLinks = $derived(allNavLinks.filter((l) => !(l.marketing && authStore.nativeShell)));

	function handleKeydown(event: KeyboardEvent) {
		if (event.key === 'Escape' && mobileMenuOpen) {
			mobileMenuOpen = false;
		}
	}
</script>

<!-- svelte:window must be top-level (Svelte requires meta tags outside blocks).
     Safe to register unconditionally: handleKeydown only acts on mobileMenuOpen,
     which can only be true when cloudMode is true (the toggle only renders in
     the cloud branch below). On self-hosted the listener is a cheap no-op. -->
<svelte:window onkeydown={handleKeydown} />

{#if cloudMode}
	<header class="auth-header">
		<nav class="auth-header-nav" aria-label="Marketing navigation">
			<!-- In the mobile apps the wordmark is not a way to the marketing
			     site (PLAN-3291 DR-1, TASK-3299). -->
			{#if authStore.nativeShell}
				<span class="auth-header-wordmark"><AuthBrand /></span>
			{:else}
				<a href="https://getpad.dev/" class="auth-header-wordmark" aria-label="pad, getpad.dev"><AuthBrand /></a>
			{/if}

			<div class="auth-header-links">
				{#each navLinks as link (link.label)}
					<a
						href={link.href}
						target="_blank"
						rel="noopener noreferrer"
						class="auth-header-link"
					>
						{link.label}
					</a>
				{/each}
			</div>

			<div class="auth-header-end">
			<AuthThemeToggle />
			<button
				type="button"
				class="auth-header-toggle"
				onclick={() => (mobileMenuOpen = !mobileMenuOpen)}
				aria-label="Toggle menu"
				aria-expanded={mobileMenuOpen}
				aria-controls="auth-header-mobile-menu"
			>
				{#if mobileMenuOpen}
					<svg
						xmlns="http://www.w3.org/2000/svg"
						width="20"
						height="20"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="2"
						stroke-linecap="round"
						stroke-linejoin="round"
						aria-hidden="true"
					>
						<line x1="18" y1="6" x2="6" y2="18" />
						<line x1="6" y1="6" x2="18" y2="18" />
					</svg>
				{:else}
					<svg
						xmlns="http://www.w3.org/2000/svg"
						width="20"
						height="20"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="2"
						stroke-linecap="round"
						stroke-linejoin="round"
						aria-hidden="true"
					>
						<line x1="4" y1="8" x2="20" y2="8" />
						<line x1="4" y1="16" x2="20" y2="16" />
					</svg>
				{/if}
			</button>
			</div>
		</nav>

		{#if mobileMenuOpen}
			<div id="auth-header-mobile-menu" class="auth-header-mobile-menu">
				{#each navLinks as link (link.label)}
					<a
						href={link.href}
						target="_blank"
						rel="noopener noreferrer"
						class="auth-header-link"
						onclick={() => (mobileMenuOpen = false)}
					>
						{link.label}
					</a>
				{/each}
			</div>
		{/if}
	</header>
{:else}
	<header class="auth-header">
		<div class="auth-header-nav">
			<span class="auth-header-wordmark"><AuthBrand /></span>
			<div class="auth-header-end">
				{#if host}<span class="auth-header-host" title="The Pad server you are signing in to">{host}</span>{/if}
				<AuthThemeToggle />
			</div>
		</div>
	</header>
{/if}

<style>
	.auth-header {
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		z-index: 50;
		width: 100%;
		border-bottom: 1px solid var(--border-subtle);
		/* 80% opacity over --bg-primary (#1a1a1a) gives the soft-glass effect.
		   Hex equivalent of rgba(26,26,26,0.8) — kept inline because we want
		   to match pad-web's bg-bg/80 backdrop-blur pattern byte-for-byte and
		   the app's CSS variable system doesn't expose alpha-modified tokens. */
		background-color: color-mix(in srgb, var(--bg-primary) 80%, transparent);
		backdrop-filter: blur(20px);
		-webkit-backdrop-filter: blur(20px);
	}

	.auth-header-nav {
		margin: 0 auto;
		max-width: 72rem; /* matches pad-web's max-w-6xl (1152px) */
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-6);
	}

	.auth-header-wordmark {
		font-size: 1.125rem; /* text-lg */
		font-weight: 700;
		letter-spacing: -0.025em;
		color: var(--text-primary);
		text-decoration: none;
	}

	.auth-header-wordmark:hover,
	.auth-header-wordmark:focus-visible {
		text-decoration: none;
	}

	.auth-header-end {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		min-width: 0;
	}

	.auth-header-host {
		font-size: 0.8rem;
		color: var(--text-secondary);
		font-family: var(--font-mono, ui-monospace, monospace);
		padding: 4px 10px;
		border: 1px solid var(--border);
		border-radius: 999px;
		max-width: 50vw;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.auth-header-links {
		display: none;
		align-items: center;
		gap: var(--space-8);
	}

	.auth-header-link {
		font-size: 0.875rem; /* text-sm */
		color: var(--text-secondary);
		text-decoration: none;
		transition: color 150ms ease;
	}

	.auth-header-link:hover,
	.auth-header-link:focus-visible {
		color: var(--text-primary);
		text-decoration: none;
	}

	.auth-header-toggle {
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--text-secondary);
		background: transparent;
		border: none;
		padding: 0;
		cursor: pointer;
	}

	.auth-header-toggle:hover {
		color: var(--text-primary);
	}

	.auth-header-mobile-menu {
		border-top: 1px solid var(--border-subtle);
		padding: var(--space-4) var(--space-6);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	/* md breakpoint = 768px (Tailwind default; matches pad-web's md:flex). */
	@media (min-width: 768px) {
		.auth-header-links {
			display: flex;
			margin-left: auto;
			margin-right: var(--space-8);
		}
		.auth-header-toggle,
		.auth-header-mobile-menu {
			display: none;
		}
	}
</style>
