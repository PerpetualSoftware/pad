import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import source from './AttachmentPdfPreview.svelte?raw';
import AttachmentPdfPreview from './AttachmentPdfPreview.svelte';
import { PDF_MAX_PIXELS } from '$lib/attachments/pdfRenderSize';

// TASK-4075 U4/U5. jsdom cannot run pdf.js, so it is mocked at the module
// boundary, but the mock keeps the one behaviour the component's correctness
// depends on: getDocument DETACHES the buffer it is handed (measured on the real
// library: byteLength reads 0 afterwards).
const pdf = vi.hoisted(() => {
	const state = {
		tasks: [] as { destroyed: boolean; params: Record<string, unknown> }[],
		numPages: 3,
		renders: [] as { canvas: HTMLCanvasElement; scale: number; cancelled: boolean }[],
		cleanups: 0,
		fail: null as null | { name: string },
		pageSize: { width: 612, height: 792 },
		hang: false
	};
	const getDocument = (params: { data: Uint8Array }) => {
		const task = { destroyed: false, params: params as unknown as Record<string, unknown> };
		state.tasks.push(task);
		// Detach, as the real one does. structuredClone with transfer detaches the source.
		structuredClone(params.data.buffer, { transfer: [params.data.buffer] });
		const promise = state.fail
			? Promise.reject(Object.assign(new Error('x'), state.fail))
			: Promise.resolve({
					numPages: state.numPages,
					getPage: async () => {
						if (state.hang) return new Promise<never>(() => {});
						return {
						getViewport: ({ scale }: { scale: number }) => ({
							width: state.pageSize.width * scale,
							height: state.pageSize.height * scale
						}),
						render: ({ canvas, viewport }: { canvas: HTMLCanvasElement; viewport: { width: number } }) => {
							const rec = { canvas, scale: viewport.width / state.pageSize.width, cancelled: false };
							state.renders.push(rec);
							return { promise: Promise.resolve(), cancel: () => (rec.cancelled = true) };
						},
						cleanup: () => {
							state.cleanups++;
						}
						};
					}
				});
		return {
			promise,
			destroy: async () => {
				task.destroyed = true;
			}
		};
	};
	return { state, getDocument, GlobalWorkerOptions: {} as { workerSrc?: string } };
});
vi.mock('pdfjs-dist', () => ({ getDocument: pdf.getDocument, GlobalWorkerOptions: pdf.GlobalWorkerOptions }));
vi.mock('pdfjs-dist/build/pdf.worker.min.mjs?url', () => ({ default: '/_app/immutable/assets/pdf.worker.min-HASH.mjs' }));

let component: ReturnType<typeof mount> | undefined;
const props = $state<{ bytes: Uint8Array; compact?: boolean; label?: string }>({ bytes: new Uint8Array(0) });

const settle = async () => {
	for (let i = 0; i < 12; i++) {
		await Promise.resolve();
		await new Promise((r) => setTimeout(r, 0));
		flushSync();
	}
};

function pdfBytes(n = 64): Uint8Array {
	return new Uint8Array(n).fill(0x25);
}

async function open(bytes = pdfBytes(), extra: { compact?: boolean } = {}) {
	props.bytes = bytes;
	props.compact = extra.compact ?? false;
	component = mount(AttachmentPdfPreview, { target: document.body, props });
	flushSync();
	await settle();
}

const q = <T extends Element = HTMLElement>(sel: string) => document.querySelector<T>(sel)!;
const button = (label: string) => q<HTMLButtonElement>(`button[aria-label="${label}"]`);
const status = () => document.querySelector('.attachment-pdf-sr')?.textContent?.trim();

beforeEach(() => {
	pdf.state.tasks.length = 0;
	pdf.state.renders.length = 0;
	pdf.state.cleanups = 0;
	pdf.state.numPages = 3;
	pdf.state.fail = null;
	pdf.state.pageSize = { width: 612, height: 792 };
	pdf.state.hang = false;
	// jsdom has no layout: give the scroll region a width so the render effect runs.
	Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 800 });
});

afterEach(async () => {
	if (component) await unmount(component);
	component = undefined;
	document.body.replaceChildren();
	// @ts-expect-error restore jsdom's own accessor
	delete HTMLElement.prototype.clientWidth;
	vi.unstubAllGlobals();
});

