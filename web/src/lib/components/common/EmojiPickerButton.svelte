<script lang="ts">
	import EmojiPicker from '$lib/components/common/EmojiPicker.svelte';

	interface Props {
		value: string;
		placeholder?: string;
		size?: 'sm' | 'md';
	}

	let { value = $bindable(), placeholder = '⚡', size = 'sm' }: Props = $props();

	let open = $state(false);

	function handleWindowClick(e: MouseEvent) {
		if (open && !(e.target as HTMLElement)?.closest('.emoji-picker-button')) {
			open = false;
		}
	}

	function handleSelect(emoji: string) {
		value = emoji;
		open = false;
	}
</script>

<svelte:window onclick={handleWindowClick} />

<div class="emoji-picker-button" class:size-md={size === 'md'}>
	<button
		type="button"
		class="trigger"
		class:open
		class:empty={!value}
		onclick={() => (open = !open)}
	>
		{value || placeholder}
	</button>

	{#if open}
		<div class="dropdown">
			<EmojiPicker selected={value} onselect={handleSelect} />
		</div>
	{/if}
</div>

<style>
	.emoji-picker-button {
		position: relative;
		display: inline-flex;
	}

	.trigger {
		width: 36px;
		height: 36px;
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		cursor: pointer;
		font-size: 1.1em;
		line-height: 1;
		padding: 0;
	}

	.size-md .trigger {
		width: 42px;
		height: 42px;
	}

	.trigger:hover {
		border-color: var(--text-muted);
	}

	.trigger.open {
		border-color: var(--accent-blue);
	}

	.trigger.empty {
		color: var(--text-muted);
	}

	.dropdown {
		position: absolute;
		z-index: 50;
		top: calc(100% + 4px);
		left: 0;
	}
</style>
