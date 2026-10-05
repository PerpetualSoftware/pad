<script lang="ts">
	import type { AppPreviewArtifact } from '$lib/types';

	/**
	 * One app artifact under review (SPEC-6 U9a/U9b, TASK-3413; DOC-3371 §2
	 * step 4): every change Pad's importer makes, and what was fetched beside
	 * what will be stored. Shared by the install and upgrade reviews.
	 */
	interface Props {
		artifact: AppPreviewArtifact;
		/** On an upgrade: how this artifact differs from the installed one. */
		badge?: string;
	}
	let { artifact, badge }: Props = $props();
</script>

<article class="artifact" data-testid="app-artifact">
	<div class="artifact-head">
		<strong>{artifact.normalized.title}</strong>
		<span class="hint">{artifact.kind} &rarr; /{artifact.destination_collection}</span>
		{#if badge}<span class="badge">{badge}</span>{/if}
	</div>
	{#if artifact.changes.length > 0}
		<ul class="changes" aria-label="Changes Pad makes to {artifact.normalized.title}">
			{#each artifact.changes as ch (ch)}
				<li>{ch}</li>
			{/each}
		</ul>
	{:else}
		<p class="hint">Stored exactly as published.</p>
	{/if}
	<details>
		<summary>Compare what was fetched with what will be stored</summary>
		<div class="compare">
			<div>
				<h5>Fetched</h5>
				<pre>{artifact.raw}</pre>
			</div>
			<div>
				<h5>Stored</h5>
				<pre>{artifact.normalized.content}</pre>
				{#if Object.keys(artifact.normalized.fields).length > 0}
					<pre class="fields">{JSON.stringify(artifact.normalized.fields, null, 2)}</pre>
				{/if}
			</div>
		</div>
	</details>
</article>

<style>
	.artifact { border: 1px solid var(--border); border-radius: var(--radius); padding: var(--space-3); display: flex; flex-direction: column; gap: var(--space-2); }
	.artifact-head { display: flex; gap: var(--space-2); align-items: baseline; flex-wrap: wrap; }
	.hint { margin: 0; font-size: 0.85em; color: var(--text-secondary); }
	.badge { font-size: 0.8em; color: var(--accent-blue); }
	.changes { margin: 0; padding-left: var(--space-5); font-size: 0.9em; }
	.changes li { color: var(--accent-orange); }
	.compare { display: grid; grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr)); gap: var(--space-3); margin-top: var(--space-2); }
	h5 { margin: 0 0 var(--space-1); font-size: 0.8em; color: var(--text-secondary); }
	pre { margin: 0; max-height: 20rem; overflow: auto; padding: var(--space-2); background: var(--bg-tertiary); border-radius: var(--radius); font-size: 0.8em; white-space: pre-wrap; overflow-wrap: anywhere; }
	.fields { margin-top: var(--space-2); }
</style>
