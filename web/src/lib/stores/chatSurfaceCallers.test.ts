// TASK-4315 PR 3b: source rules for the callers of chatSurface, pinned by reading
// the shipped sources (jsdom cannot mount every page that calls it).
//
// 1. A chatSurface call that ACTS (it may issue a navigation, settle a latch or
//    run a parked continuation) must not run in a tracked effect body: the read of
//    page.shallow / page.state inside it would subscribe the effect to Chat opening
//    and closing, and the actions can write state the effect reads (the Svelte 5
//    self-invalidating-effect trap in CLAUDE.md). Wrap the call in untrack().
// 2. A deferred write that navigates must RETURN its navigation, because the
//    surface runs deferred writes one after another and waits on each returned
//    promise before starting the next: two writes that both rebuild the URL from
//    page.url would otherwise overwrite one another.
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const SRC = fileURLToPath(new URL('../..', import.meta.url));

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) walk(p, out);
		else if (/\.(svelte|ts)$/.test(name)) out.push(p);
	}
	return out;
}

function shipped(): Array<{ file: string; text: string }> {
	return walk(SRC)
		.map((p) => ({ file: relative(SRC, p).split('\\').join('/'), text: readFileSync(p, 'utf8') }))
		.filter(({ file }) => !file.startsWith('test/') && !/\.(test|spec)\./.test(file));
}

/** Index of the paren matching the one at `open`, skipping strings, templates and comments. */
function matchParen(s: string, open: number): number {
	let depth = 0;
	for (let i = open; i < s.length; i++) {
		const c = s[i];
		if (c === '/' && s[i + 1] === '/') i = s.indexOf('\n', i) < 0 ? s.length : s.indexOf('\n', i);
		else if (c === '/' && s[i + 1] === '*') i = s.indexOf('*/', i + 2) + 1;
		else if (c === "'" || c === '"' || c === '`') {
			for (i++; i < s.length && s[i] !== c; i++) if (s[i] === '\\') i++;
		} else if (c === '(') depth++;
		else if (c === ')' && --depth === 0) return i;
	}
	return s.length;
}

function spans(s: string, opener: RegExp): Array<[number, number]> {
	const out: Array<[number, number]> = [];
	for (const m of s.matchAll(opener)) {
		const open = m.index! + m[0].length - 1;
		out.push([open, matchParen(s, open)]);
	}
	return out;
}

// The members that ACT. Pure reads (target, isOpen, depth, routeMode, hasDeferredWrite) may be tracked.
const ACTS = /chatSurface\.(open|close|back|closeThen|runAfterClose|deferPageWrite|traversing|handleLinkClick|dropDeferredExcept)\b/g;

/**
 * Offsets of the `calls` that run DIRECTLY in the body of the effect whose call parens are
 * [open, close]: not inside a function written within it (an untrack() callback, an event or SSE
 * handler, a helper), which does not run as a tracked read. The effect's own callback is the root.
 * A function written directly as an argument to a synchronous iterator (forEach, map, filter, some,
 * every, find, reduce, flatMap, sort, Array.from) or as an immediately-invoked group runs in the effect,
 * so it is NOT exempt; an untrack(), a timer, a handler or a promise callback is.
 * Limit: a function that reaches a synchronous caller by name (const f = () => chatSurface.close();
 * items.forEach(f)), a helper declared in the effect and called there, and a custom higher-order
 * function that calls its callback synchronously, are still read as nested and exempt.
 */
const SYNC_CALLEE = /\.(?:forEach|map|filter|some|every|find|findIndex|findLast|findLastIndex|reduce|reduceRight|flatMap|sort|toSorted|from)$/;

function directCalls(s: string, open: number, close: number, calls: Set<number>): number[] {
	type Frame = { closer: string; fn: boolean; root: boolean; start: number; expr?: boolean; sync?: boolean };
	const stack: Frame[] = [];
	const out: number[] = [];
	let rootDone = false;
	let lastParenStart = -1;
	const before = (i: number) => s.slice(Math.max(0, i - 80), i).trimEnd();
	// The function being opened is an argument written directly in a synchronous iterator's call
	// parens, or the body of an immediately-invoked group.
	const runsSync = () => stack.length > 0 && !!stack[stack.length - 1].sync;
	const endExpr = () => {
		while (stack.length && stack[stack.length - 1].expr) stack.pop();
	};
	for (let i = open + 1; i < close; i++) {
		const c = s[i];
		if (calls.has(i) && !stack.some((f) => f.fn && !f.root && !f.sync)) out.push(i);
		if (c === '/' && s[i + 1] === '/') {
			const e = s.indexOf('\n', i);
			i = e < 0 ? close : e;
		} else if (c === '/' && s[i + 1] === '*') {
			i = s.indexOf('*/', i + 2) + 1;
		} else if (c === "'" || c === '"' || c === '`') {
			for (i++; i < close && s[i] !== c; i++) if (s[i] === '\\') i++;
		} else if (c === '=' && s[i + 1] === '>') {
			let j = i + 2;
			while (/\s/.test(s[j])) j++;
			if (s[j] !== '{') {
				stack.push({ closer: '', fn: true, root: !rootDone && stack.length === 0, start: i, expr: true, sync: runsSync() });
				rootDone = true;
			}
			i++;
		} else if (c === '(' || c === '[') {
			const group = c === '(' && !/[\w$)\]]$/.test(before(i));
			stack.push({ closer: c === '(' ? ')' : ']', fn: false, root: false, start: i, sync: c === '(' && (SYNC_CALLEE.test(before(i)) || group) });
		} else if (c === '{') {
			const b = before(i);
			let fn = b.endsWith('=>');
			if (!fn && b.endsWith(')') && lastParenStart >= 0) {
				fn = !/\b(if|for|while|switch|catch|with)$/.test(before(lastParenStart));
			}
			stack.push({ closer: '}', fn, root: fn && !rootDone && stack.length === 0, start: i, sync: fn && runsSync() });
			if (fn) rootDone = true;
		} else if (c === ',' || c === ';') {
			endExpr();
		} else if (c === ')' || c === ']' || c === '}') {
			endExpr();
			const f = stack.pop();
			if (f && f.closer === ')') lastParenStart = f.start;
		}
	}
	return out;
}

