<script lang="ts">
	// Legal links shown below the auth card on Pad Cloud (cloudMode=true).
	// Self-hosted installs do not need these links — the Terms/Privacy that
	// live at getpad.dev are Perpetual Software LLC's for the hosted service,
	// not for the user's own instance. Caller passes cloudMode explicitly so
	// each page can reuse its existing session fetch without a second request.
	//
	// Links open in a new tab so the auth flow is not interrupted.
	let { cloudMode = false }: { cloudMode?: boolean } = $props();
</script>

{#if cloudMode}
	<nav class="legal-footer" aria-label="Legal">
		<a href="https://getpad.dev/terms" target="_blank" rel="noopener noreferrer">Terms</a>
		<span aria-hidden="true">·</span>
		<a href="https://getpad.dev/privacy" target="_blank" rel="noopener noreferrer">Privacy</a>
		<span aria-hidden="true">·</span>
		<a href="https://getpad.dev/subprocessors" target="_blank" rel="noopener noreferrer">Sub-processors</a>
	</nav>
{/if}

<style>
	.legal-footer {
		margin-top: var(--space-6);
		display: flex;
		align-items: center;
		justify-content: center;
		gap: var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	.legal-footer a {
		color: var(--text-muted);
		text-decoration: none;
	}

	.legal-footer a:hover {
		color: var(--text-primary);
		text-decoration: underline;
	}
</style>
