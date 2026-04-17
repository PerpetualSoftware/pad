<script lang="ts">
	import { FIELD_TYPES, slugifyKey, type EditableField } from './field-editor-types';

	interface Props {
		/** The field being edited. Mutated in place via bindings. */
		field: EditableField;
		/** Position in the parent list (0-based). */
		index: number;
		/** Total number of fields in the parent list (used to disable reorder buttons at ends). */
		total: number;
		/**
		 * Whether this is a new (unsaved) field. Controls:
		 * - Label input placeholder ("Field name" vs "Field label")
		 * - Whether the Key input row is shown (new fields only)
		 * - Whether the key auto-syncs from the label (new + untouched only)
		 */
		isNew?: boolean;
		/**
		 * Optional inline validation error for the key input. Set by the parent
		 * modal based on structural validation + duplicate detection. Only
		 * surfaced when `isNew` is true.
		 */
		keyError?: string | null;
		/** Optional: parent provides a move-up handler. If omitted, the button is hidden. */
		onmoveup?: () => void;
		/** Optional: parent provides a move-down handler. If omitted, the button is hidden. */
		onmovedown?: () => void;
		/** Remove this field from the parent list. */
		onremove: () => void;
	}

	let {
		field = $bindable(),
		index,
		total,
		isNew = false,
		keyError = null,
		onmoveup,
		onmovedown,
		onremove
	}: Props = $props();

	const showReorder = $derived(onmoveup !== undefined && onmovedown !== undefined);
	const isSelectType = $derived(field.type === 'select' || field.type === 'multi_select');

	// Terminal-option toggle is currently scoped to the `status` field only.
	// This matches the existing EditCollectionModal behavior exactly.
	// Generalizing to any select field is the job of T4 (TASK-597).
	const showsTerminalColumn = $derived(field.key === 'status' && field.options.length > 0);

	// Auto-derive the key from the label for new fields, unless the user has
	// manually edited the key. Once `keyTouched` flips to true the user owns
	// the key verbatim. We use explicit handlers rather than a $effect so the
	// data flow is visible at the call site.
	function onLabelInput() {
		if (isNew && !field.keyTouched) {
			field.key = slugifyKey(field.label);
		}
	}

	function onKeyInput() {
		// Any manual edit in the key input takes ownership — stop auto-sync.
		field.keyTouched = true;
	}

	function removeOption(optIndex: number) {
		field.options.splice(optIndex, 1);
	}

	function addOption() {
		field.options.push('');
	}

	function toggleTerminal(option: string) {
		const idx = field.terminalOptions.indexOf(option);
		if (idx >= 0) {
			field.terminalOptions.splice(idx, 1);
		} else {
			field.terminalOptions.push(option);
		}
	}

	function isTerminal(option: string): boolean {
		return field.terminalOptions.includes(option);
	}
</script>

