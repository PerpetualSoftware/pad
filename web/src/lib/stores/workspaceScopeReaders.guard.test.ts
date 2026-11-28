// TASK-4902's guard: everything ItemDetail imports reads workspace rights and
// collections through `workspaceScope()`, never through the global stores.
//
// On home the root layout points `workspaceStore` / `collectionStore` at a
// shell workspace, and the item pane there opens items from other
// workspaces. A read of the global stores anywhere in ItemDetail's import
// closure would show that item with the shell workspace's rights; a write
// (`setCurrent`, `loadCollections`, any other mutator) would repoint the
// chrome at the item's workspace. So the scan bans every member of
// `workspaceStore` and every member of `collectionStore` except the classified
// ones below. The members are enumerated from the store sources, so a new
// setter is banned the day it is added.
//
// The same scan lists every persistence write the closure can trigger, each
// classified: whether it records a workspace, and if so which one.
//
// Name-based, like plan3535LifecycleSites.guard.test.ts: what the scope
// answers is proven by workspaceScope.svelte.test.ts and
// ItemDetail.workspaceScope.svelte.test.ts.
import { describe, expect, it } from 'vitest';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

const webRoot = resolve(__dirname, '../../..');
const src = (p: string) => join(webRoot, p);

const ENTRY = 'src/lib/components/items/ItemDetail.svelte';
// The scope forwards to the global stores on workspace routes; the stores
// themselves are not readers.
const EXEMPT = new Set([
	'src/lib/stores/workspaceScope.svelte.ts',
	'src/lib/stores/workspace.svelte.ts',
	'src/lib/stores/collections.svelte.ts',
]);

// `collectionStore` members the closure may use, with why each is not
// workspace state.
const COLLECTION_STORE_ALLOWED: Record<string, string> = {
	activeItem: 'keep: the item this view shows, by id; ChildItems reads it as the parent, which is the pane item in the pane',
	setActiveItem: 'keep: clear-if-owner claim on the open item (TASK-2172); it names an item, not a workspace, and nothing on home reads it',
};

