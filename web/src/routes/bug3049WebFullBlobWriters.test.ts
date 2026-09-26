// BUG-3049 — the six browser doors that write item fields must send a
// `fields_patch` naming only the keys they own, never a full `fields` blob.
//
// WHY A SOURCE GUARD, and what it cannot do. These six writes live inside route
// pages and ItemDetail, none of which can be mounted without standing up the
// whole workspace shell (the same reason given in
// [collection]/groupReorderRelationGuard.test.ts). So this file checks the
// SPELLING at each call site: a `fields_patch` argument and no `fields:`
// argument. It cannot prove the write behaves — a patch naming the wrong key,
// or a call made unreachable by an earlier return, passes here.
//
// What connects these spellings to a measured consequence, so they are not
// self-agreeing:
//   - internal/store/items_fieldpatch_test.go pins that fields_patch MERGES
//     under the row lock and that a null DELETES (the semantics these doors
//     now depend on).
//   - internal/server/bug3049_bulk_field_patch_test.go and
//     cmd/pad/bug3049_cli_field_patch_test.go prove the outcome — the
//     unmentioned key survives — at three doors of the same class, with a
//     concurrent write really landing between the read and the write.
//   - web/src/lib/webmcp/bug3055FieldsFullReplace.test.ts does the same for the
//     browser MCP door, which IS mountable.
//
// Every guard below was mutation-controlled: reverting its own door to a full
// blob fails that guard and no other.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

function source(relPath: string): string {
	// Comments stripped so a `fields:` mentioned in prose (there are several,
	// including in the very comments this fix added) cannot satisfy or trip a
	// guard. Line comments only — the block-comment form does not appear in
	// these call sites.
	return readFileSync(resolve(__dirname, relPath), 'utf8').replace(/^[ \t]*\/\/.*$/gm, '');
}

/** The body of `functionName`, from its declaration to the first line that
 *  closes it at the given indent depth. */
function functionBody(src: string, declaration: string, closer = '\n\t}'): string {
	const start = src.indexOf(declaration);
	expect(start, `${declaration} was renamed or removed — re-point this guard`).toBeGreaterThan(-1);
	const end = src.indexOf(closer, start);
	expect(end, `could not find the end of ${declaration}`).toBeGreaterThan(start);
	return src.slice(start, end);
}

/** Assert one door: it PATCHES the named keys and never sends a full blob. */
function expectPatchOnly(body: string, door: string, keys: string[]) {
	expect(body, `${door}: no fields_patch — this door still writes a full blob`).toContain(
		'fields_patch',
	);
	expect(
		/\bfields:\s/.test(body),
		`${door}: still passes a full \`fields:\` blob to the update call`,
	).toBe(false);
	for (const key of keys) {
		expect(body, `${door}: the patch does not name ${key}`).toContain(key);
	}
}

