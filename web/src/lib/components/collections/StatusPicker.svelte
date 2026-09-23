<!--
	StatusPicker — the status chip on a card or table row, as a MENU trigger
	(BUG-3157).

	It used to be a one-tap CYCLE control: a click advanced the status to the
	next option immediately. On a board grouped by status the next option is the
	next lane, so a tap meant to open the card moved it one lane instead, and on
	touch there was no hover tooltip to say the chip was a control at all. Dave's
	ruling: on every device the chip opens a picker, and nothing changes until an
	option is chosen.

	The picker is the shared Menu — a portaled popover on desktop, a BottomSheet
	at the mobile breakpoint — mounted from a host portaled to <body>. The chip
	lives inside the card's <a> and inside the board's dnd zone; rendering the
	sheet there would put its rows inside a link and under svelte-dnd-action,
	which rewrites the ARIA roles of what it contains.

	Choosing the current value writes nothing. A stored value the options do not
	declare is shown as-is with no row checked; choosing any option is then an
	explicit decision, which is exactly what the old cycle could not offer
	(BUG-3068 had to refuse the click instead).
-->
<script lang="ts">
	import Chip from '$lib/components/common/Chip.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { portal } from '$lib/utils/portalAction';
	import { statusColor, formatFieldLabel as formatLabel } from '$lib/utils/fieldColors';
	import { viewport } from '$lib/stores/breakpoint.svelte';

	interface Props {
		/** The stored status; '' when the item has none. */
		value: string;
		options: string[];
		onselect: (status: string) => void;
	}

	let { value, options, onselect }: Props = $props();

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement>();

	function toggle(e: MouseEvent) {
		// The chip sits inside the card's <a> and the table row's click target;
		// Chip already preventDefault()s, this keeps the card from opening.
		e.stopPropagation();
		open = !open;
	}

	function choose(status: string) {
		open = false;
		if (status !== value) onselect(status);
	}
</script>

<Chip
	size="sm"
	color={statusColor(value)}
	onclick={toggle}
	bind:el={triggerEl}
	haspopup={viewport.isMobile ? 'dialog' : 'menu'}
	expanded={open}
	title="Change status"
>
	{formatLabel(value)}
</Chip>

{#if open}
	<div class="status-picker-host" use:portal>
		<Menu
			{open}
			onclose={() => (open = false)}
			trigger={triggerEl}
			mode="portal"
			align="left"
			width={200}
			sheetOnMobile
			sheetMenu
			sheetTitle="Status"
			ariaLabel="Status"
		>
			{#each options as option (option)}
				<MenuItem checked={option === value} onclick={() => choose(option)}>
					{formatLabel(option)}
				</MenuItem>
			{/each}
		</Menu>
	</div>
{/if}
