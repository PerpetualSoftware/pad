import { describe, expect, it } from 'vitest';
import {
	canBrowserPreview,
	canOpenInViewer,
	formatBytes,
	iconForAttachment,
	isImage
} from './display';
import { ATTACHMENT_ICON_IDS, ATTACHMENT_ICON_PATHS, iconSvg } from './icons/index';
import familyFixture from './mime-families.json';

/**
 * Icon-mapping coverage per PLAN-2392 DR-3a: one representative MIME per
 * family, plus the unknown-MIME and no-extension cases. Full coverage of the
 * MIME list lives on the Go side (internal/attachments/mime_families_test.go),
 * which asserts the shared fixture covers the server's upload allowlist — full
 * coverage where drift happens, representative coverage where it doesn't.
 */
describe('iconForAttachment', () => {
	const representative: Array<[string, string]> = [
		['image/png', 'image'],
		['video/mp4', 'video'],
		['audio/mpeg', 'audio'],
		['application/vnd.openxmlformats-officedocument.wordprocessingml.document', 'document'],
		['application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', 'spreadsheet'],
		['application/vnd.openxmlformats-officedocument.presentationml.presentation', 'presentation'],
		['application/pdf', 'pdf'],
		['application/zip', 'archive'],
		['text/plain', 'text'],
	];

	it.each(representative)('maps %s to the %s icon', (mime, family) => {
		expect(iconForAttachment(mime, 'file.bin')).toBe(family);
	});

	it('covers every icon family except the generic fallback', () => {
		const covered = new Set(representative.map(([, family]) => family));
		const uncovered = ATTACHMENT_ICON_IDS.filter((id) => id !== 'generic' && !covered.has(id));
		expect(uncovered).toEqual([]);
	});

	it('falls back to the generic file icon for an unknown MIME, never a question mark', () => {
		expect(iconForAttachment('application/octet-stream', 'mystery.bin')).toBe('generic');
		expect(iconForAttachment('application/x-unheard-of')).toBe('generic');
	});

	it('falls back to the generic file icon when there is no extension at all', () => {
		expect(iconForAttachment('', 'LICENSE')).toBe('generic');
		expect(iconForAttachment(null, null)).toBe('generic');
		expect(iconForAttachment(undefined, undefined)).toBe('generic');
	});

	it('uses the filename extension when the MIME is missing or unhelpful', () => {
		// The editor chip renders before its HEAD probe resolves, so this is
		// the chip's first paint, not a hypothetical.
		expect(iconForAttachment(null, 'quarterly.xlsx')).toBe('spreadsheet');
		expect(iconForAttachment('application/octet-stream', 'deck.pptx')).toBe('presentation');
		expect(iconForAttachment('', 'notes.md')).toBe('text');
	});

	it('prefers a known MIME over a misleading extension', () => {
		expect(iconForAttachment('application/pdf', 'report.docx')).toBe('pdf');
		expect(iconForAttachment('text/plain', 'rows.csv')).toBe('text');
	});

	it('normalizes case and parameters like the server does', () => {
		expect(iconForAttachment('TEXT/CSV', 'a')).toBe('spreadsheet');
		expect(iconForAttachment('text/plain; charset=utf-8', 'a')).toBe('text');
	});

	it('still recognizes the office and archive families the old helpers matched by pattern', () => {
		// Not on today's upload allowlist, but rows predating it can carry
		// these — dropping them to the generic icon would be a regression.
		expect(iconForAttachment('application/x-rar-compressed', 'old')).toBe('archive');
		expect(iconForAttachment('application/vnd.ms-excel.sheet.macroEnabled.12', 'old')).toBe(
			'spreadsheet',
		);
		expect(
			iconForAttachment('application/vnd.ms-powerpoint.presentation.macroEnabled.12', 'old'),
		).toBe('presentation');
		expect(iconForAttachment('application/vnd.ms-word.document.macroEnabled.12', 'old')).toBe(
			'document',
		);
	});

	it('keeps the old helper "any other vendor office format is a document" tier', () => {
		expect(iconForAttachment('application/vnd.ms-project', 'plan')).toBe('document');
		expect(iconForAttachment('application/vnd.oasis.opendocument.graphics', 'draw')).toBe(
			'document',
		);
		expect(iconForAttachment('application/vnd.openxmlformats-officedocument.theme+xml', 'x')).toBe(
			'document',
		);
	});

	it('does not claim a MIME that merely contains an archive word', () => {
		expect(iconForAttachment('application/vnd.example.uncompressed-document', 'x')).toBe('generic');
		// application/vnd.airzip.filesecure.azf is an encrypted document, not a
		// ZIP — the archive patterns are token-delimited for exactly this.
		expect(iconForAttachment('application/vnd.airzip.filesecure.azf', 'secure')).toBe('generic');
		expect(iconForAttachment('application/zip', 'a')).toBe('archive');
		expect(iconForAttachment('application/x-zip-compressed', 'a')).toBe('archive');
		expect(iconForAttachment('application/x-rar-compressed', 'a')).toBe('archive');
	});

	it('recognizes unlisted media by type prefix', () => {
		expect(iconForAttachment('image/jxl', 'future.jxl')).toBe('image');
		expect(iconForAttachment('video/ogg', 'clip')).toBe('video');
		expect(iconForAttachment('audio/x-aiff', 'take1')).toBe('audio');
	});

	it('lets an extension win over an UNLISTED text/* MIME', () => {
		// text/* is the widest prefix fallback, so it is consulted last.
		expect(iconForAttachment('text/x-comma-separated-values', 'rows.csv')).toBe('spreadsheet');
		expect(iconForAttachment('text/x-nonsense', 'unnamed')).toBe('text');
	});

	it('only ever returns a renderable icon id', () => {
		// The Go test checks the fixture covers the server allowlist and that
		// each value is in the fixture's own `families` list — it has no way to
		// know what the TS icon set actually renders. These two assertions are
		// the other half of that contract: `families` IS the icon set, and
		// every mapped MIME resolves to something renderable. Without them a
		// fixture could be Go-green and still paint nothing.
		expect([...familyFixture.families].sort()).toEqual([...ATTACHMENT_ICON_IDS].sort());
		for (const family of Object.values(familyFixture.mime)) {
			expect(ATTACHMENT_ICON_IDS).toContain(family);
		}
	});

	it('renders every allowlisted MIME in the fixture as a real icon', () => {
		for (const [mime, family] of Object.entries(familyFixture.mime)) {
			expect(iconForAttachment(mime, 'no-extension')).toBe(family);
			expect(iconSvg(iconForAttachment(mime, 'no-extension'))).toContain('<path d="');
		}
	});
});

