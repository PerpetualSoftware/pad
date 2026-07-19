<script lang="ts">
	interface Props {
		title?: string;
		detail?: string;
		// Optional: when omitted the retry button is hidden entirely. The
		// PLAN-2154 master-freeze (TASK-2172) passes it undefined while peeking so
		// the frozen master can't trigger a provider-destroying reload.
		onRetry?: () => void;
		retryLabel?: string;
	}

	let {
		title = 'Could not load content',
		detail,
		onRetry,
		retryLabel = 'Try again'
	}: Props = $props();
</script>

<div class="content-error" role="alert">
	<div class="error-icon" aria-hidden="true">⚠️</div>
	<p class="error-title">{title}</p>
	{#if detail}
		<p class="error-detail">{detail}</p>
	{/if}
	{#if onRetry}
		<button class="retry-btn" onclick={onRetry}>
			{retryLabel}
		</button>
	{/if}
</div>

<style>
	.content-error {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		padding: var(--space-10) var(--space-6);
		text-align: center;
	}

	.error-icon {
		font-size: 2.5em;
		margin-bottom: var(--space-4);
		line-height: 1;
	}

	.error-title {
		color: var(--text-primary);
		font-size: 1em;
		font-weight: 600;
		max-width: 400px;
		margin: 0 0 var(--space-3) 0;
		line-height: 1.5;
	}

	.error-detail {
		color: var(--text-muted);
		font-size: 0.9em;
		max-width: 400px;
		margin: 0 0 var(--space-5) 0;
		line-height: 1.5;
	}

	.retry-btn {
		background: var(--accent-blue);
		color: #fff;
		border: none;
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.9em;
		font-weight: 600;
		cursor: pointer;
		transition: opacity 0.1s;
	}

	.retry-btn:hover {
		opacity: 0.85;
	}
</style>
