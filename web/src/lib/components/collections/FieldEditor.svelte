<script lang="ts" module>
	/** Minimal collection summary passed down from the modal for the relation picker. */
	export interface CollectionOption {
		slug: string;
		name: string;
		icon?: string;
	}
</script>

<script lang="ts">
	import {
		FIELD_TYPES,
		slugifyKey,
		typeSupportsDefault,
		type EditableField
	} from './field-editor-types';

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
		/**
		 * List of workspace collections, used to populate the relation target
		 * dropdown when the field type is `relation`. The parent modal fetches
		 * these once on open and passes them down. Undefined or empty = the
		 * picker renders an empty-state hint.
		 */
		collections?: CollectionOption[];
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
		collections = [],
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

	// ── Advanced section ─────────────────────────────────────────────────────
	// Contains: required, default value, suffix (number only), relation target.
	// Auto-expanded when the field already has any advanced value configured so
	// the user sees what's set without hunting for it. The user can still
	// collapse manually; the state is local to this component instance.

	const hasAdvancedValues = $derived(
		!!field.required ||
			field.default !== undefined ||
			!!field.suffix ||
			!!field.collection
	);
	let showAdvanced = $state(false);
	// Sync once on mount (and after field identity changes): open if the field
	// has any pre-set advanced value. Tracked by `field.key` so reopening the
	// modal or switching field order doesn't clobber the user's manual toggle.
	let lastSyncedKey = $state<string | undefined>(undefined);
	$effect.pre(() => {
		if (field.key !== lastSyncedKey) {
			lastSyncedKey = field.key;
			showAdvanced = hasAdvancedValues;
		}
	});

	// Default-value support depends on type. See typeSupportsDefault() for
	// the full rules. We read through the shared helper so the gating logic
	// in both modals' save paths matches the UI.
	const supportsDefault = $derived(typeSupportsDefault(field.type));

	const isRelation = $derived(field.type === 'relation');
	const isNumber = $derived(field.type === 'number');
	const isCheckboxType = $derived(field.type === 'checkbox');
	const isSelectSingle = $derived(field.type === 'select');

	// Computed fields are read-only — they're populated by the server / agent,
	// not the user. We still render them, but disable the advanced controls.
	const isComputed = $derived(!!field.computed);

	// `field.default` is polymorphic (string | number | boolean). We use typed
	// handlers to keep the serialized value in the right shape.
	function onDefaultTextInput(e: Event) {
		const v = (e.currentTarget as HTMLInputElement).value;
		field.default = v === '' ? undefined : v;
	}
	function onDefaultNumberInput(e: Event) {
		const v = (e.currentTarget as HTMLInputElement).value;
		if (v === '') {
			field.default = undefined;
			return;
		}
		const n = Number(v);
		field.default = Number.isFinite(n) ? n : undefined;
	}
	function onDefaultDateInput(e: Event) {
		const v = (e.currentTarget as HTMLInputElement).value;
		field.default = v === '' ? undefined : v;
	}
	function onDefaultCheckboxInput(e: Event) {
		field.default = (e.currentTarget as HTMLInputElement).checked;
	}
	function onDefaultSelectInput(e: Event) {
		const v = (e.currentTarget as HTMLSelectElement).value;
		field.default = v === '' ? undefined : v;
	}
	function onSuffixInput(e: Event) {
		const v = (e.currentTarget as HTMLInputElement).value;
		field.suffix = v === '' ? undefined : v;
	}
	function onCollectionInput(e: Event) {
		const v = (e.currentTarget as HTMLSelectElement).value;
		field.collection = v === '' ? undefined : v;
	}
	function onRequiredInput(e: Event) {
		// Keep undefined rather than `false` so save payloads stay compact and
		// round-trip-compatible with existing schemas.
		field.required = (e.currentTarget as HTMLInputElement).checked || undefined;
	}

	// Coerce `field.default` to a string for text/number/date/select inputs.
	const defaultAsString = $derived.by(() => {
		if (field.default === undefined || field.default === null) return '';
		if (typeof field.default === 'string') return field.default;
		if (typeof field.default === 'number') return String(field.default);
		return '';
	});
	const defaultAsBool = $derived(field.default === true);
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

	{#if isComputed}
		<div class="field-computed-badge" title="Computed fields are populated by the server and can't be configured here.">
			<span class="computed-dot" aria-hidden="true"></span>
			<span>computed</span>
		</div>
	{/if}

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

	<div class="field-advanced">
		<button
			type="button"
			class="advanced-toggle"
			onclick={() => (showAdvanced = !showAdvanced)}
			aria-expanded={showAdvanced}
		>
			<span class="advanced-chevron" class:open={showAdvanced} aria-hidden="true">
				<svg width="10" height="10" viewBox="0 0 10 10" fill="none">
					<path d="M3 2L7 5L3 8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
				</svg>
			</span>
			<span>Advanced</span>
			{#if hasAdvancedValues && !showAdvanced}
				<span class="advanced-badge" aria-label="Field has advanced settings configured">●</span>
			{/if}
		</button>

		{#if showAdvanced}
			<div class="advanced-content">
				<!-- Required -->
				<label class="advanced-row advanced-row--inline">
					<input
						type="checkbox"
						checked={!!field.required}
						onchange={onRequiredInput}
						disabled={isComputed}
					/>
					<span class="advanced-label">Required</span>
					<span class="advanced-hint">Item can't be saved without a value.</span>
				</label>

				<!-- Default value (type-appropriate) -->
				{#if supportsDefault}
					<div class="advanced-row">
						<span class="advanced-label" id="default-label-{field.key || index}">Default value</span>
						{#if isCheckboxType}
							<label class="advanced-inline-check">
								<input
									type="checkbox"
									checked={defaultAsBool}
									onchange={onDefaultCheckboxInput}
									disabled={isComputed}
								/>
								<span>Checked by default</span>
							</label>
						{:else if isSelectSingle}
							<select
								class="advanced-input"
								value={defaultAsString}
								onchange={onDefaultSelectInput}
								disabled={isComputed}
								aria-labelledby="default-label-{field.key || index}"
							>
								<option value="">— none —</option>
								{#each field.options.filter(Boolean) as opt (opt)}
									<option value={opt}>{opt}</option>
								{/each}
							</select>
						{:else if isNumber}
							<input
								class="advanced-input"
								type="number"
								value={defaultAsString}
								oninput={onDefaultNumberInput}
								disabled={isComputed}
								placeholder="e.g. 0"
								aria-labelledby="default-label-{field.key || index}"
							/>
						{:else if field.type === 'date'}
							<input
								class="advanced-input"
								type="date"
								value={defaultAsString}
								oninput={onDefaultDateInput}
								disabled={isComputed}
								aria-labelledby="default-label-{field.key || index}"
							/>
						{:else}
							<input
								class="advanced-input"
								type={field.type === 'url' ? 'url' : 'text'}
								value={defaultAsString}
								oninput={onDefaultTextInput}
								disabled={isComputed}
								placeholder={field.type === 'url' ? 'https://...' : 'Empty = no default'}
								aria-labelledby="default-label-{field.key || index}"
							/>
						{/if}
					</div>
				{/if}

				<!-- Suffix (number only) -->
				{#if isNumber}
					<div class="advanced-row">
						<span class="advanced-label" id="suffix-label-{field.key || index}">Suffix</span>
						<input
							class="advanced-input advanced-input--short"
							type="text"
							value={field.suffix ?? ''}
							oninput={onSuffixInput}
							disabled={isComputed}
							placeholder="hrs, %, kg…"
							maxlength="8"
							aria-labelledby="suffix-label-{field.key || index}"
						/>
						<span class="advanced-hint">Shown next to the number when rendered.</span>
					</div>
				{/if}

				<!-- Relation target (relation only) -->
				{#if isRelation}
					<div class="advanced-row">
						<span class="advanced-label" id="relation-label-{field.key || index}">Relates to</span>
						{#if collections.length === 0}
							<span class="advanced-hint advanced-hint--alone">
								No collections available to link. Create another collection first.
							</span>
						{:else}
							<select
								class="advanced-input"
								value={field.collection ?? ''}
								onchange={onCollectionInput}
								disabled={isComputed}
								aria-labelledby="relation-label-{field.key || index}"
							>
								<option value="">— pick a collection —</option>
								{#each collections as c (c.slug)}
									<option value={c.slug}>
										{c.icon ? `${c.icon} ` : ''}{c.name}
									</option>
								{/each}
							</select>
						{/if}
					</div>
				{/if}
			</div>
		{/if}
	</div>
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

	/* ── Computed badge ───────────────────────────────────────────────────── */

	.field-computed-badge {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-1) var(--space-3);
		margin: 0 var(--space-3) var(--space-2);
		background: color-mix(in srgb, var(--text-muted) 8%, transparent);
		border-radius: var(--radius-sm);
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		width: fit-content;
	}

	.computed-dot {
		width: 6px;
		height: 6px;
		border-radius: 50%;
		background: currentColor;
		opacity: 0.7;
	}

	/* ── Advanced section ─────────────────────────────────────────────────── */

	.field-advanced {
		border-top: 1px solid var(--border);
	}

	.advanced-toggle {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.78em;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		cursor: pointer;
		text-align: left;
	}

	.advanced-toggle:hover {
		color: var(--text-secondary);
		background: color-mix(in srgb, var(--text-muted) 4%, transparent);
	}

	.advanced-chevron {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		transition: transform 0.15s ease;
	}

	.advanced-chevron.open {
		transform: rotate(90deg);
	}

	.advanced-badge {
		margin-left: auto;
		color: var(--accent-blue);
		font-size: 0.9em;
		line-height: 1;
	}

	.advanced-content {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: 0 var(--space-3) var(--space-3);
	}

	.advanced-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}

	.advanced-row--inline {
		cursor: pointer;
	}

	.advanced-label {
		font-size: 0.78em;
		font-weight: 500;
		color: var(--text-secondary);
		min-width: 90px;
	}

	.advanced-hint {
		font-size: 0.72em;
		color: var(--text-muted);
	}

	.advanced-hint--alone {
		flex: 1;
		font-style: italic;
	}

	.advanced-input {
		flex: 1;
		min-width: 140px;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.advanced-input--short {
		flex: 0 0 auto;
		width: 90px;
		min-width: 90px;
	}

	.advanced-input:hover:not(:disabled) {
		border-color: var(--border);
	}

	.advanced-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.advanced-input:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.advanced-inline-check {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85em;
		color: var(--text-primary);
		cursor: pointer;
	}

	.advanced-inline-check input[type='checkbox']:disabled + span {
		opacity: 0.5;
	}
</style>
