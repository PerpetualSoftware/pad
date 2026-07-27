<script lang="ts">
	import { sseService, type SSEStatus } from '$lib/services/sse.svelte';

	// Small live/reconnecting/offline indicator for the realtime board.
	// Reads the existing SSE connection state (sseService.status) — it was
	// already computed for the event stream but never surfaced, so a stale
	// board gave no hint the stream was down (PLAN-1984 / TASK-2027).
	// Colour + dot idiom mirrors the collab-state badge on the item page.
	//
	// `compact` collapses the badge to the coloured dot alone (IDEA-2297). The
	// label text is CLIPPED, not removed: this span is a role="status" live
	// region, and live regions announce on text-content change — an
	// aria-label-only element wouldn't reliably announce a drop to "Offline".
	// The title tooltip still carries the full explanation.

	let { compact = false }: { compact?: boolean } = $props();

	const status = $derived(sseService.status as SSEStatus);

	const label = $derived(
		{
			connected: 'Live',
			reconnecting: 'Reconnecting…',
			disconnected: 'Offline',
			unauthorized: 'Disconnected'
		}[status]
	);

	const title = $derived(
		{
			connected: 'Real-time updates are live. The board reflects changes as they happen.',
			reconnecting: 'Connection dropped. Trying to reconnect… The board may be out of date.',
			disconnected: 'Not connected to the live update stream. The board may be out of date.',
			unauthorized:
				'Live updates stopped — this session lost access to the workspace. Reload to reconnect.'
		}[status]
	);
</script>

<span
	class="sse-state sse-state-{status}"
	class:compact
	title={title}
	role="status"
	aria-label={`Live updates: ${label}`}
>
	<span class="sse-state-dot" aria-hidden="true"></span>
	<span class="sse-state-label">{label}</span>
</span>

<style>
	.sse-state {
		display: inline-flex;
		align-items: center;
		gap: 0.35em;
		font-size: 0.8em;
		color: var(--text-muted);
		white-space: nowrap;
	}
	/* Dot-only mode: clip the label out of the layout but leave it in the
	   accessibility tree so the live region keeps announcing. */
	.sse-state.compact .sse-state-label {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
		white-space: nowrap;
		border: 0;
	}
	.sse-state.compact {
		gap: 0;
	}
	/* The dot is the only visible affordance in compact mode — size it off a
	   fixed length instead of the inherited 0.8em font so it stays legible. */
	.sse-state.compact .sse-state-dot {
		width: 8px;
		height: 8px;
	}

	/* With the label clipped, hue would be the only thing separating Live from
	   Offline — colour-alone conveyance (WCAG 1.4.1), and red/green is the
	   exact pair dichromatic vision collapses. So healthy is a FILLED dot and
	   every unhealthy state is a hollow ring: the distinction that actually
	   matters ("is the stream up?") is carried by shape. Reconnecting is then
	   separated from Offline by its pulse, and by hue for anyone who has
	   reduced-motion on. Only compact mode needs this — the labelled variant
	   already says which state it is in words. */
	.sse-state.compact.sse-state-reconnecting .sse-state-dot,
	.sse-state.compact.sse-state-disconnected .sse-state-dot,
	.sse-state.compact.sse-state-unauthorized .sse-state-dot {
		background: transparent;
		border: 2px solid currentColor;
	}

	.sse-state-dot {
		width: 0.5em;
		height: 0.5em;
		border-radius: 50%;
		background: var(--text-muted);
		flex-shrink: 0;
	}

	/* Connected stays deliberately subtle — muted label, quiet green dot. */
	.sse-state-connected {
		color: var(--text-muted);
	}
	.sse-state-connected .sse-state-dot {
		background: var(--accent-green, #2e9e5b);
	}

	.sse-state-reconnecting {
		color: var(--accent-yellow, #d4a017);
	}
	.sse-state-reconnecting .sse-state-dot {
		background: var(--accent-yellow, #d4a017);
		animation: sse-pulse 1.2s ease-in-out infinite;
	}

	.sse-state-disconnected,
	.sse-state-unauthorized {
		color: var(--accent-red);
	}
	.sse-state-disconnected .sse-state-dot,
	.sse-state-unauthorized .sse-state-dot {
		background: var(--accent-red);
	}

	@keyframes sse-pulse {
		0%,
		100% {
			opacity: 1;
		}
		50% {
			opacity: 0.3;
		}
	}

	@media (prefers-reduced-motion: reduce) {
		.sse-state-reconnecting .sse-state-dot {
			animation: none;
		}
	}
</style>
