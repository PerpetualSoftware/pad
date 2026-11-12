<!--
	DialogShell: one layout for a dialog with a title, a body and a footer.

	A Modal on desktop and a BottomSheet when `mobile` is set. The header names the
	dialog (the title is its accessible name), says what it is about (`subject`),
	and carries the close button. The footer takes the dialog's buttons: `footer`
	on the right, `footerStart` on the left for a ghost or destructive action.
	On a phone the footer buttons share the full width and are 44px tall.

	While `pending`, Escape, the close button and the backdrop do nothing; the
	caller disables its own footer buttons.
-->
<script lang="ts">
	import { tick, untrack, type Snippet } from 'svelte';
	import Modal from './Modal.svelte';
	import BottomSheet from './BottomSheet.svelte';

	interface Props {
		open: boolean;
		onclose: () => void;
		title: string;
		/** One muted line under the title naming what the dialog acts on. */
		subject?: string;
		/** A reference shown after the subject in the mono face, such as "TASK-14". */
		subjectRef?: string;
		mobile: boolean;
		pending?: boolean;
		maxWidth?: string;
		children: Snippet;
		footer?: Snippet;
		footerStart?: Snippet;
	}

	let { open, onclose, title, subject = '', subjectRef = '', mobile, pending = false, maxWidth = '480px', children, footer, footerStart }: Props = $props();

	const uid = $props.id();
	let heading = $state<HTMLHeadingElement>();
	const close = () => { if (!pending) onclose(); };
	// Opening reads out the title first; the sheet focuses its own panel.
	$effect(() => {
		const el = open && !mobile ? heading : undefined;
		untrack(() => { if (el) void tick().then(() => el.focus()); });
	});
</script>

{#snippet about()}
	{#if subject || subjectRef}<p class="subject">{subject}{#if subjectRef}{' '}<span class="ref">{subjectRef}</span>{/if}</p>{/if}
{/snippet}
{#snippet content()}
	{#if mobile}
		<div class="sheet-about">{@render about()}</div>
	{:else}
		<header class="head">
			<div class="names"><h2 id={uid} tabindex="-1" bind:this={heading}>{title}</h2>{@render about()}</div>
			<button type="button" class="close" aria-label="Close" onclick={close}>
				<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" /></svg>
			</button>
		</header>
	{/if}
	<div class="body" class:sheet={mobile}>{@render children()}</div>
	{#if footer || footerStart}
		<footer class="foot" class:sheet={mobile}>{@render footerStart?.()}<span class="spacer"></span>{@render footer?.()}</footer>
	{/if}
{/snippet}

{#if mobile}
	<BottomSheet {open} onclose={close} {title}>{@render content()}</BottomSheet>
{:else}
	<Modal {open} onclose={close} labelledby={uid} {maxWidth}>{@render content()}</Modal>
{/if}

<style>
	.head { display: flex; align-items: flex-start; gap: var(--space-2); padding: var(--space-4) var(--space-5); border-bottom: 1px solid var(--border); }
	.names { flex: 1; min-width: 0; }
	h2 { margin: 0; font-size: 16px; font-weight: 700; outline: none; }
	.subject { margin: 2px 0 0; font-size: 12px; color: var(--text-muted); overflow-wrap: anywhere; }
	.ref { font-family: var(--font-mono); }
	.close { display: grid; place-items: center; flex: 0 0 auto; width: 32px; height: 32px; padding: 0; border: 0; border-radius: var(--radius); background: transparent; color: var(--text-secondary); cursor: pointer; }
	.close:hover { background: var(--bg-hover); color: var(--text-primary); }
	.close:focus-visible { outline: 2px solid var(--accent-primary); outline-offset: 1px; }
	.body { display: flex; flex-direction: column; gap: var(--space-4); padding: var(--space-5); overflow-y: auto; overscroll-behavior: contain; flex: 1 1 auto; min-height: 0; min-width: 0; }
	.body.sheet { padding: var(--space-3) var(--space-5) var(--space-4); overflow: visible; }
	.body.sheet:first-child { padding-top: 0; }
	.body :global([role='alert']) { margin: 0; color: var(--accent-red); }
	.foot { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: var(--space-2); padding: var(--space-3) var(--space-5); border-top: 1px solid var(--border); background: color-mix(in srgb, var(--bg-tertiary) 50%, transparent); }
	.spacer { flex: 1; }
	.foot.sheet { padding: var(--space-3) var(--space-5) 0; background: none; }
	.sheet-about { padding: 0 var(--space-5); }
	.sheet-about:empty { display: none; }
	.foot.sheet .spacer { display: none; }
	.foot.sheet :global(.btn) { flex: 1; min-height: 44px; }
	@media (pointer: coarse) { .close { width: 44px; height: 44px; } }
</style>
