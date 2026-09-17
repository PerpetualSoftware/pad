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
 * The shape of LayerCake's chart context, as our layers consume it.
 *
 * layercake 11 keys this context with Svelte's `createContext()` — not the
 * string `'LayerCake'` — so layers reach it via the package's exported
 * `getLayerCakeContext()`. Each field is a PLAIN value read through an
 * enumerable getter on the context object (`LayerCake.svelte` builds it with
 * `Object.defineProperties`), not a store; reads are reactive because the
 * getters close over the chart's own `$state`/`$derived`. Under 10.x these
 * were `Readable` stores and the layers used `$xScale` etc. (TASK-3093).
 *
 * Two consequences the layers depend on:
 *   - Hold the context object and read `k.width` at the use site. Destructuring
 *     outside a `$derived` copies the getters' current values once and never
 *     updates — the package's own doc comment says so.
 *   - Keys exist for every dimension LayerCake knows about, including ones this
 *     chart never passes, and those read `undefined`. So a misspelled key is a
 *     silent `undefined` rather than a throw, which is the other half of why
 *     this interface is worth keeping.
 *
 * This is deliberately OUR interface rather than the package's exported
 * `LayerCakeContext` type: it names only the keys these layers touch, with the
 * scale shapes they actually call, so svelte-check keeps failing on a layer
 * that reaches for something outside it. The package's own type widens every
 * scale to `{ (value: any): any, [key: string]: any }`, which would check
 * nothing about the calls below.
 *
 * Keys the layers do not touch are deliberately absent, `yGet` / `y` / `xRange`
 * / `yRange` among them — they were carried in the 10.x version of this
 * interface and used by nothing. Adding one back is how a layer declares it
 * started reading it.
 *
 * Layers reach it as `getLayerCakeContext() as unknown as LayerCakeContext`.
 * The hop through `unknown` is required, not laziness. The package types every
 * scale as the loose `Scale` (`{ (value: any): any, [key: string]: any }`),
 * which DECLARES none of the methods the shapes above require, and an index
 * signature does not satisfy a required named member. So TS refuses the direct
 * cast — verbatim: "Conversion of type 'LayerCakeContext<any, ChartDatum[]>' to
 * type 'LayerCakeContext' may be a mistake because neither type sufficiently
 * overlaps with the other ... Property 'ticks' is missing in type 'Scale' but
 * required in type '{ (value: unknown): number; ticks: ... }'". The package
 * casts its own context through `unknown` for the same reason.
 *
 * What the hop gives up is only that one comparison, against a type that
 * asserts nothing. What this interface exists to check is the layer bodies, and
 * that still holds: a key it does not declare, or a scale method it does not
 * name, is an error at the read. Verified rather than assumed — probing
 * `k.xRange` and `k.yScale.bandwidth()` each fails svelte-check.
 */
export interface LayerCakeContext {
	data: ChartDatum[];
	x: (d: ChartDatum) => string | number;
	xGet: (d: ChartDatum) => number;
	/** Band scale: called for a position, and probed for `bandwidth()` by Bars and AxisX. */
	xScale: {
		(value: unknown): number;
		bandwidth?: () => number;
	};
	/** Linear scale: called for a position, and asked for `ticks()` by AxisY. */
	yScale: {
		(value: unknown): number;
		ticks: (count?: number) => number[];
	};
	width: number;
	height: number;
	padding: { top: number; right: number; bottom: number; left: number };
}
