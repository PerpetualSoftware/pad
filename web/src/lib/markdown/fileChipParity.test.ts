// TASK-4195: the Go renderer (internal/server/render/attachments.go) and this
// one must emit byte-identical file chips, images and missing placeholders.
// Both assert the same fixture, so a change on either side fails its own suite.
//
// Scope of the URL half (TASK-4195 r3-M1): TS builds download URLs with
// encodeURIComponent and Go with url.PathEscape, which differ on !'()* and
// $&+=:@. The fixture pins parity only for UUID attachment ids and workspace
// slugs minted since BUG-3769 ([a-z0-9-], where both encoders are the
// identity). A preserved legacy slug may hold those characters and encode
// differently on each side, but both forms decode to the same route.
import { describe, expect, it } from 'vitest';
import fixture from './testdata/file-chip-parity.json';
import { renderAttachmentChip, renderAttachmentImage, renderAttachmentMissing } from './attachments';

describe('attachment renderers: byte parity with the Go renderer', () => {
	it('every case array is non-empty', () => {
		expect(fixture.chips.length).toBeGreaterThan(0);
		expect(fixture.images.length).toBeGreaterThan(0);
		expect(fixture.missing.length).toBeGreaterThan(0);
	});

	describe('file chip', () => {
		it.each(fixture.chips)('$name', (c) => {
			const html = renderAttachmentChip(
				{ ...c.meta, width: null, height: null },
				c.display_text || undefined,
				c.workspace
			);
			expect(html).toBe(c.html);
		});
	});

	describe('image', () => {
		it.each(fixture.images)('$name', (c) => {
			const html = renderAttachmentImage(
				{ ...c.meta, width: c.meta.width ?? null, height: c.meta.height ?? null },
				c.alt,
				c.workspace
			);
			expect(html).toBe(c.html);
		});
	});

	describe('missing placeholder', () => {
		it.each(fixture.missing)('$name', (c) => {
			expect(renderAttachmentMissing(c.uuid, c.alt)).toBe(c.html);
		});
	});
});
