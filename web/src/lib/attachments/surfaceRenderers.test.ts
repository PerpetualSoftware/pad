import { describe, it, expect } from 'vitest';
import { getSurfaceRenderer } from './surfaceRenderers';

// The single decision of WHICH renderer draws an attachment (TASK-2476). It is
// defined AS the DR-16 raster allowlist, so this pins that it fails closed on
// everything else — the invariant the viewer's no-bytes fallback rests on.
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

	it('returns null for non-image types a browser cannot preview as a raster', () => {
		for (const mime of ['application/pdf', 'text/plain', 'application/zip', 'image/tiff']) {
			expect(getSurfaceRenderer(mime)).toBeNull();
		}
	});

	it('fails closed on an unresolved (null) MIME', () => {
		expect(getSurfaceRenderer(null)).toBeNull();
	});
});
