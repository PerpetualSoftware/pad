// The inline image NodeView owns NO overlay (PLAN-2392 phase 3a / TASK-2433).
//
// This is a STATIC check, deliberately, because the thing it guards is an
// absence and a behavioural test cannot see one. TASK-2433 deleted
// `openImageLightbox` / `closeLightbox` — a hand-rolled `<dialog>` the NodeView
// appended to `document.body` and `showModal()`d — and routed activation onto
// the viewer channel instead, so the `Lightbox` that `AttachmentViewerHost`
// mounts is the only viewer on THIS route. Everything the modal contract is
// made of lives there: the lease-stacked backdrop, the focus trap and restore, the Escape
// ordering, and the DR-16 filter re-applied over the whole set.
//
// A future edit that "just pops a quick preview" from the NodeView would
// silently reintroduce a second viewer with none of that — and it would pass
// every PRODUCER spec in this directory, because those assert what the NodeView
// emits and a stray overlay emits nothing. `attachmentImageViewerHost.svelte.test.ts`
// asserts a real viewer appears, which is the other half, but it counts
// `.lightbox-backdrop` roots and would not see an extra overlay of a different
// shape either. Hence: read the source.
//
// The CSS half is checked here too. `.attachment-image-lightbox` lived in
// `app.css`, not in the TS file, so a JS-only sweep would have left a rule for
// a dialog nothing constructs — dead weight that also reads, to the next
// person, as evidence the dialog is still a supported surface.
import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const SRC = fileURLToPath(new URL('../../..', import.meta.url));
const NODEVIEW = join(SRC, 'lib/components/editor/attachment-image.ts');

/** Source with block and line comments stripped — the prose above mentions
 *  every banned token by name, and a check that matched its own warning label
 *  would be unfalsifiable. */
function code(path: string): string {
	return readFileSync(path, 'utf8')
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/^\s*\/\/.*$/gm, '');
}

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		if (name === 'node_modules' || name === '.svelte-kit') continue;
		const full = join(dir, name);
		if (statSync(full).isDirectory()) walk(full, out);
		else if (/\.(ts|js|svelte|css)$/.test(name)) out.push(full);
	}
	return out;
}

describe('attachment-image.ts defines no overlay of its own', () => {
	const source = code(NODEVIEW);

	it('constructs no dialog and opens no modal', () => {
		expect(source).not.toMatch(/createElement\(\s*['"]dialog['"]\s*\)/);
		expect(source).not.toMatch(/\bshowModal\b/);
		expect(source).not.toMatch(/\bHTMLDialogElement\b/);
	});

	it('reaches for nothing on the document but the two things it needs', () => {
		// An ALLOWLIST, not a list of forbidden names. `document.body` was the
		// deleted lightbox's route out of this subtree, but `document['body']`,
		// `document.querySelector('body')` and `document.getElementById(…)` are
		// the same move spelled differently, and a blacklist only ever catches
		// the spellings someone thought of. This NodeView legitimately needs
		// exactly two members — one to build its own DOM, one to check where
		// focus is — so anything else is a reach for a root it does not own.
		const allowedMembers = new Set(['createElement', 'activeElement']);
		const members = Array.from(source.matchAll(/\bdocument\s*\.\s*([A-Za-z_$][\w$]*)/g)).map(
			(m) => m[1]
		);
		expect(members.length).toBeGreaterThan(0);
		expect(members.filter((m) => !allowedMembers.has(m))).toEqual([]);
		// Computed access would slip straight past the allowlist above.
		expect(source).not.toMatch(/\bdocument\s*\[/);

		// Named roots are only the obvious half. The check that survives a
		// creative reimplementation is on the RECEIVER of every insertion —
		// `someParent.appendChild(overlay)`, a portal into a captured root —
		// where a blacklist of names would only catch the ones already thought
		// of.
		//
		// The rule is DERIVED, not a list of blessed identifiers: every receiver
		// must be an element this file CREATED. Combined with the member
		// allowlist above, that is what confines the whole NodeView to its own
		// subtree — a locally created element has no route into the document
		// except the `dom` this NodeView returns, so anything appended into one
		// is inside what ProseMirror mounts. Renaming `wrapper` to `container`
		// keeps passing; appending into anything that arrived from elsewhere
		// does not.
		const created = new Set(
			Array.from(source.matchAll(/\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)[^=\n]*=\s*document\.createElement\(/g)).map((m) => m[1])
		);
		const INSERTERS = 'append|appendChild|prepend|insertBefore|replaceChildren|insertAdjacentElement';
		// The receiver is the WHOLE expression, dots included, so a reach through
		// something this file was handed (`opts.host.appendChild(overlay)`) is a
		// receiver of `opts.host` — not in `created`, and not silently skipped
		// the way a bare-identifier pattern would skip it.
		const receivers = Array.from(
			source.matchAll(new RegExp(String.raw`([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\.(?:${INSERTERS})\(`, 'g'))
		).map((m) => m[1]);
		expect(created.size).toBeGreaterThan(0);
		expect(receivers.length).toBeGreaterThan(0);
		expect(receivers.filter((r) => !created.has(r))).toEqual([]);
		// And the same call spelled computed (`x['appendChild'](overlay)`), which
		// no dot-pattern can see.
		expect(source).not.toMatch(new RegExp(String.raw`\[\s*['"\`](?:${INSERTERS})['"\`]\s*\]`));
		// WHAT THIS CANNOT SEE, stated so nobody mistakes it for a proof. It is
		// a regex over source: it has no scopes, no bindings and no call graph.
		// An overlay built by an imported helper is invisible to it, and so is
		// a name that `created` knows about being REASSIGNED to something
		// foreign before it is appended into. Closing those needs an AST pass
		// with binding resolution, which is a disproportionate amount of
		// machinery for a guard whose job is to make a deliberate re-addition
		// of the deleted dialog fail loudly. The narrower claim it does make —
		// this file appends only into elements it created, and reaches for
		// nothing on the document but `createElement` and `activeElement` — is
		// the one that would have caught the code this task removed.
	});

	it('names the deleted lightbox nowhere', () => {
		expect(source).not.toMatch(/openImageLightbox|closeLightbox|attachment-image-lightbox/);
	});

	it('routes activation through the viewer channel instead', () => {
		// The other half of the same claim: "no overlay" is only the right
		// absence if something else opens the viewer. A file that deleted the
		// dialog and emitted nothing satisfies every assertion above.
		expect(source).toMatch(/notifyViewerOpen\(/);
	});

	it('leaves no reference to the lightbox class anywhere in the app', () => {
		// Including `app.css`, whose rules a JS-only sweep does not see, and the
		// specs — a test still counting `dialog.attachment-image-lightbox` would
		// be counting an element nothing can create, i.e. asserting 0 forever.
		const offenders = walk(SRC)
			// This file, which cannot check for a name without naming it.
			.filter((f) => f !== fileURLToPath(import.meta.url))
			.filter((f) => readFileSync(f, 'utf8').includes('attachment-image-lightbox'))
			.map((f) => f.slice(SRC.length));
		expect(offenders).toEqual([]);
	});
});
