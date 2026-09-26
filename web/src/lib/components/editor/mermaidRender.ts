// The ONE mermaid loader for the app (TASK-2248 U2). It is shared by the
// editor's NodeViews and the public share page, and moved here from
// Editor.svelte's `<script module>` block.
//
// MODULE SCOPE IS LOAD-BEARING (BUG-3106 review round 1). `mermaid` is a
// singleton: initialize() and render() act on ONE global config. A second
// copy of this bookkeeping anywhere, whether per <Editor> instance or per
// page, produces two bugs at once:
//   - an `appliedMermaidTheme` guarding a global config it does not own
//     skips an initialize() it needed, and renders on the other mode's
//     palette with nothing queued to correct it;
//   - a second `renderQueue` makes overlapping render() calls routine, and
//     the queue exists precisely because mermaid cannot handle concurrent
//     renders.
// The share page used to be the case in point. Rendering there with its own
// loader would have left the editor's tracker believing a theme mermaid no
// longer had.
import { mermaidThemeForMode, type MermaidTheme } from './mermaidTheme';

let mermaidMod: typeof import('mermaid') | null = null;
let renderQueue: Promise<void> = Promise.resolve();
let appliedMermaidTheme: MermaidTheme | null = null;

/** The palette mermaid's global config currently holds, or null before the first render. */
export function mermaidAppliedTheme(): MermaidTheme | null {
	return appliedMermaidTheme;
}

// The OS-preference half of the read. `app.css` renders an ABSENT data-theme
// light when the OS prefers light, so the attribute alone cannot answer.
function prefersLightMode(): boolean {
	if (typeof window === 'undefined' || !window.matchMedia) return false;
	return window.matchMedia('(prefers-color-scheme: light)').matches;
}

/** The palette the app's CURRENT mode calls for (BUG-3106; see mermaidTheme.ts). */
export function currentMermaidTheme(): MermaidTheme {
	if (typeof document === 'undefined') return 'dark';
	return mermaidThemeForMode(document.documentElement, prefersLightMode());
}

async function initMermaid() {
	if (!mermaidMod) {
		mermaidMod = await import('mermaid');
	}
	const theme = currentMermaidTheme();
	if (theme !== appliedMermaidTheme) {
		mermaidMod.default.initialize({
			startOnLoad: false,
			// BUG-3106: follows the app's mode. This was an unconditional
			// 'dark', which drew every diagram on the dark palette in light
			// mode. The TASK-3090 comment below called it "OUR value, not a
			// default" and kept it through the 12 bump for continuity —
			// true of the BUMP, but it was never a deliberate choice to
			// ignore the app's mode, just a value nobody had revisited.
			theme,
			securityLevel: 'strict',
			fontFamily: 'inherit',
			// `layout` is the load-bearing one. Read from the shipped
			// bundles via mermaidAPI.getConfig(), 11.17.2 defaults to
			// layout="dagre" and 12.0.0 to layout="elk", so without this
			// pin every stored flowchart, state and class diagram re-flows
			// on an upgrade nobody asked them about. `look` is a defensive
			// pin, not a fix: both versions already default to "classic",
			// and it is written down so a later default change cannot move
			// our diagrams silently.
			// This is the site default, not a ceiling: a diagram that opts
			// into ELK in its own frontmatter still gets ELK.
			//
			// What the pin does NOT do, measured rather than assumed: it
			// does not make 12 render identically to 11. With dagre pinned,
			// the same flowchart goes from 377.5x623 to 426x737 and the
			// same state diagram from 120.7x412 to 152x412 — same node and
			// edge counts, same font size, same reading order, just drawn
			// larger. The pin preserves the LAYOUT, not the metrics.
			layout: 'dagre',
			look: 'classic',
		});
		appliedMermaidTheme = theme;
	}
	return mermaidMod;
}

/**
 * Render `source` into `target` through the one serialized queue. On a
 * parse failure `onError` decides what the reader sees. The default is the
 * editor's inline "invalid syntax" marker; the share page keeps the code.
 * `onRendered` runs only once the SVG is in `target`.
 */
export function queueMermaidRender(
	source: string,
	target: HTMLElement,
	onError: (target: HTMLElement) => void = markInvalid,
	onRendered?: (target: HTMLElement) => void
): void {
	renderQueue = renderQueue.then(async () => {
		try {
			const m = await initMermaid();
			// The palette THIS render bakes in, read after initMermaid set it
			// and before anything else can run in the serialized queue.
			const renderedTheme = appliedMermaidTheme;
			const id = `mmd-${Math.random().toString(36).slice(2, 10)}`;
			const { svg } = await m.default.render(id, source);
			target.innerHTML = svg;
			// BUG-3112: the print rule keys on this, because print cannot
			// re-render (see `.mermaid-diagram[data-mermaid-theme]`).
			if (renderedTheme) target.dataset.mermaidTheme = renderedTheme;
			// A successful render means the source is now valid — drop any
			// error styling left over from a prior failed render.
			target.classList.remove('mermaid-error');
			onRendered?.(target);
		} catch {
			onError(target);
		}
	});
}

function markInvalid(target: HTMLElement) {
	target.textContent = '⚠ Invalid Mermaid syntax';
	target.classList.add('mermaid-error');
	delete target.dataset.mermaidTheme;
}

/**
 * Chain a diagram-clear through the shared render queue so it executes AFTER
 * any still-pending renders for the same target. Without this, an in-flight
 * queueMermaidRender() could overwrite a synchronous clear with stale SVG.
 */
export function queueMermaidClear(target: HTMLElement): void {
	renderQueue = renderQueue.then(() => {
		target.textContent = '';
		target.classList.remove('mermaid-error');
		delete target.dataset.mermaidTheme;
	});
}
