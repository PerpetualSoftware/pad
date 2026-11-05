<script lang="ts">
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';

	// One action menu for More and long press. Delete is offered only when the
	// parent permits it; the thread owns the confirmation and frozen target.
	let { open, trigger, author, seq, canDelete, onclose, onReply, onCopy, onDelete }: {
		open: boolean;
		trigger?: HTMLElement;
		author: string;
		seq: number;
		canDelete: boolean;
		onclose: () => void;
		onReply: () => void;
		onCopy: () => void;
		onDelete: () => void;
	} = $props();
</script>

<Menu {open} {trigger} {onclose} mode="portal" sheetOnMobile sheetMenu
	sheetTitle="Message actions" ariaLabel={`Actions for ${author}, message ${seq}`}>
	<div class="message-menu-items">
		<MenuItem onclick={onReply}>Reply</MenuItem>
		<MenuItem onclick={onCopy}>Copy</MenuItem>
		{#if canDelete}<MenuItem danger onclick={onDelete}>Delete</MenuItem>{/if}
	</div>
</Menu>

<style>
	.message-menu-items :global(.mi) { min-height: 44px; }
	.message-menu-items :global(.mi:focus-visible) { outline: 2px solid var(--text-primary); outline-offset: -2px; }
</style>
