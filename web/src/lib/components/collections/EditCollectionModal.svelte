<script lang="ts">
	import { api } from '$lib/api/client';
	import type { Collection, CollectionUpdate, CollectionSettings, FieldDef, FieldMigration, QuickAction } from '$lib/types';
	import { parseSchema, parseSettings } from '$lib/types';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';
	import FieldEditor, { type CollectionOption } from './FieldEditor.svelte';
	import {
		blankField,
		coerceDefault,
		defaultsEqual,
		isSafeDoneFieldKey,
		typeSupportsDefault,
		validateFieldKey,
		type EditableField
	} from './field-editor-types';
	import DisplaySettingsEditor from './DisplaySettingsEditor.svelte';
	import QuickActionsEditor, { type EditableQuickAction } from './QuickActionsEditor.svelte';
	import {
		contextFromItem,
		placeholderContext,
		type PreviewContext
	} from '$lib/utils/quick-action-preview';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		open: boolean;
		collection: Collection;
		wsSlug: string;
		initialSection?: 'general' | 'fields' | 'display' | 'actions';
		onupdated: (updated?: Collection) => void;
		onclose: () => void;
	}

	let { open, collection, wsSlug, initialSection, onupdated, onclose }: Props = $props();

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
	let saving = $state(false);
	let error = $state('');

	// ── Field editing state ──────────────────────────────────────────────────
	// EditableField shape lives in `./field-editor-types.ts` so FieldEditor
	// and this modal share one type.

	let existingFields = $state<EditableField[]>([]);
	let newFields = $state<EditableField[]>([]);

	// Workspace collections list, used to populate the relation target picker
	// in FieldEditor for new relation-type fields. Fetched lazily on open.
	// `collectionsRequestToken` guards against a slow older response
	// overwriting a newer one on rapid reopens.
	let collectionOptions = $state<CollectionOption[]>([]);
	let collectionsRequestToken = 0;

	async function loadCollectionOptions() {
		const token = ++collectionsRequestToken;
		collectionOptions = [];
		try {
			const list = await api.collections.list(wsSlug);
			if (token !== collectionsRequestToken) return;
			collectionOptions = list.map((c) => ({
				slug: c.slug,
				name: c.name,
				icon: c.icon
			}));
		} catch {
			if (token !== collectionsRequestToken) return;
			collectionOptions = [];
		}
	}

	// ── Display settings state ──────────────────────────────────────────────

	let defaultView = $state<'list' | 'board' | 'table'>('list');
	let layout = $state<'fields-primary' | 'content-primary' | 'balanced'>('balanced');
	let boardGroupBy = $state('status');
	let listGroupBy = $state('');
	let listSortBy = $state('');

	// Which field drives backend done-detection for this collection.
	// Mirrors the DoneFieldKey() resolution in internal/models/terminal.go:
	// - only `select` fields qualify (not multi_select — both paths only
	//   handle scalar string matching)
	// - the key must match the backend's safe-key pattern or the server
	//   falls back to "status"; we apply the same check here so the UI
	//   can't show an "Active" pill on a field the server will reject.
	const activeDoneField = $derived.by(() => {
		const candidate = (boardGroupBy || '').trim();
		if (!candidate || !isSafeDoneFieldKey(candidate)) return 'status';
		const matches = existingFields.some((f) => f.key === candidate && f.type === 'select');
		return matches ? candidate : 'status';
	});

	// ── Quick actions state ─────────────────────────────────────────────────
	// Shape comes from QuickActionsEditor; the editor component owns the
	// per-card add/remove/reorder logic.

	let quickActions = $state<EditableQuickAction[]>([]);

	/**
	 * Preview context for the Quick Actions live preview. Populated from the
	 * first item in the collection on modal open; falls back to placeholder
	 * values when the collection is empty.
	 */
	let previewContext = $state<PreviewContext>(placeholderContext(''));

	async function loadPreviewContext() {
		try {
			const items = await api.items.listByCollection(wsSlug, collection.slug, {
				limit: 1
			});
			if (items && items.length > 0) {
				previewContext = contextFromItem(items[0], collection);
				return;
			}
		} catch {
			// Fall through to placeholder.
		}
		previewContext = placeholderContext(collection.name);
	}

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
				default: f.default,
				// Snapshot the load-time type AND default so the save path
				// can detect in-session type switches and default mutations
				// (used for the unsupported-type default preservation
				// logic). Without originalDefault, a user could round-trip
				// the type (relation -> text -> relation) with a new
				// default injected in the middle and we'd preserve the
				// stale value because originalType still matches.
				originalType: f.type,
				originalDefault: f.default
			}));
			newFields = [];
			error = '';
			activeTab = initialSection ?? 'general';
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
			void loadPreviewContext();
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
				// Normalize options into a local so we can both emit them on
				// def AND pass them to coerceDefault for select fields (even
				// when empty — an empty list must drop stale defaults).
				const normalizedOpts =
					f.type === 'select' || f.type === 'multi_select'
						? f.options.map((o) => o.trim()).filter(Boolean)
						: [];
				if (
					(f.type === 'select' || f.type === 'multi_select') &&
					normalizedOpts.length > 0
				) {
					def.options = normalizedOpts;
				}
				// Persist terminal-option markings for any select/multi_select
				// field (T4 / TASK-597). Filter to options that still exist
				// in the saved set so renames/removals don't leave stale
				// terminal pointers.
				if (
					(f.type === 'select' || f.type === 'multi_select') &&
					f.terminalOptions.length > 0 &&
					def.options
				) {
					const terms = f.terminalOptions.filter((t) => def.options!.includes(t));
					if (terms.length > 0) def.terminal_options = terms;
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
				// - If the active type is UI-unsupported for defaults
				//   (multi_select, relation), only preserve the default when
				//   BOTH type AND default are unchanged from load-time.
				//   Matching only on type misses the round-trip case:
				//   relation → text → relation with a new default injected
				//   in the middle. The UI hides default controls for the
				//   final type, so any mutation must be discarded.
				// - Anything else: drop the default.
				//
				// Pass the full normalized options array (including []) for
				// select so that removing all options drops a stale default.
				if (f.default !== undefined) {
					if (typeSupportsDefault(f.type)) {
						const optsForCoerce = f.type === 'select' ? normalizedOpts : undefined;
						const coerced = coerceDefault(f.default, f.type, optsForCoerce);
						if (coerced !== undefined) def.default = coerced;
					} else if (
						f.originalType === f.type &&
						defaultsEqual(f.default, f.originalDefault)
					) {
						def.default = f.default;
					}
					// else: type or default was altered in-session on a type
					// the UI can't represent — drop.
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
					// markings for any new select/multi_select field (T4 /
					// TASK-597). Filter to options that still exist in the
					// saved set.
					if (
						(f.type === 'select' || f.type === 'multi_select') &&
						f.terminalOptions.length > 0 &&
						def.options
					) {
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
					// defaults against the normalized option set). Pass the
					// full opts array (including []) so defaults are dropped
					// when the options list is empty.
					if (f.default !== undefined && typeSupportsDefault(f.type)) {
						const optsForCoerce = f.type === 'select' ? opts : undefined;
						const coerced = coerceDefault(f.default, f.type, optsForCoerce);
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
							<EmojiPickerButton bind:value={selectedIcon} placeholder="+" size="md" />
							<input
								class="name-input"
								type="text"
								placeholder="Collection name"
								bind:value={name}
							/>
						</div>

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

						{#if !collection.is_default}
							<section class="danger-zone" aria-labelledby="danger-zone-heading">
								<header class="danger-zone-header">
									<h3 id="danger-zone-heading" class="danger-zone-title">Danger zone</h3>
									<p class="danger-zone-hint">
										Archiving removes this collection and all its items from the workspace.
										This can't be undone from the UI.
									</p>
								</header>

								{#if confirmArchive}
									<div class="danger-zone-confirm" role="alertdialog" aria-labelledby="archive-confirm-msg">
										<p id="archive-confirm-msg" class="danger-zone-confirm-msg">
											Archive <strong>"{collection.name}"</strong> and all its items?
										</p>
										<div class="danger-zone-confirm-actions">
											<button
												class="btn-archive-confirm"
												type="button"
												onclick={handleArchive}
												disabled={archiving}
											>
												{archiving ? 'Archiving…' : 'Yes, archive'}
											</button>
											<button
												class="btn-archive-cancel"
												type="button"
												onclick={() => (confirmArchive = false)}
												disabled={archiving}
											>
												Cancel
											</button>
										</div>
									</div>
								{:else}
									<button class="btn-archive" type="button" onclick={handleArchive}>
										Archive collection
									</button>
								{/if}
							</section>
						{/if}
					</div>
				{:else if activeTab === 'fields'}
					<!-- ── Fields Tab ──────────────────────────────────────── -->
					<div class="tab-content">
						{#if existingFields.length === 0 && newFields.length === 0}
							<div class="empty-state">
								<div class="empty-state-icon" aria-hidden="true">
									<svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
										<path d="M3 6h18M3 12h18M3 18h12" />
									</svg>
								</div>
								<h4 class="empty-state-title">No fields yet</h4>
								<p class="empty-state-desc">
									Fields define the structured data each item in this collection carries —
									like status, priority, or a due date.
								</p>
							</div>
						{:else}
							<div class="fields-list">
								{#each existingFields as field, i (field.key)}
									<FieldEditor
										bind:field={existingFields[i]}
										index={i}
										total={existingFields.length}
										collections={collectionOptions}
										{activeDoneField}
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
										{activeDoneField}
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
						<DisplaySettingsEditor
							bind:defaultView
							bind:layout
							bind:boardGroupBy
							bind:listGroupBy
							bind:listSortBy
							{selectFieldKeys}
							{sortableFieldKeys}
						/>
					</div>
				{:else if activeTab === 'actions'}
					<!-- ── Quick Actions Tab ──────────────────────────────── -->
					<div class="tab-content">
						<QuickActionsEditor bind:actions={quickActions} {previewContext} />
					</div>
				{/if}
			</div>

			<div class="modal-footer">
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
		padding: 8vh var(--space-4) var(--space-4);
		animation: overlay-in 140ms ease-out;
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
		animation: modal-in 160ms ease-out;
	}

	@keyframes overlay-in {
		from { opacity: 0; }
		to { opacity: 1; }
	}

	@keyframes modal-in {
		from { opacity: 0; transform: translateY(-4px) scale(0.98); }
		to { opacity: 1; transform: translateY(0) scale(1); }
	}

	@media (prefers-reduced-motion: reduce) {
		.overlay, .modal { animation: none; }
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

	.form-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.form-label {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.05em;
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

	/* ── Empty state (Fields tab) ─────────────────────────────────────────── */

	.empty-state {
		display: flex;
		flex-direction: column;
		align-items: center;
		text-align: center;
		padding: var(--space-8) var(--space-4);
	}

	.empty-state-icon {
		color: var(--text-muted);
		opacity: 0.5;
		margin-bottom: var(--space-3);
	}

	.empty-state-title {
		margin: 0 0 var(--space-2);
		font-size: 1em;
		font-weight: 600;
		color: var(--text-secondary);
	}

	.empty-state-desc {
		margin: 0;
		max-width: 360px;
		font-size: 0.85em;
		line-height: 1.5;
		color: var(--text-muted);
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

	/* ── Danger zone ───────────────────────────────────────────────────────── */

	/*
	 * Lives at the bottom of the General tab. Visually separated by tint
	 * and border so destructive actions are unambiguously distinct from
	 * safe edits in the same tab.
	 */
	.danger-zone {
		margin-top: var(--space-6);
		padding: var(--space-4);
		background: color-mix(in srgb, #ef4444 6%, var(--bg-secondary));
		border: 1px solid color-mix(in srgb, #ef4444 30%, var(--border));
		border-radius: var(--radius);
	}

	.danger-zone-header {
		margin-bottom: var(--space-3);
	}

	.danger-zone-title {
		margin: 0 0 var(--space-1);
		font-size: 0.82em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #ef4444;
	}

	.danger-zone-hint {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-secondary);
		line-height: 1.5;
	}

	.btn-archive {
		padding: var(--space-2) var(--space-4);
		background: transparent;
		border: 1px solid #ef4444;
		border-radius: var(--radius);
		color: #ef4444;
		font-size: 0.88em;
		font-weight: 500;
		cursor: pointer;
		transition: background 0.15s, color 0.15s;
	}

	.btn-archive:hover {
		background: #ef4444;
		color: #fff;
	}

	.danger-zone-confirm {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}

	.danger-zone-confirm-msg {
		margin: 0;
		font-size: 0.9em;
		color: var(--text-primary);
	}

	.danger-zone-confirm-msg strong {
		color: #ef4444;
	}

	.danger-zone-confirm-actions {
		display: flex;
		gap: var(--space-2);
	}

	.btn-archive-confirm {
		padding: var(--space-2) var(--space-4);
		background: #ef4444;
		border: 1px solid #ef4444;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.88em;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-archive-confirm:hover:not(:disabled) {
		filter: brightness(1.08);
	}

	.btn-archive-confirm:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.btn-archive-cancel {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.88em;
		cursor: pointer;
	}

	.btn-archive-cancel:hover:not(:disabled) {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.btn-archive-cancel:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	/* ── Responsive ────────────────────────────────────────────────────────── */

	@media (max-width: 640px) {
		.overlay {
			padding: var(--space-3);
			align-items: stretch;
		}

		.modal {
			max-width: 100%;
			max-height: calc(100vh - var(--space-6));
		}

		.modal-header,
		.tab-content,
		.modal-footer {
			padding-left: var(--space-4);
			padding-right: var(--space-4);
		}

		.tab-bar {
			padding: 0 var(--space-4);
			overflow-x: auto;
			scrollbar-width: none;
			/* Fade mask at the right edge hints more tabs scroll off */
			mask-image: linear-gradient(to right, #000 0, #000 calc(100% - 24px), transparent);
		}

		.tab-bar::-webkit-scrollbar { display: none; }

		.tab {
			flex-shrink: 0;
		}

		.modal-footer {
			flex-wrap: wrap;
			gap: var(--space-2);
		}

		.btn-cancel, .btn-save {
			flex: 1 1 auto;
		}
	}
</style>
