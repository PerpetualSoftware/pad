/**
 * BUG-3197, ItemDetail: the SITE PINS for priming the canonical seed.
 *
 * The collab flush dedupes a no-edit view against the editor's own
 * serialization of the seed, memoized on the flush context. The page primes
 * that memo while the editor is alive, because the teardown flush runs after
 * the child editor is destroyed. A missing prime is invisible to every
 * behavioural test today: the flush computes the memo itself as a fallback,
 * and a destroyed Tiptap still serializes. So these pins are source-shaped, and
 * that is their limit: they say the calls are present and untracked, not that
 * the lifecycle orders them (collabFlushCanonical.test.ts owns the memo's
 * semantics, e2e/bug-3197-view-keeps-dialect.spec.ts the end-to-end result).
 */
import { describe, it, expect } from 'vitest';
import { readFenceSource } from '../../../test/identityFenceSource';

const src = readFenceSource(new URL('./ItemDetail.svelte', import.meta.url));
const SCRIPT = src.script;
const MARKUP = src.markup;

function body(startMarker: string, endMarker: string, label: string): string {
	const at = SCRIPT.indexOf(startMarker);
	expect(at, `${label} is gone — re-point this pin`).toBeGreaterThan(-1);
	const end = SCRIPT.indexOf(endMarker, at);
	expect(end, `could not delimit ${label} — re-point this pin`).toBeGreaterThan(at);
	return SCRIPT.slice(at, end);
}

describe('ItemDetail primes the canonical seed while the editor is alive (BUG-3197)', () => {
	it('the helper reads editorInstance and the active context only inside untrack', () => {
		const helper = body('function primeCanonicalSeed(): void {', '\n\t}\n', 'primeCanonicalSeed');
		const inner = helper.indexOf('untrack(() => {');
		expect(inner, 'primeCanonicalSeed no longer untracks: the collab effect would rebuild its provider when the editor mounts').toBeGreaterThan(-1);
		expect(helper.slice(0, inner)).not.toMatch(/editorInstance|activeCollabContext/);
		expect(helper.slice(inner)).toMatch(/collabFlusher\.prime\(activeCollabContext\)/);
	});

	it('context creation primes right after the context becomes active', () => {
		const at = SCRIPT.indexOf('activeCollabContext = ctx;');
		expect(at).toBeGreaterThan(-1);
		const next = SCRIPT.slice(at, SCRIPT.indexOf('\n', SCRIPT.indexOf('\n', at) + 1) + 1);
		expect(next, 'context creation no longer primes the canonical seed').toMatch(/primeCanonicalSeed\(\);/);
	});

	it('every editor mount callback primes after assigning the editor', () => {
		const callbacks = [...MARKUP.matchAll(/onEditor=\{([^}]*\}?)\}/g)].map((m) => m[1]);
		expect(callbacks.length, 'the number of editor mounts changed — decide whether the new one primes').toBe(2);
		for (const cb of callbacks) {
			expect(cb).toMatch(/editorInstance = e;\s*primeCanonicalSeed\(\);/);
		}
	});

	it('the lazy seed resets the memo and re-primes after replacing seedMd', () => {
		const at = SCRIPT.indexOf('ctx.seedMd = unescapeDocLinks(seedMd);');
		expect(at, 'the lazy seed no longer replaces seedMd — re-point this pin').toBeGreaterThan(-1);
		const after = SCRIPT.slice(at, at + 200);
		expect(after).toMatch(/ctx\.seedMd = unescapeDocLinks\(seedMd\);\s*ctx\.seedCanonical = undefined;\s*collabFlusher\.prime\(ctx\);/);
	});
});
