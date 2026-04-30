<script lang="ts">
	// Resources block rendered inside the user-menu dropdown in TopBar.svelte
	// (desktop and mobile branches both consume this).
	//
	// Closes the product → marketing handoff seam: a logged-in user looking
	// for Docs / Changelog / GitHub / Status / Support has no obvious path
	// from inside the app today. This block sits at the bottom of the user
	// menu (just above "Connect a project…" and "Sign out") and gives them
	// a quiet escape hatch back out to the surrounding ecosystem.
	//
	// Cloud (cloudMode=true): Docs / Changelog / GitHub / Status / Support
	// Self-hosted (cloudMode=false): Docs / GitHub only — Changelog and
	// Status are Cloud-specific surfaces, and getpad.dev's support@getpad.dev
	// mailbox is not the operator's to direct people to. The Docs link still
	// points at getpad.dev because that's the canonical project documentation
	// even for self-hosted deployments.
	//
	// All links open in a new tab so a user mid-task doesn't lose state.
	// Replaces the prior inline Cloud-only Support/Status block in
	// TopBar.svelte; that block became a special case of this one.
	//
	// Visual contract: docs/brand.md §6 (link-list canonical order) and §7
	// (external-link convention). Companion to AuthHeader/AuthFooter/+error
	// from PLAN-900.

	let {
		cloudMode = false,
		onclose
	}: {
		cloudMode?: boolean;
		// Caller hands in the close-the-dropdown handler (typically
		// `closeUserMenu` from TopBar). Triggered on every link click so
		// the menu collapses without waiting for the link to navigate.
		onclose?: () => void;
	} = $props();

	type ResourceLink = {
		label: string;
		href: string;
		// Most links open in a new tab. The Support entry on Cloud uses a
		// mailto:, which doesn't navigate the browser but still benefits
		// from `noopener` semantics; we keep target="_blank" for parity
		// and to surface the action as "off-property".
	};

	const cloudLinks: ResourceLink[] = [
		{ label: 'Docs', href: 'https://getpad.dev/docs' },
		{ label: 'Changelog', href: 'https://getpad.dev/changelog' },
		{ label: 'GitHub', href: 'https://github.com/PerpetualSoftware/pad' },
		{ label: 'Status', href: 'https://status.getpad.dev' },
		{ label: 'Support', href: 'mailto:support@getpad.dev' }
	];

	const selfHostedLinks: ResourceLink[] = [
		{ label: 'Docs', href: 'https://getpad.dev/docs' },
		{ label: 'GitHub', href: 'https://github.com/PerpetualSoftware/pad' }
	];

	const links = $derived(cloudMode ? cloudLinks : selfHostedLinks);
</script>

<div class="dropdown-divider"></div>
<div class="resources-label">Resources</div>
{#each links as link (link.label)}
	<a
		href={link.href}
		target="_blank"
		rel="noopener noreferrer"
		class="dropdown-item resources-item"
		onclick={() => onclose?.()}
	>
		<span>{link.label}</span>
		<svg
			class="external-icon"
			xmlns="http://www.w3.org/2000/svg"
			width="12"
			height="12"
			viewBox="0 0 24 24"
			fill="none"
			stroke="currentColor"
			stroke-width="2"
			stroke-linecap="round"
			stroke-linejoin="round"
			aria-hidden="true"
		>
			<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
			<polyline points="15 3 21 3 21 9" />
			<line x1="10" y1="14" x2="21" y2="3" />
		</svg>
	</a>
{/each}

<style>
	/* The dropdown-divider and dropdown-item rules live on TopBar.svelte's
	   .user-dropdown scope. Svelte's scoped CSS would not let us touch them
	   from a child component; we use :global so callers can keep their
	   existing rules without forcing a refactor of the dropdown surface. */
	:global(.user-dropdown) .resources-label {
		padding: var(--space-2) var(--space-4) var(--space-1);
		color: var(--text-muted);
		font-size: 0.7rem;
		font-weight: 500;
		letter-spacing: 0.05em;
		text-transform: uppercase;
	}

	:global(.user-dropdown) .resources-item {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
	}

	:global(.user-dropdown) .external-icon {
		color: var(--text-muted);
		flex-shrink: 0;
		opacity: 0.7;
		transition: opacity 150ms ease, color 150ms ease;
	}

	:global(.user-dropdown) .resources-item:hover .external-icon,
	:global(.user-dropdown) .resources-item:focus-visible .external-icon {
		opacity: 1;
		color: var(--text-secondary);
	}
</style>
