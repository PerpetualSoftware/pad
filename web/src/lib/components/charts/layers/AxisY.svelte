<script lang="ts">
	import { getLayerCakeContext } from 'layercake';
	import type { LayerCakeContext } from '../theme';

	// layercake 11 keys its context with Svelte's `createContext()` rather than
	// the string 'LayerCake', and hands back plain values behind getters instead
	// of stores. The context object is held whole and read at each use site:
	// destructuring it here would snapshot the getters once and never update
	// (TASK-3093).
	// `LayerCakeContext` in ../theme carries the rest, including why the cast
	// hops through `unknown` and what it still checks.
	const k = getLayerCakeContext() as unknown as LayerCakeContext;

	const ticks = $derived(k.yScale.ticks(4));
</script>

<g class="axis-y">
	{#each ticks as tick (tick)}
		<line
			x1={0}
			y1={k.yScale(tick)}
			x2={k.width}
			y2={k.yScale(tick)}
			stroke="var(--border, #e5e7eb)"
			stroke-width="0.5"
		/>
		<text
			x={-8}
			y={k.yScale(tick)}
			text-anchor="end"
			dominant-baseline="middle"
			fill="var(--text-muted, #6b7280)"
			font-size="11"
		>
			{tick}
		</text>
	{/each}
</g>
