<script lang="ts">
	import { onDestroy, untrack } from 'svelte';
	import { page } from '$app/state';
	import { api, PadApiError } from '$lib/api/client';
	import { announceAttachmentDeleted } from '$lib/attachments/events';
	import { invalidateAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
	import type {
		AttachmentListItem,
		AttachmentListFilters,
		AttachmentListResponse,
		Collection,
		WorkspaceStorageInfo
	} from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { iconForAttachment, formatBytes, isImage } from '$lib/attachments/display';
	import {
		buildStorageFilters,
		hasActiveStorageFilters,
		type StorageFilterSelections
	} from '$lib/attachments/storageFilters';
	import AttachmentIcon from '$lib/attachments/icons/AttachmentIcon.svelte';
	import { viewIdentity, createFence, createPaintFence } from '$lib/attachments/viewFence';

	// ── Props ────────────────────────────────────────────────────────────────
	interface Props {
		wsSlug: string;
		collections: Collection[];
		/**
		 * Parent-item UUID to scope the list to — the item attachment strip's
		 * "View all (N)" continuation passes it via `?attachment_item=`
		 * (PLAN-2392 DR-18). Seeds the filter, and re-seeds whenever the
		 * deep-link CHANGES, so following a second item's link while this tab
		 * is already mounted retargets instead of keeping the old scope
		 * (Codex round 3). Clearing the scope in the UI rewrites the URL, which
		 * is why this can go back to '' without fighting the user.
		 */
		initialItemId?: string;
		/**
		 * Called when the user clears the item scope, so the OWNER of the URL
		 * can drop `?attachment_item=`. It lives up there because only a real
		 * navigation updates `page.url` — which is what `initialItemId` is
		 * derived from (Codex round 4).
		 */
		onClearScope?: () => void;
	}
	let { wsSlug, collections, initialItemId = '', onClearScope }: Props = $props();

	// ── State ────────────────────────────────────────────────────────────────
	let loading = $state(true);
	let usage = $state<WorkspaceStorageInfo | null>(null);
	let attachments = $state<AttachmentListItem[]>([]);
	let total = $state(0);
	let limit = $state(50);
	let offset = $state(0);

	// Filters (UI selections — empty string means "All" / not applied)
	type CategoryValue = '' | 'image' | 'video' | 'audio' | 'document' | 'text' | 'archive' | 'other';
	type ItemValue = '' | 'attached' | 'unattached';
	type SortValue =
		| 'created_at_desc'
		| 'created_at'
		| 'size_desc'
		| 'size'
		| 'filename'
		| 'filename_desc';

	let filterCategory = $state<CategoryValue>('');
	let filterItem = $state<ItemValue>('');
	/**
	 * Scope to a single parent item. Seeded from `initialItemId` (the strip's
	 * "View all" handoff) and cleared by the user. Mutually exclusive with the
	 * attached/unattached selector — combining `item_id` with `item=unattached`
	 * yields an empty set server-side — so while it is set, that selector is
	 * disabled and not sent.
	 */
	// untrack: the initializer takes the value at mount; ongoing changes are
	// handled by the re-seed effect below (reading a prop directly in an
	// initializer otherwise warns about capturing only the initial value).
	let filterItemId = $state<string>(untrack(() => initialItemId));
	// Last deep-link value applied, so the re-seed effect fires only on a
	// genuine change and never fights a scope the user cleared by hand.
	let seededItemId = untrack(() => initialItemId);
	let filterCollection = $state<string>('');
	let sortValue = $state<SortValue>('created_at_desc');

	// ── Helpers ──────────────────────────────────────────────────────────────
	// formatBytes / iconForAttachment / isImage live in $lib/attachments/display
	// (extracted in TASK-2383 so the item attachment strip shares them;
	// categoryIcon became iconForAttachment in TASK-2417, which also moved the
	// glyph to the shared SVG set in $lib/attachments/icons).

	function formatDate(iso: string): string {
		try {
			return new Date(iso).toLocaleDateString(undefined, {
				year: 'numeric',
				month: 'short',
				day: 'numeric'
			});
		} catch {
			return iso;
		}
	}

	// ── Derived values ───────────────────────────────────────────────────────

	let usagePercent = $derived.by(() => {
		if (!usage || usage.limit_bytes < 0) return 0;
		if (usage.limit_bytes === 0) return 0;
		return (usage.used_bytes / usage.limit_bytes) * 100;
	});

	let usageBarClass = $derived.by(() => {
		if (!usage || usage.limit_bytes < 0) return '';
		if (usagePercent >= 100) return 'crit';
		if (usagePercent >= 80) return 'warn';
		return '';
	});

	let pageStart = $derived(total === 0 ? 0 : offset + 1);
	let pageEnd = $derived(Math.min(offset + attachments.length, total));
	let canPrev = $derived(offset > 0);
	let canNext = $derived(offset + limit < total);

	let username = $derived(page.params.username ?? '');

	// ── Data loading ─────────────────────────────────────────────────────────

	function selections(): StorageFilterSelections {
		return {
			limit,
			offset,
			sort: sortValue,
			category: filterCategory,
			item: filterItem,
			itemId: filterItemId,
			collection: filterCollection
		};
	}

	function buildFilters(): AttachmentListFilters {
		return buildStorageFilters(selections());
	}

	// Drives the empty-state wording: "nothing matches" is honest when a filter
	// is narrowing the list (including a foreign or stale `attachment_item`
	// uuid, which is indistinguishable from an item with no attachments);
	// "nothing uploaded yet" is only true unfiltered (Codex round 2).
	// One projection, not two: re-listing the fields here let the two drift
	// (final-review P3). `selections()` reads the same $state, so the derived
	// still tracks every filter.
	let anyFilterActive = $derived(hasActiveStorageFilters(selections()));

	/**
	 * Label for the item-scope chip. The rows themselves carry the item title,
	 * so the chip names the item once the first page lands and falls back to a
	 * generic phrasing before that (or when the item has no live attachments
	 * left, which is exactly when the list is empty anyway).
	 */
	let scopedItemTitle = $derived(
		attachments.find((a) => a.item_id === filterItemId)?.item_title ?? ''
	);

	// ── View identity + fences (PLAN-2392, shared with the item attachment
	// strip) ─────────────────────────────────────────────────────────────────
	//
	// This tab lives at `/{user}/{ws}/settings` — ONE SvelteKit route — so a
	// workspace switch changes `wsSlug` under a MOUNTED component and every
	// request, continuation and rendered control has to say which workspace it
	// belongs to. `$lib/attachments/viewFence` owns that invariant; see its
	// header. The identity is stated ONCE here, so no individual fence can
	// restate a shorter one, and each request reads its workspace back off its
	// own token rather than off the live prop.
	const view = viewIdentity(() => ({ ws: wsSlug }));
	/**
	 * Fence 2 — the view generation. Mutations fence on this rather than on the
	 * paint alone: a paint token has no generation, so an A→B→A round trip (or
	 * an unmount) leaves an identity that still matches and a stale continuation
	 * would sail through (Codex round 3).
	 */
	const viewFence = createFence(view);
	/** Fence 3 — the workspace whose view is currently painted (and loaded). */
	const paint = createPaintFence(view);

	// Destroy invalidates EVERY fence and un-paints. The strip gets its request
	// fence invalidated for free (its load lives in an $effect, whose teardown
	// runs on destroy); this tab's loaders are plain async functions, so nothing
	// else retires their tokens — and a list or usage request that fails after
	// unmount would otherwise raise an error toast for a tab that is gone
	// (Codex round 5). "Nothing is painted any more" is exactly what the paint
	// fence is for, so no separate destroyed flag is needed.
	onDestroy(() => {
		viewFence.invalidate();
		listFence.invalidate();
		usageFence.invalidate();
		workspaceViewFence.invalidate();
		paint.record(null);
	});

	/**
	 * The tab's only load trigger. It owns BOTH transitions, in one effect and
	 * one dependency set, because they can arrive in the same flush and the
	 * order matters:
	 *
	 *  - **Workspace change.** `/{user}/{ws}/settings` is one SvelteKit route,
	 *    so switching workspaces changes `wsSlug` under a MOUNTED tab. Without
	 *    a reactive reload the previous workspace's rows and usage figure stay
	 *    on screen, and a list request that was in flight across the switch is
	 *    dropped by `loadList`'s workspace fence — which, with no successor
	 *    request, would strand `listLoading` at true forever (final review P1).
	 *    Since this fires on mount too, it REPLACES the old `onMount` load
	 *    rather than sitting alongside it — an effect plus `onMount` would
	 *    fetch the first page twice.
	 *  - **Deep-link change.** A new `?attachment_item=` on the same route
	 *    (a second "View all" followed from an already-open settings page)
	 *    retargets the scope instead of silently keeping the first item's
	 *    (Codex round 3).
	 *
	 * A workspace change subsumes the deep-link branch: it re-seeds the scope
	 * from the incoming link itself, so a single load covers both and the
	 * deep-link branch can't fire a second one behind it.
	 */
	$effect(() => {
		// Captured outside `untrack` so `wsSlug` (via the identity) and
		// `initialItemId` are this effect's dependencies.
		const here = view.capture();
		const incoming = initialItemId;
		if (!here.key) return;
		untrack(() => {
			if (!paint.isCurrent()) {
				paint.record(here);
				// The view itself changed, so in-flight mutations captured
				// against the old one must stop reconciling. Bumped here, not
				// derived from the paint: an A→B→A round trip lands back on an
				// identity that MATCHES, and only a generation can tell that
				// apart from never having left (Codex round 3).
				viewFence.invalidate();
				// Everything workspace-scoped is meaningless in the new
				// workspace and must not survive the switch: the rows and
				// usage figure obviously, but also the two workspace-scoped
				// filter values — an item uuid and a collection uuid from the
				// old workspace would silently filter the new list down to
				// nothing. Category / attached / sort / page size are plain
				// preferences with no workspace identity, so they carry over.
				seededItemId = incoming;
				filterItemId = incoming;
				filterCollection = '';
				offset = 0;
				attachments = [];
				total = 0;
				usage = null;
				void loadWorkspaceView();
				return;
			}
			if (incoming === seededItemId) return;
			seededItemId = incoming;
			filterItemId = incoming;
			retargetScope();
		});
	});

	/**
	 * Refetch after the item scope changed. Clears the rendered rows first:
	 * the list fence stops a stale RESPONSE from landing, but on its own
	 * it would leave the previous item's rows on screen while the new request
	 * runs — indefinitely if that request fails (Codex round 5).
	 */
	function retargetScope() {
		attachments = [];
		total = 0;
		onFiltersChanged();
	}

	function clearItemScope() {
		filterItemId = '';
		// Pre-claim the incoming '' so the re-seed effect treats the resulting
		// URL change as already applied and doesn't fire a second fetch.
		seededItemId = '';
		// Drop the deep-link too, or a tab switch / reload / restored route
		// re-applies a scope the user just dismissed (Codex rounds 2-4).
		onClearScope?.();
		retargetScope();
	}

	/**
	 * Fence 1, usage channel. Both halves matter, for the same reasons as the
	 * list's. The workspace check: this tab stays mounted across a workspace
	 * change, so a figure fetched for the workspace the user just left must not
	 * paint over the one they switched to (nor raise its error toast). The
	 * generation check: on an A→B→A round trip the workspace matches again, so
	 * only the generation can tell the first A request from the current one
	 * (Codex round 1).
	 */
	const usageFence = createFence(view);

	async function loadUsage() {
		const req = usageFence.restart();
		try {
			const next = await api.attachments.storageUsage(req.value.ws);
			if (req.stale()) return;
			usage = next;
		} catch (err) {
			if (req.stale()) return;
			const msg = err instanceof Error ? err.message : 'Failed to load storage usage';
			toastStore.show(msg, 'error');
		}
	}

	// Every list load shares one fence, so a slower earlier request can never
	// overwrite a newer one's rows/total/offset — the deep-link re-seed can
	// start a load while onMount's or a filter change's is still in flight
	// (Codex round 4).
	const listFence = createFence(view);
	// A list request is in flight. Only meaningful once the tab's initial
	// `loading` gate is down — it keeps a scope retarget (which clears the
	// rows) from flashing "no attachments match" before the new page lands.
	let listLoading = $state(false);
	// The last list request failed. Without it a failure renders as "no
	// attachments match", which is the same misleading empty-vs-broken
	// conflation DR-10 removed from the item strip (Codex round 6).
	let listError = $state(false);

	/**
	 * @param opts.retry - This load is an explicit Retry after a failure, so it
	 * carries the same cache-invalidation duty the item strip's Retry does
	 * (PLAN-2392 DR-10, final-review P2). `fetchAttachmentMetadata` latches
	 * `null` on a failed HEAD for the page lifetime, so the outage that broke
	 * this list also poisoned every chip and inline image that probed during
	 * it. Dropping a healthy entry is harmless — the cache holds immutable data
	 * and nothing refetches eagerly.
	 */
	async function loadList(opts: { retry?: boolean } = {}) {
		// `wsSlug` is reactive and this tab stays mounted across a workspace
		// change, so the request's workspace is captured and re-checked: the
		// generation alone can't tell a superseded workspace from the current
		// one, and both the rows and the metadata-cache keys are workspace
		// scoped (Codex fresh-angle round 2).
		const req = listFence.restart();
		const reqWsSlug = req.value.ws;
		listLoading = true;
		listError = false;
		if (opts.retry) {
			for (const a of attachments) invalidateAttachmentMetadata(reqWsSlug, a.id);
		}
		try {
			const resp: AttachmentListResponse = await api.attachments.list(reqWsSlug, buildFilters());
			if (req.stale()) return;
			attachments = resp.attachments ?? [];
			if (opts.retry) {
				// The rows the refetch returned get the same treatment — at
				// failure time the list held the stale page, or nothing at all.
				for (const a of attachments) invalidateAttachmentMetadata(reqWsSlug, a.id);
			}
			total = resp.total ?? 0;
			limit = resp.limit ?? limit;
			offset = resp.offset ?? offset;
		} catch (err) {
			if (req.stale()) return;
			listError = true;
			const msg = err instanceof Error ? err.message : 'Failed to load attachments';
			toastStore.show(msg, 'error');
		} finally {
			if (!req.stale()) listLoading = false;
		}
	}

	async function reload() {
		await Promise.all([loadList(), loadUsage()]);
	}

	/**
	 * Load (or reload) everything the tab shows for the current workspace, behind
	 * the whole-tab `loading` gate. Used on mount and on every workspace change.
	 */
	const workspaceViewFence = createFence(view);

	async function loadWorkspaceView() {
		const req = workspaceViewFence.restart();
		loading = true;
		try {
			await Promise.all([loadList(), loadUsage()]);
		} finally {
			// Only the NEWEST load lowers the gate; a superseded one doing it
			// would reveal the next workspace's half-loaded view. The workspace
			// check alone isn't enough — on an A→B→A round trip the first A load
			// finishes with `wsSlug` back at A while A's current load is still in
			// flight (Codex round 1). It can't strand: whatever supersedes a load
			// is itself a load, and the newest one's `finally` always runs.
			if (!req.stale()) loading = false;
		}
	}

	// Filter / sort changes reset to the first page and refetch. Using an
	// explicit handler instead of an $effect keeps the side-effect tied to
	// the user action and avoids an effect that mutates state + calls a
	// fetch on every dependency tick.
	function onFiltersChanged() {
		offset = 0;
		loadList();
	}

	// ── Actions ──────────────────────────────────────────────────────────────

	async function handleDelete(att: AttachmentListItem) {
		// ENTRY fence (fence 3). The clicked row was painted for the workspace
		// this tab has LOADED, which during the prop-update → effect-flush window
		// is not necessarily the one `wsSlug` already names. Taking the workspace
		// off the paint means the DELETE targets the row the user actually
		// clicked; refusing when the paint is stale is the only lever that works
		// at all, because every check below runs after the await and no fence can
		// unsend a request.
		const painted = paint.painted();
		if (!painted || !paint.isCurrent()) return;
		const reqWsSlug = painted.value.ws;
		// Fence 2 for everything after the await. Deliberately NOT the paint
		// token: that carries an identity but no generation, so an A→B→A round
		// trip (or an unmount) would leave it matching again and let a stale
		// continuation toast and refetch (Codex round 3).
		const req = viewFence.begin();

		const ok = confirm(
			`Delete ${att.filename}? The blob is reclaimed by garbage collection after a grace period.`
		);
		if (!ok) return;
		try {
			await api.attachments.delete(reqWsSlug, att.id);
			// Same broadcast the item attachment strip does (PLAN-2382 /
			// TASK-2384): an editor open in another tab-pane still holds live
			// <img>/chip NodeViews for this attachment, and an already-loaded
			// image never re-requests, so without this they keep presenting a
			// row the server no longer has (Codex round 14).
			//
			// Deliberately AHEAD of the fence, like the strip's: this is a GLOBAL
			// side effect keyed by (workspace, id), not a write into this view's
			// local state, and a workspace switch says nothing about whether the
			// row is gone. Skipping it would leave every other mounted surface
			// stale on proof we already have.
			announceAttachmentDeleted(reqWsSlug, att.id);
			// The toast DOES belong to a view: after an A→B switch mid-request it
			// would announce A's deletion over B (final review round 4).
			if (req.stale()) {
				// The refetch does NOT. This tab holds no tombstones and does not
				// subscribe to the deletion bus, so a list request that raced the
				// DELETE can have repainted the row — and on an A→B→A round trip
				// the fence above suppresses the only thing that heals it. A
				// refetch is a request for the truth about what is ON SCREEN, not
				// a write into a view the user left, so it is gated on the paint
				// rather than on the generation (Codex round 4).
				if (paint.isCurrent() && !painted.changed()) await reload();
				return;
			}
			toastStore.show(`Deleted ${att.filename}`, 'success');
			await reload();
		} catch (err) {
			// A 404 is authoritative that the row is gone — the list was simply
			// stale (another tab, another user). Treat it exactly like a
			// success: broadcast, invalidate, and refresh, rather than showing
			// an error for something that is in fact already done
			// (Codex round 20; matches the attachment strip's handling).
			if (err instanceof PadApiError && err.code === 'not_found') {
				announceAttachmentDeleted(reqWsSlug, att.id);
				if (req.stale()) {
					// Same reasoning as the success arm: a 404 is just as
					// authoritative that the row is gone, so the on-screen list
					// still deserves the correction.
					if (paint.isCurrent() && !painted.changed()) await reload();
					return;
				}
				toastStore.show(`${att.filename} was already deleted`, 'info');
				await reload();
				return;
			}
			if (req.stale()) return;
			const msg = err instanceof Error ? err.message : 'Failed to delete attachment';
			toastStore.show(msg, 'error');
		}
	}

	function gotoPrev() {
		if (!canPrev) return;
		offset = Math.max(0, offset - limit);
		loadList();
	}

	function gotoNext() {
		if (!canNext) return;
		offset = offset + limit;
		loadList();
	}

	function itemHref(att: AttachmentListItem): string {
		// Item route is /{username}/{wsSlug}/{collection_slug}/{item_slug}.
		// item_ref (collection-prefix + number, e.g. "TASK-5") doesn't
		// match the route shape — only item_slug does — so we ignore
		// item_ref here even if a future API surfaces one.
		const collSlug = att.collection_slug ?? '';
		const itemSlug = att.item_slug ?? '';
		if (!collSlug || !itemSlug) return '#';
		if (username) {
			return `/${username}/${wsSlug}/${collSlug}/${itemSlug}`;
		}
		// Settings page sits at /{username}/{wsSlug}/settings, so two
		// levels up is the item route's parent.
		return `../../${collSlug}/${itemSlug}`;
	}
</script>

<div class="storage-tab">
	{#if loading}
		<p class="empty-text">Loading storage…</p>
	{:else}
		<!-- Usage bar -->
		<div class="card usage-bar-card">
			{#if usage}
				<div class="usage-line">
					{#if usage.limit_bytes >= 0}
						<span>
							<strong>{formatBytes(usage.used_bytes)}</strong>
							used of
							<strong>{formatBytes(usage.limit_bytes)}</strong>
							({usagePercent.toFixed(1)}%)
						</span>
					{:else}
						<span>
							<strong>{formatBytes(usage.used_bytes)}</strong>
							used
							<span class="usage-unlimited">(unlimited)</span>
						</span>
					{/if}
					{#if usage.override_active}
						<span class="override-badge">custom override</span>
					{/if}
				</div>

				{#if usage.limit_bytes >= 0}
					<div class="usage-bar">
						<div
							class="usage-fill {usageBarClass}"
							style:width="{Math.min(100, usagePercent)}%"
						></div>
					</div>
				{/if}

				{#if usage.override_active && usage.plan}
					<div class="usage-subline">
						Plan: {usage.plan} — admin override active
					</div>
				{/if}
			{:else}
				<p class="empty-text">Unable to load usage info.</p>
			{/if}
		</div>

		<!-- Filters -->
		<div class="filter-row">
			<label>
				Category
				<select
					class="role-select"
					bind:value={filterCategory}
					onchange={onFiltersChanged}
				>
					<option value="">All</option>
					<option value="image">Images</option>
					<option value="video">Videos</option>
					<option value="audio">Audio</option>
					<option value="document">Documents</option>
					<option value="text">Text</option>
					<option value="archive">Archive</option>
					<option value="other">Other</option>
				</select>
			</label>
			<label>
				Item
				<select
					class="role-select"
					bind:value={filterItem}
					onchange={onFiltersChanged}
					disabled={!!filterItemId}
					title={filterItemId
						? 'Scoped to one item — clear the scope to filter by attached/unattached'
						: undefined}
				>
					<option value="">All</option>
					<option value="attached">Attached</option>
					<option value="unattached">Unattached</option>
				</select>
			</label>
			<label>
				Collection
				<select
					class="role-select"
					bind:value={filterCollection}
					onchange={onFiltersChanged}
				>
					<option value="">All</option>
					{#each collections as coll (coll.id)}
						<option value={coll.id}>{coll.name}</option>
					{/each}
				</select>
			</label>
			<label>
				Sort
				<select
					class="role-select"
					bind:value={sortValue}
					onchange={onFiltersChanged}
				>
					<option value="created_at_desc">Newest first</option>
					<option value="created_at">Oldest first</option>
					<option value="size_desc">Largest first</option>
					<option value="size">Smallest first</option>
					<option value="filename">Filename A→Z</option>
					<option value="filename_desc">Filename Z→A</option>
				</select>
			</label>
			<label>
				Per page
				<select
					class="role-select"
					bind:value={limit}
					onchange={onFiltersChanged}
				>
					<option value={25}>25</option>
					<option value={50}>50</option>
					<option value={100}>100</option>
				</select>
			</label>
		</div>

		<!-- Item scope (PLAN-2392 DR-18): set by the item strip's "View all"
		     link. Always visible while active, so the list is never silently
		     filtered. -->
		{#if filterItemId}
			<div class="item-scope">
				<span>
					Showing attachments for
					<strong>{scopedItemTitle || 'one item'}</strong>
				</span>
				<button type="button" class="scope-clear" onclick={clearItemScope}>
					Show all attachments
				</button>
			</div>
		{/if}

		<!-- Attachment list -->
		{#if listLoading && attachments.length === 0}
			<p class="empty-text">Loading attachments…</p>
		{:else if listError}
			<p class="empty-text">
				Couldn't load attachments.
				<button type="button" class="scope-clear" onclick={() => loadList({ retry: true })}>
					Retry
				</button>
			</p>
		{:else if total === 0}
			<p class="empty-text">
				{#if anyFilterActive}
					No attachments match the current filters.
				{:else}
					No attachments yet — paste or drag a file into any item to upload one.
				{/if}
			</p>
		{:else}
			<div class="att-list">
				{#each attachments as att (att.id)}
					<div class="att-row card">
						<a
							class="att-thumb"
							href={api.attachments.downloadUrl(wsSlug, att.id)}
							target="_blank"
							rel="noopener"
							aria-label="Open {att.filename}"
						>
							{#if isImage(att.mime_type)}
								<img
									src={api.attachments.downloadUrl(wsSlug, att.id, 'thumb-sm')}
									alt={att.filename}
									loading="lazy"
								/>
							{:else}
								<span class="att-file-icon">
									<AttachmentIcon id={iconForAttachment(att.mime_type, att.filename)} />
								</span>
							{/if}
						</a>

						<div class="att-meta">
							<div class="att-line1">
								<span class="att-filename" title={att.filename}>{att.filename}</span>
							</div>
							<div class="att-line2">
								<span class="att-size">{formatBytes(att.size_bytes)}</span>
								·
								<span class="att-mime mono">{att.mime_type}</span>
								·
								<span class="att-date">{formatDate(att.created_at)}</span>
							</div>
							<div class="att-line3">
								{#if att.item_title && att.collection_slug}
									in
									{#if att.item_deleted}
										<!-- Parent item is soft-deleted: render the title without
										     a link so the user knows the link target would 404. -->
										<span class="deleted-item">[[{att.item_title}]]</span>
										<span class="deleted-tag">deleted</span>
									{:else}
										<a href={itemHref(att)}>[[{att.item_title}]]</a>
									{/if}
								{:else}
									<span class="unattached-tag">Unattached</span>
								{/if}
							</div>
						</div>

						<div class="att-actions">
							<button
								type="button"
								class="btn btn-small btn-remove"
								onclick={() => handleDelete(att)}
							>
								Delete
							</button>
						</div>
					</div>
				{/each}
			</div>

			<!-- Pager -->
			<div class="pager">
				<span>Showing {pageStart}–{pageEnd} of {total}</span>
				<div class="pager-btns">
					<button
						type="button"
						class="btn btn-small"
						disabled={!canPrev}
						onclick={gotoPrev}
					>
						Prev
					</button>
					<button
						type="button"
						class="btn btn-small"
						disabled={!canNext}
						onclick={gotoNext}
					>
						Next
					</button>
				</div>
			</div>
		{/if}
	{/if}
</div>

<style>
	/* ── Local copies of the parent settings page primitives ────────────────
	 * Svelte 5 scopes styles to the component, so a child cannot reach the
	 * parent's `.card`/`.btn`/etc. These match the parent's visual conventions
	 * so the Storage tab feels native inside the settings page.
	 */
	.card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
	}

	.btn {
		padding: var(--space-2) var(--space-4);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.85em;
		cursor: pointer;
		color: var(--text-primary);
	}

	.btn:hover {
		background: var(--bg-hover);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-small {
		padding: var(--space-1) var(--space-3);
		font-size: 0.8em;
	}

	.btn-remove {
		color: var(--accent-red);
		border-color: transparent;
		background: none;
	}

	.btn-remove:hover {
		background: color-mix(in srgb, var(--accent-red) 15%, transparent);
	}

	.role-select {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		padding: var(--space-1) var(--space-2);
		font-size: 0.8em;
		color: var(--text-primary);
	}

	.empty-text {
		color: var(--text-muted);
		font-size: 0.9em;
		text-align: center;
		padding: var(--space-6);
	}

	.mono {
		font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
	}

	/* ── Storage-specific styles ──────────────────────────────────────────── */

	.storage-tab {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.usage-bar-card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.usage-line {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		font-size: 1em;
		flex-wrap: wrap;
	}

	.usage-unlimited {
		color: var(--text-muted);
		font-weight: 400;
	}

	.override-badge {
		display: inline-block;
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
		font-size: 0.72em;
		font-weight: 500;
		letter-spacing: 0.02em;
	}

	.usage-bar {
		height: 8px;
		width: 100%;
		background: var(--bg-tertiary);
		border-radius: 4px;
		overflow: hidden;
	}

	.usage-fill {
		height: 100%;
		background: var(--accent-blue);
		transition: width 0.3s ease;
	}

	.usage-fill.warn {
		background: color-mix(in srgb, #f59e0b 80%, transparent);
	}

	.usage-fill.crit {
		background: color-mix(in srgb, var(--accent-red) 85%, transparent);
	}

	.usage-subline {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	.filter-row {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
		margin: var(--space-3) 0;
	}

	.filter-row label {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		color: var(--text-secondary);
	}

	.item-scope {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
		font-size: 0.85em;
		color: var(--text-secondary);
	}

	.scope-clear {
		padding: 2px var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm, 4px);
		background: transparent;
		color: var(--text-secondary);
		font: inherit;
		cursor: pointer;
	}
	.scope-clear:hover {
		color: var(--text-primary);
		border-color: var(--accent, var(--border));
	}

	.att-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.att-row {
		display: flex;
		flex-direction: row;
		gap: var(--space-3);
		align-items: center;
		padding: var(--space-2) var(--space-3);
	}

	.att-thumb {
		flex-shrink: 0;
		width: 48px;
		height: 48px;
		border-radius: var(--radius-sm);
		background: var(--bg-tertiary);
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 1.4em;
		overflow: hidden;
		text-decoration: none;
	}

	/* File-type icon (TASK-2417). Monochrome and currentColor-driven, so it
	   themes with light/dark and stays visible under forced-colors. */
	.att-file-icon {
		display: flex;
		color: var(--text-secondary);
		font-size: 24px;
		line-height: 1;
	}

	.att-thumb img {
		width: 100%;
		height: 100%;
		object-fit: cover;
	}

	.att-meta {
		flex: 1;
		min-width: 0;
	}

	.att-filename {
		display: block;
		font-weight: 500;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.att-line2,
	.att-line3 {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	.att-line2 {
		margin-top: 2px;
	}

	.att-line3 {
		margin-top: 2px;
	}

	.att-line3 a {
		color: var(--accent-blue);
		text-decoration: none;
	}

	.att-line3 a:hover {
		text-decoration: underline;
	}

	.unattached-tag {
		font-style: italic;
	}

	.deleted-item {
		opacity: 0.7;
		text-decoration: line-through;
	}

	.deleted-tag {
		margin-left: var(--space-1);
		font-size: 0.7em;
		text-transform: uppercase;
		color: var(--accent-red);
		background: color-mix(in srgb, var(--accent-red) 12%, transparent);
		padding: 0 var(--space-1);
		border-radius: var(--radius-sm);
	}

	.att-actions {
		flex-shrink: 0;
	}

	.pager {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-top: var(--space-4);
		font-size: 0.85em;
		color: var(--text-muted);
	}

	.pager-btns {
		display: flex;
		gap: var(--space-2);
	}
</style>
