<script lang="ts">
	interface Props {
		visible: boolean;
		onclose: () => void;
	}

	let { visible, onclose }: Props = $props();

	interface Shortcut {
		key: string;
		description: string;
	}

	interface ShortcutGroup {
		title: string;
		shortcuts: Shortcut[];
	}

	const groups: ShortcutGroup[] = [
		{
			title: 'Global',
			shortcuts: [
				{ key: '\u2318K', description: 'Search / Command palette' },
				{ key: '\u2318N', description: 'New item' },
				{ key: '\u2318\\', description: 'Toggle sidebar' },
				{ key: '?', description: 'Show keyboard shortcuts' }
			]
		},
		{
			title: 'Navigation',
			shortcuts: [
				{ key: 'j / \u2193', description: 'Move down' },
				{ key: 'k / \u2191', description: 'Move up' },
				{ key: 'Enter', description: 'Open selected item' },
				{ key: 'Esc', description: 'Go back / Close' }
			]
		},
		{
			title: 'Item Detail',
			shortcuts: [
				{ key: '\u2318Enter', description: 'Save' },
				{ key: 'Esc', description: 'Cancel editing' }
			]
		}
	];

	function handleKeydown(e: KeyboardEvent) {
		if (!visible) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			onclose();
		}
	}

	function handleBackdropClick(e: MouseEvent) {
		if (e.target === e.currentTarget) {
			onclose();
		}
	}
</script>

<svelte:window onkeydown={handleKeydown} />

{#if visible}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div class="backdrop" onclick={handleBackdropClick} role="dialog" aria-modal="true" aria-label="Keyboard shortcuts" tabindex="-1">
		<div class="modal">
			<div class="header">
				<h2 class="title">Keyboard Shortcuts</h2>
				<button class="close-btn" onclick={onclose} aria-label="Close">
					<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
						<path d="M4 4L12 12M12 4L4 12" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
					</svg>
				</button>
			</div>
			<div class="body">
				{#each groups as group (group.title)}
					<div class="group">
						<h3 class="group-title">{group.title}</h3>
						<div class="shortcut-list">
							{#each group.shortcuts as shortcut (shortcut.key)}
								<div class="shortcut-row">
									<kbd class="key">{shortcut.key}</kbd>
									<span class="description">{shortcut.description}</span>
								</div>
							{/each}
						</div>
					</div>
				{/each}
			</div>
		</div>
	</div>
{/if}

<style>
	.backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		display: flex;
		align-items: center;
		justify-content: center;
		z-index: 100;
	}

	.modal {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		max-width: 500px;
		width: 90%;
		max-height: 80vh;
		overflow-y: auto;
		box-shadow: 0 8px 32px rgba(0, 0, 0, 0.3);
	}

	.header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-5);
		border-bottom: 1px solid var(--border);
	}

	.title {
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.close-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		cursor: pointer;
		transition: all 0.1s;
	}

	.close-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.body {
		padding: var(--space-4) var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-5);
	}

	.group-title {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
	}

	.shortcut-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.shortcut-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
	}

	.shortcut-row:hover {
		background: var(--bg-tertiary);
	}

	.key {
		display: inline-block;
		padding: 2px var(--space-2);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-secondary);
		min-width: 28px;
		text-align: center;
		line-height: 1.6;
	}

	.description {
		color: var(--text-primary);
		font-size: 0.875em;
	}
</style>
