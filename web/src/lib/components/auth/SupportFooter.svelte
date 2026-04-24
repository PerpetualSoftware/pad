<script lang="ts">
	// Support/help links shown below the auth card on Pad Cloud.
	//
	// Gated on cloudMode — the support@getpad.dev mailbox and status.getpad.dev
	// page are for the hosted service, not for self-hosted deployments. Caller
	// typically reads cloudMode from authStore. Links open in a new tab so
	// clicking one mid-login does not lose the form state.
	let { cloudMode = false }: { cloudMode?: boolean } = $props();
</script>

{#if cloudMode}
	<nav class="support-footer" aria-label="Support">
		<a href="mailto:support@getpad.dev">Support</a>
		<span aria-hidden="true">·</span>
		<a href="https://status.getpad.dev" target="_blank" rel="noopener noreferrer">Status</a>
	</nav>
{/if}

<style>
	.support-footer {
		margin-top: var(--space-2);
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.support-footer a {
		color: var(--text-muted);
		text-decoration: underline;
		text-decoration-thickness: 1px;
		text-underline-offset: 2px;
		border-radius: 2px;
	}

	.support-footer a:hover {
		color: var(--text-primary);
	}

	.support-footer a:focus-visible {
		color: var(--text-primary);
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}
</style>