describe('BUG-3049 — browser item-field writers send a patch, not a blob', () => {
	it('ItemDetail source-URL stamping patches its two keys', () => {
		const body = functionBody(
			source('../lib/components/items/ItemDetail.svelte'),
			'async function stampSourceUrl(',
		);
		expectPatchOnly(body, 'stampSourceUrl', ['pad_source_url', 'pad_imported_at']);
		// The re-read this door used to do existed only to build the blob. It
		// narrowed the window and could not close it, so its removal is part of
		// the fix rather than incidental.
		expect(body, 'the pre-write re-read is back — a patch makes it pointless').not.toContain(
			'await api.items.get(',
		);
	});

	it('the collection page status move patches only the group field', () => {
		// The patched KEY is what this leg is about, and it is unchanged. The
		// VALUE spelling moved under BUG-3057: the lane key is now converted
		// through the field's declared type before it is written, because
		// assigning the raw key sent a string to a number / checkbox /
		// multi_select field and the server refuses those writes. `laneWrite`
		// IS `newValue` converted, so "patches only the group field" holds
		// exactly as before — only the literal this guard anchors on changed.
		const body = functionBody(
			source('./[username]/[workspace]/[collection]/+page.svelte'),
			'async function handleStatusChange(',
		);
		expectPatchOnly(body, 'handleStatusChange', ['[fieldKey]: laneWrite.value']);
	});

	it('the conventions status toggle patches only status', () => {
		const body = functionBody(
			source('./[username]/[workspace]/conventions/+page.svelte'),
			'async function toggleStatus(',
		);
		expectPatchOnly(body, 'conventions toggleStatus', ['status: newStatus']);
	});

	it('the conventions bulk group toggle patches only status', () => {
		const body = functionBody(
			source('./[username]/[workspace]/conventions/+page.svelte'),
			'async function bulkToggleGroup(',
		);
		expectPatchOnly(body, 'bulkToggleGroup', ['status: targetStatus']);
	});

	it('the playbooks list status toggle patches only status', () => {
		const body = functionBody(
			source('./[username]/[workspace]/playbooks/+page.svelte'),
			'async function toggleStatus(',
		);
		expectPatchOnly(body, 'playbooks toggleStatus', ['status: next']);
	});

	it('the playbook editor patches the five keys it owns, and clears the slug with null', () => {
		// The patch is built in $lib/playbooks/editorPatch since BUG-3075, which
		// sends only the keys the user CHANGED (editorPatch.test.ts pins that). This
		// guard keeps its question: a patch, never a blob, naming only these keys.
		const body = functionBody(
			source('./[username]/[workspace]/playbooks/[slug]/+page.svelte'),
			'async function save(',
		);
		expectPatchOnly(body, 'playbook editor save', ['playbookFieldsPatch(']);
		const builder = source('../lib/playbooks/editorPatch.ts');
		for (const key of ['patch.status', 'patch.trigger', 'patch.scope', 'patch.arguments', 'patch.invocation_slug']) {
			expect(builder, `the editor patch no longer names ${key}`).toContain(key);
		}
		// The clear is a null (a patch delete), not a `delete` on a spread blob:
		// storing "" would hit the unique index.
		expect(builder, 'the invocation_slug clear is no longer a null patch value').toContain(
			'invocation_slug = trimmedSlug ? trimmedSlug : null',
		);
		// The blob spread is what made this door revert concurrent writes.
		expect(body, 'the editor is spreading the loaded fields again').not.toContain(
			'...parseFields(item)',
		);
	});
});

// BUG-3050 U1, door A4: ItemDetail's raw-editor fallback. The decision lives in
// $lib/items/rawSeed (rawSeed.test.ts forces every fallback: no provider, a
// thrown flush, the loop cap, a deduped flush while another tab typed). This
// guard pins the WIRING, which that test cannot see because ItemDetail cannot
// be mounted: the switch asks rawSeedDecision, ahead of the mode flip, with the
// editor read AFTER the flush loop.
describe('BUG-3050 U1: the raw-editor fallback does not seed from a stale body', () => {
	it('the raw toggle refuses through rawSeedDecision before rawMode = true', () => {
		const src = source('../lib/components/items/ItemDetail.svelte');
		const guard = src.indexOf('rawSeedDecision({ liveNow, lastFlushed: lastFlushedOut, stored: item.content ?? \'\', contentState: item.content_state }).refuse');
		expect(guard, 'the raw toggle no longer asks rawSeedDecision').toBeGreaterThan(-1);
		const flip = src.indexOf('rawMode = true;', guard);
		expect(flip, 'the refusal no longer precedes the raw-mode flip').toBeGreaterThan(guard);
		expect(src.slice(guard, flip), 'the refusal no longer returns before the flip').toContain('return;');
		const read = src.indexOf('liveNow = typeof read === \'string\' ? read : undefined;');
		const loopEnd = src.indexOf('if (aborted) return;');
		expect(read, 'the live read is gone').toBeGreaterThan(-1);
		expect(read, 'the live read no longer happens AFTER the flush loop').toBeGreaterThan(loopEnd);
		expect(read, 'the live read no longer precedes the decision').toBeLessThan(guard);
	});
});
