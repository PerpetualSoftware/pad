<script lang="ts" module>
	/**
	 * Shape of an editable quick action — identical to QuickAction in $lib/types
	 * but with `icon` as a required string (empty when unset) to avoid
	 * undefined-vs-empty churn in the bound inputs.
	 */
	export interface EditableQuickAction {
		label: string;
		prompt: string;
		scope: 'item' | 'collection';
		icon: string;
	}
</script>

<script lang="ts">
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';
	import {
		parsePrompt,
		TEMPLATE_VARIABLES,
		type PreviewContext
	} from '$lib/utils/quick-action-preview';

	interface Props {
		/** The full list of quick actions — bindable so the parent owns the state. */
		actions: EditableQuickAction[];
		/**
		 * Preview context used to render the live substitution below each
		 * prompt input. Parent decides whether this came from a real item
		 * or a placeholder.
		 */
		previewContext: PreviewContext;
	}

	let { actions = $bindable(), previewContext }: Props = $props();

	let itemActions = $derived(
		actions.map((a, i) => ({ action: a, index: i })).filter(({ action }) => action.scope === 'item')
	);
	let collectionActions = $derived(
		actions
			.map((a, i) => ({ action: a, index: i }))
			.filter(({ action }) => action.scope === 'collection')
	);

	function addAction(scope: 'item' | 'collection') {
		actions.push({ label: '', prompt: '', scope, icon: '' });
	}

	function removeAction(index: number) {
		actions.splice(index, 1);
	}

	function moveAction(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= actions.length) return;
		const temp = actions[index];
		actions[index] = actions[target];
		actions[target] = temp;
	}

	// Human-readable list of supported variables, used in the help line.
	const variableHelp = TEMPLATE_VARIABLES.map((v) => `{${v}}`).join(' · ');
</script>

<p class="qa-hint">
	Quick actions copy agent prompts to your clipboard. Template variables:
	<code class="qa-var-help">{variableHelp}</code>
</p>

