<script lang="ts">
	import { onDestroy, tick } from 'svelte';
	import { api } from '$lib/api/client';
	import { sseService } from '$lib/services/sse.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import type { TimelineEntry, TimelineResponse, Item } from '$lib/types';
	import TimelineCommentCard from './TimelineCommentCard.svelte';
	import TimelineActivityCard from './TimelineActivityCard.svelte';
	import TimelineVersionCard from './TimelineVersionCard.svelte';
	import { attachmentRefsIn } from '$lib/utils/commentAttachments';
	import { fetchAttachmentMetadata } from '$lib/components/editor/attachment-metadata';
	import { attachmentDownloadUrl, type AttachmentMeta } from '$lib/markdown/attachments';
	import Lightbox, { type LightboxImage } from '$lib/components/common/Lightbox.svelte';
	import CommentEditor from '$lib/components/CommentEditor.svelte';

	interface Props {
		wsSlug: string;
		/**
		 * Workspace owner's username — needed so comment wiki-links render
		 * with the full `/{username}/{workspace}/...` href. Without it the
		 * links resolve visually but navigate to a dead 3-segment route
		 * (BUG-1744 verification finding).
		 */
		username?: string;
		itemSlug: string;
		currentContent: string;
		items?: Item[];
		onRestore?: (item: Item) => void;
		/**
		 * itemId + collectionId let the timeline answer
		 * `workspaceStore.canEditItem(...)` for write-affordance gating.
		 * Optional — when missing, the composer / reply / reaction / delete
		 * controls fall back to "no edit" (PLAN-1100 / TASK-1107). The slug
		 * page already has the full item, so always passes both.
		 */
		itemId?: string;
		collectionId?: string;
	}

	let { wsSlug, username = '', itemSlug, currentContent, items = [], onRestore, itemId, collectionId }: Props = $props();

	// Resolve canEditItem reactively; falls to false if itemId/collectionId
	// aren't supplied (e.g. an older caller).
	let canEdit = $derived(
		itemId && collectionId
			? workspaceStore.canEditItem({ id: itemId, collection_id: collectionId })
			: false
	);

	let entries: TimelineEntry[] = $state([]);
	let hasMore: boolean = $state(false);
	let loading: boolean = $state(false);
	let loadingMore: boolean = $state(false);
	let error: string = $state('');

	// Resolver for `pad-attachment:UUID` references in comment bodies.
	// Metadata (MIME + size) is fetched lazily per UUID via a HEAD probe and
	// cached here; renderMarkdown reads it through the derived resolver so a
	// newly-resolved attachment re-renders its comment inline.
	let attMeta = $state<Map<string, AttachmentMeta>>(new Map());
	let attachmentResolver = $derived((uuid: string) => attMeta.get(uuid) ?? null);

	// In-flight / settled UUIDs — a non-reactive guard so the probe effect
	// fires one HEAD per attachment without re-triggering on attMeta writes.
	const probed = new Set<string>();

	function probeAttachment(uuid: string) {
		if (probed.has(uuid)) return;
		probed.add(uuid);
		fetchAttachmentMetadata(wsSlug, uuid, (id, variant) =>
			attachmentDownloadUrl(wsSlug, id, variant)
		).then((m) => {
			if (!m) return;
			const next = new Map(attMeta);
			// filename is left empty — the markdown alt text is the chip/img
			// label, and renderAttachmentImage only falls back to filename
			// when alt is blank.
			next.set(uuid, { id: uuid, mime_type: m.mime, filename: '', size_bytes: m.size });
			attMeta = next;
		});
	}

	// Probe every attachment referenced by a comment or reply as the
	// timeline loads / changes. Depends only on `entries`.
	$effect(() => {
		for (const entry of entries) {
			if (entry.kind !== 'comment' || !entry.comment) continue;
			for (const id of attachmentRefsIn(entry.comment.body)) probeAttachment(id);
			for (const reply of entry.comment.replies ?? []) {
				for (const id of attachmentRefsIn(reply.body)) probeAttachment(id);
			}
		}
	});

	// Lightbox state (IDEA-1660). Set when a thumbnail is activated; cleared
	// on close. Null = closed, so the host remounts fresh on each open.
	let lightbox: { images: LightboxImage[]; index: number } | null = $state(null);
	let entryListEl: HTMLElement | undefined = $state();

	// Open the lightbox for a clicked/activated thumbnail, gathering sibling
	// attachment images in the same comment/reply body so ←/→ can page them.
	function openLightboxFromImg(imgEl: HTMLElement) {
		const scope = imgEl.closest('.comment-body, .reply-body') ?? imgEl.parentElement;
		const els = scope
			? Array.from(scope.querySelectorAll<HTMLElement>('img[data-attachment-id]'))
			: [imgEl];
		const list: LightboxImage[] = els
			.map((el) => ({ id: el.getAttribute('data-attachment-id') ?? '', alt: el.getAttribute('alt') ?? '' }))
			.filter((x) => x.id !== '');
		if (list.length === 0) return;
		lightbox = { images: list, index: Math.max(0, els.indexOf(imgEl)) };
	}

	function onThumbClick(e: MouseEvent) {
		const imgEl = (e.target as HTMLElement | null)?.closest(
			'img[data-attachment-id]'
		) as HTMLElement | null;
		if (!imgEl) return;
		e.preventDefault();
		openLightboxFromImg(imgEl);
	}

	function onThumbKeydown(e: KeyboardEvent) {
		if (e.key !== 'Enter' && e.key !== ' ') return;
		const imgEl = (e.target as HTMLElement | null)?.closest(
			'img[data-attachment-id]'
		) as HTMLElement | null;
		if (!imgEl) return;
		e.preventDefault(); // Space would otherwise scroll the page
		openLightboxFromImg(imgEl);
	}

	// Delegated click + keydown on the entry list (rather than declarative
	// handlers on the static container, which the a11y lint would flag).
	$effect(() => {
		const el = entryListEl;
		if (!el) return;
		el.addEventListener('click', onThumbClick);
		el.addEventListener('keydown', onThumbKeydown);
		return () => {
			el.removeEventListener('click', onThumbClick);
			el.removeEventListener('keydown', onThumbKeydown);
		};
	});

	// Inline attachment images come from sanitized {@html}, so we can't wrap
	// them in a <button> at render time. Instead make each one a focusable,
	// announced control imperatively so keyboard users can open the lightbox.
	// Depends on BOTH `entries` (new comments) AND `attMeta` (an image only
	// renders as an <img> once its metadata resolves — before that it's a
	// "missing" placeholder span — so the pass must re-run on resolution).
	$effect(() => {
		void entries;
		void attMeta;
		const el = entryListEl;
		if (!el) return;
		tick().then(() => {
			for (const img of el.querySelectorAll<HTMLElement>('img[data-attachment-id]')) {
				if (img.getAttribute('role') === 'button') continue;
				img.setAttribute('role', 'button');
				img.setAttribute('tabindex', '0');
				const alt = img.getAttribute('alt');
				img.setAttribute('aria-label', alt ? `View image: ${alt}` : 'View attachment image');
			}
		});
	});

	// Current user ID for reaction toggle — read from the global auth store.
	let currentUserId = $derived(authStore.userId);
	let isAdmin = $derived(authStore.user?.role === 'admin');

	// Track IDs from the most recent first-page fetch, used by SSE merge
	// to detect deletions without incorrectly removing older-page entries.
	let firstPageIds = $state<Set<string>>(new Set());

	async function loadTimeline() {
		loading = true;
		error = '';
		try {
			const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug);
			entries = resp.entries;
			hasMore = resp.has_more;
			firstPageIds = new Set(resp.entries.map((e) => e.id));
		} catch (err: any) {
			error = err?.message ?? 'Failed to load timeline';
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		if (loadingMore || entries.length === 0) return;
		const oldest = entries[entries.length - 1];
		loadingMore = true;
		try {
			const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug, {
				before: oldest.created_at,
				before_id: oldest.id
			});
			// Deduplicate by ID to handle boundary overlap from <= queries.
			const existingIds = new Set(entries.map((e) => e.id));
			const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));
			entries = [...entries, ...newEntries];
			hasMore = resp.has_more;
		} catch (err: any) {
			error = err?.message ?? 'Failed to load more';
		} finally {
			loadingMore = false;
		}
	}

	$effect(() => {
		void wsSlug;
		void itemSlug;
		loadTimeline();
	});

	// Only refresh the timeline for comment/reaction events — NOT item_updated.
	// Content saves create version-diff entries that appear on next natural
	// refresh (new comment, page load). Refreshing on every content save caused
	// visible shakiness and rate-limit errors from rapid SSE replay.
	const relevantEvents = new Set([
		'comment_created',
		'comment_updated',
		'comment_deleted',
		'reaction_added',
		'reaction_removed'
	]);

	// Debounce SSE-driven refreshes so rapid-fire event replays (e.g. on
	// page reconnect) don't hammer the timeline endpoint.
	let sseRefreshTimer: ReturnType<typeof setTimeout> | undefined;

	const unsubscribe = sseService.onItemEvent((event) => {
		if (relevantEvents.has(event.type)) {
			clearTimeout(sseRefreshTimer);
			sseRefreshTimer = setTimeout(async () => {
				try {
					const resp: TimelineResponse = await api.timeline.list(wsSlug, itemSlug);
					const freshIds = new Set(resp.entries.map((e) => e.id));
					const existingIds = new Set(entries.map((e) => e.id));

					// Prepend genuinely new entries.
					const newEntries = resp.entries.filter((e) => !existingIds.has(e.id));

					// Update existing entries from the fresh response (e.g., reaction changes).
					// Remove entries that were previously on the first page but are now gone (deleted).
					// Keep all entries from older pages (loaded via "Load more") untouched.
					const freshById = new Map(resp.entries.map((e) => [e.id, e]));
					const updatedExisting = entries
						.filter((e) => {
							if (firstPageIds.has(e.id) && !freshIds.has(e.id)) return false;
							return true;
						})
						.map((e) => freshById.get(e.id) ?? e);

					entries = [...newEntries, ...updatedExisting];
					firstPageIds = freshIds;
				} catch {
					// Silently ignore SSE refresh failures.
				}
			}, 500);
		}
	});

	onDestroy(() => {
		unsubscribe();
	});

	let submitting: boolean = $state(false);

	// Posts a new comment. Throws on failure so CommentEditor preserves the
	// draft; clears itself on success.
	async function submitComment(body: string) {
		submitting = true;
		error = '';
		try {
			await api.comments.create(wsSlug, itemSlug, {
				body,
				created_by: 'user',
				source: 'web'
			});
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to post comment';
			throw err;
		} finally {
			submitting = false;
		}
	}

	async function handleReply(commentId: string, body: string) {
		try {
			await api.comments.reply(wsSlug, commentId, {
				body,
				created_by: 'user',
				source: 'web'
			});
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to post reply';
			throw err; // let CommentEditor keep the draft
		}
	}

	// Edits a comment or reply (author/admin enforced server-side). Throws on
	// failure so the inline CommentEditor preserves the draft.
	async function handleEdit(commentId: string, body: string) {
		try {
			await api.comments.update(wsSlug, commentId, { body });
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to edit comment';
			throw err;
		}
	}

	async function handleDelete(commentId: string) {
		if (!confirm('Delete this comment?')) return;
		try {
			await api.comments.delete(wsSlug, commentId);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to delete comment';
		}
	}

	async function handleReaction(commentId: string, emoji: string) {
		try {
			await api.comments.addReaction(wsSlug, commentId, emoji);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to add reaction';
		}
	}

	async function handleRemoveReaction(commentId: string, emoji: string) {
		try {
			await api.comments.removeReaction(wsSlug, commentId, emoji);
			await loadTimeline();
		} catch (err: any) {
			error = err?.message ?? 'Failed to remove reaction';
		}
	}

	function dotClass(kind: TimelineEntry['kind']): string {
		if (kind === 'comment') return 'dot-comment';
		if (kind === 'version') return 'dot-version';
		return 'dot-activity';
	}
