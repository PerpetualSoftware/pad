#!/usr/bin/env node
// Renders the GitHub social preview for PerpetualSoftware/pad (TASK-3514).
//
//   node docs/brand/social/render.mjs            # both versions
//   node docs/brand/social/render.mjs a          # durable facts, no counts
//   node docs/brand/social/render.mjs b          # a snapshot of the counts
//
// Writes docs/brand/social/pad-social-{a,b}.png at 1280x640, GitHub's
// recommended size. Upload one under the repo's Settings › Social preview
// (owner only; GitHub has no API for it). An uploaded image is static, so
// version B's counts are as of the render: re-run this and re-upload to
// refresh them.
//
// Needs the `gh` CLI signed in (it reads the languages and counts from the
// GitHub API) and web/node_modules (`npm ci` in web/) for Playwright.

import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..', '..', '..');
const REPO = 'PerpetualSoftware/pad';
const require = createRequire(join(ROOT, 'web', 'package.json'));
const { chromium } = require('@playwright/test');

const gh = (...args) => JSON.parse(execFileSync('gh', ['api', ...args], { encoding: 'utf8' }));

// GitHub's linguist colours for the languages this repo has (the bar is the
// only colour on GitHub's own card); anything else is grey.
const LANG_COLOURS = {
	Go: '#00ADD8', TypeScript: '#3178c6', Svelte: '#ff3e00', Shell: '#89e051', JavaScript: '#f1e05a',
	CSS: '#663399', PLpgSQL: '#336790', Makefile: '#427819', Python: '#3572A5', Nix: '#7e7eff',
	Dockerfile: '#384d54', HTML: '#e34c26'
};

function languageBar() {
	const langs = gh(`repos/${REPO}/languages`);
	const total = Object.values(langs).reduce((a, b) => a + b, 0);
	return Object.entries(langs)
		.map(([name, bytes]) => `<span style="flex:${bytes / total};background:${LANG_COLOURS[name] ?? '#bbb'}"></span>`)
		.join('');
}

function counts() {
	const r = gh(`repos/${REPO}`);
	const q = gh('graphql', '-f', `query={repository(owner:"PerpetualSoftware",name:"pad"){discussions{totalCount} issues(states:OPEN){totalCount}}}`).data.repository;
	const contributors = gh(`repos/${REPO}/contributors?per_page=100&anon=1`).length;
	return { contributors, issues: q.issues.totalCount, discussions: q.discussions.totalCount, stars: r.stargazers_count, forks: r.forks_count };
}

// Stroke icons in the weight of GitHub's card.
const I = (d) => `<svg viewBox="0 0 24 24" width="34" height="34" fill="none" stroke="#57606a" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${d}</svg>`;
const ICON = {
	people: I('<circle cx="9" cy="8" r="3.2"/><path d="M3 19c0-3.3 2.7-5.5 6-5.5s6 2.2 6 5.5"/><circle cx="17" cy="9" r="2.5"/><path d="M16 13.6c2.7.1 5 2 5 5"/>'),
	issue: I('<circle cx="12" cy="12" r="8.5"/><circle cx="12" cy="12" r="1.5" fill="#57606a"/>'),
	discussion: I('<path d="M4 5h11v8H8l-4 3z"/><path d="M15 9h5v8l-3-2h-6v-2"/>'),
	star: I('<path d="M12 3.5l2.6 5.3 5.9.9-4.3 4.1 1 5.8-5.2-2.7-5.2 2.7 1-5.8-4.3-4.1 5.9-.9z"/>'),
	fork: I('<circle cx="6" cy="5" r="2"/><circle cx="18" cy="5" r="2"/><circle cx="12" cy="19" r="2"/><path d="M6 7v2a3 3 0 0 0 3 3h6a3 3 0 0 0 3-3V7M12 12v5"/>'),
	law: I('<path d="M12 4v16M5 20h14M5 7h14M7 7l-3 6a3 3 0 0 0 6 0zM17 7l-3 6a3 3 0 0 0 6 0z"/>'),
	code: I('<path d="M8 7l-5 5 5 5M16 7l5 5-5 5"/>'),
	server: I('<rect x="4" y="4" width="16" height="7" rx="1.5"/><rect x="4" y="13" width="16" height="7" rx="1.5"/><path d="M8 7.5h.01M8 16.5h.01"/>'),
	plug: I('<path d="M9 3v5M15 3v5M6 8h12v3a6 6 0 0 1-12 0zM12 17v4"/>')
};

