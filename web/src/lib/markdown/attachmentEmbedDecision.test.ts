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

function meta(mime: string, derived?: string[]): AttachmentMeta {
	return {
		id: 'att-1',
		mime_type: mime,
		filename: 'photo',
		size_bytes: 1234,
		derived_variants: derived
	};
}

/** The variant the body renderer requests. */
const MD = 'thumb-md' as const;
/** The variant the timeline renderer requests. */
const SM = 'thumb-sm' as const;

describe('shouldEmbedAsImage — the defect', () => {
	it('HEIC with NO derived variant is NOT embedded as an image', () => {
		expect(shouldEmbedAsImage(meta('image/heic', []), MD)).toBe(false);
		expect(shouldEmbedAsImage(meta('image/heif', []), MD)).toBe(false);
	});

	it('HEIC WITH a derived variant is embedded as an image — libvips builds and Pad Cloud are unaffected', () => {
		expect(shouldEmbedAsImage(meta('image/heic', [MD]), MD)).toBe(true);
	});
});

describe('shouldEmbedAsImage — the halves of the disjunction', () => {
	it('CONTROL — AVIF with NO derived variant is STILL an image, because the browser paints it', () => {
		// Fails if the rule is collapsed to variant availability alone. The
		// pure-Go processor derives no AVIF thumbnail either, so availability
		// cannot be the whole rule without turning good AVIF embeds into chips.
		expect(shouldEmbedAsImage(meta('image/avif', []), MD)).toBe(true);
	});

	it('CONTROL — SVG with NO derived variant is STILL an image', () => {
		// SVG is excluded from the in-app VIEWER for active-content reasons, and
		// reusing that predicate here would flip every existing SVG embed in
		// every existing document to a file chip. An SVG in an `<img>` runs no
		// script.
		expect(shouldEmbedAsImage(meta('image/svg+xml', []), MD)).toBe(true);
	});

	it('the browser-paintable formats are images with no variant at all', () => {
		for (const m of ['image/png', 'image/jpeg', 'image/gif', 'image/webp']) {
			expect(shouldEmbedAsImage(meta(m, []), MD), m).toBe(true);
		}
	});

	it('a non-image MIME is never an image, variant or not', () => {
		expect(shouldEmbedAsImage(meta('application/pdf', [MD]), MD)).toBe(false);
		expect(shouldEmbedAsImage(meta('application/zip', []), MD)).toBe(false);
	});

	it('parameters on the MIME do not defeat the match', () => {
		expect(shouldEmbedAsImage(meta('image/svg+xml; charset=utf-8', []), MD)).toBe(true);
		expect(shouldEmbedAsImage(meta('image/heic; foo=bar', []), MD)).toBe(false);
	});
});

describe('shouldEmbedAsImage — UNKNOWN is not FALSE', () => {
	it('an unprobed / older-server attachment keeps the old MIME-prefix behaviour', () => {
		// The compatibility hinge. A server predating BUG-2964 sends no header,
		// and reading that silence as "no variants exist" would flip every embed
		// in every document against it — a far bigger change than the bug.
		expect(shouldEmbedAsImage(meta('image/heic', undefined), MD)).toBe(true);
		expect(shouldEmbedAsImage(meta('image/png', undefined), MD)).toBe(true);
		expect(shouldEmbedAsImage(meta('application/pdf', undefined), MD)).toBe(false);
	});
});

describe('resolveAttachmentImage — what the reader actually gets', () => {
	const resolver = (m: AttachmentMeta) => () => m;

	it('renders a downloadable file chip for undecodable HEIC, not a broken img', () => {
		const html = resolveAttachmentImage(
			'pad-attachment:att-1',
			'Beach',
			'ws',
			resolver(meta('image/heic', []))
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
			resolver(meta('image/avif', []))
		);
		expect(html).toContain('<img');
	});
});

/**
 * BUG-2964, codex round 1 — the question is about THE VARIANT THIS RENDER WILL
 * REQUEST, not about whether any thumbnail exists.
 *
 * Derivation writes `thumb-sm` and `thumb-md` independently, so a partial
 * derivation leaves one present and the other absent. A boolean "has a
 * thumbnail" flag answers the wrong question: it says "image", the render asks
 * for the variant that is missing, the endpoint silently falls back to the
 * undecodable original, and the broken image is back — now with a layer of
 * machinery in front of it that looks like it should have prevented exactly
 * this.
 *
 * The body renderer requests `thumb-md`; the timeline requests `thumb-sm`. Both
 * directions are here, because a rule that only happens to work for the caller
 * you were thinking about is the same defect.
 */
describe('shouldEmbedAsImage — the requested variant is the one that matters', () => {
	it('HEIC with ONLY thumb-sm is a chip when the render asks for thumb-md', () => {
		expect(shouldEmbedAsImage(meta('image/heic', [SM]), MD)).toBe(false);
	});

	it('HEIC with ONLY thumb-md is a chip when the render asks for thumb-sm', () => {
		expect(shouldEmbedAsImage(meta('image/heic', [MD]), SM)).toBe(false);
	});

	it('each render is satisfied by its OWN variant', () => {
		expect(shouldEmbedAsImage(meta('image/heic', [SM]), SM)).toBe(true);
		expect(shouldEmbedAsImage(meta('image/heic', [MD]), MD)).toBe(true);
		expect(shouldEmbedAsImage(meta('image/heic', [SM, MD]), MD)).toBe(true);
	});

	it('a render pointed at the ORIGINAL cannot be carried by a variant existing', () => {
		// `original` is not a derivative — asking for it means the browser gets
		// the uploader's bytes, so only the paintable half can carry the embed.
		expect(shouldEmbedAsImage(meta('image/heic', [SM, MD]), 'original')).toBe(false);
		expect(shouldEmbedAsImage(meta('image/png', [SM, MD]), 'original')).toBe(true);
	});

	it('an omitted wantVariant behaves like the original, not like a wildcard', () => {
		expect(shouldEmbedAsImage(meta('image/heic', [SM, MD]))).toBe(false);
	});
});
