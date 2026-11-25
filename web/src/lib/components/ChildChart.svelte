<script lang="ts">
	import { safeText } from '$lib/fields/fieldShape';
	import type { Item } from '$lib/types';
	import { countedChildren, isChildDone } from '$lib/collections/childProgress';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { collectionsNotStaleFor } from '$lib/collections/categoricalFieldValue';
	import { buildBurndown } from './childChart';

	interface Props {
		children: Item[];
		startDate?: string;
		endDate?: string;
		/** The workspace the children belong to, so a collection store stamped
		 * for another workspace is not used to judge them. */
		wsSlug?: string;
	}

	let { children, startDate, endDate, wsSlug }: Props = $props();

	// The parent's `start_date` / `end_date` are stored field values, typed
	// `string` here but not guaranteed to be one (BUG-3052). `new Date` threw on a
	// stored `{"toString":0}`. Only an OBJECT (arrays included) is converted, to
	// its safe text; every primitive — a date string, an epoch number, even a
	// boolean — reaches `new Date` untouched, exactly as before.
	function dateInput(v: unknown): unknown {
		return typeof v === 'object' && v !== null ? safeText(v) : v;
	}
	// Each child by its own collection's done field and terminal/abandoned
	// values. An abandoned child is not part of the work any more and leaves
	// the burndown entirely (BUG-3195).
	const collections = $derived(
		collectionsNotStaleFor(collectionStore.collectionsWorkspace, wsSlug) ? (collectionStore.collections ?? []) : []
	);

	// Chart dimensions
	const padding = { top: 20, right: 20, bottom: 30, left: 40 };
	const width = 600;
	const height = 200;
	const chartW = width - padding.left - padding.right;
	const chartH = height - padding.top - padding.bottom;

	// The burndown math lives in childChart.ts (BUG-4838): every series uses the
	// axis's value->y mapping, and a date-only bound is a local calendar day.
	let chartData = $derived(
		buildBurndown({
			counted: countedChildren(children, collections),
			isDone: (child) => isChildDone(child, collections),
			startDate: startDate ? dateInput(startDate) : undefined,
			endDate: endDate ? dateInput(endDate) : undefined,
			chartW,
			chartH
		})
	);
</script>

{#if chartData}
	<div class="child-chart">
		<svg viewBox="0 0 {width} {height}" preserveAspectRatio="xMidYMid meet">
			<g transform="translate({padding.left},{padding.top})">
				<!-- Y axis gridlines -->
				{#each chartData.yTicks as tick (tick.value)}
					<line
						x1={0}
						y1={tick.y}
						x2={chartW}
						y2={tick.y}
						stroke="var(--border)"
						stroke-width="0.5"
					/>
					<text
						x={-8}
						y={tick.y}
						text-anchor="end"
						dominant-baseline="middle"
						fill="var(--text-muted)"
						font-size="11"
					>
						{tick.value}
					</text>
				{/each}

				<!-- Ideal line (dashed, gray) -->
				<line
					x1={chartData.idealLine.x1}
					y1={chartData.idealLine.y1}
					x2={chartData.idealLine.x2}
					y2={chartData.idealLine.y2}
					stroke="var(--text-muted)"
					stroke-width="1.5"
					stroke-dasharray="6 4"
					opacity="0.6"
				/>

				<!-- Actual area fill -->
				<path
					d={chartData.areaPath}
					fill="var(--accent-green)"
					opacity="0.1"
				/>

				<!-- Actual line -->
				<path
					d={chartData.actualPath}
					fill="none"
					stroke="var(--accent-green)"
					stroke-width="2"
					stroke-linejoin="round"
				/>

				<!-- Today marker -->
				{#if chartData.showTodayMarker}
					<line
						x1={chartData.todayX}
						y1={0}
						x2={chartData.todayX}
						y2={chartH}
						stroke="var(--status-blue)"
						stroke-width="1"
						stroke-dasharray="4 3"
						opacity="0.7"
					/>
					<text
						x={chartData.todayX}
						y={-6}
						text-anchor="middle"
						fill="var(--status-blue)"
						font-size="10"
						opacity="0.8"
					>
						today
					</text>
				{/if}

				<!-- X axis: baseline -->
				<line
					x1={0}
					y1={chartH}
					x2={chartW}
					y2={chartH}
					stroke="var(--border)"
					stroke-width="1"
				/>

				<!-- X axis labels -->
				<text
					x={0}
					y={chartH + 18}
					text-anchor="start"
					fill="var(--text-muted)"
					font-size="11"
				>
					{chartData.startLabel}
				</text>
				<text
					x={chartW}
					y={chartH + 18}
					text-anchor="end"
					fill="var(--text-muted)"
					font-size="11"
				>
					{chartData.endLabel}
				</text>
			</g>
		</svg>
	</div>
{/if}

<style>
	.child-chart {
		width: 100%;
		margin: var(--space-3) 0;
	}

	.child-chart svg {
		width: 100%;
		height: 200px;
		display: block;
	}
</style>
