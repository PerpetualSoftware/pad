// TASK-4195: the Go renderer (internal/server/render/attachments.go) and this
// one must emit byte-identical file chips, images and missing placeholders.
// Both assert the same fixture, so a change on either side fails its own suite.
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