describe('iconSvg', () => {
	it('renders every family as a currentColor-driven SVG', () => {
		for (const id of ATTACHMENT_ICON_IDS) {
			const svg = iconSvg(id);
			expect(svg).toContain('stroke="currentColor"');
			expect(svg).toContain('aria-hidden="true"');
			expect(svg).toContain(ATTACHMENT_ICON_PATHS[id]);
		}
	});

	it('gives every family a distinct shape', () => {
		const paths = new Set(Object.values(ATTACHMENT_ICON_PATHS));
		expect(paths.size).toBe(ATTACHMENT_ICON_IDS.length);
	});

	it('renders the generic icon for an unknown id rather than throwing', () => {
		expect(iconSvg('not-a-family')).toBe(iconSvg('generic'));
	});

	it('scales with the surrounding type by default and accepts an explicit size', () => {
		expect(iconSvg('pdf')).toContain('width="1em"');
		expect(iconSvg('pdf', { size: 24 })).toContain('width="24px"');
		expect(iconSvg('pdf', { size: '2rem' })).toContain('width="2rem"');
	});

	it('refuses a size that would break out of the attribute', () => {
		expect(iconSvg('pdf', { size: '1em" onload="alert(1)' })).toContain('width="1em"');
		expect(iconSvg('pdf', { size: Number.NaN })).toContain('width="1em"');
		expect(iconSvg('pdf', { size: -8 })).toContain('width="1em"');
	});
});

describe('formatBytes', () => {
	// Unchanged by TASK-2417 — pinned here because the editor chip now depends
	// on it rendering "0 B" (the chip hides that at its own call site, DR-3b).
	it('renders a zero size rather than an empty string', () => {
		expect(formatBytes(0)).toBe('0 B');
	});

	it('picks a unit so the value stays under 1024', () => {
		expect(formatBytes(999)).toBe('999 B');
		expect(formatBytes(1024)).toBe('1.0 KB');
		expect(formatBytes(1048575)).toBe('1.0 MB');
		expect(formatBytes(15 * 1024 * 1024)).toBe('15 MB');
	});
});

