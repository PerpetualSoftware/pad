import { describe, expect, it } from 'vitest';
import {
	shouldEmbedAsImage,
	resolveAttachmentImage,
	type AttachmentMeta
} from './attachments';

/**
 * BUG-2964 — a HEIC attachment rendered as a broken image on a pure-Go build.
 *
 * The chain: `deriveThumbnails` skips a source the processor cannot decode, and
 * the pure-Go processor decodes PNG/JPEG/GIF/BMP/TIFF but not HEIC. The byte
 * endpoint then SILENTLY falls back to the original when the variant row is
 * missing. So an `<img?variant=thumb-md>` chosen on a MIME PREFIX handed the
 * browser HEIC bytes, which Chrome and Firefox do not decode.
 *
 * The rule is a disjunction — a derived variant exists, OR the browser paints
 * the original — and both halves have a leg here. The AVIF leg is a CONTROL:
 * it fails if anyone collapses the rule to variant availability alone, which is
 * the tempting simplification, because AVIF has no derived variant on a pure-Go
 * build either and is decoded by every current browser.
 */

function meta(mime: string, derived?: boolean): AttachmentMeta {
	return {
		id: 'att-1',
		mime_type: mime,
		filename: 'photo',
		size_bytes: 1234,
		derived_variant: derived
	};
}

describe('shouldEmbedAsImage — the defect', () => {
	it('HEIC with NO derived variant is NOT embedded as an image', () => {
		expect(shouldEmbedAsImage(meta('image/heic', false))).toBe(false);
		expect(shouldEmbedAsImage(meta('image/heif', false))).toBe(false);
	});

	it('HEIC WITH a derived variant is embedded as an image — libvips builds and Pad Cloud are unaffected', () => {
		expect(shouldEmbedAsImage(meta('image/heic', true))).toBe(true);
	});
});

describe('shouldEmbedAsImage — the halves of the disjunction', () => {
	it('CONTROL — AVIF with NO derived variant is STILL an image, because the browser paints it', () => {
		// Fails if the rule is collapsed to variant availability alone. The
		// pure-Go processor derives no AVIF thumbnail either, so availability
		// cannot be the whole rule without turning good AVIF embeds into chips.
		expect(shouldEmbedAsImage(meta('image/avif', false))).toBe(true);
	});

	it('CONTROL — SVG with NO derived variant is STILL an image', () => {
		// SVG is excluded from the in-app VIEWER for active-content reasons, and
		// reusing that predicate here would flip every existing SVG embed in
		// every existing document to a file chip. An SVG in an `<img>` runs no
		// script.
		expect(shouldEmbedAsImage(meta('image/svg+xml', false))).toBe(true);
	});

	it('the browser-paintable formats are images with no variant at all', () => {
		for (const m of ['image/png', 'image/jpeg', 'image/gif', 'image/webp']) {
			expect(shouldEmbedAsImage(meta(m, false)), m).toBe(true);
		}
	});

	it('a non-image MIME is never an image, variant or not', () => {
		expect(shouldEmbedAsImage(meta('application/pdf', true))).toBe(false);
		expect(shouldEmbedAsImage(meta('application/zip', false))).toBe(false);
	});

	it('parameters on the MIME do not defeat the match', () => {
		expect(shouldEmbedAsImage(meta('image/svg+xml; charset=utf-8', false))).toBe(true);
		expect(shouldEmbedAsImage(meta('image/heic; foo=bar', false))).toBe(false);
	});
});

describe('shouldEmbedAsImage — UNKNOWN is not FALSE', () => {
	it('an unprobed / older-server attachment keeps the old MIME-prefix behaviour', () => {
		// The compatibility hinge. A server predating BUG-2964 sends no header,
		// and reading that silence as "no variants exist" would flip every embed
		// in every document against it — a far bigger change than the bug.
		expect(shouldEmbedAsImage(meta('image/heic', undefined))).toBe(true);
		expect(shouldEmbedAsImage(meta('image/png', undefined))).toBe(true);
		expect(shouldEmbedAsImage(meta('application/pdf', undefined))).toBe(false);
	});
});

describe('resolveAttachmentImage — what the reader actually gets', () => {
	const resolver = (m: AttachmentMeta) => () => m;

	it('renders a downloadable file chip for undecodable HEIC, not a broken img', () => {
		const html = resolveAttachmentImage(
			'pad-attachment:att-1',
			'Beach',
			'ws',
			resolver(meta('image/heic', false))
		);
		expect(html).toContain('class="file-chip"');
		expect(html).toContain('download=');
		expect(html).not.toContain('<img');
		// The alt text the author wrote is the chip label — the document still
		// reads correctly, it just offers the file instead of a broken picture.
		expect(html).toContain('Beach');
	});

	it('still renders an img for AVIF with no variant', () => {
		const html = resolveAttachmentImage(
			'pad-attachment:att-1',
			'Shot',
			'ws',
			resolver(meta('image/avif', false))
		);
		expect(html).toContain('<img');
	});
});
