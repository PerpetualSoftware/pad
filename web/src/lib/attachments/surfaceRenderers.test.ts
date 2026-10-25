import { describe, it, expect } from 'vitest';
import { getSurfaceRenderer } from './surfaceRenderers';

// The single decision of WHICH renderer draws an attachment (TASK-2476). Each id
// is defined AS a predicate's allowlist — 'raster-image' as DR-16's, 'text' as
// canPreviewAsText's (IDEA-2712) — so this pins that it fails closed on
// everything else: the invariant the viewer's no-bytes fallback rests on.
describe('getSurfaceRenderer', () => {
	it("returns 'raster-image' for every DR-16 raster type", () => {
		for (const mime of [
			'image/png',
			'image/jpeg',
			'image/gif',
			'image/webp',
			'image/avif',
		]) {
			expect(getSurfaceRenderer(mime)).toBe('raster-image');
		}
	});

	it('returns null for an unsafe / active type (SVG), which becomes the fallback', () => {
		// image/svg+xml carries active content — the exact hole the allowlist closes.
		expect(getSurfaceRenderer('image/svg+xml')).toBeNull();
	});

	it("returns 'text' for the in-app text-preview allowlist", () => {
		// IDEA-2712 / GitHub #1169. text/plain moved from the null arm to here —
		// a deliberate contract change, not a widened raster set: it is the
		// in-app renderer claiming it, and canBrowserPreview is untouched.
		for (const mime of ['text/markdown', 'text/plain', 'application/json', 'application/yaml', 'application/toml']) {
			expect(getSurfaceRenderer(mime)).toBe('text');
		}
	});

	it('returns null for types no renderer claims', () => {
		// PDF is still PLAN-2393's reserved slot, not ours.
		for (const mime of ['application/pdf', 'application/zip', 'image/tiff']) {
			expect(getSurfaceRenderer(mime)).toBeNull();
		}
	});

	it('fails closed on the force-download bucket, which is CategoryText server-side', () => {
		// PLAN-2393 DR-6: these would XSS if inlined. They share a server-side
		// CATEGORY with markdown, which is exactly why the predicate behind
		// 'text' is a MIME allowlist and never a category test — a category
		// selector would admit all three of these.
		//
		// HONEST ABOUT WHAT THIS IS (codex R3 #5): it passes against origin/main
		// too, where EVERYTHING non-raster returned null. It is a REGRESSION
		// GUARD against a future widening — the one that would follow from
		// reaching for `CategoryText` — not evidence that this branch's change
		// works. The discriminating tests for that are the `'text'` cases above.
		for (const mime of ['text/html', 'text/javascript', 'application/javascript']) {
			expect(getSurfaceRenderer(mime)).toBeNull();
		}
	});

	it("returns 'table' for exactly the CSV and TSV allowlist", () => {
		for (const mime of ['text/csv', 'text/tab-separated-values']) {
			expect(getSurfaceRenderer(mime)).toBe('table');
		}
	});

	it.each(['text/xml', 'application/xhtml+xml', 'text/html', 'text/javascript',
		'image/svg+xml', 'application/xml', 'text/yaml', 'text/unknown', null])(
		'privacy boundary: %s claims no renderer even though text categories might admit it',
		(mime) => expect(getSurfaceRenderer(mime)).toBeNull()
	);

	it('fails closed on an unresolved (null) MIME', () => {
		expect(getSurfaceRenderer(null)).toBeNull();
	});
});
