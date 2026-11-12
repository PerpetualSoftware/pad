<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';
	import Button from '$lib/components/common/Button.svelte';
	import Menu from '$lib/components/common/Menu.svelte';

	interface Props extends Omit<HTMLButtonAttributes, 'children' | 'title' | 'class'> {
		/** Accessible name of the button and of the menu it opens. */
		label: string;
		/** Heading of the bottom sheet the menu becomes on a phone. */
		title: string;
		disabled?: boolean;
		/** The menu items. Each wraps its handler in `pick`, which closes the menu and returns focus to the button first. */
		children: Snippet<[pick: (run: () => void) => void]>;
	}

	let { label, title, disabled = false, children, ...rest }: Props = $props();
	let open = $state(false);
	let trigger = $state<HTMLElement>();
	function pick(run: () => void) {
		open = false;
		trigger?.focus();
		run();
	}
</script>

<Button {...rest} size="sm" variant="ghost" class="agenda-row-menu" aria-label={label} aria-haspopup="menu" aria-expanded={open} {disabled}
	onclick={(event: MouseEvent) => { trigger = event.currentTarget as HTMLElement; open = !open; }}>
	<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="1.7" /><circle cx="12" cy="12" r="1.7" /><circle cx="19" cy="12" r="1.7" /></svg>
</Button>
<Menu {open} {trigger} mode="portal" sheetOnMobile sheetMenu sheetTitle={title} ariaLabel={label} onclose={() => open = false}>
	{@render children(pick)}
</Menu>

<style>
	:global(.btn.agenda-row-menu) { width: 32px; height: 32px; padding: 0; flex: 0 0 auto; }
	@media (pointer: coarse) {
		:global(.btn.agenda-row-menu) { width: 44px; height: 44px; min-height: 44px; }
	}
</style>
