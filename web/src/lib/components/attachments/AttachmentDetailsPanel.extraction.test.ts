/**
 * Grep-gate for TASK-2473 (PLAN-2392 3c-i-B).
 *
 * The panel's metadata machine and delete-confirm machinery were lifted into
 * the shared `surfaceMetadata.svelte.ts` / `surfaceDeleteConfirm.svelte.ts`
 * rune modules so the panel and the converged viewer share ONE implementation.
 * This test fails if any of that machinery creeps back into the panel — the
 * whole point of the extraction is that there is no second copy to drift.
 *
 * It reads the component SOURCE (not its behavior — the behavior is pinned by
 * AttachmentPanelHost.svelte.test.ts) and asserts the machinery lives in the
 * modules and the panel merely consumes them.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const panelSrc = readFileSync(
	fileURLToPath(new URL('./AttachmentDetailsPanel.svelte', import.meta.url)),
	'utf8'
);

// Only the <script> — the top-of-file doc comment legitimately NAMES the moved
// helpers in prose, and gating on prose would be brittle nonsense.
const scriptSrc = panelSrc.slice(panelSrc.indexOf('<script'));

describe('AttachmentDetailsPanel machinery extraction (TASK-2473)', () => {
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
		// dependencies now, not the panel's.
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
		// The 'root' | 'delete' sub-view state and its resolver moved out.
		expect(scriptSrc).not.toMatch(/\$state<\s*'root'\s*\|\s*'delete'\s*>/);
		expect(scriptSrc).not.toMatch(/\battachmentDeletePrompt\s*\(/);
	});
});