const metric = (icon, value, label) =>
	`<div class="m"><div class="mv">${icon}<b>${value}</b></div><div class="ml">${label}</div></div>`;

function card(mode) {
	const mark = readFileSync(join(ROOT, 'docs/brand/mark/pad-app-icon-shaped.svg'), 'utf8');
	let row;
	if (mode === 'a') {
		row = [
			metric(ICON.law, 'Apache-2.0', 'License'),
			metric(ICON.code, 'Go + Svelte', 'One binary'),
			metric(ICON.server, 'Self-host', 'or Pad Cloud'),
			metric(ICON.plug, 'MCP + CLI', 'For your agents')
		].join('');
	} else {
		const c = counts();
		row = [
			metric(ICON.people, c.contributors, 'Contributors'),
			metric(ICON.issue, c.issues, c.issues === 1 ? 'Issue' : 'Issues'),
			metric(ICON.discussion, c.discussions, c.discussions === 1 ? 'Discussion' : 'Discussions'),
			metric(ICON.star, c.stars, 'Stars'),
			metric(ICON.fork, c.forks, 'Forks')
		].join('');
	}
	return `<!doctype html><meta charset="utf-8"><style>
html,body{margin:0}
body{width:1280px;height:640px;background:#fff;color:#1f2328;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Helvetica Neue",Arial,sans-serif;position:relative;overflow:hidden}
.t{position:absolute;left:85px;top:92px;width:780px;font-size:70px;line-height:1.13;letter-spacing:-.01em}
.t b{font-weight:700}
.d{position:absolute;left:85px;top:345px;width:900px;font-size:32px;color:#57606a}
.av{position:absolute;right:85px;top:85px;width:212px;height:212px}
.av svg{width:212px;height:212px;display:block}
.row{position:absolute;left:85px;bottom:70px;display:flex;gap:${mode === 'a' ? 62 : 52}px}
.m .mv{display:flex;align-items:center;gap:12px}
.m b{font-size:31px;font-weight:600}
.m .ml{margin:4px 0 0 46px;font-size:23px;color:#57606a}
.gh{position:absolute;right:85px;bottom:78px}
.bar{position:absolute;left:0;right:0;bottom:0;height:17px;display:flex}
</style><body>
<div class="t">PerpetualSoftware/<br><b>pad</b></div>
<div class="d">Project management for the agent era</div>
<div class="av">${mark}</div>
<div class="row">${row}</div>
<svg class="gh" width="46" height="46" viewBox="0 0 16 16" fill="#57606a"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>
<div class="bar">${languageBar()}</div>
</body>`;
}

const modes = process.argv[2] ? [process.argv[2]] : ['a', 'b'];
const browser = await chromium.launch();
try {
	const page = await browser.newPage({ viewport: { width: 1280, height: 640 }, deviceScaleFactor: 1 });
	const dir = mkdtempSync(join(tmpdir(), 'pad-social-'));
	for (const mode of modes) {
		if (mode !== 'a' && mode !== 'b') throw new Error(`unknown version "${mode}": use a or b`);
		const file = join(dir, `card-${mode}.html`);
		writeFileSync(file, card(mode));
		await page.goto('file://' + file);
		const out = join(HERE, `pad-social-${mode}.png`);
		await page.screenshot({ path: out });
		console.log('wrote', out);
	}
} finally {
	await browser.close();
}
