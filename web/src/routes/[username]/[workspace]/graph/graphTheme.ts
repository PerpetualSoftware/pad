// The graph's WebGL colors, per theme (TASK-2239).
//
// WebGL can't resolve CSS variables, so the graph needs literal colors, and it
// used to have only the dark set. In the light theme its labels were slate-300
// on white (about 1.3:1), the touch glow mixed toward white and vanished, and
// completed items faded toward near-black, so they STOOD OUT instead of
// receding. Each value below plays the same role in both themes, measured
// against that theme's backdrop.

export type GraphThemeName = 'dark' | 'light';

export interface GraphPalette {
	/** The canvas backdrop; terminal nodes fade toward it. */
	backdrop: string;
	/** Node ref labels. */
	label: string;
	/** A touched node mixes toward this: AWAY from the backdrop, so it reads as emphasis. */
	glow: string;
	/** Structural and soft edges, as "r, g, b" for rgba(). */
	edgeRgb: string;
}

export const GRAPH_PALETTES: Record<GraphThemeName, GraphPalette> = {
	// The values the page has always used.
	dark: { backdrop: '#0a0a1a', label: '#cbd5e1', glow: '#ffffff', edgeRgb: '148, 163, 184' },
	// The app's light --bg-primary; slate-700 labels; glow toward slate-900;
	// slate-500 edges, which keep their alphas readable on white.
	light: { backdrop: '#f5f5f9', label: '#334155', glow: '#0f172a', edgeRgb: '100, 116, 139' }
};

/**
 * The theme the page is showing: the stored choice the root layout applied as
 * data-theme, else the system preference (the app's own rule).
 */
export function resolveGraphTheme(): GraphThemeName {
	if (typeof document === 'undefined') return 'dark';
	const attr = document.documentElement.getAttribute('data-theme');
	if (attr === 'light' || attr === 'dark') return attr;
	return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

/** Relative luminance of #rrggbb (WCAG 2). */
export function luminance(hex: string): number {
	const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
	const lin = c.map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
	return 0.2126 * lin[0] + 0.7152 * lin[1] + 0.0722 * lin[2];
}

/** WCAG contrast ratio between two #rrggbb colors. */
export function contrast(a: string, b: string): number {
	const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
	return (hi + 0.05) / (lo + 0.05);
}
