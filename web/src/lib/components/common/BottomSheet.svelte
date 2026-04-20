<!--
	BottomSheet — mobile-first modal primitive.

	Intentionally simple: mirrors the working CreateCollectionModal pattern
	(overlay + inner panel with stopPropagation) so event handling behaves
	exactly like every other modal in the app on mobile.

	Minimal usage:

	```svelte
	<script>
		let open = $state(false);
	</script>
	<BottomSheet {open} onclose={() => (open = false)} title="Example">
		<p>Sheet content here</p>
	</BottomSheet>
	```
-->
<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		open: boolean;
		onclose: () => void;
		title?: string;
		children: Snippet;
	}

	let { open, onclose, title, children }: Props = $props();

	// Stable per-instance heading id so aria-labelledby can point at the
	// visible title when one is provided. $props.id() must be the direct
	// initializer of a top-level const, so we bind it to `uid` and compose
	// the full id separately.
	const uid = $props.id();
	const headingId = `bottom-sheet-heading-${uid}`;

	function handleKeydown(e: KeyboardEvent) {
		if (!open) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			onclose();
		}
	}
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="bs-overlay" onclick={onclose}>
		<div
			class="bs-sheet"
			role="dialog"
			aria-modal="true"
			aria-labelledby={title ? headingId : undefined}
			aria-label={title ? undefined : 'Dialog'}
			onclick={(e) => e.stopPropagation()}
		>
			{#if title}
				<header class="bs-header">
					<h2 id={headingId} class="bs-title">{title}</h2>
					<button class="bs-close" type="button" onclick={onclose} aria-label="Close">
						&#10005;
					</button>
				</header>
			{/if}
			<div class="bs-content">
				{@render children()}
			</div>
		</div>
	</div>
{/if}

<style>
	.bs-overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		align-items: flex-end;
	}

	.bs-sheet {
		width: 100%;
		max-height: 85vh;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-bottom: none;
		border-radius: var(--radius-lg) var(--radius-lg) 0 0;
		box-shadow: 0 -12px 40px rgba(0, 0, 0, 0.4);
		overflow: hidden;
		display: flex;
		flex-direction: column;
	}

	.bs-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-3) var(--space-5);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.bs-title {
		margin: 0;
		font-size: 1.05em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.bs-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1em;
		cursor: pointer;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		line-height: 1;
	}

	.bs-close:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.bs-content {
		padding: var(--space-3) 0;
		overflow-y: auto;
		flex: 1 1 auto;
		min-height: 0;
	}

	/* Desktop: center the panel as a modal rather than docking to bottom. */
	@media (min-width: 640px) {
		.bs-overlay {
			align-items: flex-start;
			padding: 10vh var(--space-4) var(--space-4);
		}
		.bs-sheet {
			max-width: 520px;
			border: 1px solid var(--border);
			border-radius: var(--radius-lg);
		}
	}
</style>
