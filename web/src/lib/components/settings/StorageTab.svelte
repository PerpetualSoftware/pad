<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api } from '$lib/api/client';
	import type {
		AttachmentListItem,
		AttachmentListFilters,
		AttachmentListResponse,
		Collection,
		WorkspaceStorageInfo
	} from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';

	// ── Props ────────────────────────────────────────────────────────────────
	interface Props {
		wsSlug: string;
		collections: Collection[];
	}
	let { wsSlug, collections }: Props = $props();

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
	let filterCollection = $state<string>('');
	let sortValue = $state<SortValue>('created_at_desc');

	// ── Helpers ──────────────────────────────────────────────────────────────

	// Same algorithm as web/src/routes/console/billing/+page.svelte. Picks a
	// unit so the displayed value is < 1024; bump thresholds nudged down half
	// the previous unit so 1,048,575 bytes reads as "1.0 MB" rather than the
	// misleading "1024 KB" you'd get from a straight Math.round at the KB tier.
	function formatBytes(bytes: number): string {
		if (bytes < 0) return `${bytes} B`;
		const KB = 1024;
		const MB = KB * 1024;
		const GB = MB * 1024;
		const bumpGB = GB - MB / 2;
		const bumpMB = MB - KB / 2;
		if (bytes >= bumpGB) return formatUnit(bytes / GB, 'GB');
		if (bytes >= bumpMB) return formatUnit(bytes / MB, 'MB');
		if (bytes >= KB) return formatUnit(bytes / KB, 'KB');
		return `${bytes} B`;
	}

	function formatUnit(value: number, unit: string): string {
		if (value >= 10) return `${Math.round(value)} ${unit}`;
		return `${value.toFixed(1)} ${unit}`;
	}

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

	function categoryIcon(mime: string): string {
		if (mime.startsWith('image/')) return '🖼️';
		if (mime.startsWith('video/')) return '🎬';
		if (mime.startsWith('audio/')) return '🔊';
		if (mime.startsWith('text/')) return '📄';
		if (mime === 'application/pdf') return '📄';
		if (
			mime === 'application/zip' ||
			mime === 'application/x-tar' ||
			mime === 'application/gzip' ||
			mime === 'application/x-7z-compressed' ||
			mime === 'application/x-rar-compressed'
		)
			return '📦';
		if (
			mime.startsWith('application/vnd.openxmlformats') ||
			mime.startsWith('application/vnd.ms-') ||
			mime.startsWith('application/vnd.oasis') ||
			mime === 'application/msword'
		)
			return '📄';
		return '❓';
	}

	function isImage(mime: string): boolean {
		return mime.startsWith('image/');
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

	function buildFilters(): AttachmentListFilters {
		const f: AttachmentListFilters = {
			limit,
			offset,
			sort: sortValue
		};
		if (filterCategory) f.category = filterCategory;
		if (filterItem) f.item = filterItem;
		if (filterCollection) f.collection = filterCollection;
		return f;
	}

	async function loadUsage() {
		try {
			usage = await api.attachments.storageUsage(wsSlug);
		} catch (err) {
			const msg = err instanceof Error ? err.message : 'Failed to load storage usage';
			toastStore.show(msg, 'error');
		}
	}

	async function loadList() {
		try {
			const resp: AttachmentListResponse = await api.attachments.list(wsSlug, buildFilters());
			attachments = resp.attachments ?? [];
			total = resp.total ?? 0;
			limit = resp.limit ?? limit;
			offset = resp.offset ?? offset;
		} catch (err) {
			const msg = err instanceof Error ? err.message : 'Failed to load attachments';
			toastStore.show(msg, 'error');
		}
	}

	async function reload() {
		await Promise.all([loadList(), loadUsage()]);
	}

	onMount(async () => {
		try {
			await Promise.all([loadList(), loadUsage()]);
		} finally {
			loading = false;
		}
	});

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
		const ok = confirm(
			`Delete ${att.filename}? The blob is reclaimed by garbage collection after a grace period.`
		);
		if (!ok) return;
		try {
			await api.attachments.delete(wsSlug, att.id);
			toastStore.show(`Deleted ${att.filename}`, 'success');
			await reload();
		} catch (err) {
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

		<!-- Attachment list -->
		{#if total === 0}
			<p class="empty-text">
				No attachments yet — paste or drag a file into any item to upload one.
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
								<span aria-hidden="true">{categoryIcon(att.mime_type)}</span>
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
		color: #ef4444;
		border-color: transparent;
		background: none;
	}

	.btn-remove:hover {
		background: color-mix(in srgb, #ef4444 15%, transparent);
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
		background: color-mix(in srgb, #ef4444 85%, transparent);
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
		color: #ef4444;
		background: color-mix(in srgb, #ef4444 12%, transparent);
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
