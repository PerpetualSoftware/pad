<script lang="ts">
	interface Props {
		values: number[];
		width?: number;
		height?: number;
		color?: string;
		ariaLabel?: string;
	}

	let {
		values,
		width = 80,
		height = 24,
		color = 'var(--chart-1, #4f46e5)',
		ariaLabel = 'Sparkline'
	}: Props = $props();

	const path = $derived.by(() => {
		const n = values.length;
		if (n === 0) return '';

		const min = Math.min(...values);
		const max = Math.max(...values);
		const span = max - min || 1;
		const pad = 1; // keep the stroke inside the viewbox
		const usableH = height - pad * 2;

		// Single point: draw a flat line across the middle.
		if (n === 1) {
			const y = pad + usableH / 2;
			return `M0,${y} L${width},${y}`;
		}

		const stepX = width / (n - 1);
		return values
			.map((v, i) => {
				const x = i * stepX;
				const y = pad + usableH - ((v - min) / span) * usableH;
				return `${i === 0 ? 'M' : 'L'}${x},${y}`;
			})
			.join(' ');
	});
</script>

{#if values.length > 0}
	<svg
		class="sparkline"
		viewBox="0 0 {width} {height}"
		width={width}
		height={height}
		preserveAspectRatio="none"
		role="img"
		aria-label={ariaLabel}
	>
		<path
			d={path}
			fill="none"
			stroke={color}
			stroke-width="1.5"
			stroke-linejoin="round"
			stroke-linecap="round"
			vector-effect="non-scaling-stroke"
		/>
	</svg>
{/if}

<style>
	.sparkline {
		display: inline-block;
		vertical-align: middle;
	}
</style>
