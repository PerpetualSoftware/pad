<script lang="ts">
	// 'logs_token' added in TASK-1167 / PLAN-1166: the server returns this
	// value when running self-host with no users + a loaded first-run
	// bootstrap token. It tells the user to grab the token from container
	// logs and finish setup at /setup#token=<x>. Cloud mode never returns
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
		<p class="body">
			This Pad instance is fresh. The first-run bootstrap token was logged on startup —
			grab it from your container logs and finish setup in your browser.
		</p>

		<div class="instructions">
			<div class="instruction">
				<p class="instruction-label">Find the token</p>
				<code>docker logs &lt;container&gt; 2&gt;&amp;1 | grep -A1 'Pad first-run setup'</code>
			</div>

			<div class="instruction">
				<p class="instruction-label">Then continue setup</p>
				<a href="/setup">Open setup page</a>
			</div>
		</div>

		<p class="hint">
			You can also paste the URL printed in the logs directly — it points at
			<code>/setup</code> with the token in the URL fragment.
		</p>
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
