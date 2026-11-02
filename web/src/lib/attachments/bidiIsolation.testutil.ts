/**
 * Shared fixtures and assertions for the file-name bidi-isolation tests
 * (TASK-4195). Every bidi control is written as an escape: no literal
 * right-to-left override or isolate character may appear in the source.
 */
import { expect } from 'vitest';

/** FSI / PDI: the first-strong isolate pair `isolateText` emits. */
export const FSI = String.fromCharCode(0x2068);
export const PDI = String.fromCharCode(0x2069);

/** A name carrying a right-to-left override (U+202E): "evil" + RLO + "gnp.exe". */
export const EVIL_NAME = 'evil' + String.fromCharCode(0x202e) + 'gnp.exe';

/** The text node under `root` whose data contains `name`. */
function textNodeContaining(root: Node, name: string): Text | null {
	const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
	let n: Node | null;
	while ((n = walker.nextNode())) {
		if ((n as Text).data.includes(name)) return n as Text;
	}
	return null;
}

/** The name's text node sits inside a `<bdi>` (visible-text sites). */
export function expectIsolated(root: Node, name: string): void {
	const node = textNodeContaining(root, name);
	expect(node, `no text node containing the name`).not.toBeNull();
	expect(node!.parentElement!.closest('bdi'), 'name text is not inside a <bdi>').not.toBeNull();
}

/** The string carries FSI, then the name, then PDI (attribute and toast sites). */
export function expectFsiPdi(str: string | null | undefined, name: string): void {
	expect(str ?? '').toContain(FSI + name + PDI);
}

/** The string carries no isolate controls (data sites: download=, node attrs). */
export function expectNoIsolates(str: string | null | undefined): void {
	expect(str ?? '').not.toContain(FSI);
	expect(str ?? '').not.toContain(PDI);
}
