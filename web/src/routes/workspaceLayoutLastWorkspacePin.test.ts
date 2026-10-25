import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * TASK-4044 — a SOURCE PIN, with the limit stated: the workspace layout is a
 * route component with no harness, so whether it records the workspace the user
 * is in (for /messages's Back link) is not reachable from a store test. This
 * proves the call is still there, not that it is reached; a short-circuit would
 * survive it. Same trade as localIndexLayoutDriverWiring.test.ts, and deleted
 * under the same condition.
 */
const src = readFileSync(
	fileURLToPath(new URL('./[username]/[workspace]/+layout.svelte', import.meta.url)),
	'utf8'
);

describe('workspace layout records the last workspace (TASK-4044)', () => {
	it('notes the owner and slug in the route-noting effect', () => {
		expect(src).toContain('untrack(() => lastWorkspace.note(username, wsSlug));');
		expect(src).toContain("import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';");
	});
});
