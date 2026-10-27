<script lang="ts">
	// TASK-4075: the in-pane PDF viewer. Untrusted bytes in, a canvas out.
	//
	// WHAT THIS DOES NOT DO, on purpose, each pinned by a test: no text layer, no
	// annotation layer, and so no `<a>`, `{@html}`, `<iframe>`, `<embed>` or
	// `<object>` built from anything in the PDF; no scripting; no `blob:` URL; no
	// URL handed to pdf.js (it gets a buffer). Page content is drawn to ONE
	// <canvas> and nothing else of the document reaches the DOM.
	//
	// pdf.js is imported on mount, so it is its own lazy chunk and the main entry
	// chunk does not grow. The worker is our own Vite asset, served by the same
	// origin under the baseline CSP (`script-src 'self'`), so no CSP changes.
	import { PDFJS_FALLBACK_DIR } from '$lib/attachments/pdfjsAssets';
	import { pageRenderSize, stepZoom, PDF_MAX_IMAGE_PIXELS } from '$lib/attachments/pdfRenderSize';
	import type { PDFDocumentLoadingTask, PDFDocumentProxy, RenderTask } from 'pdfjs-dist';

	let {
		bytes,
		compact = false,
		label = 'PDF document'
	}: { bytes: Uint8Array; compact?: boolean; label?: string } = $props();

	type Phase = 'loading' | 'ready' | 'error' | 'password';

	// `.raw`: a PDFDocumentProxy has private fields and must never be proxied.
	let doc = $state.raw<PDFDocumentProxy | null>(null);
	let phase = $state<Phase>('loading');
	let numPages = $state(0);
	let pageNo = $state(1);
	let zoom = $state(1);
	let pageInput = $state('1');
	let scrollWidth = $state(0);
	let canvasEl = $state<HTMLCanvasElement>();
	let scrollEl = $state<HTMLDivElement>();
	let cssSize = $state({ width: 0, height: 0 });
	let renderFailed = $state(false);

	/** Previous page by `delta`, clamped. Exposed so the Lightbox's PageUp/PageDown can drive it. */
	export function pageBy(delta: number): void {
		goTo(pageNo + delta);
	}
	/** The scroll region, for the Lightbox's focus and key rules. */
	export function scroller(): HTMLElement | undefined {
		return scrollEl;
	}

	function goTo(n: number): void {
		if (!doc || !Number.isFinite(n)) return;
		pageNo = Math.min(numPages, Math.max(1, Math.trunc(n)));
		pageInput = String(pageNo);
		if (scrollEl) {
			scrollEl.scrollTop = 0;
			scrollEl.scrollLeft = 0;
		}
	}

	// OPEN. Re-runs when `bytes` changes; the cleanup destroys the previous
	// loading task, which also tears down its worker and document.
	$effect(() => {
		const source = bytes;
		let cancelled = false;
		let task: PDFDocumentLoadingTask | null = null;
		phase = 'loading';
		doc = null;
		numPages = 0;
		pageNo = 1;
		pageInput = '1';
		zoom = 1;
		(async () => {
			try {
				const [pdfjs, worker] = await Promise.all([
					import('pdfjs-dist'),
					import('pdfjs-dist/build/pdf.worker.min.mjs?url')
				]);
				if (cancelled) return;
				pdfjs.GlobalWorkerOptions.workerSrc = worker.default;
				task = pdfjs.getDocument({
					// A COPY: getDocument DETACHES the buffer it is given (byteLength reads
					// 0 afterwards), and the loader's buffer must stay valid for a
					// remount or a Retry.
					data: source.slice(),
					// No wasm is ever compiled from PDF-derived input: the CSP has no
					// 'wasm-unsafe-eval' and none is added. JPX/JBIG2 take their JS
					// fallbacks, served from our own origin (see pdfjsAssets.ts).
					useWasm: false,
					// Decompression-bomb bound, NOT a memory budget: pdf.js leaves decoded
					// image size unbounded by default (maxImageSize -1). Counted in
					// pixels (w*h); an image over it is skipped with a warning and the
					// rest of the page still draws. 64 MP keeps a 600 dpi US-letter scan
					// (5100 x 6600 = 33.7 MP) that 16 MP would blank; one image at the
					// cap costs ~256 MiB to decode at 4 B/px.
					maxImageSize: PDF_MAX_IMAGE_PIXELS,
					wasmUrl: `${location.origin}${PDFJS_FALLBACK_DIR}`
				});
				const opened = await task.promise;
				if (cancelled) {
					void task.destroy();
					return;
				}
				doc = opened;
				numPages = opened.numPages;
				phase = 'ready';
			} catch (err) {
				if (cancelled) return;
				phase = (err as { name?: string })?.name === 'PasswordException' ? 'password' : 'error';
			}
		})();
		return () => {
			cancelled = true;
			void task?.destroy();
			doc = null;
		};
	});

	// One render at a time on the one canvas: pdf.js refuses a second render()
	// into a canvas whose previous render has not finished unwinding.
	let lastRender: Promise<unknown> = Promise.resolve();

	// RENDER the current page. Re-runs on page, zoom, width or document change.
	$effect(() => {
		const d = doc;
		const n = pageNo;
		const z = zoom;
		const w = scrollWidth;
		const canvas = canvasEl;
		if (!d || !canvas || w <= 0) return;
		let cancelled = false;
		let task: RenderTask | null = null;
		const run = lastRender.then(async () => {
			if (cancelled) return;
			renderFailed = false;
			const page = await d.getPage(n);
			try {
				if (cancelled) return;
				const base = page.getViewport({ scale: 1 });
				const size = pageRenderSize(base.width, base.height, w * z, window.devicePixelRatio || 1);
				if (!size) throw new Error('page has no drawable size');
				const viewport = page.getViewport({ scale: size.pixelWidth / base.width });
				canvas.width = size.pixelWidth;
				canvas.height = size.pixelHeight;
				cssSize = { width: size.cssWidth, height: size.cssHeight };
				task = page.render({ canvas, viewport });
				await task.promise;
			} finally {
				page.cleanup();
			}
		});
		lastRender = run.catch(() => {});
		run
			.catch((err) => {
				if (cancelled || (err as { name?: string })?.name === 'RenderingCancelledException') return;
				renderFailed = true;
			})
			;
		return () => {
			cancelled = true;
			task?.cancel();
		};
	});

	function commitPageInput(): void {
		const n = Number.parseInt(pageInput, 10);
		if (Number.isNaN(n)) pageInput = String(pageNo);
		else goTo(n);
	}
