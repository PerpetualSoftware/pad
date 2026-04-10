<script lang="ts">
	let { cloud = false }: { cloud?: boolean } = $props();
</script>

<a href="/" class="pad-logo" class:has-cloud={cloud}>
	<span class="wordmark">Pad</span>
	{#if cloud}
		<span class="cloud-badge">Cloud</span>
	{/if}
</a>

<style>
	.pad-logo {
		display: inline-flex;
		align-items: baseline;
		position: relative;
		text-decoration: none;
		height: 44px;
		line-height: 44px;
		flex-shrink: 0;
		user-select: none;
		-webkit-user-select: none;
	}

	.pad-logo:hover {
		text-decoration: none;
	}

	.wordmark {
		font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif;
		font-size: 1.35rem;
		font-weight: 800;
		letter-spacing: -0.03em;
		color: var(--accent-blue);
		line-height: 44px;
		transition: opacity 0.15s;
	}

	.pad-logo:hover .wordmark {
		opacity: 0.85;
	}

	.cloud-badge {
		position: absolute;
		right: -22px;
		bottom: 6px;
		font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', system-ui, sans-serif;
		font-size: 0.6rem;
		font-weight: 600;
		font-style: italic;
		letter-spacing: 0.04em;
		color: color-mix(in srgb, var(--accent-blue) 65%, var(--text-secondary));
		transform: rotate(-5deg);
		transform-origin: left center;
		line-height: 1;
		pointer-events: none;
		white-space: nowrap;
	}

	.has-cloud {
		margin-right: 24px;
	}
</style>
