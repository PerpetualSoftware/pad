<script lang="ts">
	import { fly, fade } from 'svelte/transition';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { HistoryEntry } from '$lib/stores/toast.svelte';

	let { visible, onclose }: { visible: boolean; onclose: () => void } = $props();

	function formatRelativeTime(timestamp: number): string {
		const seconds = Math.floor((Date.now() - timestamp) / 1000);
		if (seconds < 5) return 'just now';
		if (seconds < 60) return `${seconds}s ago`;
		const minutes = Math.floor(seconds / 60);
		if (minutes < 60) return `${minutes}m ago`;
		const hours = Math.floor(minutes / 60);
		if (hours < 24) return `${hours}h ago`;
		const days = Math.floor(hours / 24);
		return `${days}d ago`;
	}

	function dotColor(type: HistoryEntry['type']): string {
		switch (type) {
			case 'success': return 'var(--accent-green)';
			case 'error': return 'var(--accent-red, #ef4444)';
			case 'info': return 'var(--accent-blue)';
		}
	}

	function handleClearAll() {
		toastStore.clearHistory();
	}

	$effect(() => {
		if (visible) {
			toastStore.markAllRead();
		}
	});
</script>

{#if visible}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="panel-backdrop" transition:fade={{ duration: 150 }} onclick={onclose}></div>

	<div class="panel" transition:fly={{ x: 320, duration: 200 }}>
		<div class="panel-header">
			<h3 class="panel-title">Notifications</h3>
			<button class="panel-close" onclick={onclose} aria-label="Close notifications">&times;</button>
		</div>

		<div class="panel-body">
			{#if toastStore.history.length === 0}
				<div class="empty-state">
					<span class="empty-icon">
						<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
							<path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/>
							<path d="M13.73 21a2 2 0 0 1-3.46 0"/>
						</svg>
					</span>
					<p>No notifications yet</p>
				</div>
			{:else}
				{#each toastStore.history as entry (entry.id)}
					{#if entry.link}
						<a href={entry.link} class="notification-row clickable" onclick={(e: MouseEvent) => { e.stopPropagation(); }}>
							<span class="notification-dot" style="background: {dotColor(entry.type)}"></span>
							<span class="notification-message">{entry.message}</span>
							<span class="notification-time">{formatRelativeTime(entry.timestamp)}</span>
						</a>
					{:else}
						<div class="notification-row">
							<span class="notification-dot" style="background: {dotColor(entry.type)}"></span>
							<span class="notification-message">{entry.message}</span>
							<span class="notification-time">{formatRelativeTime(entry.timestamp)}</span>
						</div>
					{/if}
				{/each}
			{/if}
		</div>

		{#if toastStore.history.length > 0}
			<div class="panel-footer">
				<button class="clear-btn" onclick={handleClearAll}>Clear all</button>
			</div>
		{/if}
	</div>
{/if}

<style>
	.panel-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.3);
		z-index: 99;
	}

	.panel {
		position: fixed;
		right: 0;
		top: 0;
		width: 320px;
		height: 100vh;
		height: 100dvh;
		background: var(--bg-secondary);
		border-left: 1px solid var(--border);
		z-index: 100;
		display: flex;
		flex-direction: column;
	}

	.panel-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}

	.panel-title {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-primary);
	}

	.panel-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1.3em;
		cursor: pointer;
		padding: 0;
		width: 24px;
		height: 24px;
		display: flex;
		align-items: center;
		justify-content: center;
		border-radius: var(--radius-sm);
	}
	.panel-close:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.panel-body {
		flex: 1;
		overflow-y: auto;
		min-height: 0;
	}

	.empty-state {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		padding: var(--space-8) var(--space-4);
		color: var(--text-muted);
		gap: var(--space-3);
	}
	.empty-state .empty-icon {
		opacity: 0.4;
	}
	.empty-state p {
		margin: 0;
		font-size: 0.88em;
	}

	.notification-row {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		width: 100%;
		text-align: left;
		background: none;
		border-top: none;
		border-left: none;
		border-right: none;
		font-family: inherit;
		font-size: inherit;
		color: inherit;
	}
	.notification-row:last-child {
		border-bottom: none;
	}
	.notification-row.clickable {
		cursor: pointer;
		transition: background 0.15s;
		text-decoration: none;
		color: inherit;
	}
	.notification-row.clickable:hover {
		background: var(--bg-hover);
		text-decoration: none;
	}

	.notification-dot {
		flex-shrink: 0;
		width: 8px;
		height: 8px;
		border-radius: 50%;
		margin-top: 5px;
	}

	.notification-message {
		flex: 1;
		font-size: 0.85em;
		color: var(--text-secondary);
		line-height: 1.4;
		min-width: 0;
		word-break: break-word;
	}

	.notification-time {
		flex-shrink: 0;
		font-size: 0.75em;
		color: var(--text-muted);
		white-space: nowrap;
		margin-top: 1px;
	}

	.panel-footer {
		flex-shrink: 0;
		border-top: 1px solid var(--border);
		padding: var(--space-3) var(--space-4);
	}

	.clear-btn {
		width: 100%;
		padding: var(--space-2);
		background: var(--bg-tertiary);
		border: none;
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.82em;
		cursor: pointer;
	}
	.clear-btn:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}

	@media (max-width: 480px) {
		.panel {
			width: 100vw;
		}
	}
</style>
