import { test, expect } from './fixtures';
import type { Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { browserLogin, seedDoc } from './lib/collab-helpers';
import {
	CdpTouch,
	DESKTOP,
	TILE,
	VIEWER,
	VIEWER_COUNTER,
	VIEWER_FALLBACK,
	VIEWER_SHEET,
	itemUrl,
	uploadAttachment,
	viewerDialog,
	viewerNext
} from './lib/attachment-viewer';
import {
	buildHostilePdf,
	buildMultiPagePdf,
	buildPaddedPdf,
	buildPostScriptFunctionPdf
} from './lib/pdf-fixtures';

/**
 * THE IN-PANE PDF VIEWER, IN A REAL BROWSER AGAINST THE BUILT BINARY
 * (TASK-4075, CONVE-34).
 *
 * jsdom has no CSP, no worker and no canvas, and the failure this slice is most
 * likely to have is production-only: pdf.js running under Pad's real headers.
 * Everything here is therefore read from the real server's real responses.
 *
 * THE INSTRUMENT, AND WHY IT IS BUILT THE WAY IT IS. A `securitypolicyviolation`
 * listener on the DOCUMENT cannot see a Worker's violations: measured, a module
 * worker's blocked `WebAssembly` compile and `new Function` are reported to the
 * WORKER's scope only, and the document listener saw nothing. So this spec puts
 * a listener inside the worker itself (the pdf.js worker is our own asset, and
 * the route below prepends one line to it) and reads it back through the worker's
 * console. The control leg proves that instrument can fail: the same page with
 * wasm forced back on must record the `wasm-eval` violation. A listener that
 * cannot see the failure is not a test.
 */

const HERE = dirname(fileURLToPath(import.meta.url));
const PDF_FIXTURES = resolve(HERE, 'fixtures', 'pdf');

const ARM = `${VIEWER} .lightbox-pdf`;
const CANVAS = `${ARM} canvas`;
const SCROLL = `${ARM} .attachment-pdf-scroll`;
const STATUS = `${ARM} .attachment-pdf-sr`;

/** The worker-scope listener, prepended to the pdf.js worker module. */
const WORKER_PROBE =
	"self.addEventListener('securitypolicyviolation', (e) => console.error('PADCSP ' + e.violatedDirective + ' ' + e.blockedURI));\n";

interface Instrument {
	/** Every CSP violation seen, document or worker, as `where:directive blocked`. */
	violations: string[];
	/** Console errors and page errors that are not CSP violations. */
	errors: string[];
	workerUrls: string[];
	/** Dialogs, popups and top-level navigations: none may happen. */
	dialogs: string[];
	popups: string[];
	navigations: string[];
	requests: string[];
}

async function instrument(page: Page, opts: { forceWasm?: boolean } = {}): Promise<Instrument & { wasmRewrites: () => number }> {
	const inst: Instrument = { violations: [], errors: [], workerUrls: [], dialogs: [], popups: [], navigations: [], requests: [] };
	let rewrites = 0;
	await page.addInitScript(() => {
		(window as unknown as { __csp: string[] }).__csp = [];
		document.addEventListener('securitypolicyviolation', (e) => {
			(window as unknown as { __csp: string[] }).__csp.push(`doc:${e.violatedDirective} ${e.blockedURI}`);
		});
	});
	// The worker-scope listener. Fetch the real asset (under the real CSP headers)
	// and prepend the probe: nothing else about the response changes.
	await page.route(/pdf\.worker\.min.*\.mjs/, async (route) => {
		const response = await route.fetch();
		await route.fulfill({ response, body: WORKER_PROBE + (await response.text()) });
	});
	if (opts.forceWasm) {
		// CONTROL: flip the one option the viewer sets, in the emitted chunk.
		await page.route(/\/_app\/immutable\/.*\.js$/, async (route) => {
			const response = await route.fetch();
			const body = await response.text();
			if (!/useWasm:\s*!1/.test(body)) return route.fulfill({ response, body });
			rewrites++;
			await route.fulfill({ response, body: body.replace(/useWasm:\s*!1/g, 'useWasm:!0') });
		});
	}
	page.on('worker', (w) => {
		inst.workerUrls.push(w.url());
		w.on('console', (m) => {
			const t = m.text();
			if (t.startsWith('PADCSP ')) inst.violations.push(`worker:${t.slice(7)}`);
			else if (m.type() === 'error') inst.errors.push(`worker console: ${t}`);
		});
	});
	page.on('console', (m) => {
		if (m.type() !== 'error') return;
		const t = m.text();
		if (/Content Security Policy|Refused to/i.test(t)) inst.violations.push(`console:${t.slice(0, 160)}`);
		else inst.errors.push(`console: ${t.slice(0, 160)}`);
	});
	page.on('pageerror', (e) => inst.errors.push(`pageerror: ${String(e).slice(0, 160)}`));
	page.on('dialog', async (d) => {
		inst.dialogs.push(`${d.type()}: ${d.message()}`);
		await d.dismiss();
	});
	page.context().on('page', (p) => inst.popups.push(p.url()));
	page.on('popup', (p) => inst.popups.push(p.url()));
	page.on('framenavigated', (f) => {
		if (f === page.mainFrame()) inst.navigations.push(f.url());
	});
	page.on('request', (r) => inst.requests.push(`${r.method()} ${r.url()}`));
	return Object.assign(inst, { wasmRewrites: () => rewrites });
}

/** The document-level violations, plus the instrument's worker-scope ones. */
async function allViolations(page: Page, inst: Instrument): Promise<string[]> {
	const doc = await page.evaluate(() => (window as unknown as { __csp?: string[] }).__csp ?? []);
	return [...doc, ...inst.violations];
}

interface Paint {
	w: number;
	h: number;
	/** Count of pixels that are not near-white. */
	ink: number;
	/** Mean red of the inked pixels (0 when none): tells one page's fill from another's. */
	meanRed: number;
}

function paint(page: Page, selector = CANVAS): Promise<Paint> {
	return page.evaluate((sel) => {
		const c = document.querySelector<HTMLCanvasElement>(sel);
		if (!c) return { w: 0, h: 0, ink: 0, meanRed: 0 };
		const { data } = c.getContext('2d')!.getImageData(0, 0, c.width, c.height);
		let ink = 0;
		let red = 0;
		for (let i = 0; i < data.length; i += 4) {
			if (data[i] < 245 || data[i + 1] < 245 || data[i + 2] < 245) {
				ink++;
				red += data[i];
			}
		}
		return { w: c.width, h: c.height, ink, meanRed: ink ? red / ink : 0 };
	}, selector);
}

/** Wait for the current page to be painted, not merely mounted. */
async function painted(page: Page, min = 500): Promise<Paint> {
	await expect.poll(async () => (await paint(page)).ink, { timeout: 15_000 }).toBeGreaterThan(min);
	return paint(page);
}

async function openPdf(
	page: Page,
	fixture: Parameters<typeof seedDoc>[0],
	request: Parameters<typeof seedDoc>[1],
	title: string,
	files: { name: string; bytes: Buffer }[]
): Promise<{ ids: string[] }> {
	await browserLogin(page);
	const doc = await seedDoc(fixture, request, title);
	const ids: string[] = [];
	for (const f of files) ids.push(await uploadAttachment(fixture, request, doc.id, f.name, 'application/pdf', f.bytes));
	await page.goto(itemUrl(fixture, doc.slug));
	await expect(page.locator(TILE)).toHaveCount(files.length);
	await page.locator(`${TILE}[aria-label*="${files[0].name}"]`).click();
	await expect(page.locator(ARM)).toBeVisible();
	return { ids };
}

test.describe('attachment viewer — in-pane PDF (TASK-4075), desktop', () => {
	test.describe.configure({ timeout: 90_000 });
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'desktop-chromium', 'desktop legs only');
		await page.setViewportSize(DESKTOP);
	});

	test('E1: a real multi-page PDF paints, pages, and runs under the real CSP with NO violation in the document OR the worker', async ({ page, fixture, request }) => {
		const inst = await instrument(page);
		const { ids } = await openPdf(page, fixture, request, 'PDF E1', [{ name: 'report.pdf', bytes: buildMultiPagePdf(3) }]);

		// THE SERVER'S OWN WORD for the type, not the fixture's declared one.
		const head = await request.head(`/api/v1/workspaces/${fixture.workspaceSlug}/attachments/${ids[0]}`, {
			headers: { Authorization: `Bearer ${fixture.apiToken}` }
		});
		expect(head.headers()['content-type']).toContain('application/pdf');

		await expect(page.locator(STATUS)).toHaveText('Page 1 of 3');
		const first = await painted(page);
		await expect(page.locator(VIEWER_FALLBACK)).toHaveCount(0);

		// Next: page 2 is a different fill, so a repaint is distinguishable from a stale canvas.
		await page.getByRole('button', { name: 'Next page' }).click();
		await expect(page.locator(STATUS)).toHaveText('Page 2 of 3');
		await expect.poll(async () => (await paint(page)).meanRed, { timeout: 15_000 }).not.toBeCloseTo(first.meanRed, 0);
		// PageDown / PageUp page the document; Left/Right stay gallery paging (one entry: inert).
		await page.keyboard.press('PageDown');
		await expect(page.locator(STATUS)).toHaveText('Page 3 of 3');
		await page.keyboard.press('PageUp');
		await expect(page.locator(STATUS)).toHaveText('Page 2 of 3');
		await page.getByRole('button', { name: 'Previous page' }).click();
		await expect(page.locator(STATUS)).toHaveText('Page 1 of 3');
		await expect.poll(async () => (await paint(page)).meanRed, { timeout: 15_000 }).toBeCloseTo(first.meanRed, 0);

		// THE WORKER LOADED, for real. pdf.js falls back to running its worker on the
		// main thread when a Worker cannot start, which would pass every pixel check
		// above while exercising none of the worker CSP path.
		expect(inst.workerUrls.some((u) => /pdf\.worker\.min.*\.mjs/.test(u)), `workers: ${inst.workerUrls}`).toBe(true);

		expect(await allViolations(page, inst), 'CSP violations (document + worker scope)').toEqual([]);
		expect(inst.errors, 'console / page errors').toEqual([]);
		// Never navigated, framed or blob'd.
		await expect(page.locator(`${ARM} :is(iframe, embed, object, a)`)).toHaveCount(0);
		expect(inst.requests.filter((r) => r.includes('blob:'))).toEqual([]);
	});

	test('E1 control: the instrument DOES see a worker-scope violation when wasm is forced back on', async ({ page, fixture, request }) => {
		// A PDF with a PostScript calculator function: with wasm on, pdf.js compiles it
		// to a generated wasm module, which the CSP (no 'wasm-unsafe-eval') refuses.
		const inst = await instrument(page, { forceWasm: true });
		await openPdf(page, fixture, request, 'PDF E1 control', [{ name: 'ps.pdf', bytes: buildPostScriptFunctionPdf() }]);
		await expect.poll(() => inst.violations.length + 0, { timeout: 15_000 }).toBeGreaterThan(0);
		// The probe is not vacuous: the rewrite really happened...
		expect(inst.wasmRewrites(), 'the useWasm option was rewritten in exactly the chunk that sets it').toBeGreaterThan(0);
		// ...and what it saw is a WORKER-scope wasm refusal, which the document listener cannot see.
		expect(inst.violations.join('\n')).toMatch(/worker:script-src wasm-eval/);
		expect(await page.evaluate(() => (window as unknown as { __csp: string[] }).__csp)).toEqual([]);
	});

	test('E1 interpreter: the same PostScript-function PDF paints with wasm OFF, as shipped, and no violation', async ({ page, fixture, request }) => {
		const inst = await instrument(page);
		await openPdf(page, fixture, request, 'PDF E1 interpreter', [{ name: 'ps.pdf', bytes: buildPostScriptFunctionPdf() }]);
		await painted(page);
		expect(await allViolations(page, inst)).toEqual([]);
		expect(inst.errors).toEqual([]);
	});

	test('E2: a PDF with a JavaScript OpenAction and link annotations causes no dialog, no navigation, no popup, no request and no anchor', async ({ page, fixture, request }) => {
		const inst = await instrument(page);
		await openPdf(page, fixture, request, 'PDF E2', [{ name: 'hostile.pdf', bytes: buildHostilePdf() }]);
		await painted(page);
		const url = page.url();
		// Click where the URI link and the JavaScript link sit on the page.
		const box = (await page.locator(CANVAS).boundingBox())!;
		await page.mouse.click(box.x + box.width * 0.4, box.y + box.height * 0.45);
		await page.mouse.click(box.x + box.width * 0.4, box.y + box.height * 0.87);
		await page.waitForTimeout(500);
		expect(inst.dialogs).toEqual([]);
		expect(inst.popups).toEqual([]);
		expect(page.url()).toBe(url);
		expect(inst.navigations.filter((u) => u !== url && !u.endsWith(new URL(url).pathname))).toEqual([]);
		expect(inst.requests.filter((r) => r.includes('example.invalid'))).toEqual([]);
		await expect(page.locator(`${ARM} a`)).toHaveCount(0);
		expect(await allViolations(page, inst)).toEqual([]);
		// The viewer is still the viewer.
		await expect(page.locator(VIEWER)).toHaveCount(1);
	});

	for (const [file, fallback] of [
		['bug_jpx.pdf', 'openjpeg_nowasm_fallback.js'],
		['jbig2_symbol_offset.pdf', 'jbig2_nowasm_fallback.js']
	] as const) {
		test(`E3: ${file} renders through ${fallback} with no wasm and no violation`, async ({ page, fixture, request }) => {
			const inst = await instrument(page);
			const bytes = readFileSync(resolve(PDF_FIXTURES, file));
			expect(bytes.subarray(0, 5).toString('latin1')).toBe('%PDF-');
			const served: { url: string; status: number; type: string }[] = [];
			page.on('response', (r) => {
				if (r.url().includes('/_app/pdfjs/')) served.push({ url: r.url(), status: r.status(), type: r.headers()['content-type'] ?? '' });
			});
			await openPdf(page, fixture, request, `PDF E3 ${file}`, [{ name: file, bytes }]);
			await painted(page, 20);
			// The decoder really came from the fallback file, same-origin, as JavaScript.
			await expect
				.poll(() => served.filter((s) => s.url.endsWith(`/_app/pdfjs/${fallback}`) && s.status === 200).length, { timeout: 15_000 })
				.toBeGreaterThan(0);
			expect(served.find((s) => s.url.endsWith(fallback))!.type).toMatch(/javascript/);
			expect(inst.requests.filter((r) => r.endsWith('.wasm'))).toEqual([]);
			expect(await allViolations(page, inst)).toEqual([]);
		});
	}

	test('E3: only the two fallbacks ship: the wasm files and qcms are not served', async ({ page, fixture }) => {
		await browserLogin(page);
		for (const f of ['openjpeg_nowasm_fallback.js', 'jbig2_nowasm_fallback.js']) {
			const r = await page.request.get(`/_app/pdfjs/${f}`);
			expect(r.status(), f).toBe(200);
			expect(r.headers()['content-type'], f).toMatch(/javascript/);
		}
		for (const f of ['openjpeg.wasm', 'jbig2.wasm', 'qcms_bg.wasm', 'quickjs-eval.js', 'quickjs-eval.wasm']) {
			const r = await page.request.get(`/_app/pdfjs/${f}`);
			const head = Buffer.from(await r.body()).subarray(0, 4);
			expect(r.headers()['content-type'] ?? '', f).not.toMatch(/wasm|javascript/);
			expect(head.equals(Buffer.from([0x00, 0x61, 0x73, 0x6d])), `${f} is not wasm`).toBe(false);
		}
		void fixture;
	});

	test('E5: an over-cap PDF shows the too-large copy and makes NO download request (metadata gate)', async ({ page, fixture, request }) => {
		const inst = await instrument(page);
		// The server's per-file upload cap is the same 25 MiB, so a file over it cannot be
		// stored on this server; one stored under a raised operator cap can. Report the size
		// such a file would carry in the attachment list (which seeds the viewer), and count
		// the GETs that follow.
		const over = 27_021_815;
		await page.route(/\/api\/v1\/workspaces\/[^/]+\/attachments\?/, async (route) => {
			if (route.request().method() !== 'GET') return route.continue();
			const response = await route.fetch();
			const json = await response.json();
			for (const row of json.attachments ?? []) row.size_bytes = over;
			await route.fulfill({ response, json });
		});
		await browserLogin(page);
		const doc = await seedDoc(fixture, request, 'PDF E5');
		const id = await uploadAttachment(fixture, request, doc.id, 'huge.pdf', 'application/pdf', buildMultiPagePdf(1));
		await page.goto(itemUrl(fixture, doc.slug));
		await page.locator(`${TILE}[aria-label*="huge.pdf"]`).click();
		await expect(page.locator(ARM)).toContainText('too large to preview');
		await expect(page.locator(ARM)).toContainText('This file is 26 MB');
		await expect(page.locator(ARM)).toContainText('Use Download');
		await expect(page.locator(`${ARM} canvas`)).toHaveCount(0);
		await expect(page.locator(`${ARM} .lightbox-retry`)).toHaveCount(0);
		await page.waitForTimeout(500);
		const gets = inst.requests.filter((r) => r.startsWith('GET') && r.includes(`/attachments/${id}`));
		expect(gets, 'no download request was made for the over-cap file').toEqual([]);
	});

	test('E5: a body that outruns the cap is refused by the response gate with the same copy', async ({ page, fixture, request }) => {
		await instrument(page);
		await page.route(/\/api\/v1\/workspaces\/[^/]+\/attachments\/[0-9a-f-]+$/, async (route) => {
			if (route.request().method() !== 'GET') return route.continue();
			await route.fulfill({ status: 200, contentType: 'application/pdf', body: buildPaddedPdf(26 * 1024 * 1024) });
		});
		await openPdf(page, fixture, request, 'PDF E5 body', [{ name: 'liar.pdf', bytes: buildMultiPagePdf(1) }]);
		await expect(page.locator(ARM)).toContainText('too large to preview');
		await expect(page.locator(`${ARM} canvas`)).toHaveCount(0);
	});

	test('zoom and the page box work, and Left/Right keep gallery paging across two PDFs', async ({ page, fixture, request }) => {
		await instrument(page);
		await openPdf(page, fixture, request, 'PDF paging', [
			{ name: 'one.pdf', bytes: buildMultiPagePdf(2) },
			{ name: 'two.pdf', bytes: buildMultiPagePdf(2) }
		]);
		await painted(page);
		const before = await paint(page);
		await page.getByRole('button', { name: 'Zoom in' }).click();
		await expect.poll(async () => (await paint(page)).w, { timeout: 15_000 }).toBeGreaterThan(before.w);
		await page.getByRole('textbox', { name: 'Page number' }).fill('2');
		await page.keyboard.press('Enter');
		await expect(page.locator(STATUS)).toHaveText('Page 2 of 2');
		// Arrows inside the page box move the caret, not the gallery.
		await page.getByRole('textbox', { name: 'Page number' }).press('ArrowLeft');
		await expect(viewerDialog(page, 'one.pdf')).toHaveCount(1);
		// Focus off the box: ArrowRight pages the gallery to the second PDF.
		await page.locator(VIEWER).getByRole('button', { name: 'Close' }).focus();
		await page.keyboard.press('ArrowRight');
		await expect(viewerDialog(page, 'two.pdf')).toHaveCount(1);
		await expect(page.locator(STATUS)).toHaveText('Page 1 of 2');
		await viewerNext(page).count();
	});
});