<section class="qa-section">
	<header class="qa-section-header">
		<h4 class="qa-section-title">Item actions</h4>
		<button type="button" class="qa-add-btn" onclick={() => addAction('item')}>+ Add</button>
	</header>
	{#if itemActions.length > 0}
		{#each itemActions as { action, index } (index)}
			<div class="qa-card">
				<div class="qa-card-top">
					<EmojiPickerButton bind:value={actions[index].icon} placeholder="⚡" />
					<input
						class="qa-label-input"
						type="text"
						placeholder="Action label"
						bind:value={actions[index].label}
					/>
					<div class="qa-card-btns">
						<button
							type="button"
							class="qa-reorder-btn"
							disabled={index === 0}
							onclick={() => moveAction(index, -1)}
							title="Move up"
							aria-label="Move action up"
						>&#9650;</button>
						<button
							type="button"
							class="qa-reorder-btn"
							disabled={index === actions.length - 1}
							onclick={() => moveAction(index, 1)}
							title="Move down"
							aria-label="Move action down"
						>&#9660;</button>
						<button
							type="button"
							class="qa-remove-btn"
							onclick={() => removeAction(index)}
							title="Remove"
							aria-label="Remove action"
						>&#10005;</button>
					</div>
				</div>
				<input
					class="qa-prompt-input"
					type="text"
					placeholder={'/pad implement {ref} "{title}"'}
					bind:value={actions[index].prompt}
				/>
				{#if action.prompt.trim()}
					{@const segments = parsePrompt(action.prompt, previewContext)}
					{@const hasUnknown = segments.some((s) => s.type === 'unknown')}
					<div class="qa-preview" class:has-error={hasUnknown}>
						<span class="qa-preview-label">Preview</span>
						<div class="qa-preview-body">
							{#each segments as seg, i (i)}
								{#if seg.type === 'text'}<span class="qa-seg-text"
										>{seg.value}</span
									>{:else if seg.type === 'known'}<span
										class="qa-seg-known"
										title={'{' + seg.name + '}'}>{seg.resolved || `{${seg.name}}`}</span
									>{:else}<span
										class="qa-seg-unknown"
										title="Unknown variable — will copy literally">{'{' + seg.name + '}'}</span
									>{/if}
							{/each}
						</div>
						{#if hasUnknown}
							<div class="qa-preview-warn">
								Highlighted variables aren't recognized and will be copied verbatim. Check for typos.
							</div>
						{/if}
					</div>
				{/if}
			</div>
		{/each}
	{:else}
		<div class="qa-empty">
			<p>No per-item actions yet.</p>
			<p class="qa-empty-hint">
				Add one to surface a one-click agent prompt on every item in this collection — e.g.
				"Summarize for standup" or "Draft release notes".
			</p>
		</div>
	{/if}
</section>

<section class="qa-section">
	<header class="qa-section-header">
		<h4 class="qa-section-title">Collection actions</h4>
		<button type="button" class="qa-add-btn" onclick={() => addAction('collection')}>+ Add</button>
	</header>
	{#if collectionActions.length > 0}
		{#each collectionActions as { action, index } (index)}
			<div class="qa-card">
				<div class="qa-card-top">
					<EmojiPickerButton bind:value={actions[index].icon} placeholder="⚡" />
					<input
						class="qa-label-input"
						type="text"
						placeholder="Action label"
						bind:value={actions[index].label}
					/>
					<div class="qa-card-btns">
						<button
							type="button"
							class="qa-reorder-btn"
							disabled={index === 0}
							onclick={() => moveAction(index, -1)}
							title="Move up"
							aria-label="Move action up"
						>&#9650;</button>
						<button
							type="button"
							class="qa-reorder-btn"
							disabled={index === actions.length - 1}
							onclick={() => moveAction(index, 1)}
							title="Move down"
							aria-label="Move action down"
						>&#9660;</button>
						<button
							type="button"
							class="qa-remove-btn"
							onclick={() => removeAction(index)}
							title="Remove"
							aria-label="Remove action"
						>&#10005;</button>
					</div>
				</div>
				<input
					class="qa-prompt-input"
					type="text"
					placeholder="/pad triage all new items"
					bind:value={actions[index].prompt}
				/>
				{#if action.prompt.trim()}
					{@const segments = parsePrompt(action.prompt, previewContext)}
					{@const hasUnknown = segments.some((s) => s.type === 'unknown')}
					<div class="qa-preview" class:has-error={hasUnknown}>
						<span class="qa-preview-label">Preview</span>
						<div class="qa-preview-body">
							{#each segments as seg, i (i)}
								{#if seg.type === 'text'}<span class="qa-seg-text"
										>{seg.value}</span
									>{:else if seg.type === 'known'}<span
										class="qa-seg-known"
										title={'{' + seg.name + '}'}>{seg.resolved || `{${seg.name}}`}</span
									>{:else}<span
										class="qa-seg-unknown"
										title="Unknown variable — will copy literally">{'{' + seg.name + '}'}</span
									>{/if}
							{/each}
						</div>
						{#if hasUnknown}
							<div class="qa-preview-warn">
								Highlighted variables aren't recognized and will be copied verbatim. Check for typos.
							</div>
						{/if}
					</div>
				{/if}
			</div>
		{/each}
	{:else}
		<div class="qa-empty">
			<p>No collection-level actions yet.</p>
			<p class="qa-empty-hint">
				Collection actions apply to the whole list — e.g. "Triage new items" or "Archive
				completed".
			</p>
		</div>
	{/if}
</section>

<style>
	.qa-hint {
		margin: 0 0 var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		line-height: 1.5;
	}

	.qa-var-help {
		display: block;
		margin-top: var(--space-1);
		font-family: var(--font-mono);
		font-size: 0.92em;
		color: var(--text-secondary);
		line-height: 1.6;
	}

	.qa-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		margin-top: var(--space-4);
	}

	.qa-section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin: 0;
	}

	.qa-section-title {
		margin: 0;
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.qa-add-btn {
		padding: 2px var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.qa-add-btn:hover {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}

	.qa-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}

	.qa-card-top {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.qa-label-input {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.qa-label-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.qa-card-btns {
		display: flex;
		gap: 2px;
	}

	.qa-reorder-btn,
	.qa-remove-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: 2px var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1.2;
	}

	.qa-reorder-btn {
		font-size: 0.6em;
	}

	.qa-remove-btn {
		font-size: 0.82em;
	}

	.qa-reorder-btn:hover:not(:disabled),
	.qa-remove-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.qa-remove-btn:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.qa-reorder-btn:disabled {
		opacity: 0.25;
		cursor: default;
	}

	.qa-prompt-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.82em;
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.qa-prompt-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Live preview ─────────────────────────────────────────────────────── */

	.qa-preview {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding: var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
	}

	.qa-preview.has-error {
		border-color: color-mix(in srgb, var(--accent-amber, #fbbf24) 50%, var(--border));
	}

	.qa-preview-label {
		font-size: 0.68em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.qa-preview-body {
		font-family: var(--font-mono);
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: pre-wrap;
		word-break: break-word;
		line-height: 1.5;
	}

	.qa-seg-text {
		color: var(--text-secondary);
	}

	.qa-seg-known {
		color: var(--text-primary);
		background: color-mix(in srgb, var(--accent-blue) 14%, transparent);
		padding: 0 2px;
		border-radius: 2px;
	}

	.qa-seg-unknown {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 14%, transparent);
		padding: 0 2px;
		border-radius: 2px;
		text-decoration: underline wavy currentColor;
		text-underline-offset: 2px;
	}

	.qa-preview-warn {
		font-size: 0.72em;
		color: var(--accent-amber, #fbbf24);
		line-height: 1.4;
	}

	/* ── Empty state ─────────────────────────────────────────────────────── */

	.qa-empty {
		padding: var(--space-3) var(--space-4);
		border: 1px dashed var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		line-height: 1.5;
	}

	.qa-empty p {
		margin: 0;
	}

	.qa-empty .qa-empty-hint {
		margin-top: var(--space-1);
		color: var(--text-muted);
		font-size: 0.92em;
	}
</style>
