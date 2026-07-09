<!--
  Modal — shared dialog primitive built on the native <dialog> element and
  dialog.showModal() (PLAN-1984 Web-UX audit / TASK-2023).

  Why native <dialog>:
  - Focus trap, Escape-to-dismiss, and the top-layer (renders above every
    stacking context, no z-index juggling) all come for free with showModal().
  - We add the two things the platform doesn't: focus SAVE/RESTORE across
    open/close, and single-source-of-truth open state driven by the `open`
    prop.

  State model (CONVE-1688): the open/close effect READS `open` (a prop) and
  the DOM's `dialogEl.open`, and WRITES nothing reactive — no $state is both
  written and read inside it, so the effect can't self-invalidate. Focus
  bookkeeping (`previouslyFocused`) is a plain `let`, never $state.

  The dialog is always mounted; visibility is toggled via showModal()/close()
  (a not-open <dialog> is display:none via the UA stylesheet). Consumers must
  NOT wrap <Modal> in {#if open} — pass `open` and let the primitive drive it.
  Inner content is gated on `open` here, so consumers keep their fresh-on-open
  reset semantics.

  Consumers own their own header/body/footer markup (passed as children) so
  each migrated modal keeps its exact styling. Wire `labelledby` to the id of
  the heading element inside your children for the aria-labelledby link, or
  pass `ariaLabel` when there's no visible heading.
-->
<script lang="ts">
	import type { Snippet } from 'svelte';

	interface Props {
		/** Whether the modal is shown. Single source of truth — drive it from parent state. */
		open: boolean;
		/** Called when the user dismisses via Escape, backdrop click, or the primitive needs the parent to close. The parent must flip `open` to false. */
		onclose: () => void;
		/** id of the heading element inside `children` — wired to the dialog's aria-labelledby. */
		labelledby?: string;
		/** Accessible name when there's no visible heading to point at. */
		ariaLabel?: string;
		/** Dismiss when the backdrop (area outside the dialog box) is clicked. Default true. */
		closeOnBackdrop?: boolean;
		/** Max width of the dialog box (any CSS length). Default 480px. */
		maxWidth?: string;
		/** Vertical placement: 'top' sits it ~10vh from the top (matches most existing modals); 'center' vertically centers. Default 'top'. */
		placement?: 'top' | 'center';
		/** Extra class(es) appended to the dialog element. */
		class?: string;
		children: Snippet;
	}

	let {
		open,
		onclose,
		labelledby,
		ariaLabel,
		closeOnBackdrop = true,
		maxWidth = '480px',
		placement = 'top',
		class: klass = '',
		children
	}: Props = $props();

	// bind:this target — $state so the open/close effect re-runs once the
	// element is mounted. The effect only READS this; it never writes it.
	let dialogEl = $state<HTMLDialogElement>();

	// Plain variable (NOT $state): focus bookkeeping read/written only inside
	// the effect + teardown, never in reactive position.
	let previouslyFocused: HTMLElement | null = null;

	function restoreFocus() {
		if (previouslyFocused && document.contains(previouslyFocused)) {
			previouslyFocused.focus();
		}
		previouslyFocused = null;
	}

	// Drive the native dialog from the `open` prop. Reads `open` (prop) and
	// `el.open` (DOM) only — writes no reactive state, so it can't loop.
	$effect(() => {
		const el = dialogEl;
		if (!el) return;
		if (open && !el.open) {
			previouslyFocused = (document.activeElement as HTMLElement | null) ?? null;
			el.showModal();
		} else if (!open && el.open) {
			el.close();
			restoreFocus();
		}
	});

	// If the component is torn down while open, make sure focus is returned.
	$effect(() => {
		return () => {
			if (dialogEl?.open) {
				dialogEl.close();
			}
			restoreFocus();
		};
	});

	// Escape fires a `cancel` event. Prevent the native close so the parent's
	// `open` stays the single source of truth: we ask the parent to close, the
	// parent flips `open`, and the effect above performs the actual close +
	// focus restore. This keeps focus-restore on ONE path.
	function handleCancel(e: Event) {
		e.preventDefault();
		onclose();
	}

	// A click whose target is the dialog element itself landed on the backdrop
	// (the ::backdrop pseudo dispatches its clicks to the dialog). Clicks on the
	// content target descendant nodes, so this cleanly distinguishes the two.
	function handleClick(e: MouseEvent) {
		if (closeOnBackdrop && e.target === dialogEl) {
			onclose();
		}
	}
</script>

<dialog
	bind:this={dialogEl}
	class={['modal', klass]}
	style:--modal-max-width={maxWidth}
	data-placement={placement}
	aria-labelledby={labelledby}
	aria-label={ariaLabel}
	oncancel={handleCancel}
	onclick={handleClick}
>
	{#if open}
		{@render children()}
	{/if}
</dialog>

<style>
	dialog.modal {
		position: fixed;
		inset: 0;
		margin: auto;
		padding: 0;
		width: min(var(--modal-max-width, 480px), calc(100vw - 2 * var(--space-4, 16px)));
		max-width: var(--modal-max-width, 480px);
		max-height: 85vh;
		display: flex;
		flex-direction: column;
		overflow: hidden;
		color: var(--text-primary);
		/* Surface tokens are overridable via the `--modal-*` custom properties
		   (e.g. `<Modal --modal-bg="var(--bg-primary)">`) so migrated modals keep
		   their exact original surface where it differed from the default. */
		background: var(--modal-bg, var(--bg-secondary));
		border: var(--modal-border, 1px solid var(--border));
		border-radius: var(--modal-radius, var(--radius-lg));
		box-shadow: var(--modal-shadow, 0 20px 60px rgba(0, 0, 0, 0.5));
	}

	/* The dialog is always mounted (visibility driven by showModal()/close()).
	   Our `display: flex` above ties the UA `dialog:not([open]) { display: none }`
	   rule on specificity and would win by source order, leaving a CLOSED dialog
	   rendered as an empty bordered/shadowed box that can intercept clicks. This
	   higher-specificity rule restores the hidden-when-closed behavior. */
	dialog.modal:not([open]) {
		display: none;
	}

	/* Sit near the top (the dominant idiom across the app's modals) rather than
	   dead-center. margin-inline stays `auto` from the rule above, so horizontal
	   centering is preserved. */
	dialog.modal[data-placement='top'] {
		margin-top: 10vh;
		margin-bottom: auto;
	}

	dialog.modal::backdrop {
		background: rgba(0, 0, 0, 0.5);
	}

	dialog.modal[open] {
		animation: modal-in 160ms ease-out;
	}

	dialog.modal[open]::backdrop {
		animation: backdrop-in 140ms ease-out;
	}

	@keyframes modal-in {
		from {
			opacity: 0;
			transform: translateY(-4px) scale(0.98);
		}
		to {
			opacity: 1;
			transform: translateY(0) scale(1);
		}
	}

	@keyframes backdrop-in {
		from {
			opacity: 0;
		}
		to {
			opacity: 1;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		dialog.modal[open],
		dialog.modal[open]::backdrop {
			animation: none;
		}
	}

	@media (max-width: 640px) {
		dialog.modal {
			max-height: calc(100vh - var(--space-6, 24px));
		}
		dialog.modal[data-placement='top'] {
			margin-top: var(--space-4, 16px);
		}
	}
</style>
