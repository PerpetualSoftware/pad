<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { parseFields, parseSchema, itemUrlId, type Collection, type Item } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { titleEditError } from '$lib/items/titleLimit';
	import { createScrollRestoration } from '$lib/scroll/restore.svelte';
	import { exportAndDownloadArtifact } from '$lib/utils/artifacts';
	import PlaybookFormFields from '$lib/components/playbooks/PlaybookFormFields.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import {
		playbookFieldsPatch,
		storedFormMismatches,
		type PlaybookFormSnapshot
	} from '$lib/playbooks/editorPatch';
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
	let exporting = $state(false);

	// Scroll position restoration (BUG-1425). Form pages are usually short
	// enough that scroll position isn't critical, but if the body grows the
	// helper keeps offset preserved on back-nav like the rest of the app.
	//
	// The identity guards (item.slug === ref || issue-id === ref) ensure
	// we don't restore against stale content from a previous playbook
	// while loadItem() for the new ref is still in flight — same race
	// as the item detail page (Codex BUG-1425 round 4 P1-B).
	const scrollRestoration = createScrollRestoration({
		ready: () =>
			!loading &&
			item !== null &&
			(item.slug === ref ||
				`${item.collection_prefix}-${item.item_number}` === ref),
		persistKey: () =>
			wsSlug ? `pad-last-scroll-${wsSlug}-${page.url.pathname}` : null,
	});
	export const snapshot = scrollRestoration.snapshot;

	// Form fields — initialized from the loaded item.
	let title = $state('');
	let bodyContent = $state('');
	let args = $state<PlaybookArgument[]>([]);
	let invocationSlug = $state('');
	let trigger = $state('manual');
	let scope = $state('all');
	let status = $state('draft');

	// What the form held when the item loaded: save sends only the keys the
	// user changed from it (BUG-3075, see $lib/playbooks/editorPatch).
	let loadedForm = $state<PlaybookFormSnapshot | null>(null);
	/** The stored values themselves, for the note on one the form cannot show. */
	let storedRaw = $state<{ status: unknown; trigger: unknown; scope: unknown }>({
		status: undefined,
		trigger: undefined,
		scope: undefined
	});

	$effect(() => {
		if (wsSlug && ref) {
			loadItem(wsSlug, ref);
			loadPlaybooks(wsSlug);
			loadCollection(wsSlug);
		}
	});

	async function loadItem(ws: string, slugOrRef: string) {
		loading = true;
		// Clear the previously-loaded playbook BEFORE the new fetch so a
		// stale playbook isn't editable under a fresh URL while a 404 is
		// in flight (Codex round 4 P2). The catch path then leaves item
		// null and the template renders "Playbook not found."
		item = null;
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
			loadedForm = { status, trigger, scope, invocationSlug, args: argumentsToJSON(args) };
			storedRaw = { status: fields.status, trigger: fields.trigger, scope: fields.scope };
		} catch {
			if (ws !== wsSlug || slugOrRef !== ref) return;
			// Explicit null on the current-request error path so a failed
			// reload doesn't leave the previous item editable.
			item = null;
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

	let storedMismatches = $derived(storedFormMismatches(storedRaw, statuses, scopes));

	async function save() {
		if (!item) return;
		// BUG-3115: refuse a too-long title before sending; the form keeps it.
		// Only a CHANGED title: save() always re-sends it, and a legacy title
		// over the limit is grandfathered by the server as long as it is echoed.
		const limitError = titleEditError(title.trim(), item.title);
		if (limitError) {
			toastStore.show(limitError, 'error');
			return;
		}
		saving = true;
		try {
			// BUG-3049: name the five keys this editor owns and send nothing
			// else. The previous shape spread `parseFields(item)` and replaced
			// the whole blob — which preserved unknown keys as they stood at
			// LOAD time (Codex round 3 P2's concern) and therefore reverted any
			// field written by anyone else while the editor was open, including
			// a status toggle from the playbooks list page. A patch preserves
			// unknown keys by not naming them, which is the same protection
			// without the revert.
			const fieldsPatch = playbookFieldsPatch(
				{ status, trigger, scope, invocationSlug, args: argumentsToJSON(args) },
				loadedForm
			);
			await api.items.update(wsSlug, item.slug, {
				title: title.trim(),
				content: bodyContent,
				...(Object.keys(fieldsPatch).length ? { fields_patch: fieldsPatch } : {})
			});
			toastStore.show('Playbook saved', 'success');
			goto(`/${username}/${wsSlug}/playbooks`);
		} catch (err) {
			toastStore.show((err as Error)?.message || 'Failed to save playbook', 'error');
		} finally {
			saving = false;
		}
	}

	function cancel() {
		goto(`/${username}/${wsSlug}/playbooks`);
	}

	async function handleExport() {
		if (!item || exporting) return;
		exporting = true;
		try {
			await exportAndDownloadArtifact(wsSlug, itemUrlId(item));
			toastStore.show('Playbook exported', 'success');
		} catch (err: unknown) {
			toastStore.show(
				err instanceof Error ? err.message : 'Failed to export playbook',
				'error'
			);
		} finally {
			exporting = false;
		}
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
				<Button variant="secondary" onclick={cancel}>Cancel</Button>
				<Button
					variant="secondary"
					disabled={exporting}
					onclick={handleExport}
					title="Download this playbook as a .pad.md artifact"
				>
					{exporting ? 'Exporting…' : 'Export'}
				</Button>
				<Button
					variant="primary"
					disabled={saving || !title.trim()}
					onclick={save}
				>
					{saving ? 'Saving…' : 'Save'}
				</Button>
			</div>
		</header>

		<div class="edit-grid">
			<aside class="edit-sidebar">
				{#each storedMismatches as m (m.label)}
					<p class="stored-mismatch">
						{m.label} is stored as <code>{m.raw}</code>, which this form can't show. It is kept unless you change {m.label.toLowerCase()} here.
					</p>
				{/each}
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
	/* A stored value the form cannot show (BUG-3075). */
	.stored-mismatch {
		margin: 0 0 var(--space-2);
		padding: var(--space-2);
		font-size: 0.85em;
		color: var(--text-secondary);
		border: 1px dashed var(--border);
		border-radius: var(--radius-sm, 4px);
	}
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
		/* Equal-width header buttons on mobile — the shared Button primitive's
		   root carries the .btn class, so :global reaches into it. */
		.header-actions :global(.btn) {
			flex: 1;
		}
	}
</style>
