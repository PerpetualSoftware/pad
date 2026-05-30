<script lang="ts">
	/**
	 * Full-screen image viewer for attachment thumbnails (IDEA-1660).
	 * Opened by a host that captures a click on an `img[data-attachment-id]`
	 * and passes the attachment id(s) — the lightbox loads the ORIGINAL
	 * (un-variant) blob so the expanded view is full resolution regardless
	 * of the thumbnail variant shown inline.
	 *
	 * Keyboard: Esc closes, ←/→ navigate when multiple images were passed.
	 * Backdrop click closes; clicking the image itself does not.
	 */
	import { untrack } from 'svelte';
	import { attachmentDownloadUrl } from '$lib/markdown/attachments';

	export interface LightboxImage {
		id: string;
		alt: string;
	}

	interface Props {
		images: LightboxImage[];
		/** Index to open at (clamped). */
		index?: number;
		wsSlug: string;
		onClose: () => void;
	}

	let { images, index = 0, wsSlug, onClose }: Props = $props();

	// Seeded once at mount — the host remounts (null → set) on each open, so
	// no prop-sync effect is needed. untrack makes the initial-value capture
	// explicit (props are constant for this component's lifetime).
	let current = $state(
		untrack(() => Math.min(Math.max(index, 0), Math.max(images.length - 1, 0)))
	);

	let hasMultiple = $derived(images.length > 1);
	let img = $derived(images[current]);
	let src = $derived(img ? attachmentDownloadUrl(wsSlug, img.id) : '');

	function prev() {
		current = (current - 1 + images.length) % images.length;
	}
	function next() {
		current = (current + 1) % images.length;
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			onClose();
		} else if (e.key === 'ArrowLeft' && hasMultiple) {
			prev();
		} else if (e.key === 'ArrowRight' && hasMultiple) {
			next();
		}
	}

	// Close only on a click of the backdrop itself — clicks on the image or
	// controls have a different target, so they don't dismiss. This avoids
	// putting a click handler (and its a11y burden) on the <img>.
	function onBackdropClick(e: MouseEvent) {
		if (e.target === e.currentTarget) onClose();
	}
</script>

<svelte:window onkeydown={onKeydown} />

<div
	class="lightbox-backdrop"
	role="presentation"
	onclick={onBackdropClick}
>
	<button class="lightbox-close" type="button" title="Close (Esc)" onclick={onClose}>
		&#10005;
	</button>

	{#if hasMultiple}
		<button class="lightbox-nav prev" type="button" title="Previous (←)" onclick={prev}>
			&#8249;
		</button>
		<button class="lightbox-nav next" type="button" title="Next (→)" onclick={next}>
			&#8250;
		</button>
	{/if}

	{#if img}
		<img class="lightbox-image" {src} alt={img.alt || 'Attachment'} />
	{/if}

	{#if hasMultiple}
		<div class="lightbox-counter">{current + 1} / {images.length}</div>
	{/if}
</div>

<style>
	.lightbox-backdrop {
		position: fixed;
		inset: 0;
		z-index: 1000;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.82);
		backdrop-filter: blur(2px);
		cursor: zoom-out;
	}

	.lightbox-image {
		max-width: 92vw;
		max-height: 92vh;
		object-fit: contain;
		border-radius: var(--radius);
		box-shadow: 0 8px 40px rgba(0, 0, 0, 0.5);
		cursor: default;
	}

	.lightbox-close {
		position: absolute;
		top: var(--space-3);
		right: var(--space-3);
		width: 40px;
		height: 40px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.1rem;
		cursor: pointer;
		line-height: 1;
	}

	.lightbox-close:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav {
		position: absolute;
		top: 50%;
		transform: translateY(-50%);
		width: 48px;
		height: 48px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: rgba(0, 0, 0, 0.4);
		border: 1px solid rgba(255, 255, 255, 0.2);
		border-radius: 50%;
		color: #fff;
		font-size: 1.8rem;
		cursor: pointer;
		line-height: 1;
	}

	.lightbox-nav:hover {
		background: rgba(0, 0, 0, 0.7);
	}

	.lightbox-nav.prev {
		left: var(--space-3);
	}

	.lightbox-nav.next {
		right: var(--space-3);
	}

	.lightbox-counter {
		position: absolute;
		bottom: var(--space-3);
		left: 50%;
		transform: translateX(-50%);
		padding: var(--space-1) var(--space-3);
		background: rgba(0, 0, 0, 0.5);
		border-radius: 9999px;
		color: #fff;
		font-size: 0.8rem;
	}
</style>
