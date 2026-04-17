<script lang="ts">
	import { api } from '$lib/api/client';
	import type { CollectionCreate, FieldDef, CollectionSettings } from '$lib/types';
	import { COLLECTION_TEMPLATES, type CollectionTemplate } from './collection-templates';
	import EmojiPicker from '$lib/components/common/EmojiPicker.svelte';
	import FieldEditor, { type CollectionOption } from './FieldEditor.svelte';
	import {
		blankField,
		coerceDefault,
		fieldFromDef,
		typeSupportsDefault,
		validateFieldKey,
		type EditableField
	} from './field-editor-types';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		open: boolean;
		wsSlug: string;
		oncreated: () => void;
		onclose: () => void;
	}

	let { open, wsSlug, oncreated, onclose }: Props = $props();

	// Step state: 'templates' or 'editor'
	let step = $state<'templates' | 'editor'>('templates');

	// Form state
	let name = $state('');
	let selectedIcon = $state('');
	let description = $state('');
	let showEmojiPicker = $state(false);
	let fields = $state<EditableField[]>([]);
	let selectedSettings = $state<CollectionSettings | null>(null);
	let creating = $state(false);
	let error = $state('');

	// Workspace collections list, used to populate the relation target picker
	// in FieldEditor. Fetched lazily on modal open.
	let collectionOptions = $state<CollectionOption[]>([]);

	// Track previous open state to detect open transitions
	let prevOpen = $state(false);

	// Reset to step 1 whenever the modal opens (false -> true transition)
	$effect.pre(() => {
		if (open && !prevOpen) {
			step = 'templates';
			name = '';
			selectedIcon = '';
			description = '';
			fields = [];
			selectedSettings = null;
			showEmojiPicker = false;
			error = '';
			void loadCollectionOptions();
		}
		prevOpen = open;
	});

	async function loadCollectionOptions() {
		// Clear the stale list from a previous open before the new request
		// resolves. Without this, reopening the modal (especially after a
		// workspace switch) briefly shows the old relation targets — and a
		// fast user can pick one and persist a slug that doesn't belong to
		// the current workspace.
		collectionOptions = [];
		try {
			const list = await api.collections.list(wsSlug);
			collectionOptions = list.map((c) => ({
				slug: c.slug,
				name: c.name,
				icon: c.icon
			}));
		} catch {
			// Relation picker falls back to its empty-state hint.
			collectionOptions = [];
		}
	}

	function resetForm() {
		name = '';
		selectedIcon = '';
		description = '';
		fields = [];
		selectedSettings = null;
		showEmojiPicker = false;
		error = '';
	}

	function selectTemplate(template: CollectionTemplate | null) {
		resetForm();
		if (template) {
			name = template.name;
			selectedIcon = template.icon;
			description = template.description;
			selectedSettings = { ...template.settings };
			// Template fields already have valid keys — use fieldFromDef with
			// existing=true so keyTouched=true and slugify doesn't overwrite.
			fields = template.fields.map((f) => fieldFromDef(f, true));
		}
		step = 'editor';
	}

	function goBack() {
		step = 'templates';
		resetForm();
	}

	function addField() {
		fields.push(blankField());
	}

	function removeField(index: number) {
		fields.splice(index, 1);
	}

	function moveField(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= fields.length) return;
		const temp = fields[index];
		fields[index] = fields[target];
		fields[target] = temp;
	}

	// ── Key validation (per-field + cross-field duplicate detection) ────────

	/**
	 * Compute a per-field key error, or null if the key is valid.
	 * - null: the field is empty (no label typed yet) — don't show an error,
	 *   but `hasBlockingErrors` still treats it as incomplete so Create is
	 *   disabled until the user fills it in.
	 * - a string: structural error (reserved, bad chars) or duplicate.
	 */
	const keyErrors = $derived.by(() => {
		const errors: (string | null)[] = [];
		// Count occurrences of each non-empty key across all fields, so we
		// can flag duplicates.
		const counts = new Map<string, number>();
		for (const f of fields) {
			const k = f.key.trim();
			if (k) counts.set(k, (counts.get(k) ?? 0) + 1);
		}
		for (const f of fields) {
			// Skip empty fields — user is still typing.
			if (!f.label.trim() && !f.key.trim()) {
				errors.push(null);
				continue;
			}
			const structural = validateFieldKey(f.key);
			if (structural) {
				errors.push(structural);
				continue;
			}
			if ((counts.get(f.key.trim()) ?? 0) > 1) {
				errors.push(`Duplicate key "${f.key.trim()}"`);
				continue;
			}
			errors.push(null);
		}
		return errors;
	});

	/** True if any field has a key error OR is partially filled (one of label/key empty). */
	const hasBlockingErrors = $derived.by(() => {
		if (keyErrors.some((e) => e !== null)) return true;
		// Any field with a label but no valid key, or vice versa, blocks save.
		for (const f of fields) {
			const hasLabel = !!f.label.trim();
			const hasKey = !!f.key.trim();
			if (hasLabel !== hasKey) return true;
		}
		return false;
	});

	async function handleCreate() {
		if (!name.trim() || creating || hasBlockingErrors) return;
		creating = true;
		error = '';
		try {
			const fieldDefs: FieldDef[] = fields
				.filter((f) => f.key.trim())
				.map((f) => {
					const key = f.key.trim();
					const label = f.label.trim() || key;
					const def: FieldDef = { key, label, type: f.type };
					const opts = f.options.map((o) => o.trim()).filter(Boolean);
					if ((f.type === 'select' || f.type === 'multi_select') && opts.length > 0) {
						def.options = opts;
					}
					// Persist terminal-option markings for status fields. Templates
					// may ship with terminal_options, and FieldEditor lets users
					// toggle them on a status field during create. Without this
					// the choices are silently dropped on save. Gated on
					// `key === 'status'` to mirror the current UI; T4 generalizes
					// this to any select field.
					if (key === 'status' && f.terminalOptions.length > 0 && def.options) {
						const terms = f.terminalOptions.filter((t) => def.options!.includes(t));
						if (terms.length > 0) def.terminal_options = terms;
					}
					// Advanced controls (T3 / TASK-596). Only emit when set so
					// payloads stay compact and round-trip with existing schemas.
					// Type-specific values are gated by `f.type` so stale state
					// from a prior type doesn't leak into the saved schema —
					// e.g. a user sets a number default/suffix, switches to
					// `relation`, and the hidden number values would otherwise
					// still be persisted.
					if (f.required) def.required = true;
					if (f.computed) def.computed = true;
					if (f.type === 'number' && f.suffix) def.suffix = f.suffix;
					if (f.type === 'relation' && f.collection) def.collection = f.collection;
					// Coerce default to match the active type. This catches
					// both type-switch drift (boolean default left on a text
					// field) and select whitespace drift (default raw text not
					// matching the normalized options set).
					if (f.default !== undefined && typeSupportsDefault(f.type)) {
						const coerced = coerceDefault(f.default, f.type, def.options);
						if (coerced !== undefined) def.default = coerced;
					}
					return def;
				});

			const data: CollectionCreate = {
				name: name.trim(),
				icon: selectedIcon || undefined,
				description: description.trim() || undefined,
				schema: JSON.stringify({ fields: fieldDefs }),
				settings: selectedSettings ? JSON.stringify(selectedSettings) : undefined
			};
			await api.collections.create(wsSlug, data);
			toastStore.show(`Created ${name.trim()}`, 'success');
			resetForm();
			oncreated();
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to create collection';
		} finally {
			creating = false;
		}
	}
