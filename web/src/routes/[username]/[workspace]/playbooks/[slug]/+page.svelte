<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { parseFields, parseSchema, type Collection, type Item } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import PlaybookFormFields from '$lib/components/playbooks/PlaybookFormFields.svelte';
	import {
		argumentsFromJSON,
		argumentsToJSON,
		type PlaybookArgument
	} from '$lib/playbooks/arguments';

	// Hardcoded fallbacks mirror the list-page convention — used until the
	// workspace's playbooks-collection schema lands.
	const FALLBACK_TRIGGERS = [
		'on-implement',
		'on-triage',
		'on-release',
		'on-plan',
		'on-review',
		'on-deploy',
		'manual'
	] as const;
	const FALLBACK_SCOPES = ['all', 'backend', 'frontend', 'mobile', 'devops'] as const;
	const FALLBACK_STATUSES = ['active', 'draft', 'deprecated'] as const;

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let ref = $derived(page.params.slug ?? '');

	let item = $state<Item | null>(null);
	let playbooksCollection = $state<Collection | null>(null);
	let existingPlaybooks = $state<Item[]>([]);
	let loading = $state(true);
	let saving = $state(false);

	// Form fields — initialized from the loaded item.
	let title = $state('');
	let bodyContent = $state('');
	let args = $state<PlaybookArgument[]>([]);
	let invocationSlug = $state('');
	let trigger = $state('manual');
	let scope = $state('all');
	let status = $state('draft');

	$effect(() => {
		if (wsSlug && ref) {
			loadItem(wsSlug, ref);
			loadPlaybooks(wsSlug);
			loadCollection(wsSlug);
		}
	});

	async function loadItem(ws: string, slugOrRef: string) {
		loading = true;
		try {
			const loaded = await api.items.get(ws, slugOrRef);
			// Stale-response guard: if the user moved away while the
			// request was in flight, drop the result rather than
			// rendering data from another route.
			if (ws !== wsSlug || slugOrRef !== ref) return;
			// `api.items.get` is cross-collection — `/playbooks/TASK-1`
			// would happily resolve to a task item, and Save would then
			// rewrite the task's fields as a playbook (Codex round 1 P2).
			// Gate on the loaded item's collection so the editor refuses
			// to touch non-playbook items.
			if (loaded.collection_slug !== 'playbooks') {
				const itemRef =
					loaded.collection_prefix && loaded.item_number
						? `${loaded.collection_prefix}-${loaded.item_number}`
						: slugOrRef;
				toastStore.show(
					`Not a playbook — ${itemRef} lives in ${loaded.collection_slug ?? 'another collection'}`,
					'error'
				);
				item = null;
				return;
			}
			item = loaded;
			title = loaded.title;
			bodyContent = loaded.content ?? '';
			const fields = parseFields(loaded);
			invocationSlug =
				typeof fields.invocation_slug === 'string' ? fields.invocation_slug : '';
			trigger = typeof fields.trigger === 'string' ? fields.trigger : 'manual';
			scope = typeof fields.scope === 'string' ? fields.scope : 'all';
			status = typeof fields.status === 'string' ? fields.status : 'draft';
			args = argumentsFromJSON(fields.arguments);
		} catch {
			if (ws !== wsSlug || slugOrRef !== ref) return;
			toastStore.show('Failed to load playbook', 'error');
		} finally {
			if (ws === wsSlug && slugOrRef === ref) loading = false;
		}
	}

	async function loadPlaybooks(ws: string) {
		try {
			const list = await api.items.listByCollection(ws, 'playbooks', {});
			if (ws !== wsSlug) return;
			existingPlaybooks = list;
		} catch {
			if (ws !== wsSlug) return;
			existingPlaybooks = [];
		}
	}

	async function loadCollection(ws: string) {
		playbooksCollection = null;
		try {
			const coll = await api.collections.get(ws, 'playbooks');
			if (ws !== wsSlug) return;
			playbooksCollection = coll;
		} catch {
			if (ws !== wsSlug) return;
			playbooksCollection = null;
		}
	}

	let schemaTriggers = $derived.by<readonly string[]>(() => {
		if (!playbooksCollection) return [];
		const schema = parseSchema(playbooksCollection);
		const field = schema.fields.find((f) => f.key === 'trigger');
		return field?.options ?? [];
	});

	let schemaScopes = $derived.by<readonly string[]>(() => {
		if (!playbooksCollection) return [];
		const schema = parseSchema(playbooksCollection);
		const field = schema.fields.find((f) => f.key === 'scope');
		return field?.options ?? [];
	});

	let schemaStatuses = $derived.by<readonly string[]>(() => {
		if (!playbooksCollection) return [];
		const schema = parseSchema(playbooksCollection);
		const field = schema.fields.find((f) => f.key === 'status');
		return field?.options ?? [];
	});

	let triggers = $derived<readonly string[]>(
		schemaTriggers.length > 0 ? schemaTriggers : (FALLBACK_TRIGGERS as readonly string[])
	);
	let scopes = $derived<readonly string[]>(
		schemaScopes.length > 0 ? schemaScopes : (FALLBACK_SCOPES as readonly string[])
	);
	let statuses = $derived<readonly string[]>(
		schemaStatuses.length > 0 ? schemaStatuses : (FALLBACK_STATUSES as readonly string[])
	);

	async function save() {
		if (!item) return;
		saving = true;
		try {
			const fieldsObj: Record<string, unknown> = {
				status,
				trigger,
				scope,
				// arguments goes in as a JSON VALUE (array of objects), not a
				// stringified array — the server stores it as a `json` field.
				// argumentsToJSON returns a string, so we parse it back to a
				// value before stuffing it into fieldsObj. This matches the
				// canonical shape in internal/collections/templates_startup_ship.go.
				arguments: JSON.parse(argumentsToJSON(args))
			};
			// Omit invocation_slug entirely when empty so the optional field
			// stays clean rather than emitting `"invocation_slug": ""`.
			if (invocationSlug.trim()) {
				fieldsObj.invocation_slug = invocationSlug.trim();
			}
			await api.items.update(wsSlug, item.slug, {
				title: title.trim(),
				content: bodyContent,
				fields: JSON.stringify(fieldsObj)
			});
			toastStore.show('Playbook saved', 'success');
			goto(`/${username}/${wsSlug}/playbooks`);
		} catch {
			toastStore.show('Failed to save playbook', 'error');
		} finally {
			saving = false;
		}
	}

	function cancel() {
		goto(`/${username}/${wsSlug}/playbooks`);
	}
