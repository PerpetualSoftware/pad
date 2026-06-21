<script lang="ts">
	/**
	 * Setup launchpad — the render-mode the workspace dashboard switches to
	 * while `needs_onboarding` is true (PLAN-1847, Phase 2 / TASK-1852).
	 *
	 * A brand-new workspace has no user items, so the normal dashboard would
	 * render as an empty board that reads as "broken" rather than "new". This
	 * component replaces that board with a three-step bridge that routes the
	 * user into the agent-driven onboard flow:
	 *   ① connect an agent  ② tell it (in natural language) to set up
	 *   ③ watch the result appear here live (via the page's SSE feed).
	 *
	 * The flag flips to false the moment any real item exists, at which point
	 * the parent swaps back to the normal dashboard — so this view is
	 * inherently transient.
	 */
	interface Props {
		workspaceName: string;
		/**
		 * Whether an agent has already acted in this workspace
		 * (dashboard.has_agent_activity). Ticks the "Agent connected" checklist
		 * step. Note: this signal flips on the first agent-CREATED item, which
		 * also clears needs_onboarding and removes the launchpad — so in
		 * practice the checklist is an orientation device (you're on step 1),
		 * not a live mid-launchpad tracker. A distinct "connected but hasn't
		 * acted yet" signal is deferred (see IDEA-1854).
		 */
		agentActive?: boolean;
		/** Opens the workspace's ConnectWorkspaceModal (mounted by the parent). */
		onconnect: () => void;
		/** Skip setup for now — parent falls back to the normal (empty) board. */
		ondismiss: () => void;
	}

	let { workspaceName, agentActive = false, onconnect, ondismiss }: Props = $props();

	// Compact setup-progress checklist (TASK-1857). Existing signals only.
	let progress = $derived([
		{ label: 'Workspace created', done: true },
		{ label: 'Agent connected', done: agentActive },
		{ label: 'Setup complete', done: false }
	]);

	// Per-surface shortcuts for step ②. Natural language is the canonical
	// trigger (works on every surface, matches TASK-1849/1851); these are
	// secondary conveniences shown for whichever agent the user connects.
	const shortcuts = [
		{ tool: 'Claude Code', cmd: '/pad onboard' },
		{ tool: 'Codex', cmd: '$pad onboard' },
		{ tool: 'Claude Desktop / MCP', cmd: 'the pad_onboard prompt' }
	];
</script>

