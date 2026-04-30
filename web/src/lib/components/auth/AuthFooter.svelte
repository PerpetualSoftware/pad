<script lang="ts">
	// Footer rendered below the auth-page family.
	//
	// On Pad Cloud (cloudMode=true) this carries the full getpad.dev marketing
	// footer link list — same canonical order as pad-web/src/routes/+layout.svelte
	// — so the auth pages feel like one continuous property with the marketing
	// site rather than a stripped-down stub.
	//
	// On self-hosted (cloudMode=false) we drop to the legal-essential subset
	// (Terms / Privacy / Sub-processors). Operators ship Pad under their own
	// brand and getpad.dev's GitHub / Changelog / FAQ / Security / Support
	// links are not theirs to advertise.
	//
	// Visual contract: docs/brand.md, section 7. Source of truth for tokens,
	// link order, and copyright format is pad-web/src/routes/+layout.svelte.
	// When changing those, update the brand doc and this file together.
	//
	// Replaces the prior LegalFooter + SupportFooter pair — the brand spec
	// describes a single footer pattern, not two separate strips.

	let { cloudMode = false }: { cloudMode?: boolean } = $props();

	// Canonical Cloud-mode link order from docs/brand.md §7. Do not reorder.
	// All Cloud links are off-property (github.com or getpad.dev) so every
	// anchor opens in a new tab — the user is mid-auth-flow and we don't want
	// to lose their form state by navigating away.
	const cloudLinks: Array<{ label: string; href: string }> = [
		{ label: 'GitHub', href: 'https://github.com/PerpetualSoftware/pad' },
		{ label: 'Docs', href: 'https://getpad.dev/docs' },
		{ label: 'Changelog', href: 'https://getpad.dev/changelog' },
		{ label: 'Contribute', href: 'https://getpad.dev/contribute' },
		{ label: 'FAQ', href: 'https://getpad.dev/faq' },
		{ label: 'Security', href: 'https://getpad.dev/security' },
		{ label: 'Privacy', href: 'https://getpad.dev/privacy' },
		{ label: 'Terms', href: 'https://getpad.dev/terms' },
		{ label: 'Sub-processors', href: 'https://getpad.dev/subprocessors' }
	];

	// Self-hosted gets the legal-essentials only — same set as the prior
	// LegalFooter cloudMode branch so existing self-hosted users see the
	// same footer they had before this PR.
	const selfHostedLinks: Array<{ label: string; href: string }> = [
		{ label: 'Terms', href: 'https://getpad.dev/terms' },
		{ label: 'Privacy', href: 'https://getpad.dev/privacy' },
		{ label: 'Sub-processors', href: 'https://getpad.dev/subprocessors' }
	];

	const links = $derived(cloudMode ? cloudLinks : selfHostedLinks);

	// Year is computed once per page render — no auto-refresh, but auth pages
	// don't sit open across a year boundary in any realistic flow.
	const year = new Date().getFullYear();
</script>

{#if cloudMode}
	<footer class="auth-footer cloud">
		<div class="auth-footer-row">
			<p class="auth-footer-copyright">
				&copy; {year} Pad
				<span class="auth-footer-sep" aria-hidden="true">·</span>
				<a
					href="https://perpetualsoftware.org"
					target="_blank"
					rel="noopener noreferrer"
					class="auth-footer-perpetual"
				>
					Perpetual Software
				</a>
			</p>
			<nav class="auth-footer-links" aria-label="Site navigation">
				{#each links as link (link.label)}
					<a
						href={link.href}
						target="_blank"
						rel="noopener noreferrer"
						class="auth-footer-link"
					>
						{link.label}
					</a>
				{/each}
			</nav>
		</div>
	</footer>
{:else}
	<!-- Self-hosted footer: legal-essentials only. No copyright line — that
	     belongs to the operator's own deployment, not to Pad. Kept minimal
	     by design; an operator-customizable footer is deferred to its own
	     plan after PLAN-900 ships. -->
	<nav class="auth-footer-legal" aria-label="Legal">
		{#each links as link, i (link.label)}
			<a href={link.href} target="_blank" rel="noopener noreferrer">{link.label}</a>
			{#if i < links.length - 1}
				<span aria-hidden="true">·</span>
			{/if}
		{/each}
	</nav>
{/if}

<style>
	/* Cloud footer — full marketing-style strip. Matches pad-web's footer
	   structure: a top border, max-w-6xl container, copyright line on the
	   left and link row on the right. Stacks vertically on small screens. */
	.auth-footer.cloud {
		margin-top: var(--space-10);
		border-top: 1px solid var(--border-subtle);
		padding-top: var(--space-6);
		width: 100%;
	}

	.auth-footer-row {
		max-width: 72rem; /* matches max-w-6xl */
		margin: 0 auto;
		padding: 0 var(--space-6);
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: var(--space-4);
		text-align: center;
	}

	.auth-footer-copyright {
		font-size: 0.875rem; /* text-sm */
		color: var(--text-muted);
		margin: 0;
	}

	.auth-footer-sep {
		margin: 0 0.25rem;
	}

	.auth-footer-perpetual {
		color: color-mix(in srgb, var(--text-muted) 60%, transparent);
		text-decoration: none;
		transition: color 150ms ease;
	}

	.auth-footer-perpetual:hover,
	.auth-footer-perpetual:focus-visible {
		color: var(--text-secondary);
		text-decoration: none;
	}

	.auth-footer-links {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: center;
		column-gap: var(--space-6);
		row-gap: var(--space-3);
	}

	.auth-footer-link {
		font-size: 0.875rem;
		color: var(--text-muted);
		text-decoration: none;
		transition: color 150ms ease;
	}

	.auth-footer-link:hover,
	.auth-footer-link:focus-visible {
		color: var(--text-secondary);
		text-decoration: none;
	}

	/* sm breakpoint = 640px (Tailwind default; matches pad-web's sm:flex-row). */
	@media (min-width: 640px) {
		.auth-footer-row {
			flex-direction: row;
			justify-content: space-between;
			text-align: left;
		}
	}

	/* Self-hosted legal strip — preserves the look of the prior LegalFooter
	   so existing self-hosted deployments see no change after this PR. */
	.auth-footer-legal {
		margin-top: var(--space-6);
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.auth-footer-legal a {
		color: var(--text-muted);
		text-decoration: underline;
		text-decoration-thickness: 1px;
		text-underline-offset: 2px;
		border-radius: 2px;
	}

	.auth-footer-legal a:hover {
		color: var(--text-primary);
	}

	.auth-footer-legal a:focus-visible {
		color: var(--text-primary);
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}
</style>