describe('AttachmentPdfPreview — pdf.js options (U4)', () => {
	it('opens with { data, useWasm: false, wasmUrl } and never a url or scripting', async () => {
		await open();
		expect(pdf.state.tasks).toHaveLength(1);
		const p = pdf.state.tasks[0].params;
		expect(Object.keys(p).sort()).toEqual(['data', 'maxImageSize', 'useWasm', 'wasmUrl']);
		expect(p.maxImageSize).toBe(64 * 1024 * 1024);
		expect(p.useWasm).toBe(false);
		expect(p.url).toBeUndefined();
		expect(p.enableScripting).toBeUndefined();
		// Absolute, own-origin, fixed path ending in a slash (the worker concatenates the file name).
		expect(p.wasmUrl).toBe(`${location.origin}/_app/pdfjs/`);
		expect(pdf.GlobalWorkerOptions.workerSrc).toBe('/_app/immutable/assets/pdf.worker.min-HASH.mjs');
	});

	it("hands pdf.js a COPY: the loader's buffer is still full-length after the open", async () => {
		const bytes = pdfBytes(64);
		await open(bytes);
		expect(pdf.state.tasks).toHaveLength(1);
		expect(bytes.byteLength).toBe(64);
		expect(bytes[0]).toBe(0x25);
	});
});

describe('AttachmentPdfPreview — paging (U4)', () => {
	it('shows Page N of M and walks forward and back, clamping at both ends', async () => {
		await open();
		expect(status()).toBe('Page 1 of 3');
		expect(button('Previous page').disabled).toBe(true);
		button('Next page').click();
		flushSync();
		expect(status()).toBe('Page 2 of 3');
		button('Next page').click();
		flushSync();
		expect(status()).toBe('Page 3 of 3');
		expect(button('Next page').disabled).toBe(true);
		button('Next page').click();
		flushSync();
		expect(status()).toBe('Page 3 of 3');
		button('Previous page').click();
		button('Previous page').click();
		flushSync();
		expect(status()).toBe('Page 1 of 3');
		button('Previous page').click();
		flushSync();
		expect(status()).toBe('Page 1 of 3');
	});

	it('jumps with the page-number box and clamps an out-of-range or junk entry', async () => {
		await open();
		const input = q<HTMLInputElement>('.attachment-pdf-page-input');
		const commit = (v: string) => {
			input.value = v;
			input.dispatchEvent(new Event('input', { bubbles: true }));
			input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));
			flushSync();
		};
		commit('2');
		expect(status()).toBe('Page 2 of 3');
		commit('99');
		expect(status()).toBe('Page 3 of 3');
		expect(input.value).toBe('3');
		commit('0');
		expect(status()).toBe('Page 1 of 3');
		commit('abc');
		expect(status()).toBe('Page 1 of 3');
		expect(input.value).toBe('1');
	});

	it('pageBy() pages the document, clamped (the Lightbox PageUp/PageDown path)', async () => {
		const inst = mount(AttachmentPdfPreview, { target: document.body, props: { bytes: pdfBytes() } });
		flushSync();
		await settle();
		const api = inst as unknown as { pageBy(d: number): void };
		api.pageBy(1);
		flushSync();
		expect(status()).toBe('Page 2 of 3');
		api.pageBy(-5);
		flushSync();
		expect(status()).toBe('Page 1 of 3');
		await unmount(inst);
	});
});

describe('AttachmentPdfPreview — render caps (U4, Q4)', () => {
	it('renders the current page to the canvas and releases it with page.cleanup()', async () => {
		await open();
		expect(pdf.state.renders.length).toBeGreaterThanOrEqual(1);
		expect(pdf.state.renders[0].canvas).toBe(q('canvas'));
		expect(pdf.state.cleanups).toBeGreaterThanOrEqual(1);
		const before = pdf.state.cleanups;
		button('Next page').click();
		flushSync();
		await settle();
		expect(pdf.state.cleanups).toBeGreaterThan(before);
	});

	it('caps the backing store at DPR 2 and 16 MP even when zoomed in on a DPR-3 screen', async () => {
		vi.stubGlobal('devicePixelRatio', 3);
		await open();
		const canvas = q<HTMLCanvasElement>('canvas');
		// Fit width 800 css px at DPR 3 would be 2400 px wide; the DPR cap makes it 1600.
		expect(canvas.width).toBe(1600);
		for (let i = 0; i < 6; i++) button('Zoom in').click();
		flushSync();
		await settle();
		expect(canvas.width * canvas.height).toBeLessThanOrEqual(PDF_MAX_PIXELS);
		expect(canvas.width * canvas.height).toBeGreaterThan(PDF_MAX_PIXELS * 0.9);
	});

	it('a page with no honest size shows "couldn\'t be drawn" and never sizes the canvas', async () => {
		pdf.state.pageSize = { width: 0, height: 792 };
		await open();
		expect(document.body.textContent).toContain("This page couldn't be drawn.");
		expect(pdf.state.renders).toHaveLength(0);
	});
});

