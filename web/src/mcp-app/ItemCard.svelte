<script lang="ts">
	// The pad_item card an MCP Apps host renders inline in a conversation.
	// Everything reaches the server through the bridge passed in as `connect`;
	// item text is only ever rendered as escaped text.
	import { onMount, tick } from 'svelte';
	import type { Bridge, HostContext, connectApp } from './bridge';
	import {
		CARD_ACTIONS, buildUpdateArgs, contentPreview, editableFields, isItemResult, parseToolError, shouldRenderCard,
		type ItemLike, type SchemaField, type ToolResultLike
	} from './model';

	// Wire names: internal/models/item.go (Item JSON tags). All optional on the wire.
	type CardItem = ItemLike & {
		collection_name?: string;
		updated_at?: string;
		content_state?: string;
		assigned_user_name?: string;
		agent_role_name?: string;
	};
	type Pending = { kind: 'title' } | { kind: 'field'; key: string };
	type Notice = { tone: 'info' | 'error'; text: string };

	let { connect }: { connect: typeof connectApp } = $props();

	let item = $state.raw<CardItem | null>(null);
	/** Shown instead of the card when the item itself could not be read. */
	let stub = $state.raw<{ ref: string; title?: string } | null>(null);
	let collections = $state.raw<Map<string, SchemaField[]> | null>(null);
	let pending = $state.raw<(Pending & { value: string }) | null>(null);
	let notice = $state.raw<Notice | null>(null);
	/** Non-null while the title is being edited. */
	let draft = $state<string | null>(null);
	let host = $state.raw<HostContext>({});
	let root = $state<HTMLElement>();

	// Plain variables on purpose: nothing renders from them.
	let args: Record<string, unknown> = {};
	let generation = 0;
	let schemaRequested = false;
	let schemaWorkspace: string | undefined;
	let bridge: Promise<Bridge> | undefined;

	onMount(() => {
		bridge = connect({
			onToolInput(next) { args = next; reset(); },
			onToolResult(result) { void showResult(result); },
			onHostContext(ctx) { host = { ...host, ...ctx }; }
		});
		// A host that never completes the handshake leaves an empty view, not an unhandled rejection.
		bridge.catch(() => {});
	});

	async function call(tool: string, callArgs: Record<string, unknown>): Promise<ToolResultLike> {
		if (!bridge) throw new Error('Not connected');
		return (await bridge).call(tool, callArgs);
	}

	function stringArg(key: string): string | undefined {
		const value = args[key];
		return typeof value === 'string' && value !== '' ? value : undefined;
	}
	/** Only when the tool input named one: the server resolves its own default otherwise. */
	function workspaceArg(): { workspace?: string } {
		const workspace = stringArg('workspace');
		return workspace === undefined ? {} : { workspace };
	}

	function reset() {
		generation++;
		item = null; stub = null; pending = null; notice = null; draft = null;
	}

	async function showResult(result: ToolResultLike) {
		reset();
		const mine = generation;
		const structured = result.structuredContent;
		if (shouldRenderCard(args, structured)) return adopt(structured as CardItem);
		const ref = stringArg('ref');
		if (!ref || !CARD_ACTIONS.some(action => action === args.action)) return;
		const fetched = await fetchItem(ref);
		if (mine !== generation) return;
		if (fetched) return adopt(fetched);
		const title = structured !== null && typeof structured === 'object' ? (structured as { title?: unknown }).title : undefined;
		stub = { ref, title: typeof title === 'string' && title !== '' ? title : undefined };
	}

	function adopt(next: CardItem) {
		item = next;
		stub = null;
		loadSchema();
	}

	async function fetchItem(ref: string): Promise<CardItem | null> {
		try {
			const result = await call('pad_item', { action: 'get', ref, ...workspaceArg() });
			return !result.isError && isItemResult(result.structuredContent) ? result.structuredContent : null;
		} catch {
			return null;
		}
	}

	/** One pad_collection list per workspace; a failure leaves the fields read-only. */
	function loadSchema() {
		const workspace = stringArg('workspace');
		if (schemaRequested && schemaWorkspace === workspace) return;
		schemaRequested = true;
		schemaWorkspace = workspace;
		collections = null;
		call('pad_collection', { action: 'list', ...workspaceArg() })
			.then(result => { if (schemaWorkspace === workspace) collections = parseCollections(result); })
			.catch(() => {});
	}

	// A top-level array is wrapped as {items: [...]} (internal/mcp/dispatch.go, packageJSONResult)
	// and `schema` stays a JSON string (internal/models/collection.go, Collection.Schema).
	function parseCollections(result: ToolResultLike): Map<string, SchemaField[]> {
		const out = new Map<string, SchemaField[]>();
		const rows = result.isError ? undefined : (result.structuredContent as { items?: unknown } | undefined)?.items;
		if (!Array.isArray(rows)) return out;
		for (const row of rows) {
			if (row === null || typeof row !== 'object' || typeof row.slug !== 'string') continue;
			let schema: unknown = row.schema;
			if (typeof schema === 'string') {
				try { schema = JSON.parse(schema); } catch { continue; }
			}
			const fields = (schema as { fields?: unknown } | null)?.fields;
			if (!Array.isArray(fields)) continue;
			out.set(row.slug, fields.filter((field): field is SchemaField =>
				field !== null && typeof field === 'object' && typeof field.key === 'string' && typeof field.type === 'string'));
		}
		return out;
	}

	const text = (value: unknown): string => value === undefined || value === null ? '' : String(value);
	const titleCase = (key: string): string => key.charAt(0).toUpperCase() + key.slice(1).replaceAll('_', ' ');

	const ref = $derived(item?.ref ?? '');
	const selects = $derived.by(() => {
		if (!item) return [];
		const current = item;
		const defs = collections?.get(current.collection_slug);
		if (!defs) {
			// No schema (yet): the two fields every card is expected to show, read-only.
			return ['status', 'priority']
				.map(key => ({ key, label: titleCase(key), options: [] as string[], value: text(current.fields?.[key]), editable: false }))
				.filter(field => field.value !== '');
		}
		const editable = new Set(editableFields({ fields: defs }, current).map(field => field.key));
		return defs.filter(def => def.type === 'select')
			.map(def => ({
				key: def.key, label: def.label || titleCase(def.key), options: def.options ?? [],
				value: text(current.fields?.[def.key]), editable: ref !== '' && editable.has(def.key)
			}))
			.filter(field => field.editable || field.value !== '');
	});
	const assignee = $derived(item?.assigned_user_name || item?.agent_role_name || '');
	// Still plain text: blank lines are dropped so a markdown body does not spend the preview on gaps.
	const preview = $derived(contentPreview(item?.content).replace(/\n\s*\n/g, '\n').trim());
	const updated = $derived.by(() => {
		const iso = item?.updated_at;
		const date = iso ? new Date(iso) : undefined;
		if (!iso || !date || Number.isNaN(date.getTime())) return undefined;
		let label: string;
		try {
			label = new Intl.DateTimeFormat(host.locale, { dateStyle: 'medium', timeStyle: 'short', timeZone: host.timeZone }).format(date);
		} catch {
			label = date.toLocaleString();
		}
		return { iso, label };
	});
	// Values: internal/models/item.go (ContentStatePendingFlush, ContentStateSetAside). Absent means current.
	const contentNote = $derived.by(() => {
		const state = item?.content_state;
		if (!state || state === 'current') return '';
		if (state === 'applied_pending_flush') return 'Live edits are not saved to the item yet; this preview may be behind.';
		if (state === 'superseded_set_aside') return 'Some edits were set aside; this preview does not include them.';
		return `This preview may be out of date (${state}).`;
	});
	const shownTitle = $derived(pending?.kind === 'title' ? pending.value : item?.title ?? '');
	const locked = $derived(pending !== null);
	const insets = $derived(host.safeAreaInsets);

	async function save(change: Pending & { value: string }) {
		if (!item || pending || ref === '') return;
		const before = item;
		const mine = generation;
		pending = change;
		notice = null;
		let next: CardItem | null = null;
		let outcome: Notice | null = null;
		try {
			const result = await call('pad_item', buildUpdateArgs({
				...workspaceArg(), ref, expectedSeq: before.seq,
				...(change.kind === 'title' ? { title: change.value } : { field: { key: change.key, value: change.value } })
			}));
			const error = parseToolError(result);
			if (!error) {
				next = isItemResult(result.structuredContent) ? result.structuredContent : await fetchItem(ref);
				if (!next) outcome = { tone: 'error', text: 'Saved, but the item could not be reloaded' };
			} else if (error.code === 'update_conflict') {
				next = await fetchItem(ref);
				outcome = next ? { tone: 'info', text: 'Changed elsewhere; reloaded' } : { tone: 'error', text: 'Changed elsewhere; could not reload' };
			} else {
				outcome = { tone: 'error', text: error.message };
			}
		} catch (error) {
			outcome = { tone: 'error', text: error instanceof Error && error.message ? error.message : 'Update failed' };
		}
		// A newer tool result replaced this card while the call was out.
		if (mine !== generation) return;
		if (next) item = next;
		pending = null;
		notice = outcome;
		// The control was disabled while saving, which drops focus to the body: hand it back.
		await tick();
		if (document.activeElement && document.activeElement !== document.body) return;
		const key = change.kind === 'title' ? 'title' : `field:${change.key}`;
		Array.from(root?.querySelectorAll<HTMLElement>('[data-control]') ?? []).find(el => el.dataset.control === key)?.focus();
	}

	function startTitleEdit() {
		if (!item || locked) return;
		draft = item.title;
	}
	async function cancelTitleEdit() {
		draft = null;
		await tick();
		root?.querySelector<HTMLElement>('[data-control="title"]')?.focus();
	}
	function commitTitle() {
		if (draft === null || !item) return;
		const value = draft.trim();
		if (value === '' || value === item.title) return void cancelTitleEdit();
		draft = null;
		void save({ kind: 'title', value });
	}
	function titleKeydown(event: KeyboardEvent) {
		if (event.isComposing) return;
		if (event.key === 'Enter') { event.preventDefault(); commitTitle(); }
		else if (event.key === 'Escape') { event.preventDefault(); void cancelTitleEdit(); }
	}
	function focusAndSelect(node: HTMLInputElement) {
		node.focus();
		node.select();
	}
