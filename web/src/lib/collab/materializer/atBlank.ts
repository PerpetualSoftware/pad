// VENDORED PATCH of prosemirror-markdown `MarkdownSerializerState.atBlank`
// (TASK-2198; lead ruling, not filed upstream).
//
// Upstream (prosemirror-markdown 1.13.4, dist/index.js):
//
//     atBlank() { return /(^|\n)$/.test(this.out); }
//
// runs a regex over the WHOLE accumulated output on every call, and the
// serializer calls it for every block it closes, so a document's cost is
// quadratic in its length. V8 optimises the anchored test away; goja does not,
// and the spike's profile had it at ~90% of CPU on large documents.
//
// Equivalence: with neither the `m` nor the `s` flag, `^` matches only at
// index 0 and `$` only at the end of input (ECMAScript `$` does NOT match
// before a trailing newline, unlike Perl/PCRE). So the regex is true exactly
// when the string is empty or its last code unit is LF. `RegExp.prototype.test`
// coerces its argument with ToString, which `String(...)` reproduces for a
// non-string `out` (it is always a string in practice).
//
// tiptap-markdown's MarkdownSerializerState subclasses this one and does not
// override atBlank, so patching the base prototype reaches the editor's
// serializer. Guarded by internal/materialize (atBlankPatched() must be true
// in the embedded bundle) and by atBlank.test.ts (agreement with the saved
// upstream implementation over generated states).
import { MarkdownSerializerState } from 'prosemirror-markdown';

type AtBlank = (this: { out: unknown }) => boolean;

const proto = (MarkdownSerializerState as unknown as { prototype: { atBlank: AtBlank } }).prototype;

/** The upstream implementation, saved before the patch is installed. */
export const upstreamAtBlank: AtBlank = proto.atBlank;

export const patchedAtBlank: AtBlank = function (this: { out: unknown }) {
	const o = String(this.out);
	return o.length === 0 || o.charCodeAt(o.length - 1) === 10;
};

/** Install the patch on the shared prototype. Idempotent. */
export function installAtBlankPatch(): void {
	proto.atBlank = patchedAtBlank;
}

/** True iff the prototype currently carries the patched method. */
export function atBlankPatched(): boolean {
	return proto.atBlank === patchedAtBlank;
}

/**
 * Run `fn` with the UPSTREAM atBlank on the prototype, restoring whatever was
 * there afterwards. Tests use it to produce the live editor's output with the
 * serializer exactly as a browser tab runs it, in a process where the bundle
 * entry has installed the patch.
 */
export function withUpstreamAtBlank<T>(fn: () => T): T {
	const prev = proto.atBlank;
	proto.atBlank = upstreamAtBlank;
	try {
		return fn();
	} finally {
		proto.atBlank = prev;
	}
}