</script>

<div class="attachment-pdf-preview" class:compact>
	{#if phase === 'loading'}
		<p class="attachment-pdf-status" role="status">Loading PDF…</p>
	{:else if phase === 'password'}
		<p class="attachment-pdf-status" role="status">
			This PDF is password-protected and can't be previewed. Use Download to open it.
		</p>
	{:else if phase === 'error'}
		<p class="attachment-pdf-status" role="alert">
			This PDF couldn't be displayed. Use Download to open it.
		</p>
	{:else}
		<div class="attachment-pdf-controls" role="group" aria-label="PDF controls">
			<button type="button" aria-label="Previous page" disabled={pageNo <= 1} onclick={() => goTo(pageNo - 1)}>‹</button>
			<input
				class="attachment-pdf-page-input"
				type="text"
				inputmode="numeric"
				aria-label="Page number"
				bind:value={pageInput}
				onkeydown={(e) => {
					// Typing a page number must not page the gallery behind.
					if (e.key === 'Enter') {
						e.preventDefault();
						commitPageInput();
					}
				}}
				onblur={commitPageInput}
			/>
			<span class="attachment-pdf-count">of {numPages}</span>
			<button type="button" aria-label="Next page" disabled={pageNo >= numPages} onclick={() => goTo(pageNo + 1)}>›</button>
			<span class="attachment-pdf-sep" aria-hidden="true"></span>
			<button type="button" aria-label="Zoom out" disabled={zoom <= 0.5} onclick={() => (zoom = stepZoom(zoom, -1))}>−</button>
			<span class="attachment-pdf-zoom">{Math.round(zoom * 100)}%</span>
			<button type="button" aria-label="Zoom in" disabled={zoom >= 3} onclick={() => (zoom = stepZoom(zoom, 1))}>+</button>
		</div>
		<p class="attachment-pdf-sr" role="status">Page {pageNo} of {numPages}</p>
		<!-- svelte-ignore a11y_no_noninteractive_tabindex — a scroll region needs a tab stop for keyboard scrolling. -->
		<div
			class="attachment-pdf-scroll"
			tabindex="0"
			role="region"
			aria-label={label}
			bind:this={scrollEl}
			bind:clientWidth={scrollWidth}
		>
			<canvas
				bind:this={canvasEl}
				class="attachment-pdf-canvas"
				aria-label={`${label}, page ${pageNo}`}
				style:width={cssSize.width ? `${cssSize.width}px` : undefined}
				style:height={cssSize.height ? `${cssSize.height}px` : undefined}
			></canvas>
		</div>
		{#if renderFailed}
			<p class="attachment-pdf-status" role="alert">This page couldn't be drawn.</p>
		{/if}
	{/if}
</div>

<style>
	.attachment-pdf-preview {
		display: flex;
		flex-direction: column;
		width: 100%;
		height: 100%;
		min-height: 0;
		min-width: 0;
		background: var(--bg-secondary, #f2f2f2);
		color: var(--text-primary, #111);
	}
	.attachment-pdf-controls {
		flex: none;
		display: flex;
		align-items: center;
		justify-content: center;
		gap: 0.4rem;
		padding: 0.4rem 0.5rem;
		background: var(--bg-primary, #fff);
		border-bottom: 1px solid var(--border-color, #ddd);
	}
	.compact .attachment-pdf-controls {
		padding: 0.25rem 0.35rem;
		gap: 0.25rem;
	}
	.attachment-pdf-controls button {
		min-width: 2.25rem;
		min-height: 2.25rem;
		font: inherit;
		cursor: pointer;
	}
	.attachment-pdf-controls button:disabled {
		cursor: default;
		opacity: 0.4;
	}
	.attachment-pdf-page-input {
		width: 3rem;
		min-height: 2.25rem;
		text-align: center;
		font: inherit;
	}
	.attachment-pdf-sep {
		width: 1px;
		align-self: stretch;
		background: var(--border-color, #ddd);
		margin: 0 0.25rem;
	}
	.attachment-pdf-zoom {
		min-width: 3ch;
		text-align: center;
		font-variant-numeric: tabular-nums;
	}
	.attachment-pdf-sr {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		padding: 0;
		overflow: hidden;
		clip-path: inset(50%);
		white-space: nowrap;
	}
	.attachment-pdf-scroll {
		flex: 1;
		min-height: 0;
		overflow: auto;
		overscroll-behavior: contain;
		scrollbar-gutter: stable;
		touch-action: pan-x pan-y;
	}
	.attachment-pdf-canvas {
		display: block;
		margin: 0 auto;
		background: #fff;
		/* The box is sized by inline style from the render; this is the pre-render floor. */
		max-width: none;
	}
	.attachment-pdf-status {
		margin: auto;
		padding: 1rem;
		text-align: center;
		max-width: 40ch;
		line-height: 1.5;
	}
</style>