// "path::write" -> [count, classification]. A classification starts with
// "no workspace:" (the key names no workspace) or "scoped:" (it names the
// item's own workspace, which is the scope's slug in the pane).
const PERSISTENCE: Record<string, [number, string]> = {
	'src/lib/a11y/characterShortcuts.svelte.ts::.setItem(': [1, 'no workspace: the per-browser single-key shortcut preference'],
	'src/lib/chat/chatRecords.ts::.setItem(': [1, 'no workspace: chat drafts in sessionStorage, keyed by conversation'],
	'src/lib/collab/wsProvider.svelte.ts::.setItem(': [1, 'no workspace: the collab replay cursor, keyed by item id'],
	'src/lib/items/refusedRawDraft.ts::.setItem(': [2, 'no workspace: a refused raw draft, keyed by user and item id'],
	'src/lib/stores/ui.svelte.ts::.setItem(': [3, 'no workspace: the topbar open/closed preference'],
	'src/lib/components/items/ItemDetail.svelte::localIndex.bootstrap(': [1, 'scoped: the local index for the wsSlug prop, which the home pane sets from item_in, the scope slug'],
	'src/lib/components/items/ItemDetail.svelte::tabsStore.setRoute(': [1, 'scoped: repairs the stored last route of reqWsSlug (the wsSlug prop) only when it names this dead item; never records a visit'],
};
const PERSISTENCE_WRITES = [
	/\.setItem\(/g,
	/\btabsStore\.(?:setRoute|open|land|close|reorder|pin)\(/g,
	/\blastWorkspace\.(?:note|clear)\(/g,
	/\blocalIndex\.bootstrap\(/g,
];

function stripComments(s: string): string {
	return s
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/^[ \t]*\/\/.*$/gm, '');
}

function resolveSpec(from: string, spec: string): string | null {
	let base: string;
	if (spec.startsWith('$lib/')) base = src(join('src/lib', spec.slice(5)));
	else if (spec.startsWith('.')) base = resolve(dirname(from), spec);
	else return null;
	for (const c of [base, `${base}.ts`, `${base}.js`, `${base}.svelte.ts`, join(base, 'index.ts')]) {
		if (existsSync(c) && statSync(c).isFile()) return c;
	}
	return null;
}

// Runtime imports only: `import type` / `export type` load nothing.
const IMPORT = /\b(import|export)(\s+type)?\s[^;'"]*?from\s+['"]([^'"]+)['"]|import\(\s*['"]([^'"]+)['"]\s*\)|\bimport\s+['"]([^'"]+)['"]/g;

function closure(entry: string): Map<string, string> {
	const out = new Map<string, string>();
	const queue = [src(entry)];
	while (queue.length) {
		const file = queue.pop()!;
		const rel = relative(webRoot, file).split('\\').join('/');
		if (out.has(rel)) continue;
		const text = stripComments(readFileSync(file, 'utf8'));
		out.set(rel, text);
		for (const m of text.matchAll(IMPORT)) {
			if (m[2]) continue;
			const next = resolveSpec(file, m[3] ?? m[4] ?? m[5]);
			if (next) queue.push(next);
		}
	}
	return out;
}

/** Members of `export const <name> = { ... };`, from the store source. */
function storeMembers(file: string, name: string): string[] {
	const text = readFileSync(src(file), 'utf8');
	const start = text.indexOf(`export const ${name} = {`);
	expect(start, `${name} not found in ${file}`).toBeGreaterThanOrEqual(0);
	const end = text.indexOf('\n};', start);
	const body = text.slice(start, end);
	const names = [...body.matchAll(/^\t(?:get\s+|async\s+)?([A-Za-z_$][\w$]*)\s*\(/gm)].map((m) => m[1]);
	return [...new Set(names)];
}

describe('ItemDetail import closure reads workspace state through workspaceScope (TASK-4902)', () => {
	const files = closure(ENTRY);
	const workspaceMembers = storeMembers('src/lib/stores/workspace.svelte.ts', 'workspaceStore');
	const collectionMembers = storeMembers('src/lib/stores/collections.svelte.ts', 'collectionStore');

	it('enumerates the store members, setters included', () => {
		expect(files.size).toBeGreaterThan(50);
		for (const m of ['current', 'isOwner', 'currentMembership', 'canEditItem', 'canEditCollection', 'setCurrent', 'loadAll', 'create']) {
			expect(workspaceMembers).toContain(m);
		}
		for (const m of ['collections', 'collectionsWorkspace', 'collectionsAreFreshFor', 'loadCollections', 'ensureCollections', 'loadItems', 'addItem', 'clear']) {
			expect(collectionMembers).toContain(m);
		}
		for (const m of Object.keys(COLLECTION_STORE_ALLOWED)) expect(collectionMembers).toContain(m);
	});

	it('uses no workspaceStore member and imports no workspaceStore', () => {
		const problems: string[] = [];
		for (const [rel, text] of files) {
			if (EXEMPT.has(rel)) continue;
			if (/\bimport\s*\{[^}]*\bworkspaceStore\b[^}]*\}\s*from/.test(text)) problems.push(`${rel}: imports workspaceStore`);
			for (const m of text.matchAll(/\bworkspaceStore\s*\.\s*([A-Za-z_$][\w$]*)/g)) {
				problems.push(`${rel}: workspaceStore.${m[1]}${workspaceMembers.includes(m[1]) ? '' : ' (not an enumerated member)'}`);
			}
		}
		expect(problems, 'read and write workspace state through workspaceScope()').toEqual([]);
	});

	it('uses no collectionStore member outside the classified ones', () => {
		const banned = collectionMembers.filter((m) => !(m in COLLECTION_STORE_ALLOWED));
		expect(banned).toContain('loadCollections');
		const problems: string[] = [];
		for (const [rel, text] of files) {
			if (EXEMPT.has(rel)) continue;
			for (const m of text.matchAll(/\bcollectionStore\s*\.\s*([A-Za-z_$][\w$]*)/g)) {
				if (!(m[1] in COLLECTION_STORE_ALLOWED)) problems.push(`${rel}: collectionStore.${m[1]}`);
			}
			if (/\bcollectionStore\b(?!\s*[.}])/.test(text.replace(/import\s*\{[^}]*\}\s*from[^;\n]*/g, ''))) {
				problems.push(`${rel}: collectionStore passed as a value`);
			}
		}
		expect(problems, 'read and write collections through workspaceScope()').toEqual([]);
	});

	it('classifies every persistence write the closure can trigger', () => {
		const found: Record<string, number> = {};
		for (const [rel, text] of files) {
			for (const re of PERSISTENCE_WRITES) {
				for (const m of text.matchAll(re)) {
					const api = m[0].startsWith('.setItem') ? '.setItem(' : m[0];
					const key = `${rel}::${api}`;
					found[key] = (found[key] ?? 0) + 1;
				}
			}
		}
		const problems: string[] = [];
		for (const [key, n] of Object.entries(found)) {
			const site = PERSISTENCE[key];
			if (!site) problems.push(`unclassified persistence write: ${key} (${n})`);
			else if (site[0] !== n) problems.push(`${key}: ${n} write(s), classified for ${site[0]}`);
		}
		for (const key of Object.keys(PERSISTENCE)) if (!(key in found)) problems.push(`classified write no longer exists: ${key}`);
		for (const [key, [, why]] of Object.entries(PERSISTENCE)) {
			if (!/^(no workspace|scoped): /.test(why)) problems.push(`${key}: classification must start "no workspace: " or "scoped: "`);
		}
		expect(problems).toEqual([]);
	});

	it('never notes a last-visited workspace', () => {
		const hits = [...files].filter(([, text]) => /\blastWorkspace\b|\bpickShellWorkspace\b/.test(text)).map(([rel]) => rel);
		expect(hits).toEqual([]);
	});
});