<div class="field-card">
	<div class="field-card-header">
		{#if showReorder}
			<div class="field-drag-handle">
				<button
					class="reorder-btn"
					type="button"
					disabled={index === 0}
					onclick={onmoveup}
					title="Move up"
					aria-label="Move field up"
				>&#9650;</button>
				<button
					class="reorder-btn"
					type="button"
					disabled={index === total - 1}
					onclick={onmovedown}
					title="Move down"
					aria-label="Move field down"
				>&#9660;</button>
			</div>
		{/if}
		<div class="field-header-left">
			<input
				class="field-label-input"
				type="text"
				bind:value={field.label}
				oninput={onLabelInput}
				placeholder={isNew ? 'Field name' : 'Field label'}
			/>
			{#if isNew}
				<div class="field-key-row">
					<span class="field-key-prefix" aria-hidden="true">key</span>
					<input
						class="field-key-input"
						class:has-error={!!keyError}
						type="text"
						bind:value={field.key}
						oninput={onKeyInput}
						placeholder="auto-generated from name"
						spellcheck="false"
						autocomplete="off"
						aria-label="Field key"
						aria-invalid={!!keyError}
					/>
				</div>
				{#if keyError}
					<div class="field-key-error" role="alert">{keyError}</div>
				{:else if field.keyTouched}
					<div class="field-key-hint">Keys can't be changed after save.</div>
				{/if}
			{/if}
		</div>
		<select class="field-type-select" bind:value={field.type} title="Field type">
			{#each FIELD_TYPES as ft (ft)}
				<option value={ft}>{ft.replace('_', ' ')}</option>
			{/each}
		</select>
		<button
			class="field-remove-btn"
			type="button"
			onclick={onremove}
			title="Remove field"
			aria-label="Remove field"
		>&#10005;</button>
	</div>

	{#if isSelectType}
		<div class="field-options">
			{#if showsTerminalColumn}
				<div class="options-col-headers">
					<span class="options-col-label">Options</span>
					<span
						class="options-col-terminal"
						title="Terminal statuses are treated as done/closed"
					>Done?</span>
					<span class="options-col-spacer"></span>
				</div>
			{/if}
			<div class="options-rows">
				{#each field.options as _opt, oi (oi)}
					<div
						class="option-row"
						class:option-terminal={field.key === 'status' &&
							isTerminal(field.options[oi])}
					>
						<input
							class="option-name-input"
							type="text"
							bind:value={field.options[oi]}
							placeholder="option name"
						/>
						{#if field.key === 'status'}
							<button
								class="option-done-toggle"
								class:active={isTerminal(field.options[oi])}
								type="button"
								onclick={() => toggleTerminal(field.options[oi])}
								title={isTerminal(field.options[oi])
									? 'Marked as terminal (click to unmark)'
									: 'Mark as terminal — items with this status are considered done'}
								aria-label={isTerminal(field.options[oi])
									? 'Unmark as terminal'
									: 'Mark as terminal'}
							>
								{#if isTerminal(field.options[oi])}
									<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
										<rect
											x="1"
											y="1"
											width="14"
											height="14"
											rx="3"
											fill="currentColor"
											opacity="0.15"
											stroke="currentColor"
											stroke-width="1.5"
										/>
										<path
											d="M4.5 8L7 10.5L11.5 5.5"
											stroke="currentColor"
											stroke-width="1.8"
											stroke-linecap="round"
											stroke-linejoin="round"
										/>
									</svg>
								{:else}
									<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
										<rect
											x="1.5"
											y="1.5"
											width="13"
											height="13"
											rx="2.5"
											stroke="currentColor"
											stroke-width="1"
											opacity="0.4"
										/>
									</svg>
								{/if}
							</button>
						{/if}
						<button
							class="option-remove-btn"
							type="button"
							onclick={() => removeOption(oi)}
							title="Remove option"
							aria-label="Remove option"
						>&#10005;</button>
					</div>
				{/each}
			</div>
			<button class="option-add-btn" type="button" onclick={addOption}>+ Add option</button>
		</div>
	{/if}
</div>

<style>
	.field-card {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}

	.field-card-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-3) var(--space-3);
	}

	.field-drag-handle {
		display: flex;
		flex-direction: column;
		gap: 1px;
		flex-shrink: 0;
	}

	.reorder-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.6em;
		cursor: pointer;
		padding: 2px var(--space-1);
		line-height: 1.2;
		border-radius: var(--radius-sm);
	}

	.reorder-btn:hover:not(:disabled) {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.reorder-btn:disabled {
		opacity: 0.25;
		cursor: default;
	}

	.field-header-left {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}

	.field-label-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.9em;
		font-weight: 500;
		color: var(--text-primary);
	}

	.field-label-input:hover {
		border-color: var(--border);
	}

	.field-label-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Key input row (new fields only) ───────────────────────────────────── */

	.field-key-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.field-key-prefix {
		font-size: 0.68em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		font-family: var(--font-mono);
		padding: 0 var(--space-2);
	}

	.field-key-input {
		flex: 1;
		min-width: 0;
		padding: 2px var(--space-2);
		background: transparent;
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.78em;
		font-family: var(--font-mono);
		color: var(--text-muted);
	}

	.field-key-input:hover {
		border-color: var(--border);
		background: var(--bg-secondary);
	}

	.field-key-input:focus {
		border-color: var(--accent-blue);
		background: var(--bg-secondary);
		color: var(--text-primary);
		outline: none;
	}

	.field-key-input.has-error {
		border-color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 4%, transparent);
	}

	.field-key-error {
		padding: 0 var(--space-2);
		font-size: 0.72em;
		color: var(--accent-red, #ef4444);
	}

	.field-key-hint {
		padding: 0 var(--space-2);
		font-size: 0.72em;
		color: var(--text-muted);
		font-style: italic;
	}

	.field-type-select {
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
		flex-shrink: 0;
	}

	.field-type-select:hover {
		border-color: var(--border);
	}

	.field-type-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.field-remove-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		flex-shrink: 0;
	}

	.field-remove-btn:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	/* ── Options area (select / multi_select) ──────────────────────────────── */

	.field-options {
		border-top: 1px solid var(--border);
		padding: var(--space-3) var(--space-3) var(--space-3) calc(var(--space-3) + 28px);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.options-col-headers {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding-bottom: var(--space-1);
	}

	.options-col-label {
		flex: 1;
		font-size: 0.72em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.options-col-terminal {
		width: 40px;
		text-align: center;
		font-size: 0.72em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.options-col-spacer {
		width: 22px;
		flex-shrink: 0;
	}

	.options-rows {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.option-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.option-row.option-terminal .option-name-input {
		border-color: color-mix(in srgb, var(--accent-green, #22c55e) 30%, var(--border));
		background: color-mix(in srgb, var(--accent-green, #22c55e) 4%, var(--bg-secondary));
	}

	.option-name-input {
		flex: 1;
		min-width: 0;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.option-name-input:hover {
		border-color: var(--border);
	}

	.option-name-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.option-done-toggle {
		width: 40px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		flex-shrink: 0;
		transition: color 0.15s;
	}

	.option-done-toggle:hover {
		color: var(--text-secondary);
	}

	.option-done-toggle.active {
		color: var(--accent-green, #22c55e);
	}

	.option-remove-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.75em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		flex-shrink: 0;
		opacity: 0;
		transition: opacity 0.1s;
	}

	.option-row:hover .option-remove-btn {
		opacity: 1;
	}

	.option-remove-btn:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	.option-add-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		text-align: left;
		width: fit-content;
	}

	.option-add-btn:hover {
		color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
	}
</style>
