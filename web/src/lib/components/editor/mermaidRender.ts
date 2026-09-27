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

/**
 * EVERY QUEUED JOB IS BOUNDED (BUG-3239). The queue is one promise chain, so
 * a job that never settles used to stall every diagram queued after it, in
 * every editor and on share pages, until reload. A job that runs past its
 * deadline is failed as a `'timeout'`, and the queue moves on. If it settles
 * later, it writes nothing.
 *
 * What this cannot do: mermaid is single-threaded JS, so a render stuck in a
 * SYNCHRONOUS loop blocks the page and no timer fires. Only async stalls are
 * covered. After a timeout the next job may also overlap a render that is
 * merely slow, which is the concurrency the queue exists to prevent, so the
 * bounds sit well above anything measured. Too long only delays the diagrams
 * behind a real stall, while too short fails real diagrams and risks overlap.
 *
 * RENDER, measured on 2026-09-26 in headless Chromium with mermaid 12.0.0 and
 * this file's init config, warm p50 / max over 7-14 runs:
 *   unthrottled:  flowchart 10 nodes 50 / 59 ms, 50 nodes 194 / 213 ms,
 *                 200 nodes 736 / 753 ms (cold 740); sequence 150 msgs 119 / 125 ms
 *   6x CPU throttle (a slow phone): flowchart 50 nodes 1.31 / 1.35 s,
 *                 200 nodes 4.90 / 5.04 s, 500 nodes 12.4 / 12.6 s (cold 12.8)
 * Render time is linear in diagram size and tightly distributed (max within
 * 3% of p50 at every size). 30 s is 2.4x the slowest case measured, a
 * 500-node flowchart on a throttled CPU, and it leaves room for the lazy
 * per-diagram renderer chunk that render() fetches, which those timings
 * (single-bundle mermaid.min.js) do not include.
 */
export const MERMAID_RENDER_DEADLINE_MS = 30_000;

/**
 * IMPORT, bounded separately because it is network-bound, not CPU-bound. The
 * static graph of `import('mermaid')` in the built app is 25 chunks, 718,470
 * bytes raw and 181,945 gzipped (measured on the 6e83faa6 build; :7777 serves
 * them uncompressed). At a Slow-3G-class 50 KB/s that is about 15 s before
 * round trips. 60 s is 4x that. A browser can leave a stalled chunk fetch
 * pending far longer, and a stalled fetch is one SHARED pending module
 * promise, so after one job has timed out waiting on it the jobs behind it
 * fail at once instead of each waiting out another minute (`importStalled`).
 */
export const MERMAID_IMPORT_DEADLINE_MS = 60_000;

/** Why a queued render did not draw: its source failed, or its job ran out of time. */
export type MermaidFailure = 'invalid' | 'timeout';

class MermaidTimeout extends Error {}

/**
 * Append a job to the queue so that its link SETTLES whatever the job does.
 * A throw from a caller's `onError` / `onRendered` used to reject the link,
 * and every later `.then` job was then skipped for good, which is the same
 * stall as a hung render arriving by another route (BUG-3239, codex r1).
 * The error is reported, not swallowed.
 */
function enqueue(job: () => void | Promise<void>): void {
	renderQueue = renderQueue.then(job).catch((e) => {
		console.error('mermaid render queue: a job threw', e);
	});
}

let importPromise: Promise<typeof import('mermaid')> | null = null;
/** Set once a job gave up on a still-pending import; cleared when it settles. */
let importStalled = false;

function withDeadline<T>(p: Promise<T>, ms: number): Promise<T> {
	return new Promise<T>((resolve, reject) => {
		const timer = setTimeout(() => reject(new MermaidTimeout()), ms);
		p.then(
			(v) => {
				clearTimeout(timer);
				resolve(v);
			},
			(e) => {
				clearTimeout(timer);
				reject(e);
			}
		);
	});
}

function loadMermaid(): Promise<typeof import('mermaid')> {
	if (mermaidMod) return Promise.resolve(mermaidMod);
	if (!importPromise) {
		const p = import('mermaid');
		importPromise = p;
		p.then(
			(m) => {
				mermaidMod = m;
				importStalled = false;
			},
			() => {
				// A failed import is retried by the next job, not cached.
				if (importPromise === p) importPromise = null;
				importStalled = false;
			}
		);
	}
	if (importStalled) return Promise.reject(new MermaidTimeout());
	return withDeadline(importPromise, MERMAID_IMPORT_DEADLINE_MS).catch((e) => {
		if (e instanceof MermaidTimeout) importStalled = true;
		throw e;
	});
}

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
	const mod = await loadMermaid();
	const theme = currentMermaidTheme();
	if (theme !== appliedMermaidTheme) {
		mod.default.initialize({
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
	return mod;
}

/**
 * Render `source` into `target` through the one serialized queue. When it
 * cannot draw, `onError` decides what the reader sees, and is told why
 * (`'invalid'` or `'timeout'`). The default is the editor's inline marker;
 * the share page keeps the code. `onRendered` runs only once the SVG is in
 * `target`.
 */
export function queueMermaidRender(
	source: string,
	target: HTMLElement,
	onError: (target: HTMLElement, reason: MermaidFailure) => void = markFailed,
	onRendered?: (target: HTMLElement) => void
): void {
	enqueue(async () => {
		try {
			const m = await initMermaid();
			// The palette THIS render bakes in, read after initMermaid set it
			// and before anything else can run in the serialized queue.
			const renderedTheme = appliedMermaidTheme;
			const id = `mmd-${Math.random().toString(36).slice(2, 10)}`;
			// Bounded, and a render that settles after its deadline writes
			// nothing: by then the queue has moved on, and a newer render or
			// clear may own `target` (BUG-3239).
			const { svg } = await withDeadline(m.default.render(id, source), MERMAID_RENDER_DEADLINE_MS);
			target.innerHTML = svg;
			// BUG-3112: the print rule keys on this, because print cannot
			// re-render (see `.mermaid-diagram[data-mermaid-theme]`).
			if (renderedTheme) target.dataset.mermaidTheme = renderedTheme;
			// A successful render means the source is now valid — drop any
			// error styling left over from a prior failed render.
			target.classList.remove('mermaid-error');
			onRendered?.(target);
		} catch (e) {
			onError(target, e instanceof MermaidTimeout ? 'timeout' : 'invalid');
		}
	});
}

function markFailed(target: HTMLElement, reason: MermaidFailure) {
	target.textContent =
		reason === 'timeout' ? '⚠ Diagram took too long to render' : '⚠ Invalid Mermaid syntax';
	target.classList.add('mermaid-error');
	delete target.dataset.mermaidTheme;
}

/**
 * Chain a diagram-clear through the shared render queue so it executes AFTER
 * any still-pending renders for the same target. Without this, an in-flight
 * queueMermaidRender() could overwrite a synchronous clear with stale SVG.
 */
export function queueMermaidClear(target: HTMLElement): void {
	enqueue(() => {
		target.textContent = '';
		target.classList.remove('mermaid-error');
		delete target.dataset.mermaidTheme;
	});
}
