<script lang="ts">
	import type { AgentPresence } from '$lib/chat/presence';

	// With showLabel the state is written beside the dot (DOC-4316 section 7: shape
	// and text, never colour alone) and the dot itself is decoration. `detail` is
	// extra tooltip text, such as "2 armed sessions".
	let {
		state,
		checkedAt = null,
		showLabel = false,
		detail = ''
	}: { state: AgentPresence; checkedAt?: number | null; showLabel?: boolean; detail?: string } = $props();

	const LABELS: Record<AgentPresence, string> = {
		online: 'Live · armed',
		busy_uncertain: 'May be working · armed',
		connected_not_accepting: 'Connected · not accepting',
		offline: 'Offline',
		unknown: 'Status unknown'
	};

	// Shape is what tells the states apart without colour.
	const SHAPES: Record<AgentPresence, string> = {
		online: 'solid',
		busy_uncertain: 'hollow',
		connected_not_accepting: 'square',
		offline: 'solid',
		unknown: 'dashed'
	};

	let label = $derived(LABELS[state]);

	// Busy is a guess (activity expires on its own), so the tooltip says when the
	// guess was last checked.
	let title = $derived.by(() => {
		let out = label;
		if (state === 'busy_uncertain' && checkedAt != null) {
			const time = new Date(checkedAt).toLocaleTimeString([], { hour12: false });
			out += `. Last checked ${time}`;
		}
		return detail ? `${out}. ${detail}` : out;
	});
</script>

{#if showLabel}
	<span class="presence-wrap" {title}>
		<span class="presence-dot {state}" data-shape={SHAPES[state]} aria-hidden="true"></span>
		<span class="presence-label">{label}</span>
	</span>
{:else}
	<span class="presence-dot {state}" data-shape={SHAPES[state]} role="img" aria-label={label} {title}></span>
{/if}

<style>
	.presence-wrap {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 0;
	}
	.presence-label {
		color: var(--text-secondary);
		font-size: 0.8rem;
		white-space: nowrap;
	}
	.presence-dot {
		display: inline-block;
		box-sizing: border-box;
		width: 0.65rem;
		height: 0.65rem;
		border-radius: 50%;
		vertical-align: middle;
		flex-shrink: 0;
	}
	.online {
		background: var(--accent-green);
	}
	.busy_uncertain {
		border: 2px solid var(--accent-amber);
	}
	.connected_not_accepting {
		border-radius: 2px;
		background: var(--accent-cyan);
	}
	.offline {
		background: var(--accent-gray);
	}
	.unknown {
		border: 1.5px dashed var(--text-muted);
	}
</style>