</script>

<section class="timeline">
	<header class="timeline-header">
		<h3 class="timeline-title">Timeline</h3>
		{#if entries.length > 0}
			<span class="entry-count">{entries.length}{hasMore ? '+' : ''}</span>
		{/if}
	</header>

	<!-- Comment compose — gated on canEditItem (PLAN-1100 / TASK-1107).
	     Read-only viewers / guests with view-only grants see the timeline
	     thread but cannot post; the composer is hidden entirely. -->
	{#if canEdit}
		<div class="compose">
			<CommentEditor
				{wsSlug}
				{itemId}
				placeholder="Write a comment… (paste or drop an image to attach)"
				submitLabel="Comment"
				{submitting}
				onSubmit={submitComment}
			/>
		</div>
	{/if}

	{#if loading && entries.length === 0}
		<div class="loading">
			<span class="spinner"></span>
			<span class="loading-text">Loading timeline...</span>
		</div>
	{/if}

	{#if error}
		<div class="error">{error}</div>
	{/if}

	{#if !loading || entries.length > 0}
		<div class="entry-list" bind:this={entryListEl}>
			{#each entries as entry (entry.id)}
				<div class="entry">
					<div class="entry-rail">
						<span class="dot {dotClass(entry.kind)}"></span>
						<span class="line"></span>
					</div>
					<div class="entry-content">
						{#if entry.kind === 'comment' && entry.comment}
							<TimelineCommentCard
								comment={entry.comment}
								{wsSlug}
								{username}
								{items}
								{currentUserId}
								{canEdit}
								{isAdmin}
								{attachmentResolver}
								onDelete={handleDelete}
								onReply={handleReply}
								onEdit={handleEdit}
								onReaction={handleReaction}
								onRemoveReaction={handleRemoveReaction}
							/>
						{:else if entry.kind === 'activity' && entry.activity}
							<TimelineActivityCard activity={entry.activity} />
						{:else if entry.kind === 'version' && entry.version}
							<TimelineVersionCard
								version={entry.version}
								{wsSlug}
								{itemSlug}
								{currentContent}
								{onRestore}
							/>
						{/if}
					</div>
				</div>
			{/each}

			{#if entries.length === 0 && !loading}
				<div class="empty">No timeline entries yet.</div>
			{/if}
		</div>

		{#if hasMore}
			<button class="load-more-btn" type="button" disabled={loadingMore} onclick={loadMore}>
				{loadingMore ? 'Loading...' : 'Load more'}
			</button>
		{/if}
	{/if}
</section>

{#if lightbox}
	<Lightbox
		images={lightbox.images}
		index={lightbox.index}
		{wsSlug}
		onClose={() => (lightbox = null)}
	/>
{/if}

<style>
	.timeline {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.timeline-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.timeline-title {
		margin: 0;
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.entry-count {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 1.5em;
		padding: 0 var(--space-1);
		background: var(--bg-tertiary);
		border-radius: 9999px;
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		line-height: 1.6;
	}

	/* ── Compose ──────────────────────────────────────────────────────────── */

	.compose {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	/* ── Loading / Error ──────────────────────────────────────────────────── */

	.loading {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-4);
		justify-content: center;
		color: var(--text-muted);
	}

	.spinner {
		display: inline-block;
		width: 16px;
		height: 16px;
		border: 2px solid var(--border);
		border-top-color: var(--accent-blue);
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to {
			transform: rotate(360deg);
		}
	}

	.loading-text {
		font-size: 0.85em;
	}

	.error {
		padding: var(--space-2) var(--space-3);
		background: color-mix(in srgb, var(--accent-red, #ef4444) 10%, transparent);
		border: 1px solid color-mix(in srgb, var(--accent-red, #ef4444) 30%, transparent);
		border-radius: var(--radius);
		color: var(--accent-red, #ef4444);
		font-size: 0.85em;
	}

	/* ── Timeline entries ─────────────────────────────────────────────────── */

	.entry-list {
		display: flex;
		flex-direction: column;
	}

	.entry {
		display: flex;
		gap: var(--space-3);
	}

	.entry-rail {
		display: flex;
		flex-direction: column;
		align-items: center;
		flex-shrink: 0;
		width: 16px;
		padding-top: var(--space-2);
	}

	.dot {
		width: 10px;
		height: 10px;
		border-radius: 50%;
		flex-shrink: 0;
		z-index: 1;
	}

	.dot-comment {
		background: var(--accent-blue);
	}

	.dot-activity {
		background: var(--text-muted);
	}

	.dot-version {
		background: var(--accent-green);
	}

	.line {
		width: 1px;
		flex: 1;
		background: var(--border);
	}

	.entry:last-child .line {
		display: none;
	}

	.entry-content {
		flex: 1;
		min-width: 0;
		padding-bottom: var(--space-3);
	}

	.empty {
		text-align: center;
		padding: var(--space-6);
		color: var(--text-muted);
		font-size: 0.9em;
	}

	.load-more-btn {
		display: block;
		width: 100%;
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
		text-align: center;
	}

	.load-more-btn:hover:not(:disabled) {
		color: var(--text-primary);
		border-color: var(--accent-blue);
	}

	.load-more-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
