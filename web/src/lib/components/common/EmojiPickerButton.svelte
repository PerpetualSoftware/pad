<script lang="ts">
	import EmojiPicker from '$lib/components/common/EmojiPicker.svelte';

	interface Props {
		value: string;
		placeholder?: string;
		size?: 'sm' | 'md';
	}

	let { value = $bindable(), placeholder = '⚡', size = 'sm' }: Props = $props();

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement>();
	let dropdownX = $state(0);
	let dropdownY = $state(0);

	/**
	 * Portal into the nearest <dialog> (stays in top-layer, escapes overflow
	 * children like .dialog-body), or document.body if not inside a dialog.
	 */
	function portal(node: HTMLElement) {
		const dialog = triggerEl?.closest('dialog');
		const target = dialog || document.body;
		target.appendChild(node);

		// <dialog> has browser-default overflow:auto which clips children.
		// Temporarily override while the dropdown is mounted.
		if (dialog) {
			dialog.style.overflow = 'visible';
		}

		return {
			destroy() {
				node.remove();
				if (dialog) {
					dialog.style.overflow = '';
				}
			}
		};
	}

	function handleWindowClick(e: MouseEvent) {
		if (open) {
			const target = e.target as HTMLElement;
			if (!target?.closest('.emoji-picker-button') && !target?.closest('.epb-dropdown')) {
				open = false;
			}
		}
	}

	function toggleOpen() {
		if (!open && triggerEl) {
			const triggerRect = triggerEl.getBoundingClientRect();
			const dialog = triggerEl.closest('dialog');
			if (dialog) {
				// dialog with transform creates a containing block —
				// position: absolute is relative to it, so offset accordingly
				const dialogRect = dialog.getBoundingClientRect();
				dropdownX = triggerRect.left - dialogRect.left;
				dropdownY = triggerRect.bottom - dialogRect.top + 4;
			} else {
				dropdownX = triggerRect.left;
				dropdownY = triggerRect.bottom + 4;
			}
		}
		open = !open;
	}

	function handleSelect(emoji: string) {
		value = emoji;
		open = false;
	}
</script>

<svelte:window onclick={handleWindowClick} />

<div class="emoji-picker-button" class:size-md={size === 'md'}>
	<button
		bind:this={triggerEl}
		type="button"
		class="trigger"
		class:open
		class:empty={!value}
		onclick={toggleOpen}
	>
		{value || placeholder}
	</button>

	{#if open}
		<div class="epb-dropdown" use:portal style="position:absolute; z-index:99999; left:{dropdownX}px; top:{dropdownY}px;">
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
</style>