describe('AttachmentPdfPreview — a failed page leaves no stale pixels (r5b-L1, L2)', () => {
	it('L1: a page with no honest size clears the previous page\'s canvas and box before the alert', async () => {
		await open();
		const canvas = q<HTMLCanvasElement>('canvas');
		expect(canvas.width).toBe(800);
		expect(canvas.style.width).toBe('800px');
		pdf.state.pageSize = { width: 0, height: 792 };
		button('Next page').click();
		flushSync();
		await settle();
		expect(document.body.textContent).toContain("This page couldn't be drawn.");
		expect(canvas.width).toBe(0);
		expect(canvas.height).toBe(0);
		expect(canvas.style.width).toBe('');
		expect(canvas.style.height).toBe('');
	});

	it('L2: opening new bytes resets the canvas box (the new document\'s canvas does not inherit the old size)', async () => {
		await open(pdfBytes(10));
		expect(q<HTMLCanvasElement>('canvas').style.width).toBe('800px');
		// The new document's first page never resolves, so nothing but the open effect can reset the box.
		pdf.state.hang = true;
		props.bytes = pdfBytes(20);
		flushSync();
		await settle();
		expect(q<HTMLCanvasElement>('canvas').style.width).toBe('');
	});

	it("opening new bytes clears the old document's \"couldn't be drawn\" even when the new one never renders", async () => {
		pdf.state.pageSize = { width: 0, height: 792 };
		await open(pdfBytes(10));
		expect(document.body.textContent).toContain("This page couldn't be drawn.");
		// The new document's scroll region measures 0 wide, so the render effect
		// returns before its own reset: only the open effect can clear the alert.
		pdf.state.pageSize = { width: 612, height: 792 };
		Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 0 });
		props.bytes = pdfBytes(20);
		flushSync();
		await settle();
		expect(status()).toBe('Page 1 of 3');
		expect(document.body.textContent).not.toContain("This page couldn't be drawn.");
	});
});

describe('AttachmentPdfPreview — lifecycle (U4)', () => {
	it('destroys the loading task on unmount', async () => {
		await open();
		expect(pdf.state.tasks[0].destroyed).toBe(false);
		await unmount(component!);
		component = undefined;
		expect(pdf.state.tasks[0].destroyed).toBe(true);
	});

	it('destroys the previous document when the bytes change and opens the new one', async () => {
		await open(pdfBytes(10));
		props.bytes = pdfBytes(20);
		flushSync();
		await settle();
		expect(pdf.state.tasks).toHaveLength(2);
		expect(pdf.state.tasks[0].destroyed).toBe(true);
		expect(pdf.state.tasks[1].destroyed).toBe(false);
		expect(status()).toBe('Page 1 of 3');
	});

	it('shows a terminal message for a password-protected PDF and another for a corrupt one', async () => {
		pdf.state.fail = { name: 'PasswordException' };
		await open();
		expect(document.body.textContent).toContain('password-protected');
		expect(document.querySelector('canvas')).toBeNull();
		await unmount(component!);
		component = undefined;
		document.body.replaceChildren();
		pdf.state.fail = { name: 'InvalidPDFException' };
		await open();
		expect(document.body.textContent).toContain("couldn't be displayed");
	});
});

describe('AttachmentPdfPreview — canvas only (U5)', () => {
	it('renders no PDF-derived markup: no anchors, frames, embeds or objects', async () => {
		await open();
		for (const tag of ['a', 'iframe', 'embed', 'object', 'img', 'svg', 'form', 'script']) {
			expect(document.querySelector(tag), tag).toBeNull();
		}
		expect(document.querySelectorAll('canvas')).toHaveLength(1);
	});

	// The component's own source, as TASK-4074's sanitizer proof does for its
	// pipeline: the layers and sinks this slice excludes must not even be named.
	it.each([
		['AnnotationLayer', /AnnotationLayer/],
		['TextLayer', /TextLayer/],
		['PDFViewer / EventBus', /PDFViewer|EventBus/],
		['{@html}', /\{@html/],
		['an <a> element', /<a[\s>]/],
		['<iframe>', /<iframe/i],
		['<embed>', /<embed/i],
		['<object>', /<object/i],
		['enableScripting', /enableScripting/],
		['the pdf.js sandbox / quickjs', /pdf\.sandbox|quickjs/i],
		['a blob: URL', /blob:|createObjectURL/],
		['a url: parameter to getDocument', /getDocument\(\{[^}]*\burl\s*:/s],
		['an eval', /\beval\s*\(|new Function/]
	])('the source never uses %s', (_name, re) => {
		// Strip comments: the header documents what is excluded, by name.
		const code = source.replace(/<!--[\s\S]*?-->/g, '').replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
		expect(code).not.toMatch(re);
	});
});
