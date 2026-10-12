// TASK-3612, CONVE-19: the component test vouches for the gate, not for the
// binding. Each entry point mounts the link exactly once, on its own side, so
// swapping the two props (the settings link shown on Cloud, the import link
// on a self-hosted instance) fails here.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const SRC = fileURLToPath(new URL('../../..', import.meta.url));

function mounts(rel: string): string[] {
	const text = readFileSync(SRC + rel, 'utf8');
	return [...text.matchAll(/<MoveToCloudLink\b[^>]*>/g)].map((m) => m[0]);
}

describe('move-to-Cloud entry points', () => {
	it('settings mounts the self-hosted link once', () => {
		const m = mounts('routes/[username]/[workspace]/settings/+page.svelte');
		expect(m).toHaveLength(1);
		expect(m[0]).toMatch(/\bon="self-hosted"/);
	});

	it('the New Workspace dialog mounts the Cloud link once', () => {
		const m = mounts('lib/components/layout/CreateWorkspaceModal.svelte');
		expect(m).toHaveLength(1);
		expect(m[0]).toMatch(/\bon="cloud"/);
	});
});
