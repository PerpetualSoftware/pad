<script lang="ts">
	// 'logs_token' added in TASK-1167 / PLAN-1166: the server returns this
	// value when running self-host with no users + a loaded first-run
	// bootstrap token. It directs the user to startup output to finish
	// setup in the browser. Cloud mode never returns
	// 'logs_token' (D10/F9).
	//
	// 'open' added for PAD_BYPASS_SETUP_TOKEN: the operator opted into
	// open-bootstrap on a self-host deployment. The /setup form takes
	// the first admin's details directly with no token paste step.
	type SetupMethod = 'local_cli' | 'docker_exec' | 'cloud' | 'logs_token' | 'open' | undefined;

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
	{:else if setupMethod === 'open'}
		<p class="body">
			This Pad instance is configured for open setup
			(<code class="inline">PAD_BYPASS_SETUP_TOKEN=true</code>). Create the first
			admin account directly &mdash; no token required.
		</p>

		<div class="instructions">
			<div class="instruction">
				<p class="instruction-label">Continue setup</p>
				<a href="/setup">Open setup page</a>
			</div>
		</div>
	{:else if setupMethod === 'logs_token'}
		<h2>Finish setting up Pad</h2>
		<p class="body">
			Open the setup link printed when Pad started. If you started Pad in a terminal, look there. For Docker, run
			<code class="inline">docker logs &lt;container&gt;</code> and find “Pad first-run setup”.
		</p>

		<div class="instructions">
			<div class="instruction">
				<p class="instruction-label">Then continue setup</p>
				<a href="/setup">Open setup page</a>
			</div>
		</div>
	{:else}
		<p class="body">Initialize Pad on the server host first, then come back here.</p>

		<div class="instructions">
			<div class="instruction">
				<p class="instruction-label">Local server</p>
				<code>pad auth setup</code>
			</div>

			<div class="instruction">
				<p class="instruction-label">Docker</p>
				<code>docker exec -it &lt;container&gt; pad auth setup</code>
			</div>
		</div>

		<p class="hint">If this Pad server runs on another machine, run <code>pad auth setup</code> there instead.</p>
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

	h2 {
		margin: 0;
		color: var(--text-primary);
		font-size: 1.1rem;
		font-weight: 600;
		line-height: 1.3;
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

	code.inline {
		display: inline;
		padding: 0 var(--space-1);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
	}

	.action a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.action a:hover {
		text-decoration: underline;
	}
</style>
