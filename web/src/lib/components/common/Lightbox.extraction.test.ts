/**
 * Grep-gate for the surface machinery extraction (PLAN-2392 3c-i-B / 3c-ii T2b).
 *
 * MIGRATED from `AttachmentDetailsPanel.extraction.test.ts` when the panel was
 * deleted in the T2b cutover. The contract is unchanged, only its subject moved:
 * the metadata machine and delete-confirm machinery live in the shared
 * `surfaceMetadata.svelte.ts` / `surfaceDeleteConfirm.svelte.ts` rune modules,
 * and the surface that consumes them is now the `Lightbox` (the panel was the
 * other consumer; there is no second copy to drift). This test fails if any of
 * that machinery creeps back INTO the Lightbox.
 *
 * It reads the component SOURCE (not its behavior — that is pinned by
 * `Lightbox.svelte.test.ts` and `AttachmentSurfaceHost.svelte.test.ts`) and
 * asserts the machinery lives in the modules and the surface merely consumes it.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const src = readFileSync(
	fileURLToPath(new URL('./Lightbox.svelte', import.meta.url)),
	'utf8'
);
// Only the <script> — the top-of-file doc comment legitimately names the moved
// helpers in prose, and gating on prose would be brittle nonsense.
const scriptSrc = src.slice(src.indexOf('<script'));

describe('Lightbox surface-machinery extraction (TASK-2473 / TASK-2488)', () => {
	it('consumes the shared surface modules', () => {
		expect(scriptSrc).toContain(
			"import { createSurfaceMetadata } from '$lib/attachments/surfaceMetadata.svelte'"
		);
		expect(scriptSrc).toContain(
			"import { createDeleteConfirm } from '$lib/attachments/surfaceDeleteConfirm.svelte'"
		);
	});

	it('no longer imports the machinery the modules now own', () => {
		// The metadata fetch layer and the fence factories are the modules'
		// dependencies now, not the surface's.
		expect(scriptSrc).not.toContain("from '$lib/components/editor/attachment-metadata'");
		expect(scriptSrc).not.toContain("from '$lib/attachments/viewFence'");
		// The prompt wording is composed inside the delete-confirm module.
		expect(scriptSrc).not.toContain(
			"import { attachmentDeletePrompt } from '$lib/components/attachments/AttachmentDeleteConfirm.svelte'"
		);
	});

	it('no longer builds the fences or the metadata read locally', () => {
		expect(scriptSrc).not.toMatch(/\bcreateFence\s*\(/);
		expect(scriptSrc).not.toMatch(/\bcreatePaintFence\s*\(/);
		expect(scriptSrc).not.toMatch(/\bviewIdentity\s*\(/);
		expect(scriptSrc).not.toMatch(/\bfetchAttachmentMetadata\s*\(/);
		expect(scriptSrc).not.toMatch(/\brevalidateAttachmentMetadata\s*\(/);
	});

	it('no longer owns the delete-confirmation state machine locally', () => {
		// The 'root' | 'delete' sub-view state and its resolver live in the module.
		expect(scriptSrc).not.toMatch(/\$state<\s*'root'\s*\|\s*'delete'\s*>/);
		expect(scriptSrc).not.toMatch(/\battachmentDeletePrompt\s*\(/);
	});
});