<div class="launchpad">
	<header class="lp-header">
		<span class="lp-icon" aria-hidden="true">✨</span>
		<h1 class="lp-title">Let's set up {workspaceName}</h1>
		<p class="lp-subtitle">
			Pad works best when your AI agent sets it up for you — it'll ask a few
			questions and adapt this workspace to your project.
		</p>
	</header>

	<ul class="lp-progress" aria-label="Setup progress">
		{#each progress as p (p.label)}
			<li class="lp-progress-item" class:done={p.done}>
				<span class="lp-progress-mark" aria-hidden="true">{p.done ? '✓' : '○'}</span>
				<span>{p.label}</span>
			</li>
		{/each}
	</ul>

	<ol class="lp-steps">
		<li class="lp-step">
			<span class="lp-step-num" aria-hidden="true">1</span>
			<div class="lp-step-body">
				<h2 class="lp-step-title">Connect your agent</h2>
				<p class="lp-step-text">
					Add Pad to Claude Code, Cursor, Codex, or Claude Desktop — it takes a
					few seconds.
				</p>
				<button class="lp-connect-btn" type="button" onclick={onconnect}>
					Connect agent &rarr;
				</button>
			</div>
		</li>

		<li class="lp-step">
			<span class="lp-step-num" aria-hidden="true">2</span>
			<div class="lp-step-body">
				<h2 class="lp-step-title">Tell it to set up</h2>
				<p class="lp-step-text">Once connected, just say:</p>
				<code class="lp-nl">set up my workspace</code>
				<p class="lp-shortcuts">
					Or use the shortcut for your agent:
					{#each shortcuts as s, i (s.tool)}<span class="lp-shortcut"
							><span class="lp-shortcut-tool">{s.tool}</span>
							<code>{s.cmd}</code></span
						>{#if i < shortcuts.length - 1}<span class="lp-sep"> · </span>{/if}{/each}
				</p>
			</div>
		</li>

		<li class="lp-step">
			<span class="lp-step-num" aria-hidden="true">3</span>
			<div class="lp-step-body">
				<h2 class="lp-step-title">Watch it happen</h2>
				<p class="lp-step-text">
					Your collections and first items will appear right here as your agent
					works — no need to refresh.
				</p>
			</div>
		</li>
	</ol>

	<footer class="lp-footer">
		<button class="lp-skip-btn" type="button" onclick={ondismiss}>
			Prefer to look around first? Skip setup
		</button>
	</footer>
</div>

<style>
	/* Mirrors OnboardingNudgeBanner / ConnectBanner visual language (same
	   tokens) so the setup surfaces feel like one system. */
	.launchpad {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		max-width: 640px;
		margin: var(--space-5) auto;
		padding: var(--space-5) var(--space-4);
	}

	.lp-header {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		gap: var(--space-2);
	}
	.lp-icon {
		font-size: 2em;
		line-height: 1;
		color: var(--accent-blue);
	}
	.lp-title {
		margin: 0;
		font-size: 1.5em;
		font-weight: 700;
		color: var(--text-primary);
	}
	.lp-subtitle {
		margin: 0;
		max-width: 460px;
		font-size: 0.95em;
		line-height: 1.5;
		color: var(--text-secondary);
	}

	.lp-progress {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: var(--space-2) var(--space-4);
	}
	.lp-progress-item {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.82em;
		color: var(--text-muted);
	}
	.lp-progress-item.done {
		color: var(--text-secondary);
	}
	.lp-progress-mark {
		font-weight: 700;
		color: var(--border);
	}
	.lp-progress-item.done .lp-progress-mark {
		color: var(--accent-blue);
	}

	.lp-steps {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.lp-step {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.lp-step-num {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		width: 26px;
		height: 26px;
		border-radius: 50%;
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
		font-weight: 700;
		font-size: 0.85em;
	}
	.lp-step-body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.lp-step-title {
		margin: 0;
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.lp-step-text {
		margin: 0;
		font-size: 0.88em;
		line-height: 1.45;
		color: var(--text-secondary);
	}

	.lp-connect-btn {
		align-self: flex-start;
		margin-top: var(--space-1);
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 0.88em;
		cursor: pointer;
		transition: filter 0.15s;
	}
	.lp-connect-btn:hover {
		filter: brightness(1.08);
	}
	.lp-connect-btn:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}

	.lp-nl {
		align-self: flex-start;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		font-size: 0.95em;
		color: var(--text-primary);
	}
	.lp-shortcuts {
		margin: var(--space-1) 0 0;
		font-size: 0.8em;
		line-height: 1.6;
		color: var(--text-muted);
	}
	.lp-shortcut-tool {
		color: var(--text-secondary);
	}
	.lp-shortcuts code {
		background: var(--bg-tertiary);
		padding: 1px 5px;
		border-radius: var(--radius-sm);
		font-size: 0.95em;
		color: var(--text-secondary);
	}
	.lp-sep {
		color: var(--text-muted);
	}

	.lp-footer {
		display: flex;
		justify-content: center;
	}
	.lp-skip-btn {
		background: transparent;
		border: none;
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		text-decoration: underline;
		text-underline-offset: 2px;
	}
	.lp-skip-btn:hover {
		color: var(--text-secondary);
	}

	@media (max-width: 480px) {
		.launchpad {
			gap: var(--space-4);
			margin: var(--space-3) auto;
		}
		.lp-step {
			padding: var(--space-3);
		}
	}
</style>
