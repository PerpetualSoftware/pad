<script lang="ts">
	interface Props {
		variant?: 'page' | 'inline';
	}

	let { variant = 'inline' }: Props = $props();
</script>

<div
	class="skeleton skeleton-{variant}"
	role="status"
	aria-label="Loading content"
>
	<div class="bar bar-wide" aria-hidden="true"></div>
	<div class="bar bar-medium" aria-hidden="true"></div>
	<div class="bar bar-narrow" aria-hidden="true"></div>
	{#if variant === 'page'}
		<div class="bar bar-medium" aria-hidden="true"></div>
	{/if}
</div>

<style>
	.skeleton {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.skeleton-page {
		align-items: center;
		padding: var(--space-10) var(--space-6);
	}

	.skeleton-page .bar {
		width: 100%;
		max-width: 600px;
	}

	.skeleton-inline {
		padding: var(--space-4) 0;
		width: 100%;
	}

	.bar {
		height: 14px;
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		animation: pulse 1.5s ease-in-out infinite;
	}

	.bar-wide {
		width: 70%;
	}

	.bar-medium {
		width: 90%;
	}

	.bar-narrow {
		width: 50%;
	}

	.skeleton-page .bar-wide {
		width: 70%;
		height: 28px;
	}

	.skeleton-page .bar-medium {
		height: 14px;
	}

	@keyframes pulse {
		0% {
			opacity: 1;
		}
		50% {
			opacity: 0.5;
		}
		100% {
			opacity: 1;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.bar {
			animation: none;
		}
	}
</style>
