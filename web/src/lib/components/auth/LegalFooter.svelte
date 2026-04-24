<script lang="ts">
	// Legal links shown below the auth card on Pad Cloud (cloudMode=true).
	// Self-hosted installs do not need these links — the Terms/Privacy that
	// live at getpad.dev are Perpetual Software LLC's for the hosted service,
	// not for the user's own instance.
	//
	// Caller typically reads cloudMode from authStore (which the root layout
	// already populates via authStore.load()), so no additional session fetch
	// is needed. Links open in a new tab so the auth flow is not interrupted.
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
		text-decoration: underline;
		text-decoration-thickness: 1px;
		text-underline-offset: 2px;
		border-radius: 2px;
	}

	.legal-footer a:hover {
		color: var(--text-primary);
	}

	.legal-footer a:focus-visible {
		color: var(--text-primary);
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}
</style>
