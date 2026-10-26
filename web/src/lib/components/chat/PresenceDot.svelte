<script lang="ts">
	import type { AgentPresence } from '$lib/chat/presence';

	let { state, checkedAt = null }: { state: AgentPresence; checkedAt?: number | null } = $props();

	const LABELS: Record<AgentPresence, string> = {
		online: 'Online',
		busy_uncertain: 'May be working',
		connected_not_accepting: 'Connected, not accepting',
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
		if (state !== 'busy_uncertain' || checkedAt == null) return label;
		const time = new Date(checkedAt).toLocaleTimeString([], { hour12: false });
		return `${label}. Last checked ${time}`;
	});
</script>

<span class="presence-dot {state}" data-shape={SHAPES[state]} role="img" aria-label={label} {title}></span>

<style>
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