function actingCallsInEffects(text: string): number[] {
	const calls = new Set([...text.matchAll(ACTS)].map((m) => m.index!));
	if (calls.size === 0) return [];
	const bad: number[] = [];
	for (const [a, b] of spans(text, /\$(?:effect(?:\.pre)?|derived(?:\.by)?)\(/g)) bad.push(...directCalls(text, a, b, calls));
	return bad;
}

describe('chatSurface calls that act never run directly in a tracked effect or derived body', () => {
	it('none in the shipped sources', () => {
		const bad: string[] = [];
		for (const { file, text } of shipped()) {
			for (const at of actingCallsInEffects(text)) bad.push(`${file}: ${text.slice(at, at + 40).split('(')[0]} (line ${text.slice(0, at).split('\n').length})`);
		}
		expect(bad, 'wrap the call in untrack(() => ...)').toEqual([]);
	});

	it('the classifier: direct is flagged; untrack, a handler and an expression arrow are not', () => {
		const direct = '$effect(() => {\n if (a) chatSurface.deferPageWrite("k", f);\n});';
		const untracked = '$effect(() => {\n if (a) untrack(() => chatSurface.deferPageWrite("k", f));\n});';
		const handler = '$effect(() => {\n const off = sse.on((e) => {\n if (x) { chatSurface.runAfterClose(() => {}); }\n });\n return off;\n});';
		const fnDecl = '$effect(() => {\n function heal() {\n chatSurface.closeThen(f);\n }\n run(heal);\n});';
		const exprArrow = '$effect(() => untrack(() => chatSurface.traversing));';
		const readOnly = '$effect(() => {\n const t = chatSurface.target;\n});';
		expect(actingCallsInEffects(direct)).toHaveLength(1);
		expect(actingCallsInEffects(untracked)).toEqual([]);
		expect(actingCallsInEffects(handler)).toEqual([]);
		expect(actingCallsInEffects(fnDecl)).toEqual([]);
		expect(actingCallsInEffects(exprArrow)).toEqual([]);
		expect(actingCallsInEffects(readOnly)).toEqual([]);
		// the root callback of an effect whose body is a bare expression
		expect(actingCallsInEffects('$effect(() => chatSurface.close());')).toHaveLength(1);
	});

	it('a callback a synchronous iterator runs still runs in the effect; timers, handlers and .then do not', () => {
		const wrap = (b: string) => `$effect(() => {\n ${b}\n});`;
		expect(actingCallsInEffects(wrap('items.forEach(() => chatSurface.close());'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('items.forEach((i) => { if (i) { chatSurface.close(); } });'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('items.map(function (i) { return chatSurface.open(i); });'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('items.filter((i) => chatSurface.closeThen(i));'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('items.some((i) => chatSurface.back());'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('items.reduce((a, i) => a + chatSurface.traversing, 0);'))).toHaveLength(1);
		expect(actingCallsInEffects(wrap('(() => { chatSurface.close(); })();'))).toHaveLength(1);
		expect(actingCallsInEffects('$effect(() => items.forEach(() => chatSurface.close()));')).toHaveLength(1);
		expect(actingCallsInEffects(wrap('setTimeout(() => chatSurface.close(), 10);'))).toEqual([]);
		expect(actingCallsInEffects(wrap('el.onclick = () => { chatSurface.close(); };'))).toEqual([]);
		expect(actingCallsInEffects(wrap('p.then(() => chatSurface.close());'))).toEqual([]);
		expect(actingCallsInEffects(wrap('items.forEach(() => setTimeout(() => chatSurface.close(), 1));'))).toEqual([]);
		expect(actingCallsInEffects(wrap('items.forEach(() => untrack(() => chatSurface.close()));'))).toEqual([]);
	});

	it('the scan reaches the effects it polices', () => {
		const t = shipped().find((x) => x.file === 'routes/[username]/[workspace]/[collection]/[slug]/+page.svelte')!.text;
		expect(spans(t, /\$effect\(/g).length).toBeGreaterThan(0);
		expect(t.includes('chatSurface.runAfterClose(')).toBe(true);
	});
});

describe('a deferred write that navigates returns its navigation', () => {
	const read = (file: string) => shipped().find((s) => s.file === file)!.text;

	it('activity dropFromUrl returns the goto (and its fallback) so the next write waits', () => {
		const t = read('routes/[username]/[workspace]/activity/+page.svelte');
		const fn = t.slice(t.indexOf('function dropFromUrl('), t.indexOf('function clearSince('));
		expect(fn).toMatch(/return goto\(/);
	});

	it('admin syncURL returns the goto', () => {
		const t = read('routes/(console)/settings/admin/+page.svelte');
		const fn = t.slice(t.indexOf('function syncURL('));
		expect(fn.slice(0, fn.indexOf('\n\t}\n'))).toMatch(/return goto\(/);
	});
});