describe('isImage', () => {
	it('still answers the general "is this a picture" question', () => {
		expect(isImage('image/png')).toBe(true);
		expect(isImage('application/pdf')).toBe(false);
	});

	// The DR-16 point in one assertion: isImage is deliberately looser than
	// the viewer gate, which is why the gate has to be its own helper.
	it('is looser than the viewer gate — it accepts what canOpenInViewer refuses', () => {
		expect(isImage('image/svg+xml')).toBe(true);
		expect(canOpenInViewer('image/svg+xml')).toBe(false);
	});
});

const VIEWER_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/avif'];

describe('canOpenInViewer — PLAN-2392 DR-16', () => {
	it('accepts exactly the five safe raster types', () => {
		for (const mime of VIEWER_TYPES) expect(canOpenInViewer(mime)).toBe(true);
	});

	// The whole reason this isn't `startsWith('image/')`: SVG carries active
	// content, and TIFF/HEIC are types a browser may simply not decode.
	it('refuses image/* types outside the allowlist', () => {
		expect(canOpenInViewer('image/svg+xml')).toBe(false);
		expect(canOpenInViewer('image/tiff')).toBe(false);
		expect(canOpenInViewer('image/heic')).toBe(false);
		expect(canOpenInViewer('image/bmp')).toBe(false);
		expect(canOpenInViewer('image/jxl')).toBe(false);
	});

	it('refuses non-image types and a missing MIME', () => {
		expect(canOpenInViewer('application/pdf')).toBe(false);
		expect(canOpenInViewer('text/xml')).toBe(false);
		expect(canOpenInViewer('')).toBe(false);
		expect(canOpenInViewer(null)).toBe(false);
		expect(canOpenInViewer(undefined)).toBe(false);
	});

	it('normalizes case and parameters before matching', () => {
		expect(canOpenInViewer('IMAGE/PNG')).toBe(true);
		expect(canOpenInViewer('image/jpeg; charset=binary')).toBe(true);
		expect(canOpenInViewer('  image/webp  ')).toBe(true);
	});

	// A prefix test would let `image/svg+xml; charset=utf-8` through some
	// naive normalizations; pin that it doesn't.
	it('does not admit a disallowed type by dressing it in parameters', () => {
		expect(canOpenInViewer('image/svg+xml; charset=utf-8')).toBe(false);
	});
});

describe('canBrowserPreview — PLAN-2392 DR-5', () => {
	it('accepts PDF, plain text and the whole viewer raster set', () => {
		expect(canBrowserPreview('application/pdf')).toBe(true);
		expect(canBrowserPreview('text/plain')).toBe(true);
		for (const mime of VIEWER_TYPES) expect(canBrowserPreview(mime)).toBe(true);
	});

	it('refuses the other text/* subtypes browsers handle inconsistently', () => {
		expect(canBrowserPreview('text/markdown')).toBe(false);
		expect(canBrowserPreview('text/csv')).toBe(false);
		expect(canBrowserPreview('text/xml')).toBe(false);
		expect(canBrowserPreview('application/xml')).toBe(false);
	});

	it('refuses office documents, archives and force-downloaded types', () => {
		expect(canBrowserPreview('application/msword')).toBe(false);
		expect(
			canBrowserPreview(
				'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
			)
		).toBe(false);
		expect(canBrowserPreview('application/zip')).toBe(false);
		expect(canBrowserPreview('text/html')).toBe(false);
		expect(canBrowserPreview('application/javascript')).toBe(false);
		expect(canBrowserPreview('image/svg+xml')).toBe(false);
	});

	it('refuses a missing MIME and normalizes like the viewer gate', () => {
		expect(canBrowserPreview(null)).toBe(false);
		expect(canBrowserPreview(undefined)).toBe(false);
		expect(canBrowserPreview('')).toBe(false);
		expect(canBrowserPreview('TEXT/PLAIN; charset=utf-8')).toBe(true);
	});

	// Superset relationship, stated once so a future edit to either set
	// can't silently break it.
	it('is a superset of the viewer gate', () => {
		for (const mime of VIEWER_TYPES) {
			expect(canOpenInViewer(mime) && canBrowserPreview(mime)).toBe(true);
		}
	});
});
