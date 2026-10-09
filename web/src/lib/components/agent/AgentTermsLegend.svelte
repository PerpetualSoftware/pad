<script lang="ts">
	// "What these mean" for the agent pages (TASK-2257, audit C72). A native
	// disclosure: reachable by keyboard and touch, announced by screen
	// readers, and closed until asked for.
	import { TERMS } from '$lib/agent/agentTerms';

	let { scopeWord = 'Surface' }: { scopeWord?: 'Surface' | 'Scope' } = $props();
</script>

<details class="agent-terms">
	<summary>What these mean</summary>
	<dl>
		<dt>Trigger</dt>
		<dd>{TERMS.trigger}</dd>
		<dt>{scopeWord}</dt>
		<dd>{TERMS.surface}</dd>
		<dt>Enforcement</dt>
		<dd>{TERMS.enforcement}</dd>
	</dl>
</details>

<style>
	.agent-terms {
		margin: 0 0 var(--space-4);
		font-size: 0.85rem;
		color: var(--text-secondary);
	}
	.agent-terms summary {
		cursor: pointer;
		color: var(--text-muted);
		width: fit-content;
	}
	.agent-terms dl {
		margin: var(--space-2) 0 0;
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: var(--space-1) var(--space-3);
	}
	.agent-terms dt {
		font-weight: 600;
		color: var(--text-primary);
	}
	.agent-terms dd {
		margin: 0;
	}
</style>
