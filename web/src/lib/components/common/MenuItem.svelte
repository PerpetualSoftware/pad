<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		/** Leading icon/emoji. Interpolated as TEXT — markup would render
		 *  literally; pass `iconSnippet` for an SVG icon instead. */
		icon?: string;
		/** Leading icon as markup (an SVG icon component). Wins over `icon`
		 *  when both are set; the slot is decorative either way (PLAN-2392
		 *  DR-3b). */
		iconSnippet?: Snippet;
		/** Right-aligned hint (shortcut, count). */
		hint?: string;
		/** Red row for destructive actions. */
		danger?: boolean;
		/** When defined the row is a menuitemradio with a trailing check. */
		checked?: boolean;
		disabled?: boolean;
		/** id of an element describing this row — e.g. the prompt line above a
		 *  destructive confirmation, which is presentational and otherwise
		 *  never announced when the row takes focus (PLAN-2326). */
		describedBy?: string;
		/** Native tooltip. Explanation, never the only place a fact is stated. */
		title?: string;
		/** Renders the row as an anchor instead of a button (PLAN-2392 DR-5):
		 *  Download must be a real `<a download>` and Open needs new-tab /
		 *  middle-click semantics. Ignored while `disabled` — see below. */
		href?: string;
		/** `download` attribute for the anchor branch — the filename to save
		 *  as. Only meaningful with `href`. */
		download?: string;
		/** Anchor `target` (Open sets `_blank`). Only meaningful with `href`. */
		target?: string;
		/** Anchor `rel` (pair `noopener noreferrer` with `target="_blank"`).
		 *  Only meaningful with `href`. */
		rel?: string;
		onclick?: (e: MouseEvent) => void;
		children: Snippet;
	}

	let {
		icon,
		iconSnippet,
		hint,
		danger = false,
		checked,
		disabled = false,
		describedBy,
		title,
		href,
		download,
		target,
		rel,
		onclick,
		children
	}: Props = $props();

	const role = $derived(checked !== undefined ? 'menuitemradio' : 'menuitem');

	// A disabled anchor is not a thing: `<a>` ignores `disabled`, is still
	// focusable and still navigates. Falling back to a disabled <button> keeps
	// the semantics honest AND keeps Menu's keyboard navigation correct — it
	// skips rows with `[role^="menuitem"]:not(:disabled)` (Menu.svelte:130),
	// which no anchor can ever match.
	const asAnchor = $derived(href !== undefined && !disabled);

	/**
	 * A native anchor activates on Enter but NOT on Space, while every other
	 * row in this menu is a <button>, which activates on both. `role="menuitem"`
	 * doesn't add the behavior — it only changes what is announced — so without
	 * this, Space would silently do nothing on exactly the rows a keyboard user
	 * is most likely to try it on (Download, Open).
	 */
	function anchorKeydown(e: KeyboardEvent) {
		if (e.key !== ' ' && e.key !== 'Spacebar') return;
		// Space scrolls the page by default; the menu is the active surface.
		e.preventDefault();
		(e.currentTarget as HTMLAnchorElement).click();
	}
</script>

{#snippet body()}
	{#if iconSnippet}
		<span class="mi-icon" aria-hidden="true">{@render iconSnippet()}</span>
	{:else if icon}
		<span class="mi-icon" aria-hidden="true">{icon}</span>
	{/if}
	<span class="mi-label">{@render children()}</span>
	{#if hint}<span class="mi-hint">{hint}</span>{/if}
	{#if checked}<span class="mi-check" aria-hidden="true">✓</span>{/if}
{/snippet}

{#if asAnchor}
	<a
		class="mi"
		class:danger
		{href}
		{download}
		{target}
		{rel}
		{role}
		{title}
		aria-checked={checked}
		aria-describedby={describedBy}
		{onclick}
		onkeydown={anchorKeydown}
	>
		{@render body()}
	</a>
{:else}
	<button
		type="button"
		class="mi"
		class:danger
		{role}
		{title}
		aria-checked={checked}
		aria-describedby={describedBy}
		{disabled}
		{onclick}
	>
		{@render body()}
	</button>
{/if}

<style>
	.mi {
		display: flex;
		align-items: center;
		gap: 9px;
		width: 100%;
		/* The anchor branch is not a button: it needs the border-box sizing
		   and the link-color/underline reset spelled out, or a row renders
		   wider than the panel and in the link palette. */
		box-sizing: border-box;
		text-decoration: none;
		padding: 7px 9px;
		border: none;
		border-radius: var(--radius-sm);
		background: none;
		color: var(--text-primary);
		font: inherit;
		font-size: 13px;
		text-align: left;
		cursor: pointer;
	}

	.mi:hover:not(:disabled),
	.mi:focus-visible {
		background: var(--bg-hover);
	}

	.mi:focus-visible {
		outline: none;
	}

	.mi:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.mi-icon {
		width: 18px;
		/* Centre the slot's content on BOTH axes — a text glyph as before, and the
		   `iconSnippet`'s `display:block` SVG, which `text-align` alone would leave
		   left-aligned (TASK-2472). */
		display: grid;
		place-items: center;
		flex: 0 0 18px;
		opacity: 0.85;
	}

	.mi-label {
		flex: 1;
		min-width: 0;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.mi-hint {
		color: var(--text-muted);
		font-size: 11.5px;
		flex: 0 0 auto;
	}

	.mi-check {
		color: var(--accent-primary-soft, var(--accent-blue));
		flex: 0 0 auto;
	}

	.mi.danger {
		color: var(--accent-red);
	}

	.mi.danger:hover:not(:disabled) {
		background: color-mix(in srgb, var(--accent-red) 12%, transparent);
	}

	@media (pointer: coarse) {
		.mi {
			padding: 11px 10px;
		}
	}
</style>
