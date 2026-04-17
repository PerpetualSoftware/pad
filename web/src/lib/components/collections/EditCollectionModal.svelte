<script lang="ts">
	import { api } from '$lib/api/client';
	import type { Collection, CollectionUpdate, CollectionSettings, FieldDef, FieldMigration, QuickAction } from '$lib/types';
	import { parseSchema, parseSettings } from '$lib/types';
	import EmojiPicker from '$lib/components/common/EmojiPicker.svelte';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';
	import FieldEditor, { type CollectionOption } from './FieldEditor.svelte';
	import {
		blankField,
		coerceDefault,
		typeSupportsDefault,
		validateFieldKey,
		type EditableField
	} from './field-editor-types';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		open: boolean;
		collection: Collection;
		wsSlug: string;
		onupdated: (updated?: Collection) => void;
		onclose: () => void;
	}

	let { open, collection, wsSlug, onupdated, onclose }: Props = $props();

	let confirmArchive = $state(false);
	let archiving = $state(false);

	async function handleArchive() {
		if (!confirmArchive) {
			confirmArchive = true;
			return;
		}
		archiving = true;
		try {
			await api.collections.delete(wsSlug, collection.slug);
			toastStore.show(`Archived "${collection.name}"`, 'success');
			onupdated();
			onclose();
		} catch (err) {
			toastStore.show('Failed to archive collection', 'error');
		} finally {
			archiving = false;
			confirmArchive = false;
		}
	}

	// ── Tab state ────────────────────────────────────────────────────────────
	let activeTab = $state<'general' | 'fields' | 'display' | 'actions'>('general');

	// ── Core form state ──────────────────────────────────────────────────────

	let name = $state('');
	let selectedIcon = $state('');
	let description = $state('');
	let showEmojiPicker = $state(false);
	let saving = $state(false);
	let error = $state('');

	// ── Field editing state ──────────────────────────────────────────────────
	// EditableField shape lives in `./field-editor-types.ts` so FieldEditor
	// and this modal share one type.

	let existingFields = $state<EditableField[]>([]);
	let newFields = $state<EditableField[]>([]);

	// Workspace collections list, used to populate the relation target picker
	// in FieldEditor for new relation-type fields. Fetched lazily on open.
	let collectionOptions = $state<CollectionOption[]>([]);

	async function loadCollectionOptions() {
		try {
			const list = await api.collections.list(wsSlug);
			collectionOptions = list.map((c) => ({
				slug: c.slug,
				name: c.name,
				icon: c.icon
			}));
		} catch {
			collectionOptions = [];
		}
	}

	// ── Display settings state ──────────────────────────────────────────────

	let defaultView = $state<'list' | 'board' | 'table'>('list');
	let layout = $state<'fields-primary' | 'content-primary' | 'balanced'>('balanced');
	let boardGroupBy = $state('status');
	let listGroupBy = $state('');
	let listSortBy = $state('');

	// ── Quick actions state ─────────────────────────────────────────────────

	interface EditableQuickAction {
		label: string;
		prompt: string;
		scope: 'item' | 'collection';
		icon: string;
	}

	let quickActions = $state<EditableQuickAction[]>([]);

	function addQuickAction(scope: 'item' | 'collection') {
		quickActions.push({ label: '', prompt: '', scope, icon: '' });
	}

	function removeQuickAction(index: number) {
		quickActions.splice(index, 1);
	}

	function moveQuickAction(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= quickActions.length) return;
		const temp = quickActions[index];
		quickActions[index] = quickActions[target];
		quickActions[target] = temp;
	}

	let itemActions = $derived(
		quickActions
			.map((a, i) => ({ action: a, index: i }))
			.filter(({ action }) => action.scope === 'item')
	);
	let collectionActions = $derived(
		quickActions
			.map((a, i) => ({ action: a, index: i }))
			.filter(({ action }) => action.scope === 'collection')
	);

	// Select fields available for grouping (derived from current fields)
	let selectFieldKeys = $derived(
		existingFields
			.filter((f) => f.type === 'select' || f.type === 'multi_select')
			.map((f) => ({ key: f.key, label: f.label || f.key }))
	);

	// All fields available for sorting
	let sortableFieldKeys = $derived([
		...existingFields.map((f) => ({ key: f.key, label: f.label || f.key })),
		{ key: 'created_at', label: 'Created date' },
		{ key: 'updated_at', label: 'Updated date' },
		{ key: 'sort_order', label: 'Manual order' }
	]);

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
				terminalOptions: f.terminal_options ? [...f.terminal_options] : [],
				required: f.required,
				computed: f.computed,
				collection: f.collection,
				suffix: f.suffix,
				default: f.default
			}));
			newFields = [];
			showEmojiPicker = false;
			error = '';
			activeTab = 'general';
			confirmArchive = false;

			// Sync display settings
			const s = parseSettings(collection);
			defaultView = (['board', 'list', 'table'].includes(s.default_view) ? s.default_view : 'list') as typeof defaultView;
			layout = (['fields-primary', 'content-primary', 'balanced'].includes(s.layout) ? s.layout : 'balanced') as typeof layout;
			boardGroupBy = s.board_group_by || 'status';
			listGroupBy = s.list_group_by || '';
			listSortBy = s.list_sort_by || '';

			// Sync quick actions
			quickActions = (s.quick_actions ?? []).map((a) => ({
				label: a.label,
				prompt: a.prompt,
				scope: a.scope,
				icon: a.icon ?? ''
			}));

			void loadCollectionOptions();
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

	// ── New field actions ────────────────────────────────────────────────────

	function addField() {
		newFields.push(blankField());
	}

	function removeNewField(index: number) {
		newFields.splice(index, 1);
	}

	function moveNewField(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= newFields.length) return;
		const temp = newFields[index];
		newFields[index] = newFields[target];
		newFields[target] = temp;
	}

	// ── New-field key validation ─────────────────────────────────────────────
	// Existing field keys are frozen, so validation only runs on newFields.
	// A new field's key can collide with:
	//   - another new field's key (duplicate within the add set)
	//   - an existing field's key (collision with already-saved schema)
	//   - a reserved key / structural violation

	const newKeyErrors = $derived.by(() => {
		const errors: (string | null)[] = [];
		const existingKeys = new Set(existingFields.map((f) => f.key.trim()));
		const newCounts = new Map<string, number>();
		for (const f of newFields) {
			const k = f.key.trim();
			if (k) newCounts.set(k, (newCounts.get(k) ?? 0) + 1);
		}
		for (const f of newFields) {
			if (!f.label.trim() && !f.key.trim()) {
				errors.push(null);
				continue;
			}
			const structural = validateFieldKey(f.key);
			if (structural) {
				errors.push(structural);
				continue;
			}
			const k = f.key.trim();
			if (existingKeys.has(k)) {
				errors.push(`Key "${k}" is already used by an existing field`);
				continue;
			}
			if ((newCounts.get(k) ?? 0) > 1) {
				errors.push(`Duplicate key "${k}"`);
				continue;
			}
			errors.push(null);
		}
		return errors;
	});

	/** True if any new field has a key error or is partially filled. */
	const hasNewFieldBlockingErrors = $derived.by(() => {
		if (newKeyErrors.some((e) => e !== null)) return true;
		for (const f of newFields) {
			const hasLabel = !!f.label.trim();
			const hasKey = !!f.key.trim();
			if (hasLabel !== hasKey) return true;
		}
		return false;
	});

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
		if (!name.trim() || saving || hasNewFieldBlockingErrors) return;
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
				if (f.key === 'status' && f.terminalOptions.length > 0) {
					// Only include terminal options that still exist in the options list
					def.terminal_options = f.terminalOptions.filter(t => def.options?.includes(t));
				}
				// Gate type-specific advanced values by the current type so
				// stale hidden values from a previous type (e.g. a number
				// default/suffix after switching to relation) don't leak
				// into the saved schema.
				if (f.required) def.required = true;
				if (f.computed) def.computed = true;
				if (f.type === 'relation' && f.collection) def.collection = f.collection;
				if (f.type === 'number' && f.suffix) def.suffix = f.suffix;
				// Default-value handling for existing fields:
				//
				// - If the active type has UI-editable defaults, run through
				//   coerceDefault so stale values from a prior type don't leak
				//   and select defaults are trimmed to match normalized
				//   options.
				// - If the active type is one the UI can't edit (multi_select,
				//   relation), pass the existing default through verbatim.
				//   These defaults only arrive via API / imports, so silently
				//   stripping them on save would mutate the schema in ways the
				//   user didn't intend. Let them round-trip untouched.
				if (f.default !== undefined) {
					if (typeSupportsDefault(f.type)) {
						const coerced = coerceDefault(f.default, f.type, def.options);
						if (coerced !== undefined) def.default = coerced;
					} else {
						def.default = f.default;
					}
				}
				return def;
			});

			// Build new fields.
			// T2: new fields now have a proper key/label split with slugified
			// keys. `FieldEditor` auto-syncs key <- slugify(label) until the
			// user manually edits the key, at which point the user's value is
			// kept verbatim. `newKeyErrors` / `hasNewFieldBlockingErrors`
			// prevent save when anything is invalid.
			const addedFields: FieldDef[] = newFields
				.filter((f) => f.key.trim() && f.label.trim())
				.map((f) => {
					const key = f.key.trim();
					const label = f.label.trim() || key;
					const def: FieldDef = { key, label, type: f.type };
					const opts = f.options.map((o) => o.trim()).filter(Boolean);
					if ((f.type === 'select' || f.type === 'multi_select') && opts.length > 0) {
						def.options = opts;
					}
					// Mirror the existing-fields path: persist terminal-option
					// markings for newly-added status fields. Without this,
					// choices made via the terminal toggle are silently dropped
					// on save.
					if (key === 'status' && f.terminalOptions.length > 0 && def.options) {
						const terms = f.terminalOptions.filter((t) => def.options!.includes(t));
						if (terms.length > 0) def.terminal_options = terms;
					}
					// Advanced controls (T3 / TASK-596). Only emit when set so
					// payloads stay compact and round-trip with existing schemas.
					// Type-specific values are gated by `f.type` so stale state
					// from a prior type doesn't leak into the saved schema.
					if (f.required) def.required = true;
					if (f.computed) def.computed = true;
					if (f.type === 'number' && f.suffix) def.suffix = f.suffix;
					if (f.type === 'relation' && f.collection) def.collection = f.collection;
					// Coerce default to the active type (and normalize select
					// defaults against the normalized option set).
					if (f.default !== undefined && typeSupportsDefault(f.type)) {
						const coerced = coerceDefault(f.default, f.type, def.options);
						if (coerced !== undefined) def.default = coerced;
					}
					return def;
				});

			const allFields = [...updatedExisting, ...addedFields];
			const migrations = buildMigrations();

			// Build quick actions (filter out empty labels)
			const savedActions: QuickAction[] = quickActions
				.filter((a) => a.label.trim() && a.prompt.trim())
				.map((a) => ({
					label: a.label.trim(),
					prompt: a.prompt.trim(),
					scope: a.scope,
					...(a.icon.trim() ? { icon: a.icon.trim() } : {})
				}));

			const settingsObj: CollectionSettings = {
				default_view: defaultView,
				layout,
				board_group_by: boardGroupBy || undefined,
				list_group_by: listGroupBy || undefined,
				list_sort_by: listSortBy || undefined,
				...(savedActions.length > 0 ? { quick_actions: savedActions } : {})
			};

			const data: CollectionUpdate = {
				name: name.trim(),
				icon: selectedIcon || undefined,
				description: description.trim() || undefined,
				schema: JSON.stringify({ fields: allFields }),
				settings: JSON.stringify(settingsObj)
			};

			if (migrations.length > 0) {
				data.migrations = migrations;
			}

			const updated = await api.collections.update(wsSlug, collection.slug, data);
			toastStore.show(`Updated ${name.trim()}`, 'success');
			onupdated(updated);
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

			<div class="tab-bar">
				<button
					class="tab"
					class:active={activeTab === 'general'}
					type="button"
					onclick={() => (activeTab = 'general')}
				>General</button>
				<button
					class="tab"
					class:active={activeTab === 'fields'}
					type="button"
					onclick={() => (activeTab = 'fields')}
				>Fields</button>
				<button
					class="tab"
					class:active={activeTab === 'display'}
					type="button"
					onclick={() => (activeTab = 'display')}
				>Display</button>
				<button
					class="tab"
					class:active={activeTab === 'actions'}
					type="button"
					onclick={() => (activeTab = 'actions')}
				>Quick Actions</button>
			</div>

			{#if error}
				<div class="error-banner">{error}</div>
			{/if}

			<div class="modal-body">
				{#if activeTab === 'general'}
					<!-- ── General Tab ──────────────────────────────────────── -->
					<div class="tab-content">
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

						<div class="form-group">
							<label class="form-label" for="edit-desc">Description</label>
							<input
								id="edit-desc"
								class="form-input"
								type="text"
								placeholder="What is this collection for?"
								bind:value={description}
							/>
						</div>

						{#if collection.prefix}
							<div class="form-group">
								<span class="form-label">Prefix</span>
								<div class="prefix-display">{collection.prefix}</div>
							</div>
						{/if}
					</div>
				{:else if activeTab === 'fields'}
					<!-- ── Fields Tab ──────────────────────────────────────── -->
					<div class="tab-content">
						{#if existingFields.length === 0 && newFields.length === 0}
							<div class="empty-state">No fields defined yet.</div>
						{:else}
							<div class="fields-list">
								{#each existingFields as field, i (field.key)}
									<FieldEditor
										bind:field={existingFields[i]}
										index={i}
										total={existingFields.length}
										collections={collectionOptions}
										onmoveup={() => moveField(i, -1)}
										onmovedown={() => moveField(i, 1)}
										onremove={() => removeExistingField(i)}
									/>
								{/each}
								{#each newFields as _field, i (i)}
									<FieldEditor
										bind:field={newFields[i]}
										index={i}
										total={newFields.length}
										isNew
										keyError={newKeyErrors[i]}
										collections={collectionOptions}
										onmoveup={() => moveNewField(i, -1)}
										onmovedown={() => moveNewField(i, 1)}
										onremove={() => removeNewField(i)}
									/>
								{/each}
							</div>
						{/if}

						<button class="add-field-btn" type="button" onclick={addField}>+ Add field</button>
					</div>
				{:else if activeTab === 'display'}
					<!-- ── Display Tab ─────────────────────────────────────── -->
					<div class="tab-content">
						<div class="settings-grid">
							<div class="setting-item">
								<label class="setting-label" for="edit-default-view">Default view</label>
								<select id="edit-default-view" class="setting-select" bind:value={defaultView}>
									<option value="list">List</option>
									<option value="board">Board</option>
									<option value="table">Table</option>
								</select>
							</div>

							<div class="setting-item">
								<label class="setting-label" for="edit-layout">Item layout</label>
								<select id="edit-layout" class="setting-select" bind:value={layout}>
									<option value="balanced">Balanced</option>
									<option value="fields-primary">Fields primary</option>
									<option value="content-primary">Content primary</option>
								</select>
							</div>

							{#if selectFieldKeys.length > 0}
								<div class="setting-item">
									<label class="setting-label" for="edit-board-group">Board group by</label>
									<select id="edit-board-group" class="setting-select" bind:value={boardGroupBy}>
										{#each selectFieldKeys as f (f.key)}
											<option value={f.key}>{f.label}</option>
										{/each}
									</select>
								</div>

								<div class="setting-item">
									<label class="setting-label" for="edit-list-group">List group by</label>
									<select id="edit-list-group" class="setting-select" bind:value={listGroupBy}>
										<option value="">None</option>
										{#each selectFieldKeys as f (f.key)}
											<option value={f.key}>{f.label}</option>
										{/each}
									</select>
								</div>
							{/if}

							<div class="setting-item">
								<label class="setting-label" for="edit-list-sort">List sort by</label>
								<select id="edit-list-sort" class="setting-select" bind:value={listSortBy}>
									<option value="">Default</option>
									{#each sortableFieldKeys as f (f.key)}
										<option value={f.key}>{f.label}</option>
									{/each}
								</select>
							</div>
						</div>
					</div>
				{:else if activeTab === 'actions'}
					<!-- ── Quick Actions Tab ──────────────────────────────── -->
					<div class="tab-content">
						<p class="tab-description">
							Quick actions copy agent prompts to your clipboard. Use template variables: <code>{'{ref}'}</code>, <code>{'{title}'}</code>, <code>{'{status}'}</code>, <code>{'{priority}'}</code>, <code>{'{collection}'}</code>, <code>{'{content}'}</code>, <code>{'{fields}'}</code>.
						</p>

						<div class="actions-section">
							<div class="actions-section-header">
								<span class="actions-section-title">Item actions</span>
								<button class="add-action-btn" type="button" onclick={() => addQuickAction('item')}>+ Add</button>
							</div>
							{#if itemActions.length > 0}
								{#each itemActions as { action, index } (index)}
									<div class="action-card">
										<div class="action-card-top">
											<EmojiPickerButton bind:value={action.icon} placeholder="⚡" />
											<input
												class="action-label-input"
												type="text"
												placeholder="Action label"
												bind:value={action.label}
											/>
											<div class="action-card-btns">
												<button class="reorder-btn" type="button" disabled={index === 0} onclick={() => moveQuickAction(index, -1)} title="Move up">&#9650;</button>
												<button class="reorder-btn" type="button" disabled={index === quickActions.length - 1} onclick={() => moveQuickAction(index, 1)} title="Move down">&#9660;</button>
												<button class="remove-field-btn" type="button" onclick={() => removeQuickAction(index)} title="Remove">&#10005;</button>
											</div>
										</div>
										<input
											class="action-prompt-input"
											type="text"
											placeholder="/pad implement {'{ref}'} &quot;{'{title}'}&quot;"
											bind:value={action.prompt}
										/>
									</div>
								{/each}
							{:else}
								<div class="empty-actions">No item actions defined.</div>
							{/if}
						</div>

						<div class="actions-section">
							<div class="actions-section-header">
								<span class="actions-section-title">Collection actions</span>
								<button class="add-action-btn" type="button" onclick={() => addQuickAction('collection')}>+ Add</button>
							</div>
							{#if collectionActions.length > 0}
								{#each collectionActions as { action, index } (index)}
									<div class="action-card">
										<div class="action-card-top">
											<EmojiPickerButton bind:value={action.icon} placeholder="⚡" />
											<input
												class="action-label-input"
												type="text"
												placeholder="Action label"
												bind:value={action.label}
											/>
											<div class="action-card-btns">
												<button class="reorder-btn" type="button" disabled={index === 0} onclick={() => moveQuickAction(index, -1)} title="Move up">&#9650;</button>
												<button class="reorder-btn" type="button" disabled={index === quickActions.length - 1} onclick={() => moveQuickAction(index, 1)} title="Move down">&#9660;</button>
												<button class="remove-field-btn" type="button" onclick={() => removeQuickAction(index)} title="Remove">&#10005;</button>
											</div>
										</div>
										<input
											class="action-prompt-input"
											type="text"
											placeholder="/pad triage all new items"
											bind:value={action.prompt}
										/>
									</div>
								{/each}
							{:else}
								<div class="empty-actions">No collection actions defined.</div>
							{/if}
						</div>
					</div>
				{/if}
			</div>

			<div class="modal-footer">
				{#if !collection.is_default}
					{#if confirmArchive}
						<span class="archive-confirm">
							<span class="archive-warn">Archive "{collection.name}" and all its items?</span>
							<button class="btn-archive-yes" type="button" onclick={handleArchive} disabled={archiving}>
								{archiving ? 'Archiving...' : 'Yes, archive'}
							</button>
							<button class="btn-cancel-sm" type="button" onclick={() => confirmArchive = false}>Cancel</button>
						</span>
					{:else}
						<button class="btn-archive" type="button" onclick={handleArchive}>Archive</button>
					{/if}
				{/if}
				<span class="footer-spacer"></span>
				<button class="btn-cancel" type="button" onclick={onclose}>Cancel</button>
				<button
					class="btn-save"
					type="button"
					onclick={handleSave}
					disabled={!name.trim() || saving || hasNewFieldBlockingErrors}
					title={hasNewFieldBlockingErrors
						? 'Resolve the new-field errors before saving'
						: !name.trim()
							? 'Collection name is required'
							: ''}
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
		padding-top: 8vh;
	}

	.modal {
		width: 100%;
		max-width: 680px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		display: flex;
		flex-direction: column;
		max-height: 82vh;
	}

	/* ── Header ─────────────────────────────────────────────────────────────── */

	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-6);
		flex-shrink: 0;
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

	/* ── Tab bar ────────────────────────────────────────────────────────────── */

	.tab-bar {
		display: flex;
		gap: 0;
		padding: 0 var(--space-6);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.tab {
		padding: var(--space-2) var(--space-4);
		font-size: 0.88em;
		font-weight: 500;
		color: var(--text-muted);
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		cursor: pointer;
		margin-bottom: -1px;
		transition: color 0.15s, border-color 0.15s;
	}

	.tab:hover {
		color: var(--text-secondary);
	}

	.tab.active {
		color: var(--accent-blue);
		border-bottom-color: var(--accent-blue);
	}

	/* ── Error banner ──────────────────────────────────────────────────────── */

	.error-banner {
		margin: var(--space-3) var(--space-6) 0;
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 12%, transparent);
		color: var(--accent-red, #ef4444);
		border-radius: var(--radius);
		font-size: 0.85em;
		flex-shrink: 0;
	}

	/* ── Body ───────────────────────────────────────────────────────────────── */

	.modal-body {
		flex: 1;
		overflow-y: auto;
		min-height: 0;
	}

	.tab-content {
		padding: var(--space-5) var(--space-6);
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	/* ── General tab ───────────────────────────────────────────────────────── */

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

	.emoji-picker-container {
		margin-bottom: var(--space-2);
	}

	.form-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.form-label {
		font-size: 0.82em;
		font-weight: 500;
		color: var(--text-muted);
	}

	.form-input {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.9em;
		color: var(--text-primary);
	}

	.form-input:hover {
		border-color: var(--border);
	}

	.form-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	.form-input::placeholder {
		color: var(--text-muted);
	}

	.prefix-display {
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.9em;
		color: var(--text-muted);
		font-family: var(--font-mono);
		letter-spacing: 0.04em;
	}

	/* ── Fields tab ────────────────────────────────────────────────────────── */

	.fields-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	/* Shared reorder button — used by Quick Actions rows. The Fields tab
	   gets its reorder-btn styles from FieldEditor.svelte. */
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

	.empty-state {
		padding: var(--space-6) var(--space-4);
		text-align: center;
		color: var(--text-muted);
		font-size: 0.88em;
	}

	/* ── Add field button ──────────────────────────────────────────────────── */

	.add-field-btn {
		margin-top: var(--space-3);
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

	/* ── Display tab ───────────────────────────────────────────────────────── */

	.settings-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: var(--space-4);
	}

	.setting-item {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.setting-label {
		font-size: 0.82em;
		font-weight: 500;
		color: var(--text-muted);
	}

	.setting-select {
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 0.88em;
		color: var(--text-primary);
		cursor: pointer;
	}

	.setting-select:hover {
		border-color: var(--border);
	}

	.setting-select:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* ── Footer ─────────────────────────────────────────────────────────────── */

	.modal-footer {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: var(--space-3);
		padding: var(--space-4) var(--space-6);
		border-top: 1px solid var(--border);
		flex-shrink: 0;
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

	.footer-spacer {
		flex: 1;
	}

	.btn-archive {
		padding: var(--space-2) var(--space-4);
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
	}

	.btn-archive:hover {
		border-color: #ef4444;
		color: #ef4444;
		background: rgba(239, 68, 68, 0.06);
	}

	.archive-confirm {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.archive-warn {
		font-size: 0.82em;
		color: #ef4444;
		font-weight: 500;
	}

	.btn-archive-yes {
		padding: var(--space-1) var(--space-3);
		background: #ef4444;
		border: none;
		border-radius: var(--radius-sm);
		color: #fff;
		font-size: 0.82em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-archive-yes:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-archive-yes:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-cancel-sm {
		padding: var(--space-1) var(--space-2);
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
	}

	.btn-cancel-sm:hover {
		color: var(--text-primary);
	}

	/* ── Quick Actions tab ─────────────────────────────────────────────────── */

	.tab-description {
		font-size: 0.82em;
		color: var(--text-muted);
		margin: 0;
		line-height: 1.5;
	}

	.tab-description code {
		font-family: var(--font-mono);
		font-size: 0.9em;
		background: var(--bg-tertiary);
		padding: 1px 5px;
		border-radius: var(--radius-sm);
	}

	.actions-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.actions-section-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}

	.actions-section-title {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.add-action-btn {
		padding: 2px var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.8em;
		cursor: pointer;
	}

	.add-action-btn:hover {
		background: var(--bg-secondary);
		color: var(--text-primary);
	}

	.action-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		border: 1px solid var(--border);
	}

	.action-card-top {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.action-icon-input {
		width: 36px;
		text-align: center;
		padding: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 1em;
		color: var(--text-primary);
	}

	.action-label-input {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		color: var(--text-primary);
	}

	.action-card-btns {
		display: flex;
		gap: 2px;
	}

	.action-prompt-input {
		width: 100%;
		padding: var(--space-1) var(--space-2);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: 0.82em;
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.empty-actions {
		font-size: 0.82em;
		color: var(--text-muted);
		padding: var(--space-2) 0;
	}
</style>