</script>

<div class="edit-page">
	{#if loading}
		<div class="loading">Loading playbook…</div>
	{:else if !item}
		<div class="loading">Playbook not found.</div>
	{:else}
		<header class="edit-header">
			<div class="header-left">
				<a class="back-link" href="/{username}/{wsSlug}/playbooks">&larr; Back to playbooks</a>
				<input
					class="title-input"
					type="text"
					value={title}
					placeholder="Playbook title"
					oninput={(e) => (title = (e.currentTarget as HTMLInputElement).value)}
				/>
			</div>
			<div class="header-actions">
				<button type="button" class="btn btn-secondary" onclick={cancel}>Cancel</button>
				<button
					type="button"
					class="btn btn-primary"
					disabled={saving || !title.trim()}
					onclick={save}
				>
					{saving ? 'Saving…' : 'Save'}
				</button>
			</div>
		</header>

		<div class="edit-grid">
			<aside class="edit-sidebar">
				<PlaybookFormFields
					{wsSlug}
					selfItemId={item.id}
					{invocationSlug}
					{trigger}
					{scope}
					{status}
					{args}
					{bodyContent}
					{triggers}
					{scopes}
					{statuses}
					{existingPlaybooks}
					onSlugChange={(s) => (invocationSlug = s)}
					onTriggerChange={(t) => (trigger = t)}
					onScopeChange={(s) => (scope = s)}
					onStatusChange={(s) => (status = s)}
					onArgumentsChange={(a) => (args = a)}
					onBodyContentChange={(b) => (bodyContent = b)}
				/>
			</aside>

			<section class="edit-main">
				<label class="body-label" for="pbe-body">Body</label>
				<textarea
					id="pbe-body"
					class="body-textarea"
					value={bodyContent}
					oninput={(e) => (bodyContent = (e.currentTarget as HTMLTextAreaElement).value)}
					placeholder="Describe what this playbook does, its arguments, steps, defaults, and stop conditions."
				></textarea>
			</section>
		</div>
	{/if}
</div>

<style>
	.edit-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-6);
	}
	.loading {
		text-align: center;
		padding-top: 20vh;
		color: var(--text-muted);
	}
	.edit-header {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: var(--space-4);
		margin-bottom: var(--space-6);
	}
	.header-left {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		flex: 1;
		min-width: 0;
	}
	.back-link {
		font-size: 0.85em;
		color: var(--text-secondary);
		text-decoration: none;
	}
	.back-link:hover {
		color: var(--accent-blue);
	}
	.title-input {
		font-size: 1.4em;
		font-weight: 700;
		padding: var(--space-2) var(--space-3);
		background: transparent;
		border: 1px solid transparent;
		border-radius: var(--radius);
		color: var(--text-primary);
		width: 100%;
	}
	.title-input:hover {
		border-color: var(--border);
	}
	.title-input:focus {
		border-color: var(--accent-blue);
		background: var(--bg-secondary);
		outline: none;
	}
	.header-actions {
		display: flex;
		gap: var(--space-2);
		flex-shrink: 0;
	}
	.btn {
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.85em;
		font-weight: 600;
		cursor: pointer;
		border: none;
		transition: opacity 0.15s;
	}
	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.btn:hover:not(:disabled) {
		opacity: 0.85;
	}
	.btn-primary {
		background: var(--accent-blue);
		color: #fff;
	}
	.btn-secondary {
		background: transparent;
		color: var(--text-secondary);
		border: 1px solid var(--border);
	}
	.btn-secondary:hover:not(:disabled) {
		color: var(--text-primary);
		opacity: 1;
	}
	.edit-grid {
		display: grid;
		grid-template-columns: minmax(0, 1fr) minmax(0, 1.2fr);
		gap: var(--space-6);
		align-items: start;
	}
	.edit-sidebar {
		min-width: 0;
	}
	.edit-main {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 0;
		position: sticky;
		top: var(--space-4);
	}
	.body-label {
		font-size: 0.78em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-secondary);
	}
	.body-textarea {
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-primary);
		font-family: var(--font-mono);
		font-size: 0.88em;
		line-height: 1.6;
		min-height: 600px;
		max-height: 80vh;
		resize: vertical;
		width: 100%;
	}
	.body-textarea:focus {
		border-color: var(--accent-blue);
		outline: none;
	}
	@media (max-width: 1024px) {
		.edit-grid {
			grid-template-columns: 1fr;
		}
		.edit-main {
			position: static;
		}
	}
	@media (max-width: 768px) {
		.edit-header {
			flex-direction: column;
		}
		.header-actions {
			align-self: stretch;
		}
		.header-actions .btn {
			flex: 1;
		}
	}
</style>
