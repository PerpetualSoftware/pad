import type { Readable } from 'svelte/store';

/**
 * Default color palette for chart series. Each entry references a CSS custom
 * property with a hex fallback, so apps can theme charts via `--chart-N`
 * without the variables being required to exist.
 */
export const PALETTE = [
	'var(--chart-1, #4f46e5)',
	'var(--chart-2, #06b6d4)',
	'var(--chart-3, #f59e0b)',
	'var(--chart-4, #10b981)'
];

/** Resolve a series color: explicit color wins, else fall back to the palette by index. */
export function resolveColor(color: string | undefined, index: number): string {
	return color ?? PALETTE[index % PALETTE.length];
}

/** A datum is a flat record of category/value pairs. */
export type ChartDatum = Record<string, string | number>;

/** Resolved series after color fallback has been applied. */
export interface ResolvedSeries {
	key: string;
	label: string;
	color: string;
}

/**
 * The shape of the `'LayerCake'` context. LayerCake exposes its values as
 * Svelte stores (it's built on legacy `writable`/`derived`), so each field is
 * a `Readable`. Typing the `getContext` result this way keeps svelte-check
 * happy instead of leaving everything `unknown`.
 */
export interface LayerCakeContext {
	data: Readable<ChartDatum[]>;
	xGet: Readable<(d: ChartDatum) => number>;
	yGet: Readable<(d: ChartDatum) => number>;
	xScale: Readable<{
		(value: unknown): number;
		bandwidth?: () => number;
		ticks?: (count?: number) => number[];
		domain: () => unknown[];
	}>;
	yScale: Readable<{
		(value: unknown): number;
		bandwidth?: () => number;
		ticks: (count?: number) => number[];
		domain: () => unknown[];
	}>;
	x: Readable<(d: ChartDatum) => string | number>;
	y: Readable<(d: ChartDatum) => string | number>;
	width: Readable<number>;
	height: Readable<number>;
	xRange: Readable<number[]>;
	yRange: Readable<number[]>;
}
