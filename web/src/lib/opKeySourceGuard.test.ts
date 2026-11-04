// BUG-4312: a bare crypto.randomUUID() call throws in a non-secure context
// (plain http on a LAN address). The chat composer cleared the person's text
// and sent nothing. Every call site goes through newOpKey(), or sits behind
// its own typeof check; this reads the sources so a new bare call turns red.
import { describe, expect, it } from 'vitest';

const sources = import.meta.glob(['/src/**/*.ts', '/src/**/*.svelte', '!/src/**/*.test.ts', '!/src/**/*.d.ts'], {
	query: '?raw',
	import: 'default',
	eager: true
}) as Record<string, string>;

// Files that call it only after `typeof crypto.randomUUID === 'function'`.
const GUARDED = ['lib/attachments/events.ts', 'lib/components/editor/attachment-upload.ts', 'lib/items/clientWrite.ts', 'lib/opKey.ts'];

describe('crypto.randomUUID', () => {
	it('finds the sources it guards', () => {
		expect(Object.keys(sources).some((n) => n.endsWith('lib/stores/chatThread.svelte.ts'))).toBe(true);
	});

	it('is never called bare outside a typeof-guarded file', () => {
		const bare = Object.entries(sources)
			.filter(([name]) => !GUARDED.some((g) => name.endsWith(g)))
			.filter(([, src]) => /\brandomUUID\s*\(/.test(src.replace(/\/\/.*$/gm, '').replace(/\/\*[\s\S]*?\*\//g, '')))
			.map(([name]) => name);
		expect(bare, 'use newOpKey() from $lib/opKey').toEqual([]);
	});

	it('the guarded files do check before calling', () => {
		for (const g of GUARDED) {
			const entry = Object.entries(sources).find(([name]) => name.endsWith(g));
			expect(entry, g).toBeDefined();
			expect(entry![1], g).toMatch(/typeof\s+\w+(?:\.randomUUID|\s*!==\s*'undefined')/);
		}
	});
});
