<script lang="ts">
	type SetupMethod = 'local_cli' | 'docker_exec' | 'cloud' | undefined;

	let {
		setupMethod,
		nextStep,
		actionHref,
		actionLabel
	}: {
		setupMethod?: SetupMethod;
		nextStep?: string;
		actionHref?: string;
		actionLabel?: string;
	} = $props();
</script>

<div class="setup-required">
	<p class="subtitle">This Pad instance has not been initialized yet.</p>

	{#if setupMethod === 'cloud'}
		<p class="body">Finish the hosted Pad Cloud setup flow before signing in.</p>
	{:else}
		<p class="body">Initialize Pad on the server host first, then come back here.</p>

		<div class="instructions">
			<div class="instruction">
				<p class="instruction-label">Local server</p>
				<code>pad setup</code>
			</div>

			<div class="instruction">
				<p class="instruction-label">Docker</p>
				<code>docker exec -it &lt;container&gt; pad setup</code>
			</div>
		</div>

		<p class="hint">If this Pad server runs on another machine, run <code>pad setup</code> there instead.</p>
	{/if}

	{#if nextStep}
		<p class="next-step">{nextStep}</p>
	{/if}

	{#if actionHref && actionLabel}
		<p class="action">
			<a href={actionHref}>{actionLabel}</a>
		</p>
	{/if}
</div>

<style>
	.setup-required {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.95rem;
		margin: 0;
	}

	.body,
	.hint,
	.next-step,
	.action {
		margin: 0;
		color: var(--text-secondary);
		font-size: 0.9rem;
		line-height: 1.5;
	}

	.instructions {
		display: grid;
		gap: var(--space-3);
		text-align: left;
	}

	.instruction {
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-tertiary);
	}

	.instruction-label {
		margin: 0 0 var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	code {
		display: block;
		font-family: var(--font-mono);
		font-size: 0.85rem;
		color: var(--text-primary);
		word-break: break-word;
	}

	.action a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.action a:hover {
		text-decoration: underline;
	}
</style>
