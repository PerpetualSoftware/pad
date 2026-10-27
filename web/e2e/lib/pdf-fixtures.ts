import { deflateSync } from 'node:zlib';

/**
 * Hand-assembled PDFs for the in-pane PDF viewer's browser specs (TASK-4075).
 *
 * Written out as text rather than checked in as binaries or produced by an
 * encoder, so each fixture is readable in review and the box needs no PDF
 * tooling. The two real-world fixtures with JPX and JBIG2 images are separate
 * files in `e2e/fixtures/pdf/`, with their origin and licence beside them.
 */

/** Assemble a PDF from numbered object bodies, with a correct xref table. */
function assemble(objects: string[], rootObj: number): Buffer {
	let out = '%PDF-1.4\n';
	const offsets: number[] = [];
	objects.forEach((body, i) => {
		offsets.push(Buffer.byteLength(out, 'latin1'));
		out += `${i + 1} 0 obj\n${body}\nendobj\n`;
	});
	const xref = Buffer.byteLength(out, 'latin1');
	out += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
	for (const o of offsets) out += `${String(o).padStart(10, '0')} 00000 n \n`;
	out += `trailer\n<< /Size ${objects.length + 1} /Root ${rootObj} 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
	return Buffer.from(out, 'latin1');
}

function stream(content: string): string {
	return `<< /Length ${Buffer.byteLength(content, 'latin1')} >>\nstream\n${content}\nendstream`;
}

/**
 * A multi-page PDF. Page N paints a large filled rectangle whose grey level
 * depends on N, plus the text "Page N", so "the canvas is non-blank" and "page 2
 * differs from page 1" are both measurable from pixels.
 */
export function buildMultiPagePdf(pages: number, pageHeight = 792): Buffer {
	// 1 Catalog, 2 Pages, 3 Font, then per page: Page + Contents.
	const objects: string[] = [];
	const kids: string[] = [];
	for (let n = 1; n <= pages; n++) kids.push(`${4 + (n - 1) * 2} 0 R`);
	objects.push('<< /Type /Catalog /Pages 2 0 R >>');
	objects.push(`<< /Type /Pages /Kids [${kids.join(' ')}] /Count ${pages} >>`);
	objects.push('<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>');
	for (let n = 1; n <= pages; n++) {
		const grey = (0.15 + (0.6 * (n - 1)) / Math.max(1, pages - 1)).toFixed(2);
		const contents = `${grey} g 40 ${pageHeight - 492} 400 300 re f 0 g BT /F1 48 Tf 60 150 Td (Page ${n}) Tj ET`;
		objects.push(
			`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 ${pageHeight}] /Resources << /Font << /F1 3 0 R >> >> /Contents ${5 + (n - 1) * 2} 0 R >>`
		);
		objects.push(stream(contents));
	}
	return assemble(objects, 1);
}

/**
 * E2: a PDF that tries every way a document can reach out. A JavaScript
 * OpenAction (app.alert), a Link annotation with a URI action, a Link
 * annotation with a JavaScript action, and a GoToR-style launch. The viewer
 * draws a canvas and nothing else, so none of it may produce a dialog, a
 * navigation, or an anchor.
 */
export function buildHostilePdf(): Buffer {
	const objects = [
		// 1 Catalog, with a JavaScript OpenAction
		'<< /Type /Catalog /Pages 2 0 R /OpenAction 6 0 R /Names << /JavaScript << /Names [(boot) 6 0 R] >> >> >>',
		'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
		// 3 Page with two link annotations
		'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R /Annots [7 0 R 8 0 R] >>',
		stream('0.3 g 40 300 400 300 re f 0 g BT /F1 36 Tf 60 150 Td (Hostile) Tj ET'),
		'<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
		'<< /S /JavaScript /JS (app.alert\\("pdf script ran"\\); this.print\\(\\);) >>',
		'<< /Type /Annot /Subtype /Link /Rect [40 300 440 600] /Border [0 0 0] /A << /S /URI /URI (https://example.invalid/should-never-load) >> >>',
		'<< /Type /Annot /Subtype /Link /Rect [40 100 440 200] /Border [0 0 0] /A << /S /JavaScript /JS (app.alert\\("link script ran"\\);) >> >>'
	];
	return assemble(objects, 1);
}

/**
 * A PDF whose fill colour goes through a PostScript calculator function
 * (FunctionType 4) as a Separation tint transform. With wasm on, pdf.js
 * compiles such a function to a generated wasm module, which a CSP without
 * 'wasm-unsafe-eval' refuses: that is the E1 control leg's trigger. With
 * `useWasm: false` it runs in pdf.js's JS interpreter and must paint.
 */
export function buildPostScriptFunctionPdf(): Buffer {
	const fn = '{ dup 0.2 mul exch dup 0.5 mul exch 0.9 mul }';
	const objects = [
		'<< /Type /Catalog /Pages 2 0 R >>',
		'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
		'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /ColorSpace << /CS0 5 0 R >> >> /Contents 4 0 R >>',
		stream('/CS0 cs 1 scn 40 300 400 300 re f'),
		// Separation colour space: [/Separation name alternate tintTransform]
		'[/Separation /Spot /DeviceRGB 6 0 R]',
		`<< /FunctionType 4 /Domain [0 1] /Range [0 1 0 1 0 1] /Length ${fn.length} >>\nstream\n${fn}\nendstream`
	];
	return assemble(objects, 1);
}

/** A valid PDF header and structure padded past `bytes`, for the over-the-cap leg. */
export function buildPaddedPdf(bytes: number): Buffer {
	const base = buildMultiPagePdf(1);
	// Trailing bytes after %%EOF are ignored by readers; the server sniffs the head.
	return Buffer.concat([base, Buffer.alloc(Math.max(0, bytes - base.length), 0x20)]);
}

/** The declared side of the oversize image: 10000 x 10000 = 100 MP, over the 64 MP maxImageSize. */
export const OVERSIZE_IMAGE_SIDE = 10_000;

/**
 * F2: a page that paints a page-sized image DECLARED at 10000 x 10000 px (100 MP)
 * and then a red rectangle on top. The image data is a Flate stream of zeros, so
 * the file is ~100 KB, not 100 MB. With `maxImageSize` in force pdf.js skips the
 * image and the page is white with the rectangle; without it the image decodes
 * to a black underlay and the page is almost entirely inked.
 */
export function buildOversizeImagePdf(): Buffer {
	const data = deflateSync(Buffer.alloc(OVERSIZE_IMAGE_SIDE * OVERSIZE_IMAGE_SIDE)).toString('latin1');
	const objects = [
		'<< /Type /Catalog /Pages 2 0 R >>',
		'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
		'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>',
		stream('q 612 0 0 792 0 0 cm /Im0 Do Q 1 0 0 rg 40 300 400 300 re f'),
		`<< /Type /XObject /Subtype /Image /Width ${OVERSIZE_IMAGE_SIDE} /Height ${OVERSIZE_IMAGE_SIDE} /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length ${data.length} >>\nstream\n${data}\nendstream`
	];
	return assemble(objects, 1);
}
