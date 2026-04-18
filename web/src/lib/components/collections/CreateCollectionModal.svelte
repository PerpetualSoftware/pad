<script lang="ts">
	import { api } from '$lib/api/client';
	import type { CollectionCreate, FieldDef, CollectionSettings, QuickAction } from '$lib/types';
	import { COLLECTION_TEMPLATES, type CollectionTemplate } from './collection-templates';
	import EmojiPickerButton from '$lib/components/common/EmojiPickerButton.svelte';
	import FieldEditor, { type CollectionOption } from './FieldEditor.svelte';
	import {
		blankField,
		coerceDefault,
		fieldFromDef,
		isSafeDoneFieldKey,
		typeSupportsDefault,
		validateFieldKey,
		type EditableField
	} from './field-editor-types';
	import DisplaySettingsEditor from './DisplaySettingsEditor.svelte';
	import QuickActionsEditor, { type EditableQuickAction } from './QuickActionsEditor.svelte';
	import { placeholderContext, type PreviewContext } from '$lib/utils/quick-action-preview';
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
	let fields = $state<EditableField[]>([]);
	let selectedSettings = $state<CollectionSettings | null>(null);
	let creating = $state(false);
	let error = $state('');

	// Workspace collections list, used to populate the relation target picker
	// in FieldEditor. Fetched lazily on modal open. `collectionsRequestToken`
	// monotonically increases per fetch so a slow older response can't
	// overwrite a newer one (e.g. on rapid reopens / workspace switches).
	let collectionOptions = $state<CollectionOption[]>([]);
	let collectionsRequestToken = 0;

	// ── Advanced settings (Display + Quick Actions) ─────────────────────────
	// Default view / layout / group-by / sort-by / quick-actions can all be
	// configured at create time under a collapsible Advanced reveal. The
	// reveal defaults closed so the fast-path "pick a template, name it,
	// create" flow stays uncluttered.
	let showAdvanced = $state(false);
	let defaultView = $state<'list' | 'board' | 'table'>('list');
	let layout = $state<'fields-primary' | 'content-primary' | 'balanced'>('balanced');
	let boardGroupBy = $state('status');
	let listGroupBy = $state('');
	let listSortBy = $state('');
	let quickActions = $state<EditableQuickAction[]>([]);

	// Mirrors DoneFieldKey() in internal/models/terminal.go:
	// - only accepts `select` fields (multi_select is rejected — both
	//   paths only handle scalar string matching)
	// - requires the key to match the backend's safe-key pattern or the
	//   server falls back to "status"
	const activeDoneField = $derived.by(() => {
		const candidate = (boardGroupBy || '').trim();
		if (!candidate || !isSafeDoneFieldKey(candidate)) return 'status';
		const matches = fields.some((f) => f.key.trim() === candidate && f.type === 'select');
		return matches ? candidate : 'status';
	});

	// The collection doesn't exist yet, so the preview context falls back to
	// representative placeholder values. Updates live as the collection name
	// changes so the {collection} token reflects what will be saved.
	const previewContext = $derived<PreviewContext>(placeholderContext(name.trim()));

	// Derive group-by / sort options from the fields the user has added so
	// far. Mirrors the Edit modal's derivation but against EditableField[].
	const selectFieldKeys = $derived(
		fields
			.filter((f) => (f.type === 'select' || f.type === 'multi_select') && f.key.trim())
			.map((f) => ({ key: f.key.trim(), label: f.label.trim() || f.key.trim() }))
	);
	const sortableFieldKeys = $derived([
		...fields
			.filter((f) => f.key.trim())
			.map((f) => ({ key: f.key.trim(), label: f.label.trim() || f.key.trim() })),
		{ key: 'created_at', label: 'Created date' },
		{ key: 'updated_at', label: 'Updated date' },
		{ key: 'sort_order', label: 'Manual order' }
	]);

	// If the current boardGroupBy points at a field that no longer exists
	// (or the user removed all select fields), fall back to the first
	// available select field so the Display UI stays valid. We only do this
	// when the advanced section is open to avoid surprise state mutations
	// while the user hasn't engaged with it.
	$effect(() => {
		if (!showAdvanced) return;
		if (selectFieldKeys.length === 0) return;
		if (!selectFieldKeys.some((f) => f.key === boardGroupBy)) {
			boardGroupBy = selectFieldKeys[0].key;
		}
		if (listGroupBy && !selectFieldKeys.some((f) => f.key === listGroupBy)) {
			listGroupBy = '';
		}
	});

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
			showAdvanced = false;
			defaultView = 'list';
			layout = 'balanced';
			boardGroupBy = 'status';
			listGroupBy = '';
			listSortBy = '';
			quickActions = [];
			error = '';
			void loadCollectionOptions();
		}
		prevOpen = open;
	});

	async function loadCollectionOptions() {
		// Clear the stale list from a previous open before the new request
		// resolves, and bump the request token so only this call's response
		// is allowed to write state. Without the token guard, an older slow
		// response (e.g. from a previous open or workspace) could land after
		// a newer one and overwrite it.
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
			// Relation picker falls back to its empty-state hint.
			if (token !== collectionsRequestToken) return;
			collectionOptions = [];
		}
	}

	function resetForm() {
		name = '';
		selectedIcon = '';
		description = '';
		fields = [];
		selectedSettings = null;
		showAdvanced = false;
		defaultView = 'list';
		layout = 'balanced';
		boardGroupBy = 'status';
		listGroupBy = '';
		listSortBy = '';
		quickActions = [];
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
			// Pre-fill advanced state from the template's settings so the
			// Display tab reflects what the template ships with. The user can
			// still inspect/override via the Advanced reveal.
			const s = template.settings;
			if (s.default_view === 'list' || s.default_view === 'board' || s.default_view === 'table') {
				defaultView = s.default_view;
			}
			if (
				s.layout === 'fields-primary' ||
				s.layout === 'content-primary' ||
				s.layout === 'balanced'
			) {
				layout = s.layout;
			}
			if (s.board_group_by) boardGroupBy = s.board_group_by;
			if (s.list_group_by) listGroupBy = s.list_group_by;
			if (s.list_sort_by) listSortBy = s.list_sort_by;
			if (s.quick_actions) {
				quickActions = s.quick_actions.map((a) => ({
					label: a.label,
					prompt: a.prompt,
					scope: a.scope,
					icon: a.icon ?? ''
				}));
			}
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
					// Persist terminal-option markings for any select/multi_select
					// field. FieldEditor surfaces the terminal toggle on any
					// select-type field (T4 / TASK-597); templates and custom
					// schemas can ship their own terminal_options. Filter to
					// options that still exist in the final saved set so a
					// renamed/removed option doesn't leave a stale terminal
					// pointer.
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
					//
					// For select, always pass the full normalized options
					// array (including []) so that a field with no options
					// left can't retain a stale default. Using `def.options`
					// here is wrong: it's omitted when the list is empty, and
					// coerceDefault would then skip the membership check.
					if (f.default !== undefined && typeSupportsDefault(f.type)) {
						const optsForCoerce = f.type === 'select' ? opts : undefined;
						const coerced = coerceDefault(f.default, f.type, optsForCoerce);
						if (coerced !== undefined) def.default = coerced;
					}
					return def;
				});

			// Bundle the Advanced reveal state into the saved settings. Start
			// from any template-provided settings so template defaults (e.g.
			// Bug Tracker's board_group_by) are preserved, then overlay the
			// user's explicit choices. Quick actions are filtered to those
			// with both a label and a prompt so we don't persist empty rows.
			const savedActions: QuickAction[] = quickActions
				.filter((a) => a.label.trim() && a.prompt.trim())
				.map((a) => ({
					label: a.label.trim(),
					prompt: a.prompt.trim(),
					scope: a.scope,
					...(a.icon.trim() ? { icon: a.icon.trim() } : {})
				}));

			// Strip `quick_actions` from the template base so it can't survive
			// the "user removed all quick actions" case. Template quick actions
			// are already mirrored into `quickActions` state when the template
			// is picked, so `savedActions` is the single source of truth here —
			// taking anything from selectedSettings.quick_actions would let
			// stale rows resurrect.
			const { quick_actions: _templateActions, ...baseSettings } = selectedSettings ?? {};

			const settingsObj: CollectionSettings = {
				...baseSettings,
				default_view: defaultView,
				layout,
				board_group_by: boardGroupBy || undefined,
				list_group_by: listGroupBy || undefined,
				list_sort_by: listSortBy || undefined,
				...(savedActions.length > 0 ? { quick_actions: savedActions } : {})
			};

			const data: CollectionCreate = {
				name: name.trim(),
				icon: selectedIcon || undefined,
				description: description.trim() || undefined,
				schema: JSON.stringify({ fields: fieldDefs }),
				settings: JSON.stringify(settingsObj)
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
							<span class="template-icon-wrap" aria-hidden="true">
								<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
									<path d="M12 5v14M5 12h14" />
								</svg>
							</span>
							<span class="template-name">Blank</span>
							<span class="template-desc">Start from scratch</span>
						</button>
						{#each COLLECTION_TEMPLATES as template (template.id)}
							<button
								class="template-card"
								type="button"
								onclick={() => selectTemplate(template)}
							>
								<span class="template-icon" aria-hidden="true">{template.icon}</span>
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
						<EmojiPickerButton bind:value={selectedIcon} placeholder="+" size="md" />
						<input
							class="name-input"
							type="text"
							placeholder="Collection name"
							bind:value={name}
						/>
					</div>

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
										{activeDoneField}
										onmoveup={() => moveField(i, -1)}
										onmovedown={() => moveField(i, 1)}
										onremove={() => removeField(i)}
									/>
								{/each}
							</div>
						{/if}
						<button class="add-field-btn" type="button" onclick={addField}>+ Add field</button>
					</div>

					<!-- ── Advanced: Display settings + Quick Actions ─────────
						Defaults collapsed so the simple path stays short. The
						template pre-fill already lives in these states, so even
						if the user never opens Advanced the saved settings from
						a picked template are preserved.
					-->
					<section class="advanced-section">
						<button
							type="button"
							class="advanced-toggle"
							onclick={() => (showAdvanced = !showAdvanced)}
							aria-expanded={showAdvanced}
						>
							<span class="advanced-chevron" class:open={showAdvanced} aria-hidden="true">
								<svg width="10" height="10" viewBox="0 0 10 10" fill="none">
									<path
										d="M3 2L7 5L3 8"
										stroke="currentColor"
										stroke-width="1.5"
										stroke-linecap="round"
										stroke-linejoin="round"
									/>
								</svg>
							</span>
							<span>Advanced</span>
							<span class="advanced-sub">Display settings · Quick actions</span>
						</button>

						{#if showAdvanced}
							<div class="advanced-content">
								<div class="advanced-block">
									<h3 class="advanced-block-title">Display</h3>
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

								<div class="advanced-block">
									<h3 class="advanced-block-title">Quick actions</h3>
									<QuickActionsEditor bind:actions={quickActions} {previewContext} />
								</div>
							</div>
						{/if}
					</section>
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
		padding: 10vh var(--space-4) var(--space-4);
		animation: overlay-in 140ms ease-out;
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

	/* Honor prefers-reduced-motion */
	@media (prefers-reduced-motion: reduce) {
		.overlay, .modal { animation: none; }
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

	/*
	 * Unified card: both emoji-icon templates and the Blank card share the
	 * same structure. Blank gets a muted circular icon wrapper instead of
	 * a dashed outline so it feels like a first-class option rather than
	 * a fallback.
	 */
	.template-card {
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: var(--space-1);
		padding: var(--space-3) var(--space-4);
		min-height: 108px;
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

	.template-card:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}

	.template-icon {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		font-size: 1.4em;
		line-height: 1;
		margin-bottom: var(--space-1);
	}

	.template-icon-wrap {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		border-radius: 50%;
		background: color-mix(in srgb, var(--text-muted) 18%, transparent);
		color: var(--text-secondary);
		margin-bottom: var(--space-1);
	}

	.template-card--blank:hover .template-icon-wrap {
		background: color-mix(in srgb, var(--accent-blue) 20%, transparent);
		color: var(--accent-blue);
	}

	.template-name {
		font-size: 0.92em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.template-desc {
		font-size: 0.8em;
		color: var(--text-muted);
		line-height: 1.4;
	}

	/* -- Step 2: Icon + Name row ------------------------------------------- */

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
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.05em;
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

	/* ── Advanced reveal ──────────────────────────────────────────────────── */

	.advanced-section {
		margin-top: var(--space-4);
		border-top: 1px solid var(--border);
		padding-top: var(--space-3);
	}

	.advanced-toggle {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) 0;
		background: none;
		border: none;
		color: var(--text-secondary);
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		text-align: left;
	}

	.advanced-toggle:hover {
		color: var(--text-primary);
	}

	.advanced-chevron {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		transition: transform 0.15s ease;
		color: var(--text-muted);
	}

	.advanced-chevron.open {
		transform: rotate(90deg);
	}

	.advanced-sub {
		color: var(--text-muted);
		font-size: 0.82em;
		font-weight: 400;
	}

	.advanced-content {
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
		padding: var(--space-3) 0;
	}

	.advanced-block-title {
		margin: 0 0 var(--space-2);
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
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

		.template-grid {
			grid-template-columns: 1fr;
		}

		.modal-footer {
			flex-wrap: wrap;
			gap: var(--space-2);
		}

		.btn-cancel, .btn-create {
			flex: 1 1 auto;
		}
	}
</style>
