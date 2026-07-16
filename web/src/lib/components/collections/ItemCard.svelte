<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import type { Item, Collection } from '$lib/types';
	import { parseFields, parseSchema, parseTags, formatItemRef, itemUrlId } from '$lib/types';
	import { starredStore } from '$lib/stores/starred.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import ItemActionsMenu from './ItemActionsMenu.svelte';
	import type { ReorderDirection } from '$lib/collections/reorder';
	import { shouldOpenInPane } from './itemCardClick';

	interface Props {
		item: Item;
		collection: Collection;
		compact?: boolean;
		focused?: boolean;
		showCollection?: boolean;
		statusOptions?: string[];
		onStatusClick?: (item: Item, newStatus: string) => void;
		progress?: { total: number; done: number; label?: string } | null;
		progressLabel?: string;
		/**
		 * Opt-in per-item reorder menu (IDEA-1898). The host (ListView /
		 * BoardView / TableView) passes a bound handler + the disabled
		 * directions for this item's position in its group. When omitted —
		 * the default — no menu renders, so read-only / aggregation views
		 * (share pages, starred, tags, dashboard) get nothing. ItemCard
		 * stays dumb: it forwards these, it has no ordering context.
		 */
		onReorderItem?: (item: Item, dir: ReorderDirection) => void;
		reorderDisabledDirs?: Set<ReorderDirection | 'left' | 'right'>;
		/**
		 * Board-only adjacent-column move (TASK-1908). Pass-through to the
		 * menu's `onMove`; only BoardView wires it, so left/right never
		 * appear on List/Table/Child cards. The vertical `onReorderItem`
		 * type is deliberately left untouched (DR-6).
		 */
		onMoveItem?: (item: Item, dir: 'left' | 'right') => void;
		/** Render the Move left / Move right menu entries (BoardView only). */
		horizontal?: boolean;
		/**
		 * Opt-in split-pane open (PLAN-2105 / TASK-2111). When set, a plain
		 * left-click on the card opens the item in the collection page's
		 * detail pane instead of navigating; modifier/middle clicks still
		 * fall through to the `href` so cmd/middle-click opens the full page
		 * in a new tab (the "popout" state) and right-click-copy / SSR still
		 * target the full page. Omitted everywhere except the collection page,
		 * so all other surfaces (starred / tags / roles) keep full-page
		 * anchor navigation.
		 */
		onItemOpen?: (item: Item) => void;
	}

	let { item, collection, compact = false, focused = false, showCollection = false, statusOptions, onStatusClick, progress = null, progressLabel = 'tasks', onReorderItem, reorderDisabledDirs, onMoveItem, horizontal = false, onItemOpen }: Props = $props();

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let fields = $derived(parseFields(item));
	let schema = $derived(parseSchema(collection));

	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let priorityField = $derived(schema.fields.find((f) => f.key === 'priority'));
	let itemUrl = $derived(`/${username}/${wsSlug}/${collection.slug}/${itemUrlId(item)}`);
	let itemRef = $derived(formatItemRef(item));
	let tags = $derived(parseTags(item));

	function tagUrl(tag: string): string {
		return `/${username}/${wsSlug}/tags/${encodeURIComponent(tag)}`;
	}

	// The card is an <a>; navigate to the tag page programmatically (like the
	// star/PR/status controls) so the chip doesn't trigger the card link.
	function openTag(e: MouseEvent, tag: string) {
		e.preventDefault();
		e.stopPropagation();
		goto(tagUrl(tag));
	}
	let starred = $derived(starredStore.isStarred(item.id));

	let statusCyclable = $derived(
		!!onStatusClick && !!statusOptions && statusOptions.length > 1 && !!fields.status
	);

	let pullRequest = $derived(item.code_context?.pull_request);

	let pulsing = $state(false);

	function prStateColor(state: string): string {
		switch (state?.toUpperCase()) {
			case 'OPEN': return 'var(--accent-green)';
			case 'MERGED': return 'var(--accent-purple, #8b5cf6)';
			case 'CLOSED': return 'var(--accent-red, #ef4444)';
			case 'DRAFT': return 'var(--text-muted)';
			default: return 'var(--text-muted)';
		}
	}

	function openPullRequest(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		if (pullRequest?.url) {
			window.open(pullRequest.url, '_blank', 'noopener,noreferrer');
		}
	}

	function cycleStatus(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		if (!statusOptions || !onStatusClick || !fields.status) return;
		const currentIndex = statusOptions.indexOf(fields.status);
		const nextIndex = (currentIndex + 1) % statusOptions.length;
		const nextStatus = statusOptions[nextIndex];

		pulsing = true;
		setTimeout(() => { pulsing = false; }, 300);

		onStatusClick(item, nextStatus);
	}

	function statusColor(status: string): string {
		switch (status) {
			case 'open': return 'var(--text-secondary)';
			case 'in_progress': return 'var(--accent-amber)';
			case 'done': return 'var(--accent-green)';
			case 'blocked': return 'var(--accent-orange)';
			default: return 'var(--text-muted)';
		}
	}

	function priorityColor(priority: string): string {
		switch (priority) {
			case 'critical': return 'var(--accent-orange)';
			case 'high': return 'var(--accent-amber)';
			case 'medium': return 'var(--text-secondary)';
			case 'low': return 'var(--text-muted)';
			default: return 'var(--text-muted)';
		}
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	function toggleStar(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		starredStore.toggle(wsSlug, item.slug, item.id);
	}

	// Copy the item's issue ID (e.g. IDEA-1904) without opening the card.
	// The card is an <a>, so we must swallow the click (IDEA-1904).
	let copied = $state(false);
	async function copyRef(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		if (!itemRef) return;
		const ok = await copyToClipboard(itemRef);
		if (ok) {
			copied = true;
			setTimeout(() => { copied = false; }, 1500);
		}
	}

	// Split-pane row-click interception (PLAN-2105 / TASK-2111). Only a plain
	// left-click opens the pane; modifier/middle clicks fall through to the
	// native <a href> (cmd/middle-click = full-page popout in a new tab,
	// right-click-copy / SSR target the full page). Sub-controls (star / PR /
	// status / tags / reorder) already stopPropagation, so their clicks never
	// reach this handler; `defaultPrevented` is a defensive backstop. The
	// bail-out predicate is factored into `shouldOpenInPane` (TASK-2116) so
	// it's unit-testable without mounting this component.
	function handleCardClick(e: MouseEvent) {
		if (!shouldOpenInPane(e, !!onItemOpen)) return;
		e.preventDefault();
		onItemOpen?.(item);
	}
</script>

<a href={itemUrl} class="item-card" class:compact class:focused class:has-pr={!!pullRequest} onclick={handleCardClick}>
	{#if pullRequest}
		<button
			type="button"
			class="pr-badge"
			style:background={prStateColor(pullRequest.state)}
			onclick={openPullRequest}
			title="#{pullRequest.number} {pullRequest.title} ({(pullRequest.state ?? '').toLowerCase()})"
		>
			#{pullRequest.number}
		</button>
	{/if}
	<div class="card-top-row">
		<button
			class="star-btn"
			class:starred
			onclick={toggleStar}
			title={starred ? 'Unstar' : 'Star'}
		>
			{starred ? '★' : '☆'}
		</button>
		{#if showCollection && item.collection_name}
			<span class="collection-badge">
				{#if item.collection_icon}{item.collection_icon} {/if}{item.collection_name}
			</span>
		{/if}
		{#if itemRef}
			<span class="item-ref-wrap">
				<span class="item-ref">{itemRef}</span>
				<button
					type="button"
					class="copy-ref-btn"
					class:copied
					onclick={copyRef}
					title="Copy item ID"
					aria-label={copied ? `Copied ${itemRef}` : `Copy item ID ${itemRef}`}
				>
					{#if copied}
						<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12"></polyline></svg>
					{:else}
						<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
					{/if}
				</button>
				<!-- Announce copy success to assistive tech; the icon/color swap
				     alone is invisible to screen readers (IDEA-1904 a11y review). -->
				<span class="sr-only" aria-live="polite">{copied ? `Copied ${itemRef}` : ''}</span>
			</span>
		{/if}
		{#if onReorderItem}
			<ItemActionsMenu
				{item}
				{horizontal}
				disabledDirs={reorderDisabledDirs}
				label={item.title}
				onReorder={(dir) => onReorderItem?.(item, dir)}
				onMove={onMoveItem ? (dir) => onMoveItem?.(item, dir) : undefined}
			/>
		{/if}
	</div>

	<div class="card-title">
		{item.title}
	</div>

	<div class="card-meta">
		{#if statusField && fields.status}
			{#if statusCyclable}
				<button
					class="meta-status meta-status-btn"
					class:pulsing
					style:color={statusColor(fields.status)}
					onclick={cycleStatus}
					title="Click to cycle status"
				>
					{formatLabel(fields.status).toUpperCase()}
				</button>
			{:else}
				<span class="meta-status" style:color={statusColor(fields.status)}>
					{formatLabel(fields.status).toUpperCase()}
				</span>
			{/if}
		{/if}
		{#if priorityField && fields.priority}
			{#if statusField && fields.status}<span class="meta-sep">&middot;</span>{/if}
			<span class="meta-priority" style:color={priorityColor(fields.priority)}>
				{formatLabel(fields.priority)}
			</span>
		{/if}
		{#if item.parent_title}
			<span class="meta-sep">&middot;</span>
			{@const parentLabel = item.parent_ref ? `${item.parent_ref}: ${item.parent_title}` : item.parent_title}
			<span class="meta-parent" title={parentLabel}>{parentLabel}</span>
		{/if}
		{#if item.agent_role_name}
			<span class="meta-sep">&middot;</span>
			<span class="meta-role">
				{#if item.agent_role_icon}{item.agent_role_icon} {/if}{item.agent_role_name}
			</span>
		{/if}
		{#if item.assigned_user_name}
			<span class="meta-assignee">{item.assigned_user_name}</span>
		{/if}
	</div>

	{#if tags.length > 0}
		<div class="card-tags">
			{#each tags as tag, i (i)}
				<button type="button" class="card-tag" onclick={(e) => openTag(e, tag)} title="View items tagged “{tag}”">
					{tag}
				</button>
			{/each}
		</div>
	{/if}

	{#if progress && progress.total > 0}
		<div class="card-progress">
			<div class="card-progress-bar">
				<div class="card-progress-fill" style:width="{Math.round((progress.done / progress.total) * 100)}%"></div>
			</div>
			<span class="card-progress-text">{progress.done}/{progress.total} {progress.label ?? progressLabel}</span>
		</div>
	{/if}
</a>

<style>
	.item-card {
		position: relative;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4) var(--space-5);
		text-decoration: none;
		color: inherit;
		transition: background 0.1s;
		/* When the card is a flex/grid item (board columns, list rows),
		   allow it to shrink below its intrinsic content width so a long
		   unbreakable title can wrap inside instead of pushing the card
		   past its container. Pairs with overflow-wrap on .card-title.
		   We deliberately do NOT set overflow: hidden here because the
		   .pr-badge protrudes (right: -6px) and would get clipped. */
		min-width: 0;
	}

	.pr-badge {
		position: absolute;
		top: 8px;
		right: -6px;
		z-index: 1;
		border: 1px solid var(--bg-primary);
		border-radius: 10px;
		padding: 1px 7px;
		font-family: var(--font-mono);
		font-size: 0.7em;
		font-weight: 600;
		line-height: 1.4;
		color: #fff;
		cursor: pointer;
		white-space: nowrap;
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.25);
		transition: transform 0.1s, filter 0.1s;
	}

	.pr-badge:hover {
		filter: brightness(1.1);
		transform: scale(1.05);
	}

	.pr-badge:active {
		transform: scale(0.95);
	}

	.item-card:hover,
	.item-card.focused {
		background: var(--bg-hover);
		text-decoration: none;
	}

	.item-card.focused {
		outline: 2px solid var(--accent-blue);
		outline-offset: -2px;
	}

	.item-card.compact {
		padding: var(--space-3) var(--space-4);
	}

	.card-top-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	/* Reorder kebab sits at the far right of the top row. When a PR badge
	   is present the `.has-pr` padding-right reservation keeps it clear of
	   the absolutely-positioned badge (kebab lands just left of it). */
	.card-top-row :global(.item-actions-menu) {
		margin-left: auto;
	}

	/* Reserve horizontal room on the right of the top row for the
	   absolutely-positioned PR badge. Without this, on cards with
	   showCollection=true (dashboard active items, role board) or long
	   item refs, the badge can overlap the collection chip / item ref
	   and intercept clicks. The reservation only applies when a PR
	   badge is actually present so non-PR cards keep their full width.

	   Width budget: pill is ~0.7em monospace, up to 6 chars (e.g.
	   "#12345") + 14px padding + 1px border × 2 ≈ 50px, plus the
	   badge protrudes 6px to the right (right: -6px) so the inner
	   gap from the card's right edge is ~50 − 6 = 44px. Round up to
	   52px for breathing room. */
	.item-card.has-pr .card-top-row {
		padding-right: 52px;
	}

	.collection-badge {
		background: var(--bg-tertiary);
		padding: 1px 7px;
		border-radius: 10px;
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.item-ref-wrap {
		display: inline-flex;
		align-items: center;
		gap: 2px;
	}

	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.75em;
		color: var(--text-muted);
		font-weight: 400;
		white-space: nowrap;
	}

	/* Copy-ID affordance sits just right of the item ref. Hidden until the
	   card is hovered/focused to keep dense boards clean; the check state
	   stays visible through its 1.5s window regardless of hover. */
	.copy-ref-btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 22px;
		height: 22px;
		padding: 0;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		opacity: 0;
		transition: opacity 0.15s, color 0.15s, background 0.15s;
		flex-shrink: 0;
	}

	.item-card:hover .copy-ref-btn,
	.copy-ref-btn:focus-visible,
	.copy-ref-btn.copied {
		opacity: 1;
	}

	/* Touch devices have no hover and don't reliably match :focus-visible on
	   tap, so a purely hover-revealed control would be invisible there. Keep
	   it at a low resting opacity like the sibling star button (IDEA-1904). */
	@media (hover: none) {
		.copy-ref-btn {
			opacity: 0.65;
		}
	}

	.copy-ref-btn:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}

	.copy-ref-btn.copied {
		color: var(--accent-green, #22c55e);
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}

	.card-title {
		font-size: 0.95em;
		color: var(--text-primary);
		line-height: 1.45;
		font-weight: 600;
		/* Long titles without spaces (URLs, identifiers like
		   `WorkspaceTemplateRegistryConfiguration`, code snippets pasted
		   into a title) used to push the card past its container. Allow
		   breaks at any character when there's no other option. */
		overflow-wrap: anywhere;
		min-width: 0;
	}

	.compact .card-title {
		font-size: 0.92em;
	}

	.card-meta {
		display: flex;
		align-items: center;
		gap: 5px;
		flex-wrap: wrap;
		/* Allow flex children with intrinsic content wider than the card
		   (e.g. a long parent title from an `implements` link to an idea)
		   to shrink instead of pushing the card past its column. */
		min-width: 0;
	}

	.meta-status {
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		white-space: nowrap;
	}

	.meta-status-btn {
		border: none;
		background: none;
		cursor: pointer;
		padding: 0;
		font-family: inherit;
		line-height: inherit;
		transition: filter 0.1s, transform 0.1s;
	}

	.meta-status-btn:hover {
		filter: brightness(1.3);
		transform: scale(1.05);
	}

	.meta-status-btn:active {
		transform: scale(0.95);
	}

	.meta-status-btn.pulsing {
		animation: status-pulse 0.3s ease-out;
	}

	@keyframes status-pulse {
		0% {
			text-shadow: 0 0 0 currentColor;
		}
		70% {
			text-shadow: 0 0 8px currentColor;
		}
		100% {
			text-shadow: 0 0 0 currentColor;
		}
	}

	.meta-sep {
		font-size: 0.7em;
		color: var(--text-muted);
	}

	.meta-priority {
		font-size: 0.7em;
		font-weight: 600;
		white-space: nowrap;
	}

	.meta-parent {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-purple, var(--text-secondary));
		/* Long parent titles (e.g. a task that implements an idea whose
		   title is a paragraph) used to push the card outside its
		   container on Board view (BUG-630). Cap the chip to the card
		   width and truncate with an ellipsis; the full label is still
		   accessible via the tooltip on hover. */
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		max-width: 100%;
		min-width: 0;
	}

	.meta-role {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-teal, var(--accent-blue));
		white-space: nowrap;
	}

	.meta-assignee {
		font-size: 0.7em;
		font-weight: 500;
		color: var(--accent-blue);
		margin-left: auto;
		white-space: nowrap;
	}

	.card-tags {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-1, 0.25rem);
	}

	.card-tag {
		display: inline-flex;
		align-items: center;
		padding: 0.05em 0.45em;
		font-size: 0.68em;
		line-height: 1.5;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: 999px;
		color: var(--text-secondary);
		cursor: pointer;
		max-width: 12rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.card-tag:hover {
		color: var(--text-primary);
		border-color: var(--text-tertiary, var(--text-secondary));
	}

	.card-progress {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.card-progress-bar {
		flex: 1;
		height: 4px;
		background: var(--bg-tertiary);
		border-radius: 2px;
		overflow: hidden;
	}
	.card-progress-fill {
		height: 100%;
		background: var(--accent-green);
		border-radius: 2px;
		transition: width 0.3s ease;
	}
	.card-progress-text {
		font-size: 0.7em;
		color: var(--text-muted);
		white-space: nowrap;
		flex-shrink: 0;
	}

	.star-btn {
		border: none;
		background: none;
		cursor: pointer;
		padding: 0;
		font-size: 0.95em;
		line-height: 1;
		color: var(--text-muted);
		opacity: 0.65;
		transition: opacity 0.15s, color 0.15s;
		flex-shrink: 0;
	}

	.item-card:hover .star-btn,
	.star-btn.starred {
		opacity: 1;
	}

	.star-btn:hover {
		color: var(--accent-amber);
	}

	.star-btn.starred {
		color: var(--accent-amber);
	}
</style>