</script>

{#if item}
	<article
		class="card" bind:this={root} aria-label={ref ? `${ref}: ${shownTitle}` : shownTitle} aria-busy={locked}
		style:--inset-top="{insets?.top ?? 0}px" style:--inset-right="{insets?.right ?? 0}px"
		style:--inset-bottom="{insets?.bottom ?? 0}px" style:--inset-left="{insets?.left ?? 0}px"
	>
		<header>
			<p class="eyebrow">
				{#if ref}<span class="ref">{ref}</span>{/if}
				<span class="collection">{item.collection_name || item.collection_slug}</span>
				<!-- Always present so the live region exists before it speaks, and saving never changes the card height. -->
				<span class="saving-status" role="status">{#if locked}Saving…{/if}</span>
			</p>
			{#if draft !== null}
				<div class="title-edit">
					<input
						class="title-input" type="text" aria-label="Title" autocomplete="off"
						bind:value={draft} onkeydown={titleKeydown} use:focusAndSelect
					/>
					<span class="title-actions">
						<button type="button" class="button primary" onclick={commitTitle}>Save</button>
						<button type="button" class="button" onclick={cancelTitleEdit}>Cancel</button>
					</span>
				</div>
			{:else}
				<div class="title-row">
					<h2 class="title" class:saving={pending?.kind === 'title'}>{shownTitle}</h2>
					{#if ref}
						<button type="button" class="icon-button" data-control="title" aria-label="Edit title" disabled={locked} onclick={startTitleEdit}>
							<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false">
								<path d="M11.2 2.3a1.5 1.5 0 0 1 2.1 0l.4.4a1.5 1.5 0 0 1 0 2.1l-7.6 7.6-3 .7.6-3.1z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
							</svg>
						</button>
					{/if}
				</div>
			{/if}
		</header>

		{#if selects.length > 0}
			<div class="fields">
				{#each selects as field (field.key)}
					{@const saving = pending?.kind === 'field' && pending.key === field.key}
					{@const value = saving && pending ? pending.value : field.value}
					{#if field.editable}
						<label class="field">
							<span class="label">{field.label}</span>
							<select
								class="select" class:saving data-control="field:{field.key}" disabled={locked} {value}
								onchange={event => save({ kind: 'field', key: field.key, value: event.currentTarget.value })}
							>
								{#if value === ''}<option value="" disabled>Not set</option>{/if}
								{#each field.options as option}<option value={option}>{option}</option>{/each}
								{#if value !== '' && !field.options.includes(value)}<option {value}>{value}</option>{/if}
							</select>
						</label>
					{:else}
						<div class="field">
							<span class="label">{field.label}</span>
							<span class="value">{field.value}</span>
						</div>
					{/if}
				{/each}
			</div>
		{/if}

		{#if notice}
			<p class="notice" class:error={notice.tone === 'error'} role={notice.tone === 'error' ? 'alert' : 'status'}>
				<span>{notice.text}</span>
				<button type="button" class="icon-button" aria-label="Dismiss notice" disabled={locked} onclick={() => (notice = null)}>
					<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" focusable="false">
						<path d="M4 4l8 8M12 4l-8 8" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
					</svg>
				</button>
			</p>
		{/if}

		{#if preview}
			<p class="preview">{preview}</p>
		{/if}
		{#if contentNote}
			<p class="content-note">{contentNote}</p>
		{/if}

		{#if assignee || updated}
			<footer>
				{#if assignee}<span>Assigned to <span class="who">{assignee}</span></span>{/if}
				{#if updated}<span>Updated <time datetime={updated.iso}>{updated.label}</time></span>{/if}
			</footer>
		{/if}
	</article>
{:else if stub}
	<article class="card stub" aria-label={stub.ref}>
		<p class="eyebrow"><span class="ref">{stub.ref}</span></p>
		{#if stub.title}<p class="stub-title">{stub.title}</p>{/if}
	</article>
{/if}

<style>
	/* Host style variables (applied on :root by the bridge) win; these are the fallbacks. */
	:global(:root) {
		color-scheme: light dark;
		--fallback-bg: #ffffff;
		--fallback-surface: #f4f3ef;
		--fallback-text: #1f1e1c;
		--fallback-muted: #66645e;
		--fallback-border: #d6d3cb;
		--fallback-ring: #2c6bd6;
		--fallback-danger: #a3271c;
		--fallback-danger-bg: #fbeceb;
		--fallback-info-bg: #eef2f8;
	}
	:global(:root[data-theme='dark']) {
		--fallback-bg: #262624;
		--fallback-surface: #30302e;
		--fallback-text: #f2f0ea;
		--fallback-muted: #b0ada4;
		--fallback-border: #4a4945;
		--fallback-ring: #7aa7f5;
		--fallback-danger: #f1a199;
		--fallback-danger-bg: #44211e;
		--fallback-info-bg: #2b3340;
	}
	@media (prefers-color-scheme: dark) {
		:global(:root:not([data-theme])) {
			--fallback-bg: #262624;
			--fallback-surface: #30302e;
			--fallback-text: #f2f0ea;
			--fallback-muted: #b0ada4;
			--fallback-border: #4a4945;
			--fallback-ring: #7aa7f5;
			--fallback-danger: #f1a199;
			--fallback-danger-bg: #44211e;
			--fallback-info-bg: #2b3340;
		}
	}

	.card {
		--bg: var(--color-background-primary, var(--fallback-bg));
		--surface: var(--color-background-secondary, var(--fallback-surface));
		--text: var(--color-text-primary, var(--fallback-text));
		--muted: var(--color-text-secondary, var(--fallback-muted));
		--border: var(--color-border-primary, var(--fallback-border));
		--ring: var(--color-ring-primary, var(--fallback-ring));
		--danger: var(--color-text-danger, var(--fallback-danger));
		--danger-bg: var(--color-background-danger, var(--fallback-danger-bg));
		--info-bg: var(--color-background-info, var(--fallback-info-bg));
		--radius: var(--border-radius-md, 8px);
		--small: var(--font-text-xs-size, 0.75rem);
		--body: var(--font-text-sm-size, 0.875rem);

		box-sizing: border-box;
		display: flex;
		flex-direction: column;
		gap: 12px;
		padding: calc(14px + var(--inset-top, 0px)) calc(16px + var(--inset-right, 0px)) calc(12px + var(--inset-bottom, 0px)) calc(16px + var(--inset-left, 0px));
		background: var(--bg);
		color: var(--text);
		font-family: var(--font-sans, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif);
		font-size: var(--body);
		line-height: 1.45;
		overflow-wrap: anywhere;
	}
	.card :global(*) { box-sizing: border-box; }
	p, h2 { margin: 0; }

	header { display: flex; flex-direction: column; gap: 4px; }
	.eyebrow { display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px; font-size: var(--small); color: var(--muted); }
	.ref { font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace); font-weight: var(--font-weight-semibold, 600); color: var(--text); letter-spacing: 0.01em; }

	.title-row { display: flex; align-items: flex-start; gap: 6px; }
	.title { min-width: 0; max-width: 46rem; font-size: var(--font-heading-xs-size, 1.0625rem); line-height: 1.3; font-weight: var(--font-weight-semibold, 600); }
	.title.saving, .select.saving { color: var(--muted); font-style: italic; }

	.title-edit { display: flex; flex-wrap: wrap; gap: 6px; }
	.title-input { flex: 1 1 16rem; min-width: 0; max-width: 46rem; }
	.title-actions { display: flex; gap: 6px; }

	.title-input, .select, .button {
		min-height: 32px;
		padding: 4px 8px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg);
		color: var(--text);
		font: inherit;
	}
	.select { width: 100%; padding-right: 4px; cursor: pointer; }
	.button { padding: 4px 12px; background: var(--surface); cursor: pointer; }
	.button.primary { font-weight: var(--font-weight-medium, 500); border-color: var(--text); }
	.icon-button {
		flex: none;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 26px;
		height: 26px;
		margin-top: -2px;
		padding: 0;
		border: 0;
		border-radius: var(--radius);
		background: transparent;
		color: var(--muted);
		cursor: pointer;
	}
	.icon-button:hover:not(:disabled), .button:hover:not(:disabled) { background: var(--surface); color: var(--text); }
	.select:hover:not(:disabled) { border-color: var(--muted); }
	.card :global(:disabled) { cursor: default; opacity: 0.6; }
	.card :global(:focus-visible) { outline: 2px solid var(--ring); outline-offset: 2px; }

	.fields { display: grid; grid-template-columns: repeat(auto-fill, minmax(7rem, 1fr)); gap: 10px 12px; max-width: 46rem; }
	.field { display: flex; flex-direction: column; gap: 3px; min-width: 0; }
	.label { font-size: var(--small); color: var(--muted); }
	.value { display: flex; align-items: center; min-height: 32px; }

	.notice {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 8px;
		max-width: 46rem;
		padding: 4px 4px 4px 10px;
		border-radius: var(--radius);
		background: var(--info-bg);
	}
	.notice.error { background: var(--danger-bg); color: var(--danger); }
	.notice .icon-button { margin-top: 0; color: inherit; }

	.preview {
		display: -webkit-box;
		-webkit-box-orient: vertical;
		-webkit-line-clamp: 6;
		line-clamp: 6;
		overflow: hidden;
		max-width: 46rem;
		padding-left: 10px;
		border-left: 2px solid var(--border);
		color: var(--muted);
		white-space: pre-wrap;
	}
	.content-note { max-width: 46rem; font-size: var(--small); color: var(--muted); font-style: italic; }

	footer { display: flex; flex-wrap: wrap; gap: 2px 16px; font-size: var(--small); color: var(--muted); }
	.who { color: var(--text); }
	.saving-status { margin-left: auto; }

	.stub { flex-direction: row; flex-wrap: wrap; align-items: baseline; gap: 8px; padding-bottom: calc(14px + var(--inset-bottom, 0px)); }
	.stub-title { color: var(--text); }

	@media (pointer: coarse) {
		.title-input, .select, .button, .value { min-height: 40px; }
		.icon-button { width: 36px; height: 36px; }
	}
	@media (prefers-reduced-motion: no-preference) {
		.select, .icon-button, .button { transition: background-color 120ms, border-color 120ms; }
	}
</style>
