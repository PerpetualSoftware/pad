<script lang="ts">
	// What a kept draft's restore changed (TASK-3607): the patch endpoint's
	// unified diff, as the server applied it over the newer version.
	import Button from '$lib/components/common/Button.svelte';

	interface Props {
		diff: string;
		ondismiss: () => void;
	}

	let { diff, ondismiss }: Props = $props();

	// Hunk bodies only: the ---/+++ file headers say nothing to a reader here.
	let lines = $derived(diff.split('\n').filter((l) => !l.startsWith('--- ') && !l.startsWith('+++ ') && l !== ''));

	function kind(line: string): 'added' | 'removed' | 'hunk' | 'context' {
		if (line.startsWith('@@')) return 'hunk';
		if (line.startsWith('+')) return 'added';
		if (line.startsWith('-')) return 'removed';
		return 'context';
	}
</script>

<div class="restored-draft" role="status" data-testid="restored-draft-result">
	<p>Your kept changes were applied. The rest of the item is as it was saved since.</p>
	<pre class="diff">{#each lines as line, i (i)}<span class={kind(line)}>{line}</span>
{/each}</pre>
	<div class="actions">
		<Button variant="ghost" size="sm" onclick={ondismiss}>Dismiss</Button>
	</div>
</div>

<style>
	.restored-draft {
		margin: 0 0 var(--space-3);
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		font-size: 0.88em;
		color: var(--text-secondary);
	}
	.restored-draft p {
		margin: 0 0 var(--space-2);
	}
	.diff {
		max-height: 20rem;
		overflow: auto;
		margin: 0 0 var(--space-2);
		font-size: 0.85em;
		white-space: pre-wrap;
	}
	.added {
		color: var(--accent-green);
	}
	.removed {
		color: var(--accent-red);
	}
	.hunk {
		color: var(--text-muted);
	}
	.actions {
		display: flex;
		justify-content: flex-end;
	}
</style>
