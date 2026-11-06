// Test support for lib/chatHistorySites.test.ts: finds every executable site in
// web/src that can write or traverse browser history or read the address bar.

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

export type SiteKind =
	| 'fixed' // handled in an earlier commit of this PR
	| 'not-applicable' // cannot run under an open Chat, leaves the app, or only reads a stable part
	| 'deferred' // chatSurface.deferPageWrite: waits for Chat to close
	| 'close-then' // chatSurface.closeThen: closes Chat first (the UI derives from the URL)
	| 'after-close' // chatSurface.runAfterClose: a heal that cannot wait, never dropped
	| 'page-location' // reads the page beneath Chat through utils/pageLocation or a registered provider
	| 'own'; // the surface or the helper itself

export type Site = {
	/** Path relative to web/src. */
	file: string;
	/** The trimmed source line. */
	text: string;
	/** How many identical lines in that file this row covers. */
	n: number;
	kind: SiteKind;
	/** One line: why this site cannot corrupt Chat's history entries, or how it is handled. */
	why: string;
};

/** The population grep of the TASK-4315 dev-lead ruling, as one regex. */
export const SITE_RX =
	/\bgoto\(|replaceState|pushState|history\.(go|back|forward)\(|window\.location|\blocation\.(href|search|pathname|hash|assign|replace|reload|origin)/;

export type Found = { file: string; line: number; text: string };

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) walk(p, out);
		else if (/\.(svelte|ts)$/.test(name)) out.push(p);
	}
	return out;
}

/** Executable matches in non-test sources under `src`; full-line comments and imports are skipped. */
export function scanSites(src: string): Found[] {
	const found: Found[] = [];
	for (const p of walk(src)) {
		const file = relative(src, p).split('\\').join('/');
		if (file.startsWith('test/') || /\.(test|spec)\./.test(file)) continue;
		let block: 'c' | 'h' | null = null;
		readFileSync(p, 'utf8')
			.split('\n')
			.forEach((raw, i) => {
				const t = raw.trim();
				let comment = false;
				if (block === 'h') {
					comment = true;
					if (t.includes('-->')) block = null;
				} else if (block === 'c') {
					comment = true;
					if (t.includes('*/')) block = null;
				} else if (t.startsWith('<!--')) {
					comment = true;
					if (!t.includes('-->')) block = 'h';
				} else if (t.startsWith('/*')) {
					comment = true;
					if (!t.includes('*/')) block = 'c';
				} else if (t.startsWith('//') || t.startsWith('*') || t.startsWith('import ')) {
					comment = true;
				}
				if (!comment && SITE_RX.test(raw)) found.push({ file, line: i + 1, text: t });
			});
	}
	return found;
}
