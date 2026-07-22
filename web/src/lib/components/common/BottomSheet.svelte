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
	import { paneFocusables, nextTrapTarget } from '$lib/collections/paneFocus';

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

	// bind:this the sheet panel so the open/close effect can move focus INTO it
	// and the Tab handler can cycle focus WITHIN it. `$state` so the effect
	// re-runs once the `{#if open}` block mounts the element (mirrors
	// Modal.svelte's `dialogEl`).
	let sheetEl = $state<HTMLElement>();

	// Plain `let` (NOT $state, per CONVE-1688): focus bookkeeping read/written
	// only inside the effect + teardown, never in reactive position.
	let previouslyFocused: HTMLElement | null = null;

	function restoreFocus() {
		if (previouslyFocused && document.contains(previouslyFocused)) {
			previouslyFocused.focus();
		}
		previouslyFocused = null;
	}

	// Move focus INTO the sheet when it opens, and restore it to the trigger on
	// close (BUG-2130). Without this the sheet is a `role="dialog"` that never
	// takes focus: ESC reaches the trigger's layer underneath (closing THAT),
	// and Tab escapes the sheet. Reads `open` (prop) + `sheetEl` ($state); writes
	// only the plain `previouslyFocused`, so no $state is both read and written
	// here and the effect can't self-invalidate (mirrors Modal.svelte).
	$effect(() => {
		const el = sheetEl;
		if (open && el) {
			if (previouslyFocused === null) {
				previouslyFocused = (document.activeElement as HTMLElement | null) ?? null;
			}
			// Focus the panel itself (tabindex=-1) rather than a control inside —
			// avoids implying a selection in the option-list sheets, and lets a
			// screen reader announce the dialog. Tab then steps to the first
			// control. Guarded so a benign effect re-run can't yank focus back off
			// a control the user has already tabbed to.
			if (!el.contains(document.activeElement)) {
				el.focus({ preventScroll: true });
			}
		} else if (!open) {
			restoreFocus();
		}
	});

	// If the component is torn down while open (e.g. a consumer that only mounts
	// the sheet on mobile), still return focus to the trigger.
	$effect(() => () => restoreFocus());

	function handleKeydown(e: KeyboardEvent) {
		if (!open) return;
		// Only the FRONTMOST (innermost) open sheet handles keys. A nested child
		// sheet — e.g. the emoji picker opened from inside the Quick Actions sheet
		// — renders inside our content, so while one is open IT owns Escape/Tab.
		// Every open sheet listens on `window`, so without this both handlers fire
		// and a single Escape closes two layers (BUG-2130 layer isolation, nested
		// case). Order-independent by design: a `defaultPrevented` check can't
		// work here because the outer sheet's window listener is registered first
		// and fires before the inner's.
		if (sheetEl?.querySelector('.bs-sheet')) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			onclose();
			return;
		}
		// Trap Tab within the sheet: without this, Tab past the last control
		// escapes into the obscured content behind it (BUG-2130). Reuses the
		// pane's tested trap math (paneFocus.ts) so the two focus traps can't
		// drift.
		if (e.key === 'Tab' && sheetEl) {
			const target = nextTrapTarget(
				paneFocusables(sheetEl),
				document.activeElement,
				e.shiftKey,
				sheetEl
			);
			if (target) {
				e.preventDefault();
				target.focus({ preventScroll: true });
			}
		}
	}
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="bs-overlay" onclick={onclose}>
		<div
			bind:this={sheetEl}
			class="bs-sheet"
			role="dialog"
			aria-modal="true"
			aria-labelledby={title ? headingId : undefined}
			aria-label={title ? undefined : 'Dialog'}
			tabindex="-1"
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
