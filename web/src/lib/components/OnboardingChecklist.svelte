<script lang="ts">
	interface Props {
		wsSlug: string;
		byCollection: Record<string, Record<string, number>>;
	}

	let { wsSlug, byCollection }: Props = $props();

	function collectionHasItems(slug: string): boolean {
		const breakdown = byCollection[slug];
		if (!breakdown) return false;
		return Object.values(breakdown).reduce((sum, n) => sum + n, 0) > 0;
	}

	function collectionItemCount(slug: string): number {
		const breakdown = byCollection[slug];
		if (!breakdown) return 0;
		return Object.values(breakdown).reduce((sum, n) => sum + n, 0);
	}

	interface Step {
		title: string;
		href: string;
		done: boolean;
		hint: string;
	}

	let steps = $derived<Step[]>([
		{
			title: 'Add project conventions',
			href: `/${wsSlug}/library`,
			done: collectionHasItems('conventions'),
			hint: '/pad what conventions should this project follow?'
		},
		{
			title: 'Create your first phase',
			href: `/${wsSlug}/new?collection=phases`,
			done: collectionHasItems('phases'),
			hint: '/pad create a phase for what I\'m working on'
		},
		{
			title: 'Add a few tasks',
			href: `/${wsSlug}/new?collection=tasks`,
			done: collectionItemCount('tasks') >= 3,
			hint: '/pad break down my current work into tasks'
		},
		{
			title: 'Write an architecture doc',
			href: `/${wsSlug}/new?collection=docs`,
			done: collectionHasItems('docs'),
			hint: '/pad document the architecture of this project'
		}
	]);

	let completedCount = $derived(steps.filter((s) => s.done).length);
	let progressPct = $derived(Math.round((completedCount / steps.length) * 100));
</script>

<div class="onboarding">
	<div class="onboarding-header">
		<h2>Set up your workspace</h2>
		<p class="subtitle">Complete these steps to get the most out of Pad.</p>
	</div>

	<div class="progress-section">
		<span class="progress-label">{completedCount} of {steps.length} complete</span>
		<div class="progress-track">
			<div class="progress-fill" style:width="{progressPct}%"></div>
		</div>
	</div>

	<ol class="step-list">
		{#each steps as step (step.title)}
			<li class="step" class:done={step.done}>
				<div class="step-icon">
					{#if step.done}
						<svg class="check-icon" viewBox="0 0 20 20" fill="currentColor" width="20" height="20">
							<circle cx="10" cy="10" r="10" />
							<path d="M6 10.5l2.5 2.5 5.5-5.5" stroke="#fff" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round" />
						</svg>
					{:else}
						<svg class="empty-icon" viewBox="0 0 20 20" width="20" height="20">
							<circle cx="10" cy="10" r="9" stroke="currentColor" stroke-width="1.5" fill="none" />
						</svg>
					{/if}
				</div>
				<div class="step-body">
					<a href={step.href} class="step-title">{step.title}</a>
					{#if !step.done}
						<span class="step-hint">Try: <code>{step.hint}</code></span>
					{/if}
				</div>
			</li>
		{/each}
	</ol>

	<div class="onboarding-footer">
		<a href="/{wsSlug}/library" class="footer-link">Or open the library to browse conventions and playbooks</a>
		<span class="footer-muted">View web UI at http://localhost:7777</span>
	</div>
</div>

<style>
	.onboarding {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-6);
		max-width: 600px;
	}

	.onboarding-header {
		margin-bottom: var(--space-5);
	}

	.onboarding-header h2 {
		font-size: 1.2em;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0 0 var(--space-1) 0;
	}

	.subtitle {
		font-size: 0.88em;
		color: var(--text-muted);
		margin: 0;
	}

	/* Progress */
	.progress-section {
		margin-bottom: var(--space-5);
	}

	.progress-label {
		display: block;
		font-size: 0.82em;
		font-weight: 500;
		color: var(--text-secondary);
		margin-bottom: var(--space-2);
	}

	.progress-track {
		height: 6px;
		background: var(--bg-tertiary);
		border-radius: 3px;
		overflow: hidden;
	}

	.progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 3px;
		transition: width 0.3s ease;
	}

	/* Steps */
	.step-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.step {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-3);
		border-radius: var(--radius);
		transition: background 0.1s;
	}

	.step:hover {
		background: var(--bg-tertiary);
	}

	.step-icon {
		flex-shrink: 0;
		width: 20px;
		height: 20px;
		margin-top: 1px;
	}

	.check-icon {
		color: var(--accent-green);
	}

	.empty-icon {
		color: var(--text-muted);
	}

	.step-body {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		min-width: 0;
	}

	.step-title {
		font-size: 0.92em;
		font-weight: 500;
		color: var(--text-primary);
		text-decoration: none;
	}

	.step-title:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	.done .step-title {
		color: var(--text-muted);
	}

	.step-hint {
		font-size: 0.8em;
		color: var(--text-muted);
		line-height: 1.5;
	}

	.step-hint code {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: 3px;
		padding: 1px 5px;
		font-size: 0.92em;
		word-break: break-all;
	}

	/* Footer */
	.onboarding-footer {
		margin-top: var(--space-5);
		padding-top: var(--space-4);
		border-top: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.footer-link {
		font-size: 0.85em;
		color: var(--accent-blue);
		text-decoration: none;
	}

	.footer-link:hover {
		text-decoration: underline;
	}

	.footer-muted {
		font-size: 0.78em;
		color: var(--text-muted);
	}
</style>
