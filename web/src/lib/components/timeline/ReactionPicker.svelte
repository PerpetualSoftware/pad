<script lang="ts">
	interface Props {
		onSelect: (emoji: string) => void;
	}

	const EMOJIS = ['👍', '👎', '❤️', '🎉', '🚀', '👀', '💯', '🤔', '✅', '❌', '🔥', '💡'];

	let { onSelect }: Props = $props();

	let open = $state(false);
	let pickerEl: HTMLDivElement | undefined = $state();

	function toggle() {
		open = !open;
	}

	function select(emoji: string) {
		open = false;
		onSelect(emoji);
	}

	function handleClickOutside(event: MouseEvent) {
		if (pickerEl && !pickerEl.contains(event.target as Node)) {
			open = false;
		}
	}

	$effect(() => {
		if (open) {
			document.addEventListener('click', handleClickOutside, true);
			return () => {
				document.removeEventListener('click', handleClickOutside, true);
			};
		}
	});
</script>

<div class="reaction-picker" bind:this={pickerEl}>
	<button class="trigger" type="button" onclick={toggle} title="Add reaction">
		<svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
			<circle cx="8" cy="8" r="7" stroke="currentColor" stroke-width="1.5" />
			<circle cx="5.5" cy="6.5" r="1" fill="currentColor" />
			<circle cx="10.5" cy="6.5" r="1" fill="currentColor" />
			<path d="M5 10c.5 1.5 2 2.5 3 2.5s2.5-1 3-2.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
		</svg>
		<span class="plus">+</span>
	</button>

	{#if open}
		<div class="popover">
			<div class="emoji-grid">
				{#each EMOJIS as emoji (emoji)}
					<button
						class="emoji-btn"
						type="button"
						onclick={() => select(emoji)}
					>
						{emoji}
					</button>
				{/each}
			</div>
		</div>
	{/if}
</div>

<style>
	.reaction-picker {
		position: relative;
		display: inline-block;
	}

	.trigger {
		display: flex;
		align-items: center;
		gap: 2px;
		padding: var(--space-1);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-secondary);
		color: var(--text-muted);
		cursor: pointer;
		line-height: 1;
	}

	.trigger:hover {
		color: var(--text-primary);
		border-color: var(--accent-blue);
	}

	.plus {
		font-size: 0.75em;
		font-weight: 600;
	}

	.popover {
		position: absolute;
		bottom: 100%;
		left: 0;
		margin-bottom: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-2);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
		z-index: 50;
	}

	.emoji-grid {
		display: grid;
		grid-template-columns: repeat(4, 1fr);
		gap: var(--space-1);
	}

	.emoji-btn {
		width: 36px;
		height: 36px;
		display: flex;
		align-items: center;
		justify-content: center;
		border: none;
		background: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		font-size: 1.2em;
		line-height: 1;
		padding: 0;
	}

	.emoji-btn:hover {
		background: var(--bg-tertiary);
	}
</style>