</script>

<svelte:window
	onkeydown={(e) => {
		if (e.key === 'Escape' && open) onclose();
	}}
/>

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="overlay" onclick={onclose}>
		<div class="modal" onclick={(e) => e.stopPropagation()}>
			<div class="modal-header">
				{#if step === 'editor'}
					<div class="header-left">
						<button class="back-btn" type="button" onclick={goBack} aria-label="Back to templates">
							<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
								<path d="M10 12L6 8L10 4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
							</svg>
						</button>
						<h2>New Collection</h2>
					</div>
				{:else}
					<h2>New Collection</h2>
				{/if}
				<button class="close-btn" type="button" onclick={onclose}>&#10005;</button>
			</div>

			{#if step === 'templates'}
				<div class="modal-body">
					<p class="step-hint">Choose a template or start from scratch</p>
					<div class="template-grid">
						<button
							class="template-card template-card--blank"
							type="button"
							onclick={() => selectTemplate(null)}
						>
							<span class="template-icon">+</span>
							<span class="template-name">Blank</span>
							<span class="template-desc">Start from scratch</span>
						</button>
						{#each COLLECTION_TEMPLATES as template (template.id)}
							<button
								class="template-card"
								type="button"
								onclick={() => selectTemplate(template)}
							>
								<span class="template-icon">{template.icon}</span>
								<span class="template-name">{template.name}</span>
								<span class="template-desc">{template.description}</span>
							</button>
						{/each}
					</div>
				</div>
			{:else}
				<div class="modal-body">
					{#if error}
						<div class="error-banner">{error}</div>
					{/if}

					<div class="name-row">
						<button
							class="icon-btn"
							type="button"
							onclick={() => (showEmojiPicker = !showEmojiPicker)}
						>
							{#if selectedIcon}
								<span class="icon-preview">{selectedIcon}</span>
							{:else}
								<span class="icon-placeholder">+</span>
							{/if}
						</button>
						<input
							class="name-input"
							type="text"
							placeholder="Collection name"
							bind:value={name}
						/>
					</div>

					{#if showEmojiPicker}
						<div class="emoji-picker-container">
							<EmojiPicker
								selected={selectedIcon}
								onselect={(emoji) => {
									selectedIcon = emoji;
									showEmojiPicker = false;
								}}
							/>
						</div>
					{/if}

					<input
						class="desc-input"
						type="text"
						placeholder="Description (optional)"
						bind:value={description}
					/>

					<div class="fields-section">
						<span class="fields-label">Fields</span>
						{#if fields.length > 0}
							<div class="fields-list">
								{#each fields as _field, i (i)}
									<FieldEditor
										bind:field={fields[i]}
										index={i}
										total={fields.length}
										isNew
										keyError={keyErrors[i]}
										collections={collectionOptions}
										onmoveup={() => moveField(i, -1)}
										onmovedown={() => moveField(i, 1)}
										onremove={() => removeField(i)}
									/>
								{/each}
							</div>
						{/if}
						<button class="add-field-btn" type="button" onclick={addField}>+ Add field</button>
					</div>
				</div>

				<div class="modal-footer">
					<button class="btn-cancel" type="button" onclick={onclose}>Cancel</button>
					<button
						class="btn-create"
						type="button"
						onclick={handleCreate}
						disabled={!name.trim() || creating || hasBlockingErrors}
						title={hasBlockingErrors
							? 'Resolve the field errors before creating'
							: !name.trim()
								? 'Collection name is required'
								: ''}
					>
						{creating ? 'Creating...' : 'Create Collection'}
					</button>
				</div>
			{/if}
		</div>
	</div>
{/if}

<style>
	.overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		align-items: flex-start;
		padding-top: 10vh;
	}

	.modal {
		width: 100%;
		max-width: 520px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		overflow: hidden;
		max-height: 80vh;
		overflow-y: auto;
	}

	/* -- Header ------------------------------------------------------------ */

	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}

	.modal-header h2 {
		margin: 0;
		font-size: 1.1em;
		font-weight: 600;
	}

	.header-left {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.back-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		background: none;
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		padding: 0;
	}

	.back-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
		border-color: var(--border);
	}

	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
	}

	.close-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/* -- Body -------------------------------------------------------------- */

	.modal-body {
		padding: var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.error-banner {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
	}

	/* -- Step 1: Template picker ------------------------------------------- */

	.step-hint {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.template-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-3);
	}

	.template-card {
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-1);
		padding: var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		cursor: pointer;
		text-align: left;
		transition: border-color 0.15s ease, background 0.15s ease;
	}

	.template-card:hover {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 5%, var(--bg-tertiary));
	}

	.template-card--blank {
		border-style: dashed;
		border-color: var(--border);
	}

	.template-card--blank:hover {
		border-style: dashed;
		border-color: var(--accent-blue);
	}

	.template-icon {
		font-size: 1.5em;
		line-height: 1;
	}

	.template-name {
		font-size: 0.9em;
		font-weight: 600;
		color: var(--text-primary);
		margin-top: var(--space-1);
	}

	.template-desc {
		font-size: 0.78em;
		color: var(--text-muted);
		line-height: 1.35;
	}

	/* -- Step 2: Icon + Name row ------------------------------------------- */

	.name-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
	}

	.icon-btn {
		width: 48px;
		height: 48px;
		min-width: 48px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		cursor: pointer;
		font-size: 1.5em;
		line-height: 1;
		padding: 0;
	}

	.icon-btn:hover {
		border-color: var(--border);
	}

	.icon-preview {
		font-size: 1em;
	}

	.icon-placeholder {
		color: var(--text-muted);
		font-size: 0.8em;
	}

	.name-input {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 1.05em;
		color: var(--text-primary);
	}

	.name-input:hover {
		border-color: var(--border);
	}

	.name-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* -- Emoji picker ------------------------------------------------------ */

	.emoji-picker-container {
		margin-bottom: var(--space-3);
	}

	/* -- Description ------------------------------------------------------- */

	.desc-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.9em;
		color: var(--text-primary);
	}

	.desc-input:hover {
		border-color: var(--border);
	}

	.desc-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.desc-input::placeholder {
		color: var(--text-muted);
	}

	/* -- Fields builder ---------------------------------------------------- */

	.fields-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.fields-label {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.fields-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.add-field-btn {
		margin-top: var(--space-1);
		background: none;
		border: 1px dashed var(--border);
		color: var(--accent-blue);
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius);
		width: 100%;
		text-align: center;
	}

	.add-field-btn:hover {
		background: color-mix(in srgb, var(--accent-blue) 8%, transparent);
		border-color: var(--accent-blue);
	}

	/* -- Footer ------------------------------------------------------------ */

	.modal-footer {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-5);
		border-top: 1px solid var(--border);
	}

	.btn-cancel {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.9em;
		cursor: pointer;
	}

	.btn-cancel:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.btn-create {
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.9em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-create:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-create:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