test.describe('attachment viewer — in-pane PDF (TASK-4075), phone sheet', () => {
	test.beforeEach(async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile-chromium', 'phone sheet is Pixel 7 only');
	});

	test('E4: the sheet is full-screen, the page fits the width, and a touch pan scrolls the page without paging the gallery', async ({ page, fixture, request }) => {
		const inst = await instrument(page);
		// A tall page, so there is something to pan; and a second PDF, so a swipe COULD page.
		await openPdf(page, fixture, request, 'PDF E4', [
			{ name: 'tall.pdf', bytes: buildMultiPagePdf(2, 2400) },
			{ name: 'other.pdf', bytes: buildMultiPagePdf(1) }
		]);
		await expect(page.locator(VIEWER_SHEET)).toHaveCount(1);
		await painted(page);
		const vp = page.viewportSize()!;
		const sheet = await page.locator(VIEWER_SHEET).boundingBox();
		expect(Math.abs(sheet!.width - vp.width), 'the sheet spans the phone width').toBeLessThan(2);
		expect(Math.abs(sheet!.height - vp.height), 'the sheet spans the phone height').toBeLessThan(2);

		// FIT-WIDTH: the canvas box is the scroller's content width.
		const fit = await page.evaluate(() => {
			const s = document.querySelector('.lightbox-pdf .attachment-pdf-scroll') as HTMLElement;
			const c = s.querySelector('canvas') as HTMLCanvasElement;
			return { scroller: s.clientWidth, canvas: c.getBoundingClientRect().width, overflowX: s.scrollWidth - s.clientWidth };
		});
		expect(Math.abs(fit.canvas - fit.scroller), 'the page fits the width').toBeLessThanOrEqual(1);
		expect(fit.overflowX, 'no sideways scroll at fit').toBeLessThanOrEqual(1);

		// A TOUCH PAN, through the compositor: vertical scrolls the page...
		const region = (await page.locator(SCROLL).boundingBox())!;
		expect(await page.locator(SCROLL).evaluate((el) => el.scrollHeight > el.clientHeight), 'something to scroll').toBe(true);
		const touch = await CdpTouch.attach(page);
		const x = region.x + region.width / 2;
		const y0 = region.y + region.height * 0.8;
		await touch.down(0, x, y0);
		for (let i = 1; i <= 10; i++) await touch.move(0, x, y0 - i * 30);
		await touch.lift(0);
		await expect.poll(() => page.locator(SCROLL).evaluate((el) => el.scrollTop), { timeout: 5000 }).toBeGreaterThan(50);
		// ...and a sideways swipe does not page the gallery.
		await touch.down(0, region.x + region.width * 0.85, region.y + region.height * 0.5);
		for (let i = 1; i <= 10; i++) await touch.move(0, region.x + region.width * 0.85 - i * 25, region.y + region.height * 0.5);
		await touch.lift(0);
		await page.waitForTimeout(400);
		await expect(viewerDialog(page, 'tall.pdf')).toHaveCount(1);
		await expect(page.locator(VIEWER_COUNTER)).toContainText('1');
		expect(await allViolations(page, inst)).toEqual([]);
	});
});
