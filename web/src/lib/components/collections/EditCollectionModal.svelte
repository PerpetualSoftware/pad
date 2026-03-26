<script lang="ts">
	import { api } from '$lib/api/client';
	import type { Collection, CollectionUpdate, FieldDef, FieldMigration } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import EmojiPicker from '$lib/components/common/EmojiPicker.svelte';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		open: boolean;
		collection: Collection;
		wsSlug: string;
		onupdated: () => void;
		onclose: () => void;
	}

	let { open, collection, wsSlug, onupdated, onclose }: Props = $props();

	const FIELD_TYPES: FieldDef['type'][] = [
		'text',
		'number',
		'select',
		'multi_select',
		'date',
		'checkbox',
		'url',
		'relation'
	];

	// ── Core form state ──────────────────────────────────────────────────────

	let name = $state('');
	let selectedIcon = $state('');
	let description = $state('');
	let showEmojiPicker = $state(false);
	let saving = $state(false);
	let error = $state('');

	// ── Field editing state ──────────────────────────────────────────────────

	interface EditableField {
		key: string;
		label: string;
		type: FieldDef['type'];
		options: string[];
		originalOptions: string[];
		required?: boolean;
		computed?: boolean;
		collection?: string;
		suffix?: string;
		default?: any;
	}

	let existingFields = $state<EditableField[]>([]);
	let newFields = $state<{ key: string; type: FieldDef['type']; options: string }[]>([]);

	// ── Sync from collection when modal opens ────────────────────────────────

	$effect(() => {
		if (open && collection) {
			name = collection.name;
			selectedIcon = collection.icon || '';
			description = collection.description || '';
			const schema = parseSchema(collection);
			existingFields = schema.fields.map((f) => ({
				key: f.key,
				label: f.label || f.key,
				type: f.type,
				options: f.options ? [...f.options] : [],
				originalOptions: f.options ? [...f.options] : [],
				required: f.required,
				computed: f.computed,
				collection: f.collection,
				suffix: f.suffix,
				default: f.default
			}));
			newFields = [];
			showEmojiPicker = false;
			error = '';
		}
	});

	// ── Existing field actions ───────────────────────────────────────────────

	function moveField(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= existingFields.length) return;
		const temp = existingFields[index];
		existingFields[index] = existingFields[target];
		existingFields[target] = temp;
	}

	function removeExistingField(index: number) {
		existingFields.splice(index, 1);
	}

	// ── Option editing for select fields ─────────────────────────────────────

	function removeOption(field: EditableField, optIndex: number) {
		field.options.splice(optIndex, 1);
	}

	function addOption(field: EditableField) {
		field.options.push('');
	}

	// ── New field actions ────────────────────────────────────────────────────

	function addField() {
		newFields.push({ key: '', type: 'text', options: '' });
	}

	function removeNewField(index: number) {
		newFields.splice(index, 1);
	}

	// ── Build migrations ─────────────────────────────────────────────────────

	function buildMigrations(): FieldMigration[] {
		const migrations: FieldMigration[] = [];

		for (const field of existingFields) {
			if (field.type !== 'select' && field.type !== 'multi_select') continue;
			if (field.originalOptions.length === 0) continue;

			const renames: Record<string, string> = {};
			for (let i = 0; i < field.originalOptions.length; i++) {
				const oldVal = field.originalOptions[i];
				const newVal = field.options[i];
				if (newVal !== undefined && newVal !== oldVal && newVal.trim() !== '') {
					renames[oldVal] = newVal.trim();
				}
			}

			if (Object.keys(renames).length > 0) {
				migrations.push({ field: field.key, rename_options: renames });
			}
		}

		return migrations;
	}

	// ── Save ─────────────────────────────────────────────────────────────────

	async function handleSave() {
		if (!name.trim() || saving) return;
		saving = true;
		error = '';
		try {
			// Build existing fields back into FieldDef[]
			const updatedExisting: FieldDef[] = existingFields.map((f) => {
				const def: FieldDef = {
					key: f.key,
					label: f.label.trim() || f.key,
					type: f.type
				};
				if ((f.type === 'select' || f.type === 'multi_select') && f.options.length > 0) {
					def.options = f.options.map((o) => o.trim()).filter(Boolean);
				}
				if (f.required) def.required = true;
				if (f.computed) def.computed = true;
				if (f.collection) def.collection = f.collection;
				if (f.suffix) def.suffix = f.suffix;
				if (f.default !== undefined) def.default = f.default;
				return def;
			});

			// Build new fields
			const addedFields: FieldDef[] = newFields
				.filter((f) => f.key.trim())
				.map((f) => {
					const def: FieldDef = { key: f.key.trim(), label: f.key.trim(), type: f.type };
					if ((f.type === 'select' || f.type === 'multi_select') && f.options.trim()) {
						def.options = f.options
							.split(',')
							.map((o) => o.trim())
							.filter(Boolean);
					}
					return def;
				});

			const allFields = [...updatedExisting, ...addedFields];
			const migrations = buildMigrations();

			const data: CollectionUpdate = {
				name: name.trim(),
				icon: selectedIcon || undefined,
				description: description.trim() || undefined,
				schema: JSON.stringify({ fields: allFields })
			};

			if (migrations.length > 0) {
				data.migrations = migrations;
			}

			await api.collections.update(wsSlug, collection.slug, data);
			toastStore.show(`Updated ${name.trim()}`, 'success');
			onupdated();
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to update collection';
		} finally {
			saving = false;
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
				<h2>Edit Collection</h2>
				<button class="close-btn" type="button" onclick={onclose}>&#10005;</button>
			</div>

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

				<!-- Existing fields -->
				{#if existingFields.length > 0}
					<div class="fields-section">
						<div class="fields-header">
							<span class="fields-label">Fields</span>
						</div>
						{#each existingFields as field, i (field.key)}
							<div class="field-card">
								<div class="field-row">
									<div class="field-reorder">
										<button
											class="reorder-btn"
											type="button"
											disabled={i === 0}
											onclick={() => moveField(i, -1)}
											title="Move up"
										>&#9650;</button>
										<button
											class="reorder-btn"
											type="button"
											disabled={i === existingFields.length - 1}
											onclick={() => moveField(i, 1)}
											title="Move down"
										>&#9660;</button>
									</div>
									<input
										class="field-label-input"
										type="text"
										bind:value={field.label}
										placeholder="Field label"
									/>
									<select class="field-type-select" bind:value={field.type}>
										{#each FIELD_TYPES as ft (ft)}
											<option value={ft}>{ft.replace('_', ' ')}</option>
										{/each}
									</select>
									<button
										class="remove-field-btn"
										type="button"
										onclick={() => removeExistingField(i)}
										title="Remove field"
									>&#10005;</button>
								</div>

								{#if field.type === 'select' || field.type === 'multi_select'}
									<div class="options-area">
										<div class="options-list">
											{#each field.options as _opt, oi (oi)}
												<div class="option-chip">
													<input
														class="option-input"
														type="text"
														bind:value={field.options[oi]}
														placeholder="option"
													/>
													<button
														class="option-remove"
														type="button"
														onclick={() => removeOption(field, oi)}
														title="Remove option"
													>&#10005;</button>
												</div>
											{/each}
											<button
												class="option-add-btn"
												type="button"
												onclick={() => addOption(field)}
												title="Add option"
											>+</button>
										</div>
									</div>
								{/if}
							</div>
						{/each}
					</div>
				{/if}

				<!-- Add new fields -->
				<div class="fields-section">
					<div class="fields-header">
						<span class="fields-label">Add Fields</span>
						<button class="add-field-btn" type="button" onclick={addField}>+ Add</button>
					</div>

					{#each newFields as field, i (i)}
						<div class="field-row">
							<input
								class="field-name-input"
								type="text"
								placeholder="Field name"
								bind:value={field.key}
							/>
							<select class="field-type-select" bind:value={field.type}>
								{#each FIELD_TYPES as ft (ft)}
									<option value={ft}>{ft.replace('_', ' ')}</option>
								{/each}
							</select>
							{#if field.type === 'select' || field.type === 'multi_select'}
								<input
									class="field-options-input"
									type="text"
									placeholder="option1, option2, ..."
									bind:value={field.options}
								/>
							{/if}
							<button
								class="remove-field-btn"
								type="button"
								onclick={() => removeNewField(i)}
							>&#10005;</button>
						</div>
					{/each}
				</div>
			</div>

			<div class="modal-footer">
				<button class="btn-cancel" type="button" onclick={onclose}>Cancel</button>
				<button
					class="btn-save"
					type="button"
					onclick={handleSave}
					disabled={!name.trim() || saving}
				>
					{saving ? 'Saving...' : 'Save Changes'}
				</button>
			</div>
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

	/* ── Header ─────────────────────────────────────────────────────────────── */

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

	/* ── Body ───────────────────────────────────────────────────────────────── */

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

	/* ── Icon + Name row ────────────────────────────────────────────────────── */

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

	/* ── Emoji picker ───────────────────────────────────────────────────────── */

	.emoji-picker-container {
		margin-bottom: var(--space-3);
	}

	/* ── Description ────────────────────────────────────────────────────────── */

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

	/* ── Fields sections ────────────────────────────────────────────────────── */

	.fields-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.fields-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}

	.fields-label {
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.add-field-btn {
		background: none;
		border: none;
		color: var(--accent-blue);
		font-size: 0.85em;
		cursor: pointer;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
	}

	.add-field-btn:hover {
		background: color-mix(in srgb, var(--accent-blue) 10%, transparent);
	}

	/* ── Field card (existing fields) ──────────────────────────────────────── */

	.field-card {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2) var(--space-3);
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	/* ── Field rows ────────────────────────────────────────────────────────── */

	.field-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-wrap: wrap;
	}

	/* ── Reorder buttons ───────────────────────────────────────────────────── */

	.field-reorder {
		display: flex;
		flex-direction: column;
		gap: 1px;
	}

	.reorder-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.6em;
		cursor: pointer;
		padding: 0 var(--space-1);
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

	/* ── Field label input (existing fields) ───────────────────────────────── */

	.field-label-input {
		flex: 1;
		min-width: 100px;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.88em;
		color: var(--text-primary);
	}

	.field-label-input:hover {
		border-color: var(--border);
	}

	.field-label-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Field type select ─────────────────────────────────────────────────── */

	.field-type-select {
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid transparent;
		border-radius: var(--radius-sm);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
	}

	.field-type-select:hover {
		border-color: var(--border);
	}

	.field-type-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Remove field button ───────────────────────────────────────────────── */

	.remove-field-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
	}

	.remove-field-btn:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
	}

	/* ── Options area (select / multi_select) ──────────────────────────────── */

	.options-area {
		padding-left: 28px;
	}

	.options-list {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1);
		align-items: center;
	}

	.option-chip {
		display: flex;
		align-items: center;
		gap: 2px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 12px;
		padding: 1px 4px 1px 8px;
	}

	.option-input {
		background: transparent;
		border: none;
		outline: none;
		color: var(--text-primary);
		font-size: 0.8em;
		width: 72px;
		padding: 2px 0;
	}

	.option-input:focus {
		color: var(--accent-blue);
	}

	.option-remove {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.7em;
		cursor: pointer;
		padding: 2px 4px;
		border-radius: 50%;
		line-height: 1;
	}

	.option-remove:hover {
		color: var(--accent-red, #ef4444);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
	}

	.option-add-btn {
		background: none;
		border: 1px dashed var(--border);
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
		padding: 2px 10px;
		border-radius: 12px;
		line-height: 1.4;
	}

	.option-add-btn:hover {
		color: var(--accent-blue);
		border-color: var(--accent-blue);
	}

	/* ── New field rows ────────────────────────────────────────────────────── */

	.field-name-input {
		flex: 1;
		min-width: 120px;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.field-name-input:hover {
		border-color: var(--border);
	}

	.field-name-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.field-options-input {
		flex: 1 1 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.field-options-input:hover {
		border-color: var(--border);
	}

	.field-options-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Footer ─────────────────────────────────────────────────────────────── */

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

	.btn-save {
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.9em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-save:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-save:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
