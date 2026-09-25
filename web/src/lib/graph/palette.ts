// Shared collection-color palette for the graph views (PLAN-1730 3D
// workspace graph + PLAN-1780 per-item neighborhood graph). Both views
// color nodes by collection; keeping the palette here means the 3D and
// 2D renderers agree on which collection gets which hex.

/** Ordered hex palette; collections are assigned colors round-robin. */
export const GRAPH_PALETTE = [
	'#6366f1', // indigo
	'#06b6d4', // cyan
	'#f59e0b', // amber
	'#10b981', // emerald
	'#f43f5e', // rose
	'#8b5cf6', // violet
	'#84cc16', // lime
	'#0ea5e9' // sky
] as const;

/**
 * Create a stable collection→color assigner. Colors are handed out in
 * palette order on first sight of each collection slug, so the legend and
 * the rendered nodes always agree within one payload. Build a fresh
 * assigner per payload (or per component) to keep assignment deterministic.
 */
export function createCollectionColorMap() {
	// NULL-PROTOTYPE (BUG-3054). The key is a collection slug, which a user
	// picks: a collection named "Constructor" has the slug `constructor`, and a
	// plain object answered `colors['constructor']` with Object's constructor
	// function, so that collection was "coloured" by a function. Not `$state`
	// anywhere, so the null prototype costs nothing; Object.entries/keys read it
	// the same.
	const colors: Record<string, string> = Object.create(null);
	function colorForCollection(slug: string): string {
		if (!colors[slug]) {
			const idx = Object.keys(colors).length % GRAPH_PALETTE.length;
			colors[slug] = GRAPH_PALETTE[idx];
		}
		return colors[slug];
	}
	return { colors, colorForCollection };
}
